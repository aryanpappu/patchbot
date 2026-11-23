//go:build unix

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestDoctorReportsGitHubNotConnected(t *testing.T) {
	root, bin := testpath.Dir(t), testpath.Dir(t)
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
	keyPath := filepath.Join(t.TempDir(), "github-app.pem")
	path := filepath.Join(root, "config.toml")
	content := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n"+
		"[github]\nauth=\"app\"\nprivate_key_file=%q\nsetup_public_url=\"https://patchbot.example.org\"\n"+
		"[repos.\"owner/repo\"]\ncommands={test=[\"repo-test\"]}\n", bin, data, keyPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := fake.New("anthropic")
	invoke := func() (int, string) {
		var out, errs bytes.Buffer
		app := cli.App{Context: context.Background(), Stdout: &out, Stderr: &errs,
			Config: config.Options{Path: path, LookupEnv: func(name string) (string, bool) {
				if name == "ANTHROPIC_API_KEY" {
					return "fixture-provider-key", true
				}
				return "", false
			}},
			RunnerOptions: runner.Options{Constructors: llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return p, nil }}}}
		code := app.Run([]string{"doctor"})
		return code, out.String() + errs.String()
	}
	code, text := invoke()
	if code != 0 || !strings.Contains(text, "not connected, open /setup/github") || strings.Contains(text, "FAIL") {
		t.Fatalf("not connected: %d %s", code, text)
	}
	// With a stored App but no key file the key check (not the setup hint) fails.
	db, err := store.Open(t.Context(), filepath.Join(data, "patchbot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveGitHubApp(t.Context(), store.GitHubApp{AppID: 9, Slug: "patchbot-x"}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	code, text = invoke()
	if strings.Contains(text, "open /setup/github") || !strings.Contains(text, "GitHub App key") {
		t.Fatalf("connected: %d %s", code, text)
	}
}
