package config

import (
	_ "embed" // for defaults.toml
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aryanpappu/patchbot/internal/secret"
)

//go:embed defaults.toml
var defaultsTOML string

// EnvConfig names the environment variable that points at a config file.
const EnvConfig = "PATCHBOT_CONFIG"

// SourceKind says which layer set a value.
type SourceKind int

// The layers, lowest precedence first.
const (
	SourceUnset SourceKind = iota // no layer set the value; it is the zero value
	SourceDefault
	SourceFile
	SourceEnv
	SourceFlag
)

// Source says where a config value came from.
type Source struct {
	Kind SourceKind
	// Name is the environment variable or the flag, for those two kinds.
	Name string
}

// String returns a short label such as "file" or "env PATCHBOT_PROVIDER".
func (s Source) String() string {
	switch s.Kind {
	case SourceDefault:
		return "default"
	case SourceFile:
		return "file"
	case SourceEnv:
		return "env " + s.Name
	case SourceFlag:
		return "flag " + s.Name
	default:
		return "unset"
	}
}

// Override is one value set on the command line.
type Override struct {
	// Key is a dotted config key in a fixed table, such as "defaults.provider".
	Key string
	// Value is the flag's text. Lists are comma-separated.
	Value string
	// Flag is the flag as the user typed it, such as "--provider".
	Flag string
}

// Options tell Load where to look. The zero value reads the real environment.
type Options struct {
	// Path is the --config flag. A file named here must exist.
	Path string
	// Dir is where ./patchbot.toml is looked for. Empty means the working directory.
	Dir string
	// Flags are the command-line overrides, the highest layer.
	Flags []Override
	// LookupEnv replaces os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// UserConfigDir replaces os.UserConfigDir.
	UserConfigDir func() (string, error)
	// UserHomeDir replaces os.UserHomeDir.
	UserHomeDir func() (string, error)
}

// envBindings is the complete list of settings an environment variable can
// change. Secrets are not here: they are named by api_key_env.
var envBindings = []struct {
	env  string
	keys []string
}{
	{"PATCHBOT_DATA_DIR", []string{"data.dir"}},
	{"PATCHBOT_PROVIDER", []string{"defaults.provider"}},
	// PATCHBOT_MODEL sets both stages; the two specific variables win over it.
	{"PATCHBOT_MODEL", []string{"defaults.refine_model", "defaults.code_model"}},
	{"PATCHBOT_REFINE_MODEL", []string{"defaults.refine_model"}},
	{"PATCHBOT_CODE_MODEL", []string{"defaults.code_model"}},
	{"PATCHBOT_MAX_COST_USD", []string{"defaults.max_cost_usd"}},
	{"PATCHBOT_MAX_TURNS", []string{"limits.max_turns"}},
	{"PATCHBOT_ADDR", []string{"server.addr"}},
	{"PATCHBOT_LOG_LEVEL", []string{"log.level"}},
	{"PATCHBOT_LOG_FORMAT", []string{"log.format"}},
}

// Default returns the embedded defaults with no file, environment or flags.
func Default() *Config {
	c := &Config{sources: map[string]Source{}}
	var layer Config
	md, err := toml.Decode(defaultsTOML, &layer)
	if err == nil {
		err = checkKeys(md)
	}
	if err == nil {
		err = c.apply(&layer, md, Source{Kind: SourceDefault})
	}
	if err != nil {
		// The file is compiled in, so this is a bug that the tests catch.
		panic("config: embedded defaults.toml is invalid: " + err.Error())
	}
	return c
}

// Load builds the effective configuration: defaults, then the config file,
// then environment variables, then flags. It reports syntax errors, unknown
// keys and keys that look like secrets. It does not judge the values; call
// Validate for that.
//
// An error from Load never quotes a value it could not parse, since nothing
// says the value is not a secret in the wrong place, and it is redacted with
// every API key the loader could find.
func Load(opts Options) (*Config, error) {
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	c, err := load(opts, lookupEnv)
	// The keys are read even when loading failed, to redact the error.
	for _, p := range c.Providers {
		if !envNameRE.MatchString(p.APIKeyEnv) {
			continue // Validate reports a bad name; never look it up.
		}
		if value, ok := lookupEnv(p.APIKeyEnv); ok {
			p.APIKey = secret.New(strings.TrimSpace(value))
		}
	}
	if err != nil {
		// A file that was rejected never became part of c, but the variable
		// names it gave still say where its keys are.
		redactor := c.Redactor()
		for _, name := range c.rejectedKeyEnvs {
			if value, ok := lookupEnv(name); ok && envNameRE.MatchString(name) {
				redactor.Add(strings.TrimSpace(value))
			}
		}
		return nil, &redactedError{msg: redactor.Redact(err.Error()), err: err}
	}
	return c, nil
}

