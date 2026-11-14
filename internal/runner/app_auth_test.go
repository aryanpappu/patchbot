package runner_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

// #nosec G101 -- dummy PAT that must never be sent.
const patFallback = "ghp_patFallbackMustNeverBeUsed00000000000"

// appAPI fakes the GitHub App endpoints in front of the fixture's REST fake.
// Tokens are minted with 4 minutes left, inside the 5 minute refresh margin,
// so every credential request mints anew: the token used at push time is
// never the one used at clone time (expiry between clone and push).
type appAPI struct {
	mu       sync.Mutex
	revoked  bool
	minted   []string
	byRepo   map[string]int64
	restAuth []string
	badAuth  []string
}

func (a *appAPI) tokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.minted...)
}

func (a *appAPI) handler(f *fixture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reply := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		auth := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		switch req.URL.Path {
		case "/app":
			reply(200, map[string]any{"slug": "patchbot"})
		case "/users/patchbot[bot]":
			reply(200, map[string]any{"id": 4242, "login": "patchbot[bot]"})
		case "/repos/owner/repo/installation":
			reply(200, map[string]any{"id": 77, "permissions": github.RunPermissions})
		case "/app/installations/77/access_tokens":
			var in struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Repositories  []string          `json:"repositories"`
				Permissions   map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.revoked {
				reply(404, map[string]string{"message": "Not Found"})
				return
			}
			if len(in.RepositoryIDs) > 0 && (len(in.RepositoryIDs) != 1 || in.RepositoryIDs[0] != 101 || len(in.Permissions) != 4) {
				reply(422, map[string]string{"message": "bad scope"})
				return
			}
			token := fmt.Sprintf("ghs_runnerAppToken%024d", len(a.minted)+1)
			a.minted = append(a.minted, token)
			a.byRepo[token] = 101
			reply(201, map[string]any{"token": token, "expires_at": time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339),
				"permissions": in.Permissions, "repositories": []map[string]any{{"id": 101, "full_name": "owner/repo"}}})
		default:
			a.mu.Lock()
			if a.byRepo[auth] == 101 {
				a.restAuth = append(a.restAuth, auth)
			} else {
				a.badAuth = append(a.badAuth, req.Method+" "+req.URL.Path)
			}
			a.mu.Unlock()
			f.serveHTTP(w, req)
		}
	})
}

type envRecorder struct {
	inner execx.Executor
	mu    *sync.Mutex
	envs  *[]string
}

func (e envRecorder) Run(ctx context.Context, spec execx.Spec) (execx.Result, error) {
	e.mu.Lock()
	*e.envs = append(*e.envs, strings.Join(spec.Env, "\n"))
	e.mu.Unlock()
	return e.inner.Run(ctx, spec)
}

type appFixture struct {
	*fixture
	api         *appAPI
	gitTokens   [][]string // per clone, in call order
	gitMu       sync.Mutex
	envs        []string
	envMu       sync.Mutex
	opts        runner.Options
	redactorOut *secret.Redactor
}

func writeAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newAppFixture(t *testing.T, steps ...fake.Step) (*appFixture, error) {
	t.Helper()
	f := newFixture(t, "test", steps...)
	a := &appFixture{fixture: f, api: &appAPI{byRepo: map[string]int64{}}}
	f.botAuthor = "patchbot[bot]"
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, fmt.Appendf(nil, "[github]\nauth = \"app\"\napp_id = 123\nprivate_key_file = %q\n", writeAppKey(t))...)
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil { // #nosec G703 -- test-owned temp config path.
		t.Fatal(err)
	}
	f.cfg, err = config.Load(config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	server := testhttp.NewServer(a.api.handler(f))
	t.Cleanup(server.Close)
	a.redactorOut = secret.NewRedactor()
	opts := f.opts
	opts.GitHub, opts.BotLogin = nil, ""
	opts.Tokens = runner.Token{Value: secret.New(patFallback)}
	opts.GitHubAPI, opts.GitHubHTTP = server.URL, server.Client()
	opts.Redactor = a.redactorOut
	opts.Clone = func(ctx context.Context, clone gitx.CloneOptions) (*gitx.Repo, error) {
		clone.TestOnlyLocalRemote = f.remote
		next := clone.TokenFunc
		a.gitMu.Lock()
		a.gitTokens = append(a.gitTokens, nil)
		index := len(a.gitTokens) - 1
		a.gitMu.Unlock()
		clone.TokenFunc = func(ctx context.Context) (string, error) {
			token, err := next(ctx)
			a.gitMu.Lock()
			a.gitTokens[index] = append(a.gitTokens[index], token)
			a.gitMu.Unlock()
			return token, err
		}
		return gitx.Clone(ctx, clone)
	}
	opts.Executor = func(dir string) (execx.Executor, error) {
		inner, err := execx.NewLocalExecutor(dir)
		if err != nil {
			return nil, err
		}
		return envRecorder{inner: inner, mu: &a.envMu, envs: &a.envs}, nil
	}
	a.opts = opts
	f.r, err = runner.New(t.Context(), f.cfg, f.db, opts)
	return a, err
}

