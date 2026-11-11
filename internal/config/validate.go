package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/aryanpappu/patchbot/internal/secret"
)

var (
	// Upper case only: it is the convention, and it keeps most pasted keys
	// from passing as a variable name and being printed back.
	envNameRE      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	providerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	loginRE        = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)
	repoRE         = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}$`)
	commandIDRE    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// shellMeta are the characters a shell gives meaning to. argv is run without
// a shell, so one of these in the program name is a mistake or an attempt to
// smuggle in a shell command line.
const shellMeta = " \t\r\n|&;<>()$`\\\"'*?[]{}~!#=%^"

// Validate checks the values of a loaded configuration and returns every
// problem it finds, joined with errors.Join. Messages never contain a secret.
//
// A message may quote a bad value to help the user find it. Every message is
// therefore redacted with the secrets the config holds: a key exported under
// the wrong variable name would otherwise be printed as, say, a log level.
func (c *Config) Validate() error {
	v := validator{redactor: c.Redactor()}
	c.validateDefaults(&v)
	c.validateLimits(&v)
	c.validateFixed(&v)
	for _, name := range sortedKeys(c.Providers) {
		c.validateProvider(&v, name, c.Providers[name])
	}
	for _, name := range sortedKeys(c.Repos) {
		validateRepo(&v, name, c.Repos[name])
	}
	return errors.Join(v.errs...)
}

type validator struct {
	errs     []error
	redactor *secret.Redactor
}

func (v *validator) addf(format string, args ...any) {
	v.errs = append(v.errs, errors.New(v.redactor.Redact(fmt.Sprintf(format, args...))))
}

// check adds the message when ok is false.
func (v *validator) check(ok bool, format string, args ...any) {
	if !ok {
		v.addf(format, args...)
	}
}

func (c *Config) validateDefaults(v *validator) {
	d := c.Defaults
	v.check(slices.Contains([]string{ModeFix, ModeRefine, ModeInvestigate}, d.Mode),
		"defaults.mode: %q is not one of fix, refine, investigate", d.Mode)

	if len(d.Approvers) == 0 {
		v.addf("defaults.approvers: list at least one GitHub login; coding starts only after one of them approves the spec")
	}
	for _, login := range d.Approvers {
		v.check(loginRE.MatchString(login), "defaults.approvers: %q is not a GitHub login", login)
	}

	_, ok := c.Providers[d.Provider]
	switch {
	case d.Provider == "":
		v.addf("defaults.provider: not set")
	case !ok:
		v.addf("defaults.provider: no [providers.%s] table; configured providers: %s",
			d.Provider, orNone(sortedKeys(c.Providers)))
	default:
		for _, stage := range []string{"refine", "code"} {
			if c.Model(stage) == "" {
				v.addf("no model for the %s stage: set defaults.%s_model or providers.%s.default_model "+
					"(`patchbot models --provider %s` lists the choices)", stage, stage, d.Provider, d.Provider)
			}
		}
	}

	if !validMoney(d.MaxCostUSD) || d.MaxCostUSD <= 0 {
		v.addf("defaults.max_cost_usd: must be greater than 0")
	} else if validMoney(c.Limits.HardMaxCostUSD) && d.MaxCostUSD > c.Limits.HardMaxCostUSD {
		v.addf("defaults.max_cost_usd (%v) is above limits.hard_max_cost_usd (%v)",
			d.MaxCostUSD, c.Limits.HardMaxCostUSD)
	}
}

func (c *Config) validateLimits(v *validator) {
	l := c.Limits
	v.check(validMoney(l.HardMaxCostUSD) && l.HardMaxCostUSD > 0, "limits.hard_max_cost_usd: must be greater than 0")
	for _, f := range []struct {
		key   string
		value int64
	}{
		{"limits.max_turns", int64(l.MaxTurns)},
		{"limits.refine_max_turns", int64(l.RefineMaxTurns)},
		{"limits.max_output_tokens", int64(l.MaxOutputTokens)},
		{"limits.max_total_tokens", l.MaxTotalTokens},
		{"limits.max_test_attempts", int64(l.MaxTestAttempts)},
		{"limits.run_timeout", int64(l.RunTimeout)},
		{"limits.cmd_timeout", int64(l.CmdTimeout)},
		{"limits.test_timeout", int64(l.TestTimeout)},
		{"limits.cmd_output_bytes", int64(l.CmdOutputBytes)},
		{"limits.cmd_store_bytes", int64(l.CmdStoreBytes)},
		{"approval.poll_interval", int64(c.Approval.PollInterval)},
		{"approval.ttl", int64(c.Approval.TTL)},
		{"approval.max_revisions", int64(c.Approval.MaxRevisions)},
		{"server.max_concurrent_runs", int64(c.Server.MaxConcurrentRuns)},
		{"server.max_queued_runs", int64(c.Server.MaxQueuedRuns)},
	} {
		v.check(f.value > 0, "%s: must be greater than 0", f.key)
	}
	v.check(l.CmdOutputBytes <= l.CmdStoreBytes || l.CmdStoreBytes <= 0,
		"limits.cmd_output_bytes (%d) is above limits.cmd_store_bytes (%d)", l.CmdOutputBytes, l.CmdStoreBytes)
}

