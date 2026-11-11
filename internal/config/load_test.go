package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/config"
)

// env builds an Options that reads nothing from the real machine.
type env struct {
	dir     string // working directory
	userDir string // user config directory
	vars    map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{dir: t.TempDir(), userDir: t.TempDir(), vars: map[string]string{}}
}

func (e *env) options() config.Options {
	return config.Options{
		Dir: e.dir,
		LookupEnv: func(name string) (string, bool) {
			v, ok := e.vars[name]
			return v, ok
		},
		UserConfigDir: func() (string, error) { return e.userDir, nil },
		UserHomeDir:   func() (string, error) { return "/home/tester", nil },
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (e *env) local(t *testing.T, content string) string {
	t.Helper()
	return writeFile(t, filepath.Join(e.dir, "patchbot.toml"), content)
}

func mustLoad(t *testing.T, opts config.Options) *config.Config {
	t.Helper()
	c, err := config.Load(opts)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, newEnv(t).options())

	if c.Path() != "" {
		t.Errorf("Path() = %q, want no file", c.Path())
	}
	want := config.Limits{
		MaxTurns: 40, RefineMaxTurns: 20, MaxOutputTokens: 8192, MaxTotalTokens: 2_000_000,
		HardMaxCostUSD: 10, MaxTestAttempts: 3,
		RunTimeout: 30 * time.Minute, CmdTimeout: 2 * time.Minute, TestTimeout: 10 * time.Minute,
		CmdOutputBytes: 8 << 10, CmdStoreBytes: 256 << 10, BaselineTests: true,
	}
	if diff := cmp.Diff(want, c.Limits); diff != "" {
		t.Errorf("default limits (-want +got):\n%s", diff)
	}
	if c.Defaults.Provider != "anthropic" || c.Defaults.MaxCostUSD != 2 || c.Defaults.Mode != "fix" {
		t.Errorf("defaults = %+v", c.Defaults)
	}
	if c.Approval.TTL != 14*24*time.Hour || c.Approval.MaxRevisions != 5 || c.Approval.PollInterval != time.Minute {
		t.Errorf("approval = %+v", c.Approval)
	}
	if c.Server.MaxQueuedRuns != 16 {
		t.Errorf("server.max_queued_runs = %d, want 16", c.Server.MaxQueuedRuns)
	}
	if c.Server.Addr != "127.0.0.1:8080" {
		t.Errorf("server.addr = %q", c.Server.Addr)
	}

	// The only model names in the binary are the four seed Anthropic IDs.
	if diff := cmp.Diff([]string{"anthropic"}, keys(c.Providers)); diff != "" {
		t.Errorf("default providers (-want +got):\n%s", diff)
	}
	a := c.Providers["anthropic"]
	wantModels := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2"}
	if diff := cmp.Diff(wantModels, a.Models); diff != "" {
		t.Errorf("anthropic models (-want +got):\n%s", diff)
	}
	if a.DefaultModel != "claude-sonnet-5-5" || a.APIKeyEnv != "ANTHROPIC_API_KEY" || len(a.Prices) != 0 {
		t.Errorf("anthropic = %+v; want the sonnet default, the standard key variable and no shipped prices", a)
	}
	if c.Model("refine") != "claude-sonnet-5-5" || c.Model("code") != "claude-sonnet-5-5" {
		t.Errorf("stage models = %q, %q; want the provider default", c.Model("refine"), c.Model("code"))
	}
	if got := c.Source("limits.max_turns").String(); got != "default" {
		t.Errorf("source of limits.max_turns = %q, want default", got)
	}
	if got, want := c.DataDir(), filepath.Join("/home/tester", ".patchbot"); got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
	if got, want := c.DatabasePath(), filepath.Join("/home/tester", ".patchbot", "patchbot.sqlite"); got != want {
		t.Errorf("DatabasePath() = %q, want %q", got, want)
	}
}

// TestPrecedence is the precedence table: flag > env > file > default, for
// one key of each scalar type.
func TestPrecedence(t *testing.T) {
	t.Parallel()
	const file = `
[defaults]
provider = "from-file"
max_cost_usd = 3.5
[limits]
max_turns = 11
run_timeout = "11m"
[server]
addr = "127.0.0.1:1111"
`
	fileLayer, envLayer, flagLayer := true, true, true
	tests := []struct {
		name             string
		file, env, flags bool
		wantProvider     string
		wantCost         float64
		wantTurns        int
		wantAddr         string
		wantSource       string
	}{
		{name: "defaults only", wantProvider: "anthropic", wantCost: 2, wantTurns: 40, wantAddr: "127.0.0.1:8080", wantSource: "default"},
		{name: "file over default", file: fileLayer, wantProvider: "from-file", wantCost: 3.5, wantTurns: 11, wantAddr: "127.0.0.1:1111", wantSource: "file"},
		{name: "env over default", env: envLayer, wantProvider: "from-env", wantCost: 4.5, wantTurns: 22, wantAddr: "127.0.0.1:2222", wantSource: "env PATCHBOT_PROVIDER"},
		{name: "env over file", file: fileLayer, env: envLayer, wantProvider: "from-env", wantCost: 4.5, wantTurns: 22, wantAddr: "127.0.0.1:2222", wantSource: "env PATCHBOT_PROVIDER"},
		{name: "flag over default", flags: flagLayer, wantProvider: "from-flag", wantCost: 5.5, wantTurns: 33, wantAddr: "127.0.0.1:3333", wantSource: "flag --provider"},
		{name: "flag over file", file: fileLayer, flags: flagLayer, wantProvider: "from-flag", wantCost: 5.5, wantTurns: 33, wantAddr: "127.0.0.1:3333", wantSource: "flag --provider"},
		{name: "flag over env", env: envLayer, flags: flagLayer, wantProvider: "from-flag", wantCost: 5.5, wantTurns: 33, wantAddr: "127.0.0.1:3333", wantSource: "flag --provider"},
		{name: "flag over env over file", file: fileLayer, env: envLayer, flags: flagLayer, wantProvider: "from-flag", wantCost: 5.5, wantTurns: 33, wantAddr: "127.0.0.1:3333", wantSource: "flag --provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if tt.file {
				e.local(t, file)
			}
			if tt.env {
				e.vars["PATCHBOT_PROVIDER"] = "from-env"
				e.vars["PATCHBOT_MAX_COST_USD"] = "4.5"
				e.vars["PATCHBOT_MAX_TURNS"] = "22"
				e.vars["PATCHBOT_ADDR"] = "127.0.0.1:2222"
			}
			opts := e.options()
			if tt.flags {
				opts.Flags = []config.Override{
					{Key: "defaults.provider", Value: "from-flag", Flag: "--provider"},
					{Key: "defaults.max_cost_usd", Value: "5.5", Flag: "--max-cost"},
					{Key: "limits.max_turns", Value: "33", Flag: "--max-turns"},
					{Key: "server.addr", Value: "127.0.0.1:3333", Flag: "--addr"},
				}
			}
			c := mustLoad(t, opts)
			if c.Defaults.Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", c.Defaults.Provider, tt.wantProvider)
			}
			if c.Defaults.MaxCostUSD != tt.wantCost {
				t.Errorf("max_cost_usd = %v, want %v", c.Defaults.MaxCostUSD, tt.wantCost)
			}
			if c.Limits.MaxTurns != tt.wantTurns {
				t.Errorf("max_turns = %d, want %d", c.Limits.MaxTurns, tt.wantTurns)
			}
			if c.Server.Addr != tt.wantAddr {
				t.Errorf("addr = %q, want %q", c.Server.Addr, tt.wantAddr)
			}
			if got := c.Source("defaults.provider").String(); got != tt.wantSource {
				t.Errorf("source of defaults.provider = %q, want %q", got, tt.wantSource)
			}
			// A key no upper layer touched keeps its lower source.
			wantTimeout, wantTimeoutSource := 30*time.Minute, "default"
			if tt.file {
				wantTimeout, wantTimeoutSource = 11*time.Minute, "file"
			}
			if c.Limits.RunTimeout != wantTimeout || c.Source("limits.run_timeout").String() != wantTimeoutSource {
				t.Errorf("run_timeout = %v from %q, want %v from %q",
					c.Limits.RunTimeout, c.Source("limits.run_timeout"), wantTimeout, wantTimeoutSource)
			}
		})
	}
}

func TestEnvBindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		vars  map[string]string
		check func(t *testing.T, c *config.Config)
	}{
		{
			name: "model sets both stages",
			vars: map[string]string{"PATCHBOT_MODEL": "m-both"},
			check: func(t *testing.T, c *config.Config) {
				if c.Defaults.RefineModel != "m-both" || c.Defaults.CodeModel != "m-both" {
					t.Errorf("models = %q, %q", c.Defaults.RefineModel, c.Defaults.CodeModel)
				}
				if got := c.Source("defaults.code_model").String(); got != "env PATCHBOT_MODEL" {
					t.Errorf("source = %q", got)
				}
			},
		},
		{
			name: "stage variable wins over model",
			vars: map[string]string{"PATCHBOT_MODEL": "m-both", "PATCHBOT_REFINE_MODEL": "m-refine"},
			check: func(t *testing.T, c *config.Config) {
				if c.Defaults.RefineModel != "m-refine" || c.Defaults.CodeModel != "m-both" {
					t.Errorf("models = %q, %q", c.Defaults.RefineModel, c.Defaults.CodeModel)
				}
				if got := c.Source("defaults.refine_model").String(); got != "env PATCHBOT_REFINE_MODEL" {
					t.Errorf("source = %q", got)
				}
			},
		},
		{
			name: "data dir, log level and format",
			vars: map[string]string{"PATCHBOT_DATA_DIR": "/var/pb", "PATCHBOT_LOG_LEVEL": "debug", "PATCHBOT_LOG_FORMAT": "json"},
			check: func(t *testing.T, c *config.Config) {
				if c.DataDir() != "/var/pb" || c.Log.Level != "debug" || c.Log.Format != "json" {
					t.Errorf("data.dir = %q, log = %+v", c.DataDir(), c.Log)
				}
			},
		},
		{
			name: "empty variable is ignored",
			vars: map[string]string{"PATCHBOT_PROVIDER": ""},
			check: func(t *testing.T, c *config.Config) {
				if c.Defaults.Provider != "anthropic" || c.Source("defaults.provider").String() != "default" {
					t.Errorf("provider = %q from %q", c.Defaults.Provider, c.Source("defaults.provider"))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.vars = tt.vars
			tt.check(t, mustLoad(t, e.options()))
		})
	}
}

func TestBadOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		vars    map[string]string
		flags   []config.Override
		wantErr string
	}{
		{name: "env not a number", vars: map[string]string{"PATCHBOT_MAX_TURNS": "many"}, wantErr: "environment variable PATCHBOT_MAX_TURNS: limits.max_turns: want a whole number"},
		{name: "env not a float", vars: map[string]string{"PATCHBOT_MAX_COST_USD": "$2"}, wantErr: "PATCHBOT_MAX_COST_USD: defaults.max_cost_usd: want a number"},
		{name: "flag unknown key", flags: []config.Override{{Key: "defaults.nope", Value: "x", Flag: "--nope"}}, wantErr: `flag --nope: unknown config key "defaults.nope"`},
		{name: "flag into a table", flags: []config.Override{{Key: "defaults", Value: "x", Flag: "--defaults"}}, wantErr: "flag --defaults"},
		{name: "flag bad duration", flags: []config.Override{{Key: "limits.run_timeout", Value: "soon", Flag: "--timeout"}}, wantErr: "flag --timeout: limits.run_timeout: want a duration"},
		{name: "flag bad bool", flags: []config.Override{{Key: "limits.baseline_tests", Value: "maybe", Flag: "--baseline"}}, wantErr: "want true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.vars = tt.vars
			opts := e.options()
			opts.Flags = tt.flags
			_, err := config.Load(opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOverrideTypes(t *testing.T) {
	t.Parallel()
	opts := newEnv(t).options()
	opts.Flags = []config.Override{
		{Key: "limits.run_timeout", Value: "45m", Flag: "--timeout"},
		{Key: "limits.baseline_tests", Value: "false", Flag: "--baseline"},
		{Key: "limits.max_total_tokens", Value: "123456789012", Flag: "--max-tokens"},
		{Key: "defaults.approvers", Value: "alice, bob,,carol", Flag: "--approvers"},
	}
	c := mustLoad(t, opts)
	if c.Limits.RunTimeout != 45*time.Minute || c.Limits.BaselineTests || c.Limits.MaxTotalTokens != 123456789012 {
		t.Errorf("limits = %+v", c.Limits)
	}
	if diff := cmp.Diff([]string{"alice", "bob", "carol"}, c.Defaults.Approvers); diff != "" {
		t.Errorf("approvers (-want +got):\n%s", diff)
	}
}

func TestSearchOrder(t *testing.T) {
	t.Parallel()
	body := func(provider string) string { return "[defaults]\nprovider = \"" + provider + "\"\n" }
	tests := []struct {
		name string
		// which files exist
		flagFile, envFile, localFile, userFile bool
		want                                   string
	}{
		{name: "no file", want: "anthropic"},
		{name: "user config dir", userFile: true, want: "user"},
		{name: "local over user", localFile: true, userFile: true, want: "local"},
		{name: "PATCHBOT_CONFIG over local", envFile: true, localFile: true, userFile: true, want: "env"},
		{name: "--config over everything", flagFile: true, envFile: true, localFile: true, userFile: true, want: "flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			opts := e.options()
			var wantPath string
			if tt.userFile {
				wantPath = writeFile(t, filepath.Join(e.userDir, "patchbot", "config.toml"), body("user"))
			}
			if tt.localFile {
				wantPath = e.local(t, body("local"))
			}
			if tt.envFile {
				wantPath = writeFile(t, filepath.Join(t.TempDir(), "env.toml"), body("env"))
				e.vars[config.EnvConfig] = wantPath
			}
			if tt.flagFile {
				wantPath = writeFile(t, filepath.Join(t.TempDir(), "flag.toml"), body("flag"))
				opts.Path = wantPath
			}
			c := mustLoad(t, opts)
			if c.Defaults.Provider != tt.want {
				t.Errorf("provider = %q, want the %q file to win", c.Defaults.Provider, tt.want)
			}
			if c.Path() != wantPath {
				t.Errorf("Path() = %q, want %q", c.Path(), wantPath)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	t.Parallel()
	t.Run("named by flag", func(t *testing.T) {
		t.Parallel()
		opts := newEnv(t).options()
		opts.Path = filepath.Join(t.TempDir(), "absent.toml")
		if _, err := config.Load(opts); err == nil || !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("Load error = %v, want it to name the missing file", err)
		}
	})
	t.Run("named by environment", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.vars[config.EnvConfig] = filepath.Join(t.TempDir(), "absent.toml")
		if _, err := config.Load(e.options()); err == nil {
			t.Error("Load succeeded, want an error for a missing PATCHBOT_CONFIG file")
		}
	})
	t.Run("user config dir unknown", func(t *testing.T) {
		t.Parallel()
		opts := newEnv(t).options()
		opts.UserConfigDir = func() (string, error) { return "", os.ErrNotExist }
		opts.UserHomeDir = func() (string, error) { return "", os.ErrNotExist }
		c := mustLoad(t, opts)
		if c.DataDir() != "~/.patchbot" {
			t.Errorf("DataDir() = %q, want it left unexpanded without a home directory", c.DataDir())
		}
	})
}

func TestFileMergesWithDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.local(t, `
[defaults]
approvers = ["alice"]

[providers.anthropic]
default_model = "claude-opus-5-5"

[providers.anthropic.prices."claude-opus-5-5"]
input = 1.5
output = 7.25
cache_read = 0.15
checked = 2026-11-01

[providers.anthropic.prices."claude-haiku-5-5"]
input = 0.5
output = 1
checked = "2026-10-31"

[providers.gemini]
kind = "gemini"
api_key_env = "GEMINI_API_KEY"

[providers.empty]

[repos."owner/name"]
protected = ["migrations/"]

[repos."owner/name".commands]
test = ["go", "test", "./..."]
lint = ["go", "vet", "./..."]
`)
	c := mustLoad(t, e.options())

	a := c.Providers["anthropic"]
	// One key changed; the rest of the default table is kept.
	if a.DefaultModel != "claude-opus-5-5" || a.Kind != "anthropic" || len(a.Models) != 4 {
		t.Errorf("anthropic = %+v, want default_model overridden and the other defaults kept", a)
	}
	for key, want := range map[string]string{
		"providers.anthropic.default_model":                   "file",
		"providers.anthropic.kind":                            "default",
		"providers.anthropic.models":                          "default",
		"providers.gemini.kind":                               "file",
		"providers.gemini.base_url":                           "unset",
		`providers.anthropic.prices.claude-opus-5-5.input`:    "file",
		`repos."owner/name".commands.test`:                    "file",
		`repos."owner/name".commands.lint`:                    "file",
		`repos."owner/name".protected`:                        "file",
		`repos."owner/name".test_command`:                     "unset",
		"defaults.approvers":                                  "file",
		"defaults.provider":                                   "default",
		`providers.anthropic.prices.claude-haiku-5-5.checked`: "file",
	} {
		if got := c.Source(key).String(); got != want {
			t.Errorf("source of %s = %q, want %q", key, got, want)
		}
	}

	opus := a.Prices["claude-opus-5-5"]
	if opus == nil || opus.Input == nil || *opus.Input != 1.5 || opus.Output == nil || *opus.Output != 7.25 || opus.CacheRead == nil || *opus.CacheRead != 0.15 || opus.CacheWrite != nil {
		t.Errorf("opus price = %+v", opus)
	}
	if got := opus.Checked.String(); got != "2026-11-01" {
		t.Errorf("checked (TOML date) = %q", got)
	}
	if got := a.Prices["claude-haiku-5-5"].Checked.String(); got != "2026-10-31" {
		t.Errorf("checked (string) = %q", got)
	}
	if c.Providers["empty"] == nil {
		t.Error("an empty [providers.empty] table did not create the provider")
	}
	wantCommands := map[string][]string{"test": {"go", "test", "./..."}, "lint": {"go", "vet", "./..."}}
	if diff := cmp.Diff(wantCommands, c.Repos["owner/name"].Commands); diff != "" {
		t.Errorf("commands (-want +got):\n%s", diff)
	}
	if id, ok := c.Repos["owner/name"].TestCommandID(); !ok || id != "test" {
		t.Errorf("TestCommandID() = %q, %v", id, ok)
	}
}

func TestRejectedFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		wantErr []string
		notWant string
	}{
		{name: "syntax error", file: "[defaults\nprovider = 1", wantErr: []string{"patchbot.toml"}},
		{name: "wrong type", file: "[limits]\nmax_turns = \"forty\"", wantErr: []string{"line 2: incompatible types"}, notWant: "forty"},
		{name: "unknown top-level key", file: "colour = \"blue\"", wantErr: []string{"unknown key colour"}},
		{name: "unknown table", file: "[webhooks]\nurl = \"x\"", wantErr: []string{"unknown key webhooks"}},
		{name: "unknown key in a table", file: "[limits]\nmax_turn = 3", wantErr: []string{"unknown key limits.max_turn"}},
		{name: "unknown provider key", file: "[providers.x]\ntype = \"openai\"", wantErr: []string{"unknown key providers.x.type"}},
		{name: "unknown price key", file: "[providers.x.prices.m]\ninput_per_mtok = 1", wantErr: []string{"unknown key providers.x.prices.m.input_per_mtok"}},
		{name: "unknown repo key", file: "[repos.\"o/n\"]\nallow = [[\"make\"]]", wantErr: []string{`unknown key repos."o/n".allow`}},
		{name: "two unknown keys", file: "a = 1\nb = 2", wantErr: []string{"unknown key a", "unknown key b"}},
		{name: "integer duration", file: "[limits]\nrun_timeout = 30", wantErr: []string{"limits.run_timeout", "duration string"}},
		{name: "bad date", file: "[providers.x.prices.m]\nchecked = \"yesterday\"", wantErr: []string{"date"}},
		{
			name:    "api_key in a provider",
			file:    "[providers.openai]\nkind = \"openai\"\napi_key = \"sk-" + strings.Repeat("a", 40) + "\"",
			wantErr: []string{"providers.openai.api_key", "looks like a secret", "api_key_env"},
			notWant: "aaaaaaaa",
		},
		{name: "token at top level", file: "github_token = \"abc\"", wantErr: []string{"github_token", "looks like a secret"}},
		{name: "password in a table", file: "[server]\npassword = \"hunter2\"", wantErr: []string{"server.password", "looks like a secret"}, notWant: "hunter2"},
		{name: "secret in a repo", file: "[repos.\"o/n\"]\nwebhook_secret = \"s\"", wantErr: []string{"webhook_secret", "looks like a secret"}},
		{name: "secret table is reported once", file: "[secrets]\ngithub = \"x\"\nother = \"y\"", wantErr: []string{`key secrets: "secrets" looks like a secret`}, notWant: "secrets.github"},
		{name: "upper-case API_KEY", file: "[providers.x]\nAPI_KEY = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "apikey", file: "[providers.x]\napikey = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "access key", file: "[providers.x]\naccess_key = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "camel case", file: "[providers.x]\napiKey = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "token inside a word", file: "[server]\nuitoken = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "client secret", file: "[providers.x]\nclient-secret = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "tokens then token", file: "[limits]\ntokens_token = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "secret named env but not suffixed", file: "[providers.x]\nenv_api_key = \"v\"", wantErr: []string{"looks like a secret"}},
		{name: "unknown key ending in _env is still unknown", file: "[providers.x]\ntoken_env = \"X\"", wantErr: []string{"unknown key providers.x.token_env"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.local(t, tt.file)
			_, err := config.Load(e.options())
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error %q leaks the value %q", err, tt.notWant)
			}
		})
	}
}

func TestSecretLookingNamesThatAreAllowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Free names (provider, model, command ID) and the token counters are not
	// secrets, and api_key_env holds a variable name.
	e.local(t, `
[limits]
max_total_tokens = 1000
max_output_tokens = 100

[providers.secret]
kind = "openai"
api_key_env = "MY_TOKEN"

[providers.secret.prices.token]
input = 1.0
output = 2.0

[repos."owner/password"]
commands = { token = ["true"] }
`)
	c := mustLoad(t, e.options())
	if c.Limits.MaxTotalTokens != 1000 || c.Providers["secret"].Prices["token"] == nil {
		t.Errorf("config was not loaded as written: %+v", c.Limits)
	}
}

func TestAPIKeyComesOnlyFromEnv(t *testing.T) {
	t.Parallel()
	const key = "test-key-0123456789abcdef"
	e := newEnv(t)
	e.local(t, `
[providers.anthropic]
api_key_env = "MY_ANTHROPIC_KEY"
[providers.gemini]
kind = "gemini"
api_key_env = "GEMINI_API_KEY"
[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
[providers.pasted]
kind = "openai"
api_key_env = "not a variable name"
`)
	e.vars["MY_ANTHROPIC_KEY"] = "  " + key + "\n"
	e.vars["ANTHROPIC_API_KEY"] = "the-default-variable-is-no-longer-read"
	e.vars["not a variable name"] = "must-never-be-looked-up"
	c := mustLoad(t, e.options())

	if got := c.Providers["anthropic"].APIKey.Reveal(); got != key {
		t.Errorf("anthropic key = %q, want the trimmed value of MY_ANTHROPIC_KEY", got)
	}
	for _, name := range []string{"gemini", "ollama", "pasted"} {
		if c.Providers[name].APIKey.IsSet() {
			t.Errorf("provider %s has a key, want none", name)
		}
	}
	if secrets := c.Secrets(); len(secrets) != 1 || secrets[0].Reveal() != key {
		t.Errorf("Secrets() = %d secrets, want exactly the one key", len(secrets))
	}
}

