package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	"github.com/aryanpappu/patchbot/internal/testhttp"
	"github.com/aryanpappu/patchbot/internal/tools"
)

type fixture struct {
	t                                    *testing.T
	cfg                                  *config.Config
	db                                   *store.SQLite
	r                                    *runner.Runner
	p                                    *fake.Provider
	opts                                 runner.Options
	root, remote, seed, configPath, base string
	mu                                   sync.Mutex
	issue                                github.Issue
	meta                                 github.Repo
	comments                             []github.Comment
	prs                                  []github.PullRequest
	posts                                int
	collaborators                        map[string]bool
	offset                               time.Duration
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test fixture git operations against t.TempDir.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T, command string, steps ...fake.Step) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir(), collaborators: map[string]bool{"owner": true, "outsider": true}, offset: 0}
	f.seed = filepath.Join(f.root, "seed")
	f.remote = filepath.Join(f.root, "remote.git")
	if err := os.Mkdir(f.seed, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.seed, "README.md"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "add", "README.md")
	git(t, f.seed, "commit", "-m", "initial")
	git(t, f.root, "clone", "--bare", f.seed, f.remote)
	f.base = git(t, f.seed, "rev-parse", "HEAD")
	f.issue = github.Issue{Number: 12, Title: "Add fixed file", Body: "Create fixed.txt", State: "open"}
	f.meta = github.Repo{FullName: "owner/repo", DefaultBranch: "main", Permissions: github.Permissions{Push: true}}
	f.configPath = filepath.Join(f.root, "patchbot.toml")
	commandArgs := fmt.Sprintf("[%q]", command)
	if command == "test" {
		commandArgs = `["test", "-f", "fixed.txt"]`
	}
	data := fmt.Sprintf(`[defaults]
provider = "anthropic"
approvers = ["owner", "nonmember"]
[providers.anthropic]
api_key_env = ""
default_model = "test-model"
price_free = true
[approval]
local_actor = "local-owner"
[data]
dir = %q
[repos."owner/repo"]
commands = { test = %s }
`, filepath.Join(f.root, "data"), commandArgs)
	if err := os.WriteFile(f.configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	if err := os.MkdirAll(cfg.DataDir(), 0700); err != nil {
		t.Fatal(err)
	}
	f.db, err = store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	server := testhttp.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	tokens := runner.Token{Value: secret.New("fixture-credential")}
	client, err := github.New(server.Client(), server.URL, tokens)
	if err != nil {
		t.Fatal(err)
	}
	f.p = fake.New("anthropic", steps...)
	f.opts = runner.Options{GitHub: client, Tokens: tokens, BotLogin: "patchbot", Clock: f.clock, HeartbeatInterval: 5 * time.Millisecond, Constructors: llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return f.p, nil }}, Clone: func(ctx context.Context, opts gitx.CloneOptions) (*gitx.Repo, error) {
		opts.TestOnlyLocalRemote = f.remote
		return gitx.Clone(ctx, opts)
	}}
	f.r, err = runner.New(t.Context(), cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().UTC().Add(f.offset)
}