func (c *Config) validateFixed(v *validator) {
	v.check(strings.TrimSpace(c.Approval.LocalActor) != "", "approval.local_actor: not set")
	v.check(slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level),
		"log.level: %q is not one of debug, info, warn, error", c.Log.Level)
	v.check(slices.Contains([]string{"text", "json"}, c.Log.Format),
		"log.format: %q is not one of text, json", c.Log.Format)
	v.check(slices.Contains([]string{"cdn", "local"}, c.Server.VueSrc),
		"server.vue_src: %q is not one of cdn, local", c.Server.VueSrc)
	if host, port, err := net.SplitHostPort(c.Server.Addr); err != nil || host == "" || port == "" {
		v.addf("server.addr: %q is not host:port", c.Server.Addr)
	}
	v.check(strings.TrimSpace(c.Data.Dir) != "", "data.dir: not set")
}

func (c *Config) validateProvider(v *validator, name string, p *Provider) {
	at := "providers." + name
	v.check(providerNameRE.MatchString(name),
		"%s: a provider name uses lower-case letters, digits, - and _", at)
	v.check(slices.Contains([]string{KindAnthropic, KindGemini, KindOpenAI}, p.Kind),
		"%s.kind: %q is not one of anthropic, gemini, openai", at, p.Kind)

	if p.APIKeyEnv != "" && !envNameRE.MatchString(p.APIKeyEnv) {
		// The value is not echoed: the usual cause is a key pasted here.
		v.addf("%s.api_key_env: must be the name of an environment variable (upper-case letters, digits "+
			"and _), not the key itself", at)
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			v.addf("%s.base_url: must be an http or https URL", at)
		case u.User != nil:
			v.addf("%s.base_url: must not contain a user name or password", at)
		case u.RawQuery != "" || u.Fragment != "":
			v.addf("%s.base_url: must not contain a query or fragment", at)
		}
	}
	if p.Kind == KindOpenAI && p.BaseURL == "" {
		v.addf("%s.base_url: required for kind openai (for example https://api.openai.com/v1)", at)
	}

	for _, model := range p.Models {
		v.check(validModelID(model), "%s.models: %q is not a model ID", at, model)
	}
	if p.DefaultModel != "" {
		v.check(validModelID(p.DefaultModel), "%s.default_model: %q is not a model ID", at, p.DefaultModel)
	}
	if p.PriceFree && len(p.Prices) > 0 {
		v.addf("%s: price_free = true and a prices table contradict each other; keep one", at)
	}
	for _, model := range sortedKeys(p.Prices) {
		price := p.Prices[model]
		pat := fmt.Sprintf("%s.prices.%s", at, model)
		v.check(validModelID(model), "%s: %q is not a model ID", pat, model)
		for _, rate := range []struct {
			name  string
			value *float64
		}{{"input", price.Input}, {"output", price.Output}} {
			switch {
			case rate.value == nil:
				v.addf("%s.%s: missing; a price needs both input and output, and a rate that is "+
					"really free must be written as 0", pat, rate.name)
			case !validMoney(*rate.value):
				v.addf("%s.%s: must be 0 or more", pat, rate.name)
			}
		}
		v.check(price.CacheRead == nil || validMoney(*price.CacheRead), "%s.cache_read: must be 0 or more", pat)
		v.check(price.CacheWrite == nil || validMoney(*price.CacheWrite), "%s.cache_write: must be 0 or more", pat)
	}
}

