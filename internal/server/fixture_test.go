package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

const testCredential = "fixture-private-credential"

type fakeGitHub struct {
	github.Client
	mu       sync.Mutex
	base     string
	comments []github.Comment
	prs      []github.PullRequest
	polls    int
}

func (*fakeGitHub) GetRepo(context.Context, github.RepoRef) (github.Repo, error) {
	return github.Repo{FullName: "owner/repo", DefaultBranch: "main", Permissions: github.Permissions{Push: true}}, nil
}
func (*fakeGitHub) GetIssue(_ context.Context, _ github.RepoRef, n int) (github.Issue, error) {
	return github.Issue{Number: n, Title: "Create result", Body: "Add result.txt", State: "open"}, nil
}
func (g *fakeGitHub) GetBranch(_ context.Context, _ github.RepoRef, name string) (github.Branch, bool, error) {
	branch := github.Branch{Name: name}
	branch.Commit.SHA = g.base
	return branch, name == "main", nil
}
func (*fakeGitHub) BranchExists(context.Context, github.RepoRef, string) (bool, error) {
	return false, nil
}
func (g *fakeGitHub) FindOpenPR(context.Context, github.RepoRef, string) (*github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.prs) > 0 {
		pr := g.prs[0]
		return &pr, nil
	}
	return nil, nil
}
func (g *fakeGitHub) CreatePR(_ context.Context, _ github.RepoRef, input github.NewPR) (github.PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := github.PullRequest{Number: 7, HTMLURL: "https://github.com/owner/repo/pull/7", Title: input.Title, Body: input.Body, Draft: input.Draft}
	g.prs = append(g.prs, pr)
	return pr, nil
}
func (g *fakeGitHub) CreateIssueComment(_ context.Context, _ github.RepoRef, _ int, body string) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().UTC()
	c := github.Comment{ID: int64(len(g.comments) + 1), Author: "patchbot", Body: body, CreatedAt: now, UpdatedAt: now}
	g.comments = append(g.comments, c)
	return c, nil
}
func (g *fakeGitHub) GetIssueComment(_ context.Context, _ github.RepoRef, id int64) (github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.comments {
		if c.ID == id {
			return c, nil
		}
	}
	return github.Comment{}, errors.New("missing comment")
}
func (g *fakeGitHub) ListIssueComments(context.Context, github.RepoRef, int, time.Time) ([]github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.polls++
	return append([]github.Comment(nil), g.comments...), nil
}
func (*fakeGitHub) IsCollaborator(context.Context, github.RepoRef, string) (bool, error) {
	return true, nil
}

type fixture struct {
	t        *testing.T
	cfg      *config.Config
	db       *store.SQLite
	server   *Server
	provider *fake.Provider
	gh       *fakeGitHub
	remote   string
	token    string
}