// Not parallel: it captures the process-wide slog default.
func TestAppModeRunUsesFreshRepoTokensAndBotIdentity(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	a, err := newAppFixture(t, specStep(), writeStep(), submitStep())
	if err != nil {
		t.Fatal(err)
	}
	run := a.start("fix", false)
	if run.Status != store.StatusAwaitingApproval {
		t.Fatalf("refine: %+v", run)
	}
	// The App bot's own /approve never counts, even if it looks unrecorded.
	a.addCommand("patchbot[bot]", "/approve", false)
	if err := a.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := a.get(run.ID).Status; got != store.StatusAwaitingApproval {
		t.Fatalf("bot comment changed status to %s", got)
	}
	// The owner's /approve works through the normal path: distinct identity.
	a.addCommand("owner", "/approve", false)
	if err := a.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := a.get(run.ID)
	if got.Status != store.StatusSucceeded || got.Outcome != "pr_opened" {
		t.Fatalf("result: %+v", got)
	}
	approvals, err := a.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Actor != "owner" {
		t.Fatalf("approvals = %+v, %v", approvals, err)
	}

	// git asked for a token per remote operation, and push used a fresh one.
	a.gitMu.Lock()
	if len(a.gitTokens) == 0 {
		t.Fatal("no clone")
	}
	// The execute-phase checkout: clone, ls-remote and push in one repository.
	gitTokens := append([]string(nil), a.gitTokens[len(a.gitTokens)-1]...)
	a.gitMu.Unlock()
	if len(gitTokens) < 3 || gitTokens[0] == gitTokens[len(gitTokens)-1] {
		t.Fatalf("git tokens = %d; push must not reuse the clone-time token", len(gitTokens))
	}
	a.api.mu.Lock()
	bad, rest := append([]string(nil), a.api.badAuth...), len(a.api.restAuth)
	a.api.mu.Unlock()
	if len(bad) > 0 || rest == 0 {
		t.Fatalf("REST calls without a repo-bound App token: %v", bad)
	}

	// Commits are attributed to the App bot.
	author := git(t, a.root, "--git-dir", a.remote, "log", "-1", "--format=%an <%ae>", "refs/heads/"+got.Branch)
	if author != "patchbot[bot] <4242+patchbot[bot]@users.noreply.github.com>" {
		t.Fatalf("commit author = %q", author)
	}

	// No minted token (or its git header form, or the PAT) appears in the
	// database, logs, posted comments, errors, artifacts or the command env.
	var haystack bytes.Buffer
	_ = filepath.WalkDir(a.cfg.DataDir(), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(path) //nolint:gosec // test reads its own temp data dir.
			haystack.Write(data)
		}
		return nil
	})
	haystack.WriteString(logs.String())
	a.envMu.Lock()
	haystack.WriteString(strings.Join(a.envs, "\n"))
	envCount := len(a.envs)
	a.envMu.Unlock()
	a.mu.Lock()
	for _, c := range a.comments {
		haystack.WriteString(c.Body)
	}
	a.mu.Unlock()
	if envCount == 0 {
		t.Fatal("no repository command ran")
	}
	minted := a.api.tokens()
	if len(minted) == 0 {
		t.Fatal("no tokens minted")
	}
	for _, token := range append(minted, patFallback) {
		for _, form := range []string{token, github.GitHeaderForm(token)} {
			if bytes.Contains(haystack.Bytes(), []byte(form)) {
				t.Fatalf("credential form leaked into persisted or logged output")
			}
		}
		if token != patFallback && a.redactorOut.Redact(token) == token {
			t.Fatal("minted token not registered with the caller's redactor")
		}
		if token != patFallback && a.redactorOut.Redact(github.GitHeaderForm(token)) == github.GitHeaderForm(token) {
			t.Fatal("git header form not registered with the caller's redactor")
		}
	}
}

func TestAppModeRevokedInstallationFailsClosedWithoutPAT(t *testing.T) {
	t.Parallel()
	a, err := newAppFixture(t, specStep())
	if err != nil {
		t.Fatal(err)
	}
	a.api.mu.Lock()
	a.api.revoked = true
	a.api.mu.Unlock()
	run, err := a.r.Start(t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, Mode: "fix"})
	if err == nil && run.Status != store.StatusFailed {
		t.Fatalf("revoked installation: status %s, err %v", run.Status, err)
	}
	a.api.mu.Lock()
	defer a.api.mu.Unlock()
	if len(a.api.restAuth) != 0 {
		t.Fatal("a REST call succeeded after revocation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for path := range a.hits {
		if !strings.Contains(path, "/installation") {
			t.Fatalf("request %s reached GitHub without an App token (PAT fallback?)", path)
		}
	}
}

func TestAppModeStartupFailsClosed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "test")
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	key := writeAppKey(t)
	if err := os.Chmod(key, 0o644); err != nil { // #nosec G302 -- deliberately unsafe mode under test.
		t.Fatal(err)
	}
	data = append(data, fmt.Appendf(nil, "[github]\nauth = \"app\"\napp_id = 123\nprivate_key_file = %q\n", key)...)
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil { // #nosec G703 -- test-owned temp config path.
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	opts := f.opts
	opts.GitHub = nil
	opts.Tokens = runner.Token{Value: secret.New(patFallback)}
	if _, err := runner.New(t.Context(), cfg, f.db, opts); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unsafe key mode accepted or unclear error: %v", err)
	}
}