func validateRepo(v *validator, name string, r *Repo) {
	at := fmt.Sprintf("repos.%q", name)
	_, repo, _ := strings.Cut(name, "/")
	if !repoRE.MatchString(name) || repo == "." || repo == ".." {
		v.addf("%s: a repository key looks like \"owner/name\"", at)
	}
	for _, id := range sortedKeys(r.Commands) {
		argv := r.Commands[id]
		cat := fmt.Sprintf("%s.commands.%s", at, id)
		v.check(commandIDRE.MatchString(id),
			"%s: a command ID uses lower-case letters, digits, - and _, and starts with a letter", cat)
		switch {
		case len(argv) == 0:
			v.addf("%s: must be a non-empty argv array such as [\"go\", \"test\", \"./...\"]", cat)
			continue
		case argv[0] == "":
			v.addf("%s: the program name is empty", cat)
		case strings.HasPrefix(argv[0], "-"):
			v.addf("%s: the program name %q starts with a dash", cat, argv[0])
		case strings.ContainsAny(argv[0], shellMeta) || strings.ContainsFunc(argv[0], isControl):
			v.addf("%s: the program name %q contains a shell metacharacter; commands run without a shell, "+
				"so give the program and each argument as separate array items", cat, argv[0])
		}
		for i, arg := range argv {
			v.check(!strings.ContainsRune(arg, 0), "%s: argument %d contains a NUL byte", cat, i)
		}
	}
	if r.TestCommand != "" {
		if _, ok := r.Commands[r.TestCommand]; !ok {
			v.addf("%s.test_command: no command %q in commands (have: %s)",
				at, r.TestCommand, orNone(sortedKeys(r.Commands)))
		}
	}
	for _, path := range r.Protected {
		v.check(validProtectedPath(path),
			"%s.protected: %q must be a relative path inside the repository, with no \"..\"", at, path)
	}
}

// Warnings returns problems that do not stop patchbot from starting: a model
// with no price, an API key variable that is not set, and the like. A warning
// names the environment variable and never its value.
func (c *Config) Warnings() []string {
	var out []string
	redactor := c.Redactor()
	warnf := func(format string, args ...any) {
		out = append(out, redactor.Redact(fmt.Sprintf(format, args...)))
	}

	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		switch {
		case p.APIKeyEnv == "" && p.Kind != KindOpenAI:
			warnf("provider %q: api_key_env is not set, so requests carry no API key", name)
		case p.APIKeyEnv != "" && envNameRE.MatchString(p.APIKeyEnv) && !p.APIKey.IsSet():
			warnf("provider %q: environment variable %s is not set; the provider cannot be used until it is",
				name, p.APIKeyEnv)
		}
		if p.PriceFree {
			continue
		}
		for _, model := range c.providerModels(name) {
			price, ok := p.Prices[model]
			switch {
			case !ok:
				warnf("provider %q: model %q has no price, so its cost is unknown and only the token limit applies; "+
					"add [providers.%s.prices.%q] or set price_free = true", name, model, name, model)
			case price.Checked.IsZero():
				warnf("provider %q: the price of model %q has no checked date", name, model)
			}
		}
	}

	if len(c.Repos) == 0 {
		warnf(`no [repos."owner/name"] table: no run can start until a repository is listed`)
	}
	for _, name := range sortedKeys(c.Repos) {
		if _, ok := c.Repos[name].TestCommandID(); !ok {
			warnf("repo %q: no test command, so its pull requests will be drafts marked unverified", name)
		}
	}
	if host, _, err := net.SplitHostPort(c.Server.Addr); err == nil && !isLoopback(host) {
		warnf("server.addr %q is not a loopback address; patchbot serve will require PATCHBOT_UI_TOKEN", c.Server.Addr)
	}
	return out
}

// providerModels returns the models a provider is expected to run: its list,
// its default and, for the default provider, the two stage models.
func (c *Config) providerModels(name string) []string {
	p := c.Providers[name]
	models := slices.Clone(p.Models)
	add := func(model string) {
		if model != "" && !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	add(p.DefaultModel)
	if name == c.Defaults.Provider {
		add(c.Defaults.RefineModel)
		add(c.Defaults.CodeModel)
	}
	return models
}

func validMoney(f float64) bool {
	return f >= 0 && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func validModelID(id string) bool {
	return id != "" && len(id) <= 200 && !strings.ContainsFunc(id, func(r rune) bool {
		return isControl(r) || r == ' '
	})
}

func validProtectedPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) || strings.Contains(path, `\`) {
		return false
	}
	for seg := range strings.SplitSeq(strings.TrimSuffix(path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
