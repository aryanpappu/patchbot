package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
)

const (
	validConfig = `
[defaults]
approvers = ["alice"]

[providers.anthropic.prices."claude-sonnet-5-5"]
input = 1.0
output = 2.0
checked = 2026-11-01

[repos."owner/name"]
commands = { test = ["go", "test", "./..."] }
`
	keyValue = "key-value-0123456789abcdef"
)

// runApp runs the CLI in an empty directory with only the given environment.
// A non-empty file becomes ./patchbot.toml.
func runApp(t *testing.T, file string, vars map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	if file != "" {
		if err := os.WriteFile(filepath.Join(dir, "patchbot.toml"), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	app := &cli.App{
		Stdout: &out,
		Stderr: &errOut,
		Config: config.Options{
			Dir: dir,
			LookupEnv: func(name string) (string, bool) {
				v, ok := vars[name]
				return v, ok
			},
			UserConfigDir: func() (string, error) { return filepath.Join(dir, "user-config"), nil },
			UserHomeDir:   func() (string, error) { return filepath.Join(dir, "home"), nil },
		},
	}
	code = app.Run(args)
	return code, out.String(), errOut.String()
}

func TestConfigCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		file       string
		vars       map[string]string
		wantCode   int
		wantStdout []string
		wantStderr []string
		notWant    []string
	}{
		{
			name:       "valid with a key",
			file:       validConfig,
			vars:       map[string]string{"ANTHROPIC_API_KEY": keyValue},
			wantStdout: []string{"config OK: ", "patchbot.toml", "(3 warnings)"},
			wantStderr: []string{`warning: provider "anthropic": model "claude-opus-5-5" has no price`},
			notWant:    []string{keyValue, "ANTHROPIC_API_KEY", "error:"},
		},
		{
			name:       "missing key is a warning that names the variable",
			file:       validConfig,
			wantStdout: []string{"config OK: "},
			wantStderr: []string{`warning: provider "anthropic": environment variable ANTHROPIC_API_KEY is not set`},
		},
		{
			name:       "defaults alone are invalid: no approvers",
			wantCode:   1,
			wantStderr: []string{"error: defaults.approvers: list at least one GitHub login", "config invalid: built-in defaults (no config file found): 1 error"},
			notWant:    []string{"config OK"},
		},
		{
			name:       "every error is listed",
			file:       "[defaults]\napprovers = [\"a\"]\nmode = \"x\"\n[limits]\nmax_turns = 0\n",
			wantCode:   1,
			wantStderr: []string{"error: defaults.mode:", "error: limits.max_turns:", "2 errors"},
		},
		{
			name:       "secret-looking key",
			file:       "[providers.anthropic]\napi_key = \"" + keyValue + "\"\n",
			wantCode:   1,
			wantStderr: []string{"error: ", "providers.anthropic.api_key", "looks like a secret", "api_key_env"},
			notWant:    []string{keyValue},
		},
		{
			name:       "unknown key",
			file:       validConfig + "[webhooks]\nsecret_env = \"X\"\n",
			wantCode:   1,
			wantStderr: []string{"error: ", "unknown key webhooks"},
		},
		{
			name:       "syntax error",
			file:       "[defaults",
			wantCode:   1,
			wantStderr: []string{"error: config file "},
		},
		{
			name:       "bad environment override",
			file:       validConfig,
			vars:       map[string]string{"PATCHBOT_MAX_TURNS": "lots"},
			wantCode:   1,
			wantStderr: []string{"error: environment variable PATCHBOT_MAX_TURNS"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runApp(t, tt.file, tt.vars, "config", "check")
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout)
				}
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not contain %q:\n%s", want, stderr)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(stdout+stderr, notWant) {
					t.Errorf("output contains %q:\nstdout: %s\nstderr: %s", notWant, stdout, stderr)
				}
			}
		})
	}
}

