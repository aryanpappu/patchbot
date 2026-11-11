//go:build unix

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testhttp"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestDoctorOfflineOnlineAndFailures(t *testing.T) {
	root, bin := testpath.Dir(t), t.TempDir()
	for name, output := range map[string]string{"go": "go version go1.27.3 test/arch", "git": "git version 2.50.1", "repo-test": "test"} {
		// #nosec G306 -- operator-owned executable fixture needs its owner execute bit.
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.toml")
	content := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n[repos.\"owner/repo\"]\ncommands={test=[\"repo-test\"]}\n", bin, data)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	requests := 0
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/user" || r.Method != "GET" {
			t.Errorf("unexpected online request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"login":"test"}`)
	}))
	defer server.Close()
	client, err := github.New(server.Client(), server.URL, runner.Token{Value: secret.New("dummy-github-token")})
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New("anthropic")
	constructions := 0
	constructors := llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { constructions++; return p, nil }}
	// #nosec G101 -- dummy credentials for disclosure regression tests.
	vars := map[string]string{"ANTHROPIC_API_KEY": "fixture-provider-key", "GITHUB_TOKEN": "dummy-github-token"}
	invoke := func(args ...string) (int, string) {
		var out, errs bytes.Buffer
		app := cli.App{Context: context.Background(), Stdout: &out, Stderr: &errs, Config: config.Options{Path: path, LookupEnv: func(name string) (string, bool) { v, ok := vars[name]; return v, ok }}, RunnerOptions: runner.Options{Constructors: constructors, GitHub: client}}
		code := app.Run(append([]string{"doctor"}, args...))
		text := out.String() + errs.String()
		for _, value := range vars {
			if value != "" && strings.Contains(text, value) {
				t.Fatalf("credential exposed: %s", text)
			}
		}
		return code, text
	}
	code, text := invoke()
	if code != 0 || !strings.Contains(text, "PASS") || !strings.Contains(text, "2 migrations") || requests != 0 || constructions != 0 {
		t.Fatalf("offline: %d %s, requests %d constructors %d", code, text, requests, constructions)
	}
	code, text = invoke("--online")
	if code != 0 || requests != 1 || constructions != 1 || p.ListCalls() != 1 {
		t.Fatalf("online: %d %s; requests %d constructors %d", code, text, requests, constructions)
	}
	vars["ANTHROPIC_API_KEY"] = ""
	code, text = invoke()
	if code != 1 || !strings.Contains(text, "ANTHROPIC_API_KEY") {
		t.Fatalf("missing key: %d %s", code, text)
	}
	vars["ANTHROPIC_API_KEY"] = "fixture-provider-key"
	// #nosec G302 -- deliberately unsafe directory must fail doctor.
	if err := os.Chmod(data, 0o755); err != nil {
		t.Fatal(err)
	}
	code, text = invoke()
	if code != 1 || !strings.Contains(text, "0700") {
		t.Fatalf("permissions: %d %s", code, text)
	}
	// #nosec G302 -- a private directory needs owner traversal permission.
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	// #nosec G306 -- executable fixture for the minimum-version regression.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.50.0'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, text = invoke()
	if code != 1 || !strings.Contains(text, "2.50.1") {
		t.Fatalf("old git: %d %s", code, text)
	}
	if err := os.Remove(filepath.Join(bin, "repo-test")); err != nil {
		t.Fatal(err)
	}
	code, text = invoke()
	if code != 1 || !strings.Contains(text, "owner/repo / test") {
		t.Fatalf("missing command: %d %s", code, text)
	}
}

func TestCLIErrorRedactsCredentials(t *testing.T) {
	for _, command := range []string{"config", "models"} {
		// #nosec G101 -- dummy credentials deliberately echoed by malformed CLI input.
		code, out, errs := runApp(t, validConfig, map[string]string{"GITHUB_TOKEN": "plain-github-credential", "PATCHBOT_UI_TOKEN": "tiny"}, command, "--plain-github-credential-tiny")
		if code != 2 || strings.Contains(out+errs, "plain-github-credential") || strings.Contains(out+errs, "tiny") {
			t.Fatalf("%s: %d %s %s", command, code, out, errs)
		}
	}
}