func (f *fixture) serveHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(status int, value any) {
		w.WriteHeader(status)
		if value != nil {
			_ = json.NewEncoder(w).Encode(value)
		}
	}
	path := strings.TrimPrefix(req.URL.Path, "/repos/owner/repo")
	switch {
	case req.URL.Path == "/user":
		reply(200, map[string]string{"login": "patchbot"})
	case path == "":
		reply(200, f.meta)
	case path == "/issues/12" && req.Method == http.MethodGet:
		reply(200, f.issue)
	case strings.HasPrefix(path, "/collaborators/"):
		if f.collaborators[strings.TrimPrefix(path, "/collaborators/")] {
			reply(204, nil)
		} else {
			reply(404, map[string]string{"message": "Not Found"})
		}
	case path == "/issues/12/comments" && req.Method == http.MethodGet:
		values := make([]map[string]any, 0, len(f.comments))
		for _, c := range f.comments {
			values = append(values, commentJSON(c))
		}
		reply(200, values)
	case strings.HasPrefix(path, "/issues/comments/") && req.Method == http.MethodGet:
		var id int64
		_, _ = fmt.Sscanf(path, "/issues/comments/%d", &id)
		for _, c := range f.comments {
			if c.ID == id {
				reply(200, commentJSON(c))
				return
			}
		}
		reply(404, nil)
	case path == "/issues/12/comments" && req.Method == http.MethodPost:
		var input struct{ Body string }
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			reply(400, nil)
			return
		}
		now := time.Now().UTC()
		c := github.Comment{ID: int64(len(f.comments) + 1), Author: "patchbot", Body: input.Body, CreatedAt: now, UpdatedAt: now}
		f.comments = append(f.comments, c)
		f.posts++
		reply(201, commentJSON(c))
	case path == "/pulls" && req.Method == http.MethodGet:
		reply(200, f.prs)
	case path == "/pulls" && req.Method == http.MethodPost:
		var input github.NewPR
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			reply(400, nil)
			return
		}
		pr := github.PullRequest{Number: 42, HTMLURL: "https://github.com/owner/repo/pull/42", Title: input.Title, Body: input.Body, Draft: input.Draft, State: "open", Head: github.PRBranch{Ref: input.Head}, Base: github.PRBranch{Ref: input.Base}}
		f.prs = append(f.prs, pr)
		f.posts++
		reply(201, pr)
	case strings.HasPrefix(path, "/branches/"):
		name := strings.TrimPrefix(path, "/branches/")
		if name == "main" {
			branch := github.Branch{Name: "main"}
			branch.Commit.SHA = f.base
			reply(200, branch)
		} else {
			reply(404, map[string]string{"message": "Not Found"})
		}
	default:
		f.t.Errorf("unexpected GitHub %s %s", req.Method, req.URL.Path)
		reply(404, nil)
	}
}

func commentJSON(c github.Comment) map[string]any {
	return map[string]any{"id": c.ID, "body": c.Body, "user": map[string]string{"login": c.Author}, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "html_url": c.HTMLURL}
}

func (f *fixture) addCommand(author, body string, edited bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC().Add(time.Second)
	updated := now
	if edited {
		updated = now.Add(time.Second)
	}
	f.comments = append(f.comments, github.Comment{ID: int64(len(f.comments) + 1), Author: author, Body: body, CreatedAt: now, UpdatedAt: updated})
}

func draft(questions ...string) tools.SpecDraft {
	if questions == nil {
		questions = []string{}
	}
	return tools.SpecDraft{Summary: "Add a fixed file", Risks: []string{}, AcceptanceCriteria: []string{"fixed.txt exists"}, Files: []tools.FileChange{{Path: "fixed.txt", Change: "create file"}}, TestPlan: []string{"Run tests"}, OpenQuestions: questions}
}

func toolStep(name string, args any) fake.Step {
	raw, _ := json.Marshal(args)
	return fake.Step{ToolCalls: []llm.ToolCall{{Name: name, Args: raw}}, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 50}}
}

func specStep(questions ...string) fake.Step { return toolStep("submit_spec", draft(questions...)) }
func writeStep() fake.Step {
	return toolStep("write_file", map[string]string{"path": "fixed.txt", "content": "fixed\n"})
}
func submitStep() fake.Step {
	return toolStep("submit", tools.Submission{Summary: "Created fixed.txt", PRTitle: "Add fixed file", Criteria: []tools.CriterionResult{{Text: "fixed.txt exists", Met: true}}})
}

func (f *fixture) start(mode string, dry bool) store.Run {
	f.t.Helper()
	run, err := f.r.Start(f.t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, Mode: mode, DryRun: dry})
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}
func (f *fixture) get(id int64) store.Run {
	f.t.Helper()
	run, err := f.db.GetRun(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}
func (f *fixture) approve(id int64) {
	f.t.Helper()
	if err := f.r.Approve(f.t.Context(), id, "local-owner"); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) assertClean() {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.cfg.DataDir(), "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	if len(entries) != 0 {
		f.t.Fatalf("workspace was retained: %v", entries)
	}
}
func (f *fixture) branches() string {
	return git(f.t, f.root, "--git-dir", f.remote, "for-each-ref", "--format=%(refname)", "refs/heads/patchbot/")
}

