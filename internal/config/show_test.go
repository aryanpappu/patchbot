package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/aryanpappu/patchbot/internal/config"
)

func TestWriteEffective(t *testing.T) {
	t.Parallel()
	const keyValue = "value-of-the-key-0123456789"
	e := newEnv(t)
	path := e.local(t, `
[defaults]
approvers = ["alice", "bob"]
max_cost_usd = 3.0

[providers.anthropic.prices."claude-sonnet-5-5"]
input = 1.25
output = 5.0
cache_read = 0.0
checked = 2026-11-01

[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
price_free = true

[repos."owner/name"]
commands = { test = ["go", "test", "./..."] }
protected = ["db/"]
`)
	e.vars["ANTHROPIC_API_KEY"] = keyValue
	e.vars["PATCHBOT_MAX_TURNS"] = "12"
	opts := e.options()
	opts.Flags = []config.Override{{Key: "log.level", Value: "debug", Flag: "--log-level"}}
	c := mustLoad(t, opts)

	var b strings.Builder
	if err := c.WriteEffective(&b); err != nil {
		t.Fatal(err)
	}
	got := b.String()

	if strings.Contains(got, keyValue) {
		t.Fatalf("output leaks the API key:\n%s", got)
	}
	// Comments are aligned with padding; compare with single spaces.
	squeezed := strings.Join(strings.FieldsFunc(got, func(r rune) bool { return r == ' ' }), " ")
	for _, want := range []string{
		"# config file: " + path,
		"[defaults]",
		`provider = "anthropic" # default`,
		`approvers = ["alice", "bob"] # file`,
		`max_cost_usd = 3.0 # file`,
		"# env PATCHBOT_MAX_TURNS",
		"max_turns = 12",
		`level = "debug" # flag --log-level`,
		`run_timeout = "30m0s"`,
		"[providers.anthropic]",
		"# api key: [redacted] (set in $ANTHROPIC_API_KEY)",
		`[providers.anthropic.prices.claude-sonnet-5-5]`,
		"input = 1.25 # file",
		"cache_read = 0.0 # file",
		"checked = 2026-11-01 # file",
		"[providers.ollama]",
		"# api key: none (api_key_env is not set)",
		`base_url = "http://localhost:11434/v1" # file`,
		`default_model = "" # unset`,
		`[repos."owner/name"]`,
		`[repos."owner/name".commands]`,
		`test = ["go", "test", "./..."] # file`,
		`protected = ["db/"] # file`,
	} {
		if !strings.Contains(squeezed, want) {
			t.Errorf("output does not contain %q", want)
		}
	}
	if strings.Contains(got, "cache_write") {
		t.Error("an unset optional price is printed")
	}

	// The output is itself valid TOML that describes the same config.
	var back config.Config
	if _, err := toml.Decode(got, &back); err != nil {
		t.Fatalf("the output is not valid TOML: %v\n%s", err, got)
	}
	if back.Limits.MaxTurns != 12 || back.Providers["ollama"].BaseURL != "http://localhost:11434/v1" ||
		back.Repos["owner/name"].Commands["test"][2] != "./..." {
		t.Errorf("the output does not round-trip: %+v", back.Limits)
	}
}

func TestWriteEffectiveWithoutFileOrKey(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := mustLoad(t, newEnv(t).options()).WriteEffective(&b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{
		"# config file: none found (built-in defaults)",
		"# api key: not set ($ANTHROPIC_API_KEY is empty)",
		"approvers = []",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[repos") {
		t.Error("an empty repos table is printed")
	}
}

func TestWriteEffectiveReportsWriteError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("closed pipe")
	if err := config.Default().WriteEffective(errWriter{wantErr}); !errors.Is(err, wantErr) {
		t.Errorf("WriteEffective error = %v, want %v", err, wantErr)
	}
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