func newFixture(t *testing.T, p *fake.Provider, adjust func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, provider: p, gh: &fakeGitHub{}}
	if f.provider == nil {
		f.provider = fake.New("anthropic")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "patchbot.toml")
	data := fmt.Sprintf(`[defaults]
approvers = ["owner"]
[providers.anthropic]
api_key_env = ""
default_model = "test-model"
models = ["test-model", "configured"]
price_free = true
[approval]
local_actor = "operator"
[data]
dir = %q
[server]
addr = "127.0.0.1:80"
[repos."owner/repo"]
commands = {test = ["test", "-f", "result.txt"]}
`, dir)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.cfg, err = config.Load(config.Options{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(t.Context(), f.cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	opts := Options{Runner: runner.Options{GitHub: f.gh, Tokens: runner.Token{Value: secret.New(testCredential)}, BotLogin: "patchbot", HeartbeatInterval: 10 * time.Millisecond, Constructors: llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return f.provider, nil }}, Clone: func(ctx context.Context, opts gitx.CloneOptions) (*gitx.Repo, error) {
		if f.remote == "" {
			return nil, errors.New("no local fixture remote")
		}
		opts.TestOnlyLocalRemote = f.remote
		return gitx.Clone(ctx, opts)
	}}, HeartbeatInterval: 20 * time.Millisecond, PollInterval: 10 * time.Millisecond, SyncInterval: time.Hour, StreamWriteTimeout: 100 * time.Millisecond}
	if adjust != nil {
		adjust(&opts)
	}
	f.token = opts.Token.Reveal()
	f.server, err = New(t.Context(), f.cfg, f.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return f
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // Only fixed test git commands against temporary bare repositories.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) withRepo() {
	f.t.Helper()
	dir := f.t.TempDir()
	seed := filepath.Join(dir, "seed")
	if err := os.Mkdir(seed, 0700); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("fixture\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, seed, "add", "README.md")
	runGit(f.t, seed, "commit", "-m", "seed")
	f.remote = filepath.Join(dir, "remote.git")
	runGit(f.t, dir, "clone", "--bare", seed, f.remote)
	f.gh.base = runGit(f.t, seed, "rev-parse", "HEAD")
}

func draft() tools.SpecDraft {
	return tools.SpecDraft{Summary: "Create result", AcceptanceCriteria: []string{"result.txt exists"}, Files: []tools.FileChange{{Path: "result.txt", Change: "create"}}, Risks: []string{}, TestPlan: []string{"Run test"}, OpenQuestions: []string{}}
}
func toolStep(name string, args any) fake.Step {
	raw, _ := json.Marshal(args)
	return fake.Step{ToolCalls: []llm.ToolCall{{Name: name, Args: raw}}, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 20}}
}
func specStep() fake.Step { return toolStep("submit_spec", draft()) }
func writeStep() fake.Step {
	return toolStep("write_file", map[string]string{"path": "result.txt", "content": "result\n"})
}
func submitStep() fake.Step {
	return toolStep("submit", tools.Submission{Summary: "Created result", PRTitle: "Create result", Criteria: []tools.CriterionResult{{Text: "result.txt exists", Met: true}}})
}

func (f *fixture) request(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://"+f.cfg.Server.Addr+path, strings.NewReader(body))
	if f.token != "" {
		r.Header.Set("Authorization", "Bearer "+f.token)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	for _, token := range []string{testCredential, f.token} {
		if token != "" && strings.Contains(w.Body.String(), token) {
			f.t.Fatal("credential appeared in response")
		}
	}
	return w
}

func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if status >= 400 {
		var body struct {
			Error struct{ Kind, Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Kind == "" || body.Error.Message == "" {
			t.Fatalf("invalid error envelope: %s", w.Body.String())
		}
	}
}
func (f *fixture) responseRun(w *httptest.ResponseRecorder) store.Run {
	f.t.Helper()
	var value struct{ Run store.Run }
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		f.t.Fatal(err)
	}
	if value.Run.ID == 0 {
		f.t.Fatal("missing run ID")
	}
	return value.Run
}

func (f *fixture) waitFor(id int64, status store.Status) store.Run {
	f.t.Helper()
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := f.db.GetRun(f.t.Context(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		if run.Status == status {
			return run
		}
		if !run.Status.IsActive() {
			f.t.Fatalf("unexpected run state: %+v", run)
		}
		select {
		case <-deadline:
			f.t.Fatalf("run %d did not reach %s: %+v", id, status, run)
		case <-ticker.C:
		}
	}
}

func (f *fixture) seed(status store.Status) store.Run {
	f.t.Helper()
	run, err := f.db.CreateRun(f.t.Context(), store.Run{Repo: "owner/repo", IssueNumber: time.Now().UnixNano(), Status: store.StatusAwaitingApproval, Provider: "anthropic", RefineModel: "test-model", CodeModel: "test-model", Mode: "fix", DryRun: true})
	if err != nil {
		f.t.Fatal(err)
	}
	d := draft()
	spec, err := f.db.SaveSpec(f.t.Context(), store.Spec{RunID: run.ID, BodyMD: d.Markdown(), Hash: d.Hash(), IssueHash: agent.IssueHash("Create result", "Add result.txt"), Source: "agent"})
	if err != nil {
		f.t.Fatal(err)
	}
	if status == store.StatusApproved {
		err = f.db.DecideRun(f.t.Context(), run.Status, status, store.Approval{RunID: run.ID, SpecVersion: spec.Version, SpecHash: spec.Hash, Actor: "operator", Channel: "ui", Decision: "approve"}, run.Generation)
	} else if status != run.Status {
		err = f.db.SetRunStatus(f.t.Context(), run.ID, run.Status, status, store.RunPatch{Phase: store.PhaseRefine})
	}
	if err != nil {
		f.t.Fatal(err)
	}
	run, err = f.db.GetRun(f.t.Context(), run.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}