// redactedError carries a redacted message and keeps the original error
// reachable for errors.Is and errors.As.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// load applies the layers. It always returns a Config, complete or not.
func load(opts Options, lookupEnv func(string) (string, bool)) (*Config, error) {
	if opts.UserConfigDir == nil {
		opts.UserConfigDir = os.UserConfigDir
	}
	if opts.UserHomeDir == nil {
		opts.UserHomeDir = os.UserHomeDir
	}
	c := Default()
	if home, err := opts.UserHomeDir(); err == nil {
		c.home = home
	}

	path, err := findFile(opts, lookupEnv)
	if err != nil {
		return c, err
	}
	if path != "" {
		if err := c.loadFile(path); err != nil {
			return c, err
		}
	}

	for _, b := range envBindings {
		value, ok := lookupEnv(b.env)
		if !ok || value == "" {
			continue
		}
		for _, key := range b.keys {
			if err := c.set(key, value, Source{Kind: SourceEnv, Name: b.env}); err != nil {
				return c, fmt.Errorf("environment variable %s: %w", b.env, err)
			}
		}
	}
	for _, o := range opts.Flags {
		if err := c.set(o.Key, o.Value, Source{Kind: SourceFlag, Name: o.Flag}); err != nil {
			return c, fmt.Errorf("flag %s: %w", o.Flag, err)
		}
	}
	return c, nil
}

// Source returns where the value of a dotted key came from. Quote a segment
// that is not a bare TOML key: `repos."owner/name".test_command`.
func (c *Config) Source(key string) Source { return c.sources[key] }

// findFile returns the config file to read, or "" when there is none.
func findFile(opts Options, lookupEnv func(string) (string, bool)) (string, error) {
	explicit := opts.Path
	if explicit == "" {
		explicit, _ = lookupEnv(EnvConfig)
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return explicit, nil
	}

	candidates := []string{filepath.Join(opts.Dir, "patchbot.toml")}
	if dir, err := opts.UserConfigDir(); err == nil && dir != "" {
		candidates = append(candidates, filepath.Join(dir, "patchbot", "config.toml"))
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return "", fmt.Errorf("config file: %w", err)
		case info.Mode().IsRegular():
			return path, nil
		}
	}
	return "", nil
}

func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the user chooses the config file
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	var layer Config
	md, err := toml.Decode(string(data), &layer)
	for _, p := range layer.Providers {
		if p != nil && p.APIKeyEnv != "" {
			c.rejectedKeyEnvs = append(c.rejectedKeyEnvs, p.APIKeyEnv)
		}
	}
	if err != nil {
		return fmt.Errorf("config file %s: %s", path, describeDecodeError(err))
	}
	if err := checkKeys(md); err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	if err := c.apply(&layer, md, Source{Kind: SourceFile}); err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	c.path = path
	return nil
}

var (
	// tomlPrefixRE matches the "toml: line N (last key "k"): " prefix of the
	// library's plain errors.
	tomlPrefixRE = regexp.MustCompile(`^toml: (?:line (\d+) )?(?:\(last key "(?:[^"\\]|\\.)*"\))?:? ?`)
	// quotedRE matches a Go-quoted string, which is how the library quotes
	// text from the file.
	quotedRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
)

// describeDecodeError turns a TOML library error into a message that says
// where the problem is and what kind it is, without the text that caused it.
// The library quotes the offending value (`invalid duration: "..."`,
// `expected value but found "..."`) and the last key it read, and either can
// be a key pasted into the wrong place.
func describeDecodeError(err error) string {
	line, msg := 0, err.Error()
	if pe, ok := errors.AsType[toml.ParseError](err); ok {
		line, msg = pe.Position.Line, pe.Message
	} else if m := tomlPrefixRE.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
		msg = msg[len(m[0]):]
	}
	switch {
	case strings.HasPrefix(msg, "invalid duration"):
		msg = `want a duration string such as "30m"`
	case strings.Contains(msg, "is out of range"), strings.Contains(msg, "is out of the safe"):
		msg = "a number is out of range"
	case strings.HasPrefix(msg, "expected value but found"):
		msg = "expected a value; text needs quotes"
	default:
		msg = quotedRE.ReplaceAllString(msg, "a value")
	}
	msg = (*secret.Redactor)(nil).Redact(msg)
	if line > 0 {
		return fmt.Sprintf("line %d: %s", line, msg)
	}
	return msg
}