func TestFixFlow(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep())
	run := f.start("fix", false)
	if run.Status != store.StatusAwaitingApproval || run.BaseSHA != f.base {
		t.Fatalf("refine: %+v", run)
	}
	f.assertClean()
	if err := f.r.Execute(t.Context(), run.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("unapproved execution: %v", err)
	}
	f.approve(run.ID)
	if err := f.r.Execute(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(run.ID)
	if got.Status != store.StatusSucceeded || got.Outcome != "pr_opened" || got.PRDraft || got.TestAttempts != 1 {
		t.Fatalf("result: %+v", got)
	}
	if !strings.Contains(f.branches(), "patchbot/issue-12") {
		t.Fatal("branch was not pushed")
	}
	if len(f.prs) != 1 || !strings.Contains(f.prs[0].Body, "- [x] AC1") || !strings.Contains(f.prs[0].Body, "#issuecomment-1") || !strings.Contains(f.prs[0].Body, "Harness tests: **passed**") {
		t.Fatalf("PR: %+v", f.prs)
	}
	events, err := f.db.EventsAfter(t.Context(), run.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	phases := map[store.Phase]bool{}
	for _, event := range events {
		if event.Type == "run.status" {
			var payload struct {
				Phase store.Phase `json:"phase"`
			}
			if err := json.Unmarshal(event.PayloadJSON, &payload); err != nil {
				t.Fatal(err)
			}
			phases[payload.Phase] = true
		}
	}
	for _, phase := range []store.Phase{store.PhasePreflight, store.PhaseClone, store.PhaseRefine, store.PhaseBaseline, store.PhaseCode, store.PhaseVerify, store.PhasePublish} {
		if !phases[phase] {
			t.Errorf("missing phase event %s", phase)
		}
	}
	f.p.AssertConsumed(t)
	f.assertClean()
}

func TestRefineNeverCodesAndInvestigate(t *testing.T) {
	for _, mode := range []string{"refine", "investigate"} {
		t.Run(mode, func(t *testing.T) {
			step := specStep()
			if mode == "investigate" {
				step = toolStep("submit_report", map[string]string{"report_md": "Repository evidence and recommendation."})
			}
			f := newFixture(t, "false", step)
			run := f.start(mode, false)
			if mode == "refine" {
				f.approve(run.ID)
				if err := f.r.Execute(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				if f.get(run.ID).Outcome != "spec_approved" {
					t.Fatal("refine did not finish approval")
				}
			} else {
				if run.Outcome != "investigated" {
					t.Fatalf("run: %+v", run)
				}
				if _, err := f.db.LatestSpec(t.Context(), run.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("investigation saved a spec")
				}
			}
			if f.p.Calls() != 1 || len(f.prs) != 0 || f.branches() != "" {
				t.Fatal("read-only mode coded or published")
			}
			for _, req := range f.p.Requests() {
				for _, tool := range req.Tools {
					if tool.Name == "write_file" || tool.Name == "run_cmd" {
						t.Fatal("write tool in read-only stage")
					}
				}
			}
			f.assertClean()
		})
	}
}

func TestNeedsInfoThenGitHubRevise(t *testing.T) {
	f := newFixture(t, "true", specStep("What contents?"), specStep())
	run := f.start("fix", false)
	if run.Status != store.StatusNeedsInfo {
		t.Fatal(run.Status)
	}
	if err := f.r.Approve(t.Context(), run.ID, "owner"); !errors.Is(err, store.ErrConflict) {
		t.Fatal(err)
	}
	f.addCommand("owner", "/revise Use fixed as contents", false)
	if err := f.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != 2 || !strings.Contains(spec.Notes, "fixed as contents") || f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatalf("spec: %+v", spec)
	}
	if err := f.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.p.Calls() != 2 {
		t.Fatal("replayed revision")
	}
	f.assertClean()
}

func TestApprovalInvalidation(t *testing.T) {
	for _, change := range []string{"edit", "revise", "issue", "comment"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, "true", specStep(), specStep())
			run := f.start("fix", false)
			f.approve(run.ID)
			switch change {
			case "edit":
				edited := draft()
				edited.Summary = "Revised summary"
				if _, err := f.r.EditSpec(t.Context(), run.ID, edited); err != nil {
					t.Fatal(err)
				}
			case "revise":
				if err := f.r.Revise(t.Context(), run.ID, "change it"); err != nil {
					t.Fatal(err)
				}
			case "issue":
				f.mu.Lock()
				f.issue.Body = "different issue body"
				f.mu.Unlock()
			case "comment":
				f.mu.Lock()
				f.comments[0].Body += "hand edit"
				f.mu.Unlock()
			}
			err := f.r.Execute(t.Context(), run.ID)
			if err == nil {
				t.Fatal("changed approval authorized coding")
			}
			if len(f.prs) != 0 || f.branches() != "" {
				t.Fatal("invalid approval published")
			}
			if f.get(run.ID).Status != store.StatusAwaitingApproval {
				t.Fatalf("status: %s", f.get(run.ID).Status)
			}
			if change == "issue" {
				if err := f.r.Approve(t.Context(), run.ID, "local"); !errors.Is(err, agent.ErrApprovalDenied) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGitHubApprovalAuthorization(t *testing.T) {
	for _, command := range []struct {
		name, author string
		edited       bool
	}{{"non-approver", "outsider", false}, {"non-collaborator", "nonmember", false}, {"edited", "owner", true}, {"bot", "patchbot", false}} {
		t.Run(command.name, func(t *testing.T) {
			f := newFixture(t, "true", specStep())
			run := f.start("fix", false)
			f.addCommand(command.author, "/approve", command.edited)
			if err := f.r.SyncWaiting(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.get(run.ID).Status != store.StatusAwaitingApproval || f.p.Calls() != 1 {
				t.Fatal("unauthorized command counted")
			}
		})
	}
	t.Run("allowed", func(t *testing.T) {
		f := newFixture(t, "true", specStep(), writeStep(), submitStep())
		run := f.start("fix", false)
		f.addCommand("owner", "/approve", false)
		if err := f.r.SyncWaiting(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.get(run.ID).Status != store.StatusSucceeded {
			t.Fatal(f.get(run.ID).Status)
		}
	})
}

func TestDryRun(t *testing.T) {
	f := newFixture(t, "true", specStep(), writeStep(), submitStep())
	run := f.start("fix", true)
	f.approve(run.ID)
	if err := f.r.Execute(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if f.posts != 0 || f.branches() != "" || len(f.prs) != 0 {
		t.Fatal("dry run mutated GitHub")
	}
	for _, kind := range []string{"spec", "diff", "pr_title", "pr_body"} {
		a, err := f.db.GetArtifact(t.Context(), run.ID, kind, 0)
		if err != nil || a.Content == "" {
			t.Fatalf("missing %s: %v", kind, err)
		}
	}
	if f.get(run.ID).Outcome != "dry_run" {
		t.Fatal(f.get(run.ID).Outcome)
	}
	f.assertClean()
}

func TestRejectAndDraftAttempts(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		f := newFixture(t, "true", specStep())
		run := f.start("fix", false)
		if err := f.r.Reject(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if f.get(run.ID).Status != store.StatusRejected || f.branches() != "" || len(f.prs) != 0 {
			t.Fatal("reject published or failed")
		}
	})
	t.Run("three attempts", func(t *testing.T) {
		f := newFixture(t, "false", specStep(), writeStep(), submitStep(), submitStep(), submitStep())
		run := f.start("fix", false)
		f.approve(run.ID)
		if err := f.r.Execute(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		got := f.get(run.ID)
		if got.Outcome != "pr_draft" || !got.PRDraft || got.TestAttempts != 3 || len(f.prs) != 1 || !f.prs[0].Draft {
			t.Fatalf("result: %+v", got)
		}
		f.p.AssertConsumed(t)
	})
}

func TestCancelDuringCode(t *testing.T) {
	f := newFixture(t, "true", specStep())
	entered := make(chan struct{})
	blocking := fake.NewFunc("anthropic", func(ctx context.Context, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	run := f.start("fix", false)
	f.approve(run.ID)
	f.opts.Constructors["anthropic"] = func(llm.ProviderConfig) (llm.Provider, error) { return blocking, nil }
	done := make(chan error, 1)
	go func() { done <- f.r.Execute(t.Context(), run.ID) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("code never started")
	}
	if err := f.r.Cancel(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop code")
	}
	if got := f.get(run.ID); got.Status != store.StatusCancelled {
		t.Fatal(got.Status)
	}
	if f.branches() != "" || len(f.prs) != 0 {
		t.Fatal("cancel published")
	}
	f.assertClean()
}