func TestUsageNeverEchoesUnloadedProviderSecrets(t *testing.T) {
	const value = "custom-provider-credential"
	for _, args := range [][]string{
		{"--" + value}, {"config", value}, {"config", "check", value},
		{"config", "check", "--" + value}, {"models", "--" + value},
		{"run", "--issue", value}, {"doctor", "--" + value},
	} {
		code, out, errs := runApp(t, validConfig, map[string]string{"CUSTOM_PROVIDER_ENV": value}, args...)
		if code != 2 || strings.Contains(out+errs, value) {
			t.Fatalf("usage echoed unloaded credential: %d %s %s", code, out, errs)
		}
	}
}

func TestDoctorNeverEchoesInvalidKeyEnvironmentValue(t *testing.T) {
	const value = "custom-provider-credential"
	file := validConfig + "\n[providers.anthropic]\napi_key_env = \"" + value + "\"\n"
	code, out, errs := runApp(t, file, nil, "doctor")
	if code != 1 || strings.Contains(out+errs, value) || !strings.Contains(out+errs, "upper-case letters") {
		t.Fatalf("invalid api_key_env exposed: %d %s %s", code, out, errs)
	}
}

func TestDoctorStopsBeforeExecutingInvalidConfiguration(t *testing.T) {
	bin := testpath.Dir(t)
	marker := filepath.Join(t.TempDir(), "executed")
	for _, tool := range []string{"go", "git"} {
		script := fmt.Sprintf("#!/bin/sh\necho executed > %q\necho 'git version 2.54.0'\n", marker)
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		} // #nosec G306 -- executable fixture.
	}
	for _, invalid := range []string{"data", "approvers", "syntax"} {
		t.Run(invalid, func(t *testing.T) {
			_ = os.Remove(marker)
			data, approvers := t.TempDir(), `["owner"]`
			if invalid == "data" {
				data = bin
			}
			if invalid == "approvers" {
				approvers = `[]`
			}
			content := fmt.Sprintf("[defaults]\napprovers=%s\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n", approvers, bin, data)
			if invalid == "syntax" {
				content += "[broken"
			}
			code, out, errs := runApp(t, content, nil, "doctor", "--online")
			if code != 1 || !strings.Contains(out+errs, "FAIL") {
				t.Fatalf("%d %s %s", code, out, errs)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("doctor executed a binary after config failure")
			}
			_ = os.Remove(marker)
		})
	}
}

func TestDoctorExcludesExecutableSymlinksIntoData(t *testing.T) {
	bin, data := testpath.Dir(t), t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	target := filepath.Join(data, "go")
	script := fmt.Sprintf("#!/bin/sh\necho executed > %q\necho 'go version go1.27.4 test/arch'\n", marker)
	if err := os.WriteFile(target, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306 -- executable fixture.
	if err := os.Symlink(target, filepath.Join(bin, "go")); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n[repos.\"owner/repo\"]\ncommands={test=[%q]}\n", bin, data, target)
	code, out, errs := runApp(t, content, nil, "doctor")
	if code != 1 || !strings.Contains(out+errs, "owner/repo / test") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("doctor executed an excluded data-directory binary")
	}
}

func TestDoctorAndConfigWarnAboutEveryShortCredential(t *testing.T) {
	vars := map[string]string{"ANTHROPIC_API_KEY": "code", "GITHUB_TOKEN": "test", "PATCHBOT_UI_TOKEN": "ok"}
	for _, args := range [][]string{{"config", "check"}, {"doctor"}} {
		_, out, errs := runApp(t, validConfig, vars, args...)
		for name := range vars {
			if !strings.Contains(out+errs, name) || !strings.Contains(out+errs, "cannot be masked") {
				t.Fatalf("missing credential warning for %s: %s %s", name, out, errs)
			}
		}
	}
}