// checkKeys rejects keys that look like secrets and keys the schema does not
// have. The secret check comes first because its message says what to do.
func checkKeys(md toml.MetaData) error {
	var errs []error
	seen := map[string]bool{}
	for _, key := range md.Keys() {
		if name, ok := secretLookingSegment(key); ok && !covered(seen, key) {
			seen[key.String()] = true
			errs = append(errs, fmt.Errorf(
				"key %s: %q looks like a secret; secrets are read only from environment variables, "+
					"so name the variable with api_key_env and remove the value from the file", key, name))
		}
	}
	for _, key := range md.Undecoded() {
		if covered(seen, key) {
			continue
		}
		errs = append(errs, fmt.Errorf("unknown key %s", key))
	}
	return errors.Join(errs...)
}

// covered reports whether key or one of its parents is already reported.
func covered(seen map[string]bool, key toml.Key) bool {
	for i := 1; i <= len(key); i++ {
		if seen[key[:i].String()] {
			return true
		}
	}
	return false
}

// secretLookingSegment returns the first segment of key whose name suggests it
// holds a secret. Segments the user names freely (a provider, a repository, a
// model, a command ID) are not checked.
func secretLookingSegment(key toml.Key) (string, bool) {
	for i, seg := range key {
		if freeName(key, i) {
			continue
		}
		if looksSecret(seg) {
			return seg, true
		}
	}
	return "", false
}

func freeName(key toml.Key, i int) bool {
	switch i {
	case 1:
		return key[0] == "providers" || key[0] == "repos"
	case 3:
		return (key[0] == "providers" && key[2] == "prices") || (key[0] == "repos" && key[2] == "commands")
	}
	return false
}

// secretWords mark a key name as holding a secret wherever they appear in it,
// so github_token, apiKey and client-secret are all caught.
var secretWords = []string{
	"apikey", "api_key", "api-key", "password", "passwd", "secret", "credential",
	"private_key", "privatekey", "access_key", "accesskey",
}

// looksSecret reports whether a key name says its value is a secret. A name
// ending in _env holds the name of an environment variable, which is fine.
// "token" counts unless it is the plural counter, as in max_total_tokens.
func looksSecret(name string) bool {
	name = strings.ToLower(name)
	if strings.HasSuffix(name, "_env") {
		return false
	}
	for _, word := range secretWords {
		if strings.Contains(name, word) {
			return true
		}
	}
	for rest := name; ; {
		i := strings.Index(rest, "token")
		if i < 0 {
			return false
		}
		rest = rest[i+len("token"):]
		if !strings.HasPrefix(rest, "s") {
			return true
		}
	}
}

var durationType = reflect.TypeFor[time.Duration]()

// apply copies every value the layer defines into c and records its source.
// Copying key by key, not table by table, lets a file change one field of a
// provider the defaults already define.
func (c *Config) apply(layer *Config, md toml.MetaData, source Source) error {
	dst := reflect.ValueOf(c).Elem()
	src := reflect.ValueOf(layer).Elem()
	for _, key := range md.Keys() {
		if md.Type(key...) == "Hash" {
			if err := ensureTable(dst, key); err != nil {
				return fmt.Errorf("key %s: %w", key, err)
			}
			continue
		}
		value, ok := lookup(src, key)
		if !ok {
			return fmt.Errorf("key %s: not in the schema", key)
		}
		if value.Type() == durationType && md.Type(key...) != "String" {
			return fmt.Errorf(`key %s: want a duration string such as "30m"`, key)
		}
		if err := assign(dst, key, value); err != nil {
			return fmt.Errorf("key %s: %w", key, err)
		}
		c.sources[key.String()] = source
	}
	return nil
}