func TestConfigShow(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"ANTHROPIC_API_KEY": keyValue, "PATCHBOT_LOG_LEVEL": "debug"}
	code, stdout, stderr := runApp(t, validConfig, vars, "config", "show")
	if code != 0 || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, keyValue) {
		t.Fatalf("config show leaks the API key:\n%s", stdout)
	}
	for _, want := range []string{
		"# config file: ",
		"# file",
		"# default",
		"# env PATCHBOT_LOG_LEVEL",
		"# api key: [redacted] (set in $ANTHROPIC_API_KEY)",
		`[repos."owner/name".commands]`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestConfigShowInvalid(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runApp(t, "", nil, "config", "show")
	if code != 1 {
		t.Errorf("exit code = %d, want 1 for an invalid config", code)
	}
	if !strings.Contains(stdout, "[defaults]") {
		t.Errorf("an invalid config is not shown:\n%s", stdout)
	}
	if !strings.Contains(stderr, "error: defaults.approvers") || !strings.Contains(stderr, "config invalid: 1 error") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestConfigFlagAndUsage(t *testing.T) {
	t.Parallel()
	other := filepath.Join(t.TempDir(), "other.toml")
	if err := os.WriteFile(other, []byte("[defaults]\napprovers = [\"bob\"]\nprovider = \"nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.toml")

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "--config wins over ./patchbot.toml", args: []string{"config", "check", "--config", other}, wantCode: 1, wantStderr: "defaults.provider: no [providers.nope] table"},
		{name: "--config=FILE form", args: []string{"config", "show", "--config=" + other}, wantCode: 1, wantStdout: `provider = "nope"`},
		{name: "--config names a missing file", args: []string{"config", "check", "--config", missing}, wantCode: 1, wantStderr: "error: config file: "},
		{name: "no subcommand", args: []string{"config"}, wantCode: 2, wantStderr: "Usage: patchbot config"},
		{name: "unknown subcommand", args: []string{"config", "edit"}, wantCode: 2, wantStderr: `unknown command`},
		{name: "unknown flag", args: []string{"config", "check", "--verbose"}, wantCode: 2, wantStderr: "invalid command options"},
		{name: "extra argument", args: []string{"config", "check", "now"}, wantCode: 2, wantStderr: `unexpected argument`},
		{name: "flag without a value", args: []string{"config", "check", "--config"}, wantCode: 2, wantStderr: "invalid command options"},
		{name: "help", args: []string{"config", "help"}, wantStdout: "Usage: patchbot config"},
		{name: "subcommand --help", args: []string{"config", "show", "--help"}, wantStdout: "Usage: patchbot config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runApp(t, validConfig, nil, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Errorf("stdout does not contain %q:\n%s", tt.wantStdout, stdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr does not contain %q:\n%s", tt.wantStderr, stderr)
			}
		})
	}
}

// TestConfigCommandsNeverPrintAKey runs the leaks from the review through the
// real commands and checks both output streams.
func TestConfigCommandsNeverPrintAKey(t *testing.T) {
	t.Parallel()
	// No token shape, so the pattern redactor alone would not catch it.
	const key = "Zq7pLx2MvB9cRt5WnYh3KdF8"
	withKey := func(extra map[string]string) map[string]string {
		vars := map[string]string{"ANTHROPIC_API_KEY": key}
		for k, v := range extra {
			vars[k] = v
		}
		return vars
	}
	tests := []struct {
		name string
		file string
		vars map[string]string
	}{
		{"key also set as the log level", validConfig, withKey(map[string]string{"PATCHBOT_LOG_LEVEL": key})},
		{"key also set as the provider", validConfig, withKey(map[string]string{"PATCHBOT_PROVIDER": key})},
		{"key also set as max turns", validConfig, withKey(map[string]string{"PATCHBOT_MAX_TURNS": key})},
		{"key pasted into a duration", validConfig + "[limits]\nrun_timeout = \"" + key + "\"\n", nil},
		{"key pasted into a date", validConfig + "[providers.anthropic.prices.m]\ninput = 1\noutput = 1\nchecked = \"" + key + "\"\n", nil},
		{"key pasted without quotes", validConfig + "[log]\nlevel = " + key + "\n", nil},
		{"key in the base url", validConfig + "[providers.o]\nkind = \"openai\"\nbase_url = \"https://user:" + key + "@host/v1\"\n", nil},
		{"key in the base url query", validConfig + "[providers.o]\nkind = \"openai\"\nbase_url = \"https://host/v1?api-key=" + key + "\"\n", nil},
		{"key as a stage model", validConfig, withKey(map[string]string{"PATCHBOT_MODEL": key})},
	}
	for _, tt := range tests {
		for _, sub := range []string{"check", "show"} {
			t.Run(tt.name+"/"+sub, func(t *testing.T) {
				t.Parallel()
				code, stdout, stderr := runApp(t, tt.file, tt.vars, "config", sub)
				if strings.Contains(stdout+stderr, key) {
					t.Errorf("config %s prints the key (exit %d):\nstdout: %s\nstderr: %s", sub, code, stdout, stderr)
				}
				if stdout+stderr == "" {
					t.Error("no output at all")
				}
			})
		}
	}
}

func TestConfigShortCredentialWarnsWithoutCorruptingTOML(t *testing.T) {
	for _, value := range []string{"code", "max_turns"} {
		vars := map[string]string{"ANTHROPIC_API_KEY": value}
		code, out, errs := runApp(t, validConfig, vars, "config", "show")
		if code != 0 || !strings.Contains(out, "code_model =") || !strings.Contains(out, "max_turns") {
			t.Fatalf("schema corrupted: %d %s %s", code, out, errs)
		}
	}
	code, out, errs := runApp(t, validConfig, map[string]string{"ANTHROPIC_API_KEY": "code"}, "config", "check")
	if code != 0 || !strings.Contains(errs, "cannot be masked") || !strings.Contains(errs, "ANTHROPIC_API_KEY") {
		t.Fatalf("short credential warning missing: %d %s %s", code, out, errs)
	}
}
