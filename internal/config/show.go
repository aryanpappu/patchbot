package config

import (
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// WriteEffective prints the effective configuration as TOML, with the source
// of every value in a trailing comment. Secrets are never printed: each API
// key appears as a comment that says only whether it is set.
func (c *Config) WriteEffective(w io.Writer) error {
	p := &printer{sources: c.sources, redactor: secret.NewRedactor()}
	p.redactor.AddSecrets(c.Secrets()...)

	if c.path == "" {
		p.linef("# config file: none found (built-in defaults)")
	} else {
		p.linef("# config file: %s", c.path)
	}
	p.table(nil, reflect.ValueOf(c).Elem())
	_, err := io.WriteString(w, p.redactor.Redact(p.b.String()))
	return err
}

type printer struct {
	b        strings.Builder
	sources  map[string]Source
	redactor *secret.Redactor
}

func (p *printer) linef(format string, args ...any) {
	fmt.Fprintf(&p.b, format, args...)
	p.b.WriteByte('\n')
}

type entry struct {
	key   toml.Key
	value reflect.Value
}

// table prints one table: its own values first, then its sub-tables.
func (p *printer) table(key toml.Key, v reflect.Value) {
	var leaves, tables []entry
	add := func(name string, value reflect.Value) {
		e := entry{key: append(key[:len(key):len(key)], name), value: value}
		if isTable(value) {
			tables = append(tables, e)
		} else {
			leaves = append(leaves, e)
		}
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ",")
			if name == "" || name == "-" {
				continue
			}
			add(name, v.Field(i))
		}
	case reflect.Map:
		for _, name := range sortedKeys(mapOfValues(v)) {
			add(name, v.MapIndex(reflect.ValueOf(name)))
		}
	}

	if v.Kind() == reflect.Map && v.Len() == 0 {
		return
	}
	provider, isProvider := v.Interface().(Provider)
	if len(key) > 0 && (len(leaves) > 0 || len(tables) == 0) {
		p.linef("")
		p.linef("[%s]", key)
	}
	width := 0
	rendered := make([]string, len(leaves))
	for i, leaf := range leaves {
		text, ok := formatValue(leaf.value)
		if !ok {
			continue
		}
		name := leaf.key[len(leaf.key)-1]
		if isProvider && name == "api_key_env" && provider.APIKeyEnv != "" && !envNameRE.MatchString(provider.APIKeyEnv) {
			text = strconv.Quote(secret.Mask) // most likely a key pasted into the wrong field
		}
		if isProvider && name == "base_url" {
			text = strconv.Quote(displayURL(provider.BaseURL))
		}
		rendered[i] = fmt.Sprintf("%s = %s", toml.Key{name}, text)
		width = max(width, len(rendered[i]))
	}
	for i, leaf := range leaves {
		if rendered[i] != "" {
			p.linef("%-*s  # %s", width, rendered[i], p.sources[leaf.key.String()])
		}
	}
	if isProvider {
		p.linef("%s", apiKeyStatus(provider))
	}
	for _, t := range tables {
		value := t.value
		if value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
		p.table(t.key, value)
	}
}

// displayURL returns a URL without the parts that can carry credentials: the
// user name and password, the query and the fragment. Validate rejects a
// base_url that has them, but config show prints invalid configs too, and
// that is exactly when such a URL is on screen. A URL that does not parse is
// masked whole.
func displayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return secret.Mask
	}
	clean := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	text := clean.String()
	if u.User != nil {
		text += " (credentials removed)"
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		text += " (query removed)"
	}
	return text
}

func apiKeyStatus(p Provider) string {
	switch {
	case p.APIKeyEnv == "":
		return "# api key: none (api_key_env is not set)"
	case !envNameRE.MatchString(p.APIKeyEnv):
		return "# api key: not read (api_key_env is not a variable name)"
	case p.APIKey.IsSet():
		return fmt.Sprintf("# api key: %s (set in $%s)", secret.Mask, p.APIKeyEnv)
	default:
		return fmt.Sprintf("# api key: not set ($%s is empty)", p.APIKeyEnv)
	}
}

func mapOfValues(v reflect.Value) map[string]reflect.Value {
	out := make(map[string]reflect.Value, v.Len())
	for iter := v.MapRange(); iter.Next(); {
		out[iter.Key().String()] = iter.Value()
	}
	return out
}

var dateType = reflect.TypeFor[Date]()

// isTable reports whether v prints as a TOML table and not as a value.
func isTable(v reflect.Value) bool {
	t := v.Type()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return t != dateType
	case reflect.Map:
		return true
	default:
		return false
	}
}

// formatValue renders a value as TOML. It reports false for an optional value
// that is not set, which has no TOML form.
func formatValue(v reflect.Value) (string, bool) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	switch x := v.Interface().(type) {
	case string:
		return strconv.Quote(x), true
	case bool:
		return strconv.FormatBool(x), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.ContainsAny(s, ".eEIN") {
			s += ".0"
		}
		return s, true
	case time.Duration:
		return strconv.Quote(x.String()), true
	case Date:
		if x.IsZero() {
			return "", false
		}
		return x.String(), true
	case []string:
		quoted := make([]string, len(x))
		for i, item := range x {
			quoted[i] = strconv.Quote(item)
		}
		return "[" + strings.Join(quoted, ", ") + "]", true
	default:
		return strconv.Quote(fmt.Sprint(x)), true
	}
}
