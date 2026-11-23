//go:build unix

package cli_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/testhttp"
	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestDoctorAppMode(t *testing.T) {
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyPath := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.toml")
	content := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n"+
		"[github]\nauth=\"app\"\napp_id=123\nprivate_key_file=%q\n"+
		"[repos.\"owner/repo\"]\ncommands={test=[\"repo-test\"]}\n[repos.\"owner/other\"]\ncommands={test=[\"repo-test\"]}\n", bin, data, keyPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	perms := map[string]map[string]string{
		"owner/repo":  {"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read"},
		"owner/other": {"contents": "write", "issues": "read", "pull_requests": "write", "metadata": "read"},
	}
	var minted []string
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/app":
			reply(200, map[string]any{"slug": "patchbot"})
		case r.URL.Path == "/users/patchbot[bot]":
			reply(200, map[string]any{"id": 4242, "login": "patchbot[bot]"})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			full := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation")
			if p, ok := perms[full]; ok {
				reply(200, map[string]any{"id": 77, "permissions": p})
			} else {
				reply(404, map[string]string{"message": "Not Found"})
			}
		case r.URL.Path == "/app/installations/77/access_tokens":
			var in struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			token := fmt.Sprintf("ghs_doctorMintedToken%020d", len(minted)+1)
			minted = append(minted, token)
			reply(201, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"permissions": in.Permissions, "repositories": []map[string]any{{"id": 101, "full_name": "owner/repo"}}})
		default:
			t.Errorf("unexpected doctor request %s %s", r.Method, r.URL.Path)
			reply(404, nil)
		}
	}))
	defer server.Close()
	p := fake.New("anthropic")
	constructors := llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return p, nil }}
	vars := map[string]string{"ANTHROPIC_API_KEY": "fixture-provider-key"}
	invoke := func(args ...string) (int, string) {
		var out, errs bytes.Buffer
		app := cli.App{Context: context.Background(), Stdout: &out, Stderr: &errs,
			Config:        config.Options{Path: path, LookupEnv: func(name string) (string, bool) { v, ok := vars[name]; return v, ok }},
			RunnerOptions: runner.Options{Constructors: constructors, GitHubAPI: server.URL, GitHubHTTP: server.Client()}}
		code := app.Run(append([]string{"doctor"}, args...))
		text := out.String() + errs.String()
		if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, string(keyPEM[40:80])) {
			t.Fatalf("key material printed: %s", text)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, token := range minted {
			if strings.Contains(text, token) {
				t.Fatalf("minted token printed: %s", text)
			}
		}
		return code, text
	}
	code, text := invoke()
	if code != 0 || !strings.Contains(text, "GitHub App key") || !strings.Contains(text, "GitHub App JWT") || strings.Contains(text, "set GITHUB_TOKEN") {
		t.Fatalf("offline app mode: %d %s", code, text)
	}
	// owner/other lacks issues:write, so online fails for it but passes owner/repo.
	code, text = invoke("--online")
	if code != 1 || !strings.Contains(text, "patchbot[bot]") || !strings.Contains(text, "issues:write") ||
		!strings.Contains(text, "token minted") {
		t.Fatalf("online app mode: %d %s", code, text)
	}
	// A repository without the App installed fails too.
	mu.Lock()
	delete(perms, "owner/other")
	mu.Unlock()
	if code, text = invoke("--online"); code != 1 || !strings.Contains(text, "not installed") {
		t.Fatalf("missing installation: %d %s", code, text)
	}
	// An unsafe key file fails offline, and GITHUB_TOKEN is reported as ignored.
	if err := os.Chmod(keyPath, 0o644); err != nil { // #nosec G302 -- deliberately unsafe mode under test.
		t.Fatal(err)
	}
	vars["GITHUB_TOKEN"] = "ghp_ignoredInAppMode000000000000000000"
	if code, text = invoke(); code != 1 || !strings.Contains(text, "0600") || !strings.Contains(text, "ignored in app mode") {
		t.Fatalf("unsafe key: %d %s", code, text)
	}
}