func TestDefaultIsIndependent(t *testing.T) {
	t.Parallel()
	a, b := config.Default(), config.Default()
	a.Providers["anthropic"].Models[0] = "changed"
	a.Providers["x"] = &config.Provider{}
	if b.Providers["anthropic"].Models[0] != "claude-opus-5-5" || len(b.Providers) != 1 {
		t.Error("two Default() configs share state")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLoadErrorsNeverQuoteValues: when Load cannot parse a value it does not
// know what the value is. A key pasted into the wrong field, or exported
// under the wrong variable name, must not come back in the error.
func TestLoadErrorsNeverQuoteValues(t *testing.T) {
	t.Parallel()
	// No recognizable token shape, so only the loader's own care hides it.
	const pasted = "Zq7pLx2MvB9cRt5WnYh3KdF8"
	tests := []struct {
		name     string
		file     string
		vars     map[string]string
		flags    []config.Override
		wantErr  string
		wantLine bool
	}{
		{name: "duration field", file: "[limits]\nrun_timeout = \"" + pasted + "\"", wantErr: `want a duration string such as "30m"`, wantLine: true},
		{name: "date field", file: "[providers.x.prices.m]\nchecked = \"" + pasted + "\"", wantErr: "want a date like 2026-01-31", wantLine: true},
		{name: "unquoted value", file: "[log]\nlevel = " + pasted, wantErr: "expected a value; text needs quotes", wantLine: true},
		{name: "value in a number field", file: "[limits]\nmax_turns = \"" + pasted + "\"", wantErr: "incompatible types", wantLine: true},
		{name: "unterminated string", file: "[log]\nlevel = \"" + pasted, wantErr: "config file", wantLine: true},
		{name: "bare line", file: "[log]\n" + pasted + "\n", wantErr: "config file", wantLine: true},
		{name: "key with no value", file: "[log]\nlevel = \"info\"\n" + pasted + " = ", wantErr: "config file", wantLine: true},
		{name: "huge number", file: "[limits]\nmax_turns = 99999999999999999999", wantErr: "a number is out of range", wantLine: true},
		{name: "env int", vars: map[string]string{"PATCHBOT_MAX_TURNS": pasted}, wantErr: "PATCHBOT_MAX_TURNS: limits.max_turns: want a whole number"},
		{name: "env float", vars: map[string]string{"PATCHBOT_MAX_COST_USD": pasted}, wantErr: "want a number"},
		{name: "flag duration", flags: []config.Override{{Key: "limits.run_timeout", Value: pasted, Flag: "--timeout"}}, wantErr: "flag --timeout: limits.run_timeout: want a duration"},
		{name: "flag bool", flags: []config.Override{{Key: "limits.baseline_tests", Value: pasted, Flag: "--baseline"}}, wantErr: "want true or false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			if tt.file != "" {
				e.local(t, tt.file)
			}
			if tt.vars != nil {
				e.vars = tt.vars
			}
			opts := e.options()
			opts.Flags = tt.flags
			_, err := config.Load(opts)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if strings.Contains(err.Error(), pasted) || strings.Contains(err.Error(), pasted[:8]) {
				t.Errorf("the error quotes the value: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantLine && !strings.Contains(err.Error(), "line ") {
				t.Errorf("error = %v, want a line number so the user can find the problem", err)
			}
		})
	}
}

// TestLoadErrorIsRedactedWithLoadedKeys: even a message that does name a
// value (an unknown key name, here) is masked when the text is a known key.
func TestLoadErrorIsRedactedWithLoadedKeys(t *testing.T) {
	t.Parallel()
	const key = "Zq7pLx2MvB9cRt5WnYh3KdF8"
	e := newEnv(t)
	e.local(t, "[providers.g]\nkind = \"gemini\"\napi_key_env = \"GEMINI_API_KEY\"\n[log]\n"+key+" = 1\n")
	e.vars["GEMINI_API_KEY"] = key
	_, err := config.Load(e.options())
	if err == nil || !strings.Contains(err.Error(), "unknown key log.[redacted]") {
		t.Fatalf("Load error = %v, want the key name masked", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the error shows a loaded API key: %v", err)
	}

	// The original error is still reachable.
	opts := newEnv(t).options()
	opts.Path = filepath.Join(t.TempDir(), "absent.toml")
	if _, err := config.Load(opts); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) is false for a missing file: %v", err)
	}
}

func TestDecodeErrorsNeverEchoKeys(t *testing.T) {
	t.Parallel()
	const pasted = "Zq7pLx2MvB9cRt5WnYh3KdF8"
	for _, tt := range []struct {
		name string
		body string
	}{
		{"duplicate key", pasted + " = 1\n" + pasted + " = 2\n"},
		{"duplicate table", "[" + pasted + "]\n[" + pasted + "]\n"},
		{"key env before parse failure", "[providers.x]\napi_key_env = 'CUSTOM_KEY'\n" + pasted + " = 1\n" + pasted + " = 2\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.local(t, tt.body)
			e.vars["CUSTOM_KEY"] = pasted
			_, err := config.Load(e.options())
			if err == nil {
				t.Fatal("Load succeeded, want a duplicate-key error")
			}
			if strings.Contains(err.Error(), pasted) || strings.Contains(err.Error(), pasted[:8]) {
				t.Errorf("decoder diagnostic leaks a key: %v", err)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("decoder diagnostic has no line number: %v", err)
			}
		})
	}
}
