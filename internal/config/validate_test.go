package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
)

// validBase is a complete, valid file. Each case appends to it; TOML lets a
// later dotted table add keys, and each case uses names the base does not.
const validBase = `
[defaults]
approvers = ["alice"]
`

func loadString(t *testing.T, file string, vars map[string]string) *config.Config {
	t.Helper()
	e := newEnv(t)
	e.local(t, file)
	if vars != nil {
		e.vars = vars
	}
	return mustLoad(t, e.options())
}

func TestValidateAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
	}{
		{"minimal", validBase},
		{"full provider set", validBase + `
[providers.gemini]
kind = "gemini"
api_key_env = "GEMINI_API_KEY"
[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
models = ["qwen3-coder"]
price_free = true
[providers.openrouter]
kind = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
models = ["vendor/model:free"]
[providers.openrouter.prices."vendor/model:free"]
input = 0
output = 0.0
cache_read = 0
`},
		{"repo with commands", validBase + `
[repos."Owner-1/my.repo_x"]
commands = { test = ["go", "test", "./..."], lint = ["./scripts/lint.sh", "--fix=false", "a b; c | d"], build-all = ["/usr/bin/make", "-j4"] }
protected = ["migrations/", "config/secrets.yml", ".env.d"]
test_command = "lint"
`},
		{"budget equal to the hard cap", "[defaults]\napprovers = [\"a\"]\nmax_cost_usd = 10.0\n"},
		{"models chosen per stage with no provider default", `
[defaults]
approvers = ["a"]
provider = "g"
refine_model = "m1"
code_model = "m2"
[providers.g]
kind = "gemini"
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := loadString(t, tt.file, nil).Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{"no approvers", "", "defaults.approvers: list at least one GitHub login"},
		{"empty approvers", "[defaults]\napprovers = []", "defaults.approvers: list at least one"},
		{"bad approver", "[defaults]\napprovers = [\"@alice\"]", `"@alice" is not a GitHub login`},
		{"blank approver", "[defaults]\napprovers = [\"\"]", `"" is not a GitHub login`},
		{"bad mode", validBase + "mode = \"yolo\"", `defaults.mode: "yolo"`},
		{"default provider missing", validBase + "provider = \"nope\"", "defaults.provider: no [providers.nope] table; configured providers: anthropic"},
		{"default provider empty", validBase + "provider = \"\"", "defaults.provider: not set"},
		{"model not resolvable", validBase + "provider = \"g\"\n[providers.g]\nkind = \"gemini\"", "no model for the refine stage: set defaults.refine_model or providers.g.default_model"},
		{"code model not resolvable", validBase + "provider = \"g\"\nrefine_model = \"m\"\n[providers.g]\nkind = \"gemini\"", "no model for the code stage"},
		{"budget above hard cap", validBase + "max_cost_usd = 10.01", "defaults.max_cost_usd (10.01) is above limits.hard_max_cost_usd (10)"},
		{"budget above lowered hard cap", validBase + "[limits]\nhard_max_cost_usd = 1.0", "is above limits.hard_max_cost_usd (1)"},
		{"zero budget", validBase + "max_cost_usd = 0.0", "defaults.max_cost_usd: must be greater than 0"},
		{"negative budget", validBase + "max_cost_usd = -1.0", "defaults.max_cost_usd: must be greater than 0"},
		{"infinite budget", validBase + "max_cost_usd = inf", "defaults.max_cost_usd: must be greater than 0"},
		{"nan hard cap", validBase + "[limits]\nhard_max_cost_usd = nan", "limits.hard_max_cost_usd: must be greater than 0"},
		{"zero turns", validBase + "[limits]\nmax_turns = 0", "limits.max_turns: must be greater than 0"},
		{"negative timeout", validBase + "[limits]\nrun_timeout = \"-1m\"", "limits.run_timeout: must be greater than 0"},
		{"model output above stored output", validBase + "[limits]\ncmd_output_bytes = 9\ncmd_store_bytes = 8", "limits.cmd_output_bytes (9) is above limits.cmd_store_bytes (8)"},
		{"zero revisions", validBase + "[approval]\nmax_revisions = 0", "approval.max_revisions: must be greater than 0"},
		{"blank local actor", validBase + "[approval]\nlocal_actor = \" \"", "approval.local_actor: not set"},
		{"bad log level", validBase + "[log]\nlevel = \"loud\"", `log.level: "loud"`},
		{"bad log format", validBase + "[log]\nformat = \"xml\"", `log.format: "xml"`},
		{"bad vue source", validBase + "[server]\nvue_src = \"npm\"", `server.vue_src: "npm"`},
		{"bad address", validBase + "[server]\naddr = \"localhost\"", `server.addr: "localhost" is not host:port`},
		{"address without host", validBase + "[server]\naddr = \":8080\"", "is not host:port"},
		{"empty data dir", validBase + "[data]\ndir = \"\"", "data.dir: not set"},

		{"provider kind missing", validBase + "[providers.x]", `providers.x.kind: "" is not one of anthropic, gemini, openai`},
		{"provider kind unknown", validBase + "[providers.x]\nkind = \"llama\"", `providers.x.kind: "llama"`},
		{"provider name", validBase + "[providers.\"Bad Name\"]\nkind = \"gemini\"", "a provider name uses lower-case"},
		{"openai without base url", validBase + "[providers.x]\nkind = \"openai\"", "providers.x.base_url: required for kind openai"},
		{"base url scheme", validBase + "[providers.x]\nkind = \"openai\"\nbase_url = \"ftp://host/v1\"", "providers.x.base_url: must be an http or https URL"},
		{"base url with credentials", validBase + "[providers.x]\nkind = \"openai\"\nbase_url = \"https://user:pw@host/v1\"", "must not contain a user name or password"},
		{"base url with query", validBase + "[providers.x]\nkind = \"openai\"\nbase_url = \"https://host/v1?key=abc\"", "must not contain a query"},
		{"blank model", validBase + "[providers.x]\nkind = \"gemini\"\nmodels = [\"\"]", `providers.x.models: "" is not a model ID`},
		{"model with a space", validBase + "[providers.x]\nkind = \"gemini\"\ndefault_model = \"my model\"", `providers.x.default_model: "my model"`},
		{"negative input price", validBase + "[providers.x]\nkind = \"gemini\"\n[providers.x.prices.m]\ninput = -0.01\noutput = 1", "providers.x.prices.m.input: must be 0 or more"},
		{"negative output price", validBase + "[providers.x]\nkind = \"gemini\"\n[providers.x.prices.m]\ninput = 1\noutput = -1", "providers.x.prices.m.output: must be 0 or more"},
		{"negative cache read price", validBase + "[providers.x]\nkind = \"gemini\"\n[providers.x.prices.m]\ninput = 1\noutput = 1\ncache_read = -1", "providers.x.prices.m.cache_read: must be 0 or more"},
		{"infinite cache write price", validBase + "[providers.x]\nkind = \"gemini\"\n[providers.x.prices.m]\ninput = 1\noutput = 1\ncache_write = inf", "providers.x.prices.m.cache_write: must be 0 or more"},
		{"free and priced", validBase + "[providers.x]\nkind = \"gemini\"\nprice_free = true\n[providers.x.prices.m]\ninput = 1\noutput = 1", "price_free = true and a prices table contradict"},

		{"repo key without owner", validBase + "[repos.demo]", `repos."demo": a repository key looks like "owner/name"`},
		{"repo key with three parts", validBase + "[repos.\"a/b/c\"]", "a repository key looks like"},
		{"repo key dot dot", validBase + "[repos.\"owner/..\"]", "a repository key looks like"},
		{"repo key url", validBase + "[repos.\"https://github.com/o/n\"]", "a repository key looks like"},
		{"empty argv", validBase + "[repos.\"o/n\"]\ncommands = { test = [] }", `repos."o/n".commands.test: must be a non-empty argv array`},
		{"empty program", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"\", \"x\"] }", "the program name is empty"},
		{"program is a shell line", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"go test ./...\"] }", `the program name "go test ./..." contains a shell metacharacter`},
		{"program with a pipe", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"make|sh\"] }", "contains a shell metacharacter"},
		{"program with a substitution", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"$(id)\"] }", "contains a shell metacharacter"},
		{"program with a variable", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"$HOME/bin/x\"] }", "contains a shell metacharacter"},
		{"program with a backtick", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"`id`\"] }", "contains a shell metacharacter"},
		{"program with a redirect", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"make>out\"] }", "contains a shell metacharacter"},
		{"program with a semicolon", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"make;id\"] }", "contains a shell metacharacter"},
		{"program with a newline", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"make\\nid\"] }", "contains a shell metacharacter"},
		{"program with a tilde", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"~/bin/x\"] }", "contains a shell metacharacter"},
		{"program with an env assignment", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"CI=1\", \"make\"] }", "contains a shell metacharacter"},
		{"program is a flag", validBase + "[repos.\"o/n\"]\ncommands = { test = [\"-c\", \"id\"] }", `the program name "-c" starts with a dash`},
		{"bad command id", validBase + "[repos.\"o/n\"]\ncommands = { \"Run Tests\" = [\"make\"] }", "a command ID uses lower-case"},
		{"test command not defined", validBase + "[repos.\"o/n\"]\ntest_command = \"check\"\ncommands = { test = [\"make\"] }", `repos."o/n".test_command: no command "check" in commands (have: test)`},
		{"test command with no commands", validBase + "[repos.\"o/n\"]\ntest_command = \"test\"", `no command "test" in commands (have: none)`},
		{"absolute protected path", validBase + "[repos.\"o/n\"]\nprotected = [\"/etc\"]", `repos."o/n".protected: "/etc"`},
		{"protected path escapes", validBase + "[repos.\"o/n\"]\nprotected = [\"a/../../b\"]", `repos."o/n".protected: "a/../../b"`},
		{"empty protected path", validBase + "[repos.\"o/n\"]\nprotected = [\"\"]", `repos."o/n".protected: ""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := loadString(t, tt.file, nil).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate error = %v\nwant it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()
	err := loadString(t, `
[defaults]
mode = "bad"
[limits]
max_turns = 0
[providers.x]
kind = "bad"
[repos.bad]
`, nil).Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	for _, want := range []string{"defaults.mode", "defaults.approvers", "limits.max_turns", "providers.x.kind", `repos."bad"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestValidateNeverEchoesAPastedKey(t *testing.T) {
	t.Parallel()
	const pasted = "sk-ant-api03-PASTEDKEY0123456789abcdefghij"
	c := loadString(t, validBase+"[providers.anthropic]\napi_key_env = \""+pasted+"\"", map[string]string{pasted: "x"})
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "providers.anthropic.api_key_env: must be the name of an environment variable") {
		t.Fatalf("Validate error = %v", err)
	}
	var shown strings.Builder
	if werr := c.WriteEffective(&shown); werr != nil {
		t.Fatal(werr)
	}
	for name, text := range map[string]string{"error": err.Error(), "warnings": strings.Join(c.Warnings(), "\n"), "show": shown.String()} {
		if strings.Contains(text, "PASTEDKEY") {
			t.Errorf("%s leaks the pasted key:\n%s", name, text)
		}
	}
	if c.Providers["anthropic"].APIKey.IsSet() {
		t.Error("a malformed api_key_env was looked up in the environment")
	}
}

func TestWarnings(t *testing.T) {
	t.Parallel()
	const keyValue = "value-of-the-key-0123456789"
	tests := []struct {
		name    string
		file    string
		vars    map[string]string
		want    []string
		notWant []string
	}{
		{
			name: "defaults: no key, no prices, no repos",
			file: validBase,
			want: []string{
				`provider "anthropic": environment variable ANTHROPIC_API_KEY is not set`,
				`provider "anthropic": model "claude-sonnet-5-5" has no price`,
				`provider "anthropic": model "claude-fable-5-2" has no price`,
				`no [repos."owner/name"] table`,
			},
		},
		{
			name:    "key set: named, never shown",
			file:    validBase,
			vars:    map[string]string{"ANTHROPIC_API_KEY": keyValue},
			notWant: []string{"ANTHROPIC_API_KEY", keyValue},
		},
		{
			name: "priced and free models do not warn",
			file: validBase + `
[providers.anthropic]
models = ["claude-sonnet-5-5"]
[providers.anthropic.prices."claude-sonnet-5-5"]
input = 1.0
output = 2.0
checked = 2026-11-01
[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
models = ["local"]
price_free = true
`,
			notWant: []string{"has no price", "checked date", `provider "ollama"`},
		},
		{
			name: "price without a checked date",
			file: validBase + "[providers.anthropic]\nmodels = []\n[providers.anthropic.prices.\"claude-sonnet-5-5\"]\ninput = 1.0\noutput = 2.0\n",
			want: []string{`the price of model "claude-sonnet-5-5" has no checked date`},
		},
		{
			name: "stage models of the default provider are checked",
			file: validBase + "refine_model = \"claude-custom\"\n",
			want: []string{`model "claude-custom" has no price`},
		},
		{
			name: "gemini without a key variable",
			file: validBase + "[providers.g]\nkind = \"gemini\"\n",
			want: []string{`provider "g": api_key_env is not set`},
		},
		{
			name: "repo without a test command",
			file: validBase + "[repos.\"o/n\"]\ncommands = { lint = [\"make\", \"lint\"] }\n",
			want: []string{`repo "o/n": no test command`},
		},
		{
			name:    "repo with a test command",
			file:    validBase + "[repos.\"o/n\"]\ncommands = { test = [\"make\", \"test\"] }\n",
			notWant: []string{"no test command", "no [repos"},
		},
		{
			name: "non-loopback address",
			file: validBase + "[server]\naddr = \"0.0.0.0:8080\"\n",
			want: []string{"is not a loopback address", "PATCHBOT_UI_TOKEN"},
		},
		{
			name:    "localhost and ipv6 loopback",
			file:    validBase + "[server]\naddr = \"[::1]:8080\"\n",
			notWant: []string{"loopback"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(loadString(t, tt.file, tt.vars).Warnings(), "\n")
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("warnings do not contain %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("warnings contain %q:\n%s", notWant, got)
				}
			}
		})
	}
}

// TestExampleFile keeps patchbot.example.toml loadable and valid.
func TestExampleFile(t *testing.T) {
	t.Parallel()
	opts := newEnv(t).options()
	opts.Path = filepath.Join("..", "..", "patchbot.example.toml")
	if _, err := os.Stat(opts.Path); err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, opts)
	if err := c.Validate(); err != nil {
		t.Fatalf("the example file is invalid: %v", err)
	}
	if c.Model("refine") != "claude-opus-5-5" || c.Model("code") != "claude-sonnet-5-5" {
		t.Errorf("stage models = %q, %q", c.Model("refine"), c.Model("code"))
	}
	if got := c.Providers["ollama"]; got == nil || !got.PriceFree || got.Kind != config.KindOpenAI {
		t.Errorf("ollama = %+v", got)
	}
	repo := c.Repos["aryanpappu/demo-app"]
	if repo == nil || len(repo.Commands) != 2 || len(repo.Protected) != 1 {
		t.Errorf("demo repo = %+v", repo)
	}
	for _, p := range c.Providers {
		if len(p.Prices) != 0 {
			t.Error("the example file ships a price; the plan says none are shipped")
		}
	}
}