// set parses raw for the field at a dotted key and stores it.
func (c *Config) set(dotted, raw string, source Source) error {
	key := toml.Key(strings.Split(dotted, "."))
	field, ok := lookup(reflect.ValueOf(c).Elem(), key)
	if !ok || !field.CanSet() {
		return fmt.Errorf("unknown config key %q", dotted)
	}
	if err := parseInto(field, raw); err != nil {
		return fmt.Errorf("%s: %w", dotted, err)
	}
	c.sources[key.String()] = source
	return nil
}

// parseInto never quotes raw in an error: an override comes from the
// environment, and a value there may be a secret under the wrong name.
func parseInto(field reflect.Value, raw string) error {
	switch field.Interface().(type) {
	case string:
		field.SetString(raw)
	case bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("want true or false")
		}
		field.SetBool(v)
	case int, int64:
		v, err := strconv.ParseInt(raw, 10, field.Type().Bits())
		if err != nil {
			return errors.New("want a whole number")
		}
		field.SetInt(v)
	case float64:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return errors.New("want a number")
		}
		field.SetFloat(v)
	case time.Duration:
		v, err := time.ParseDuration(raw)
		if err != nil {
			return errors.New(`want a duration such as "30m"`)
		}
		field.SetInt(int64(v))
	case []string:
		var list []string
		for item := range strings.SplitSeq(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				list = append(list, item)
			}
		}
		field.Set(reflect.ValueOf(list))
	default:
		return fmt.Errorf("a value of type %s cannot be set this way", field.Type())
	}
	return nil
}

// fieldByTag returns the field of struct v whose toml tag is name.
func fieldByTag(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		if tag, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ","); tag == name && tag != "-" {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// lookup follows key through structs, pointers and maps.
func lookup(v reflect.Value, key toml.Key) (reflect.Value, bool) {
	for _, seg := range key {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Struct:
			f, ok := fieldByTag(v, seg)
			if !ok {
				return reflect.Value{}, false
			}
			v = f
		case reflect.Map:
			v = v.MapIndex(reflect.ValueOf(seg))
			if !v.IsValid() {
				return reflect.Value{}, false
			}
		default:
			return reflect.Value{}, false
		}
	}
	return v, true
}

// descend returns the container that holds the last segment of key, creating
// maps and table entries on the way.
func descend(v reflect.Value, key toml.Key) (reflect.Value, error) {
	for _, seg := range key[:len(key)-1] {
		next, err := child(v, seg)
		if err != nil {
			return reflect.Value{}, err
		}
		v = next
	}
	return v, nil
}

// child returns the table named seg inside v, creating it when v is a map.
func child(v reflect.Value, seg string) (reflect.Value, error) {
	switch v.Kind() {
	case reflect.Struct:
		f, ok := fieldByTag(v, seg)
		if !ok {
			return reflect.Value{}, fmt.Errorf("no table %q", seg)
		}
		return f, nil
	case reflect.Map:
		if v.Type().Elem().Kind() != reflect.Pointer {
			return reflect.Value{}, fmt.Errorf("%q is not a table", seg)
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		k := reflect.ValueOf(seg)
		elem := v.MapIndex(k)
		if !elem.IsValid() || elem.IsNil() {
			elem = reflect.New(v.Type().Elem().Elem())
			v.SetMapIndex(k, elem)
		}
		return elem.Elem(), nil
	default:
		return reflect.Value{}, fmt.Errorf("%q is not a table", seg)
	}
}

// ensureTable makes the table at key exist, so an empty [providers.x] still
// creates the provider.
func ensureTable(root reflect.Value, key toml.Key) error {
	parent, err := descend(root, key)
	if err != nil {
		return err
	}
	table, err := child(parent, key[len(key)-1])
	if err != nil {
		return err
	}
	if table.Kind() == reflect.Map && table.IsNil() {
		table.Set(reflect.MakeMap(table.Type()))
	}
	return nil
}

// assign stores value at key.
func assign(root reflect.Value, key toml.Key, value reflect.Value) error {
	parent, err := descend(root, key)
	if err != nil {
		return err
	}
	last := key[len(key)-1]
	switch parent.Kind() {
	case reflect.Struct:
		f, ok := fieldByTag(parent, last)
		if !ok {
			return fmt.Errorf("no key %q", last)
		}
		f.Set(value)
	case reflect.Map:
		if parent.IsNil() {
			parent.Set(reflect.MakeMap(parent.Type()))
		}
		parent.SetMapIndex(reflect.ValueOf(last), value)
	default:
		return fmt.Errorf("%q cannot hold a value", last)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
