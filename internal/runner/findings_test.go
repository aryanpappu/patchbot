package runner_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/execx"
	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

// Hooks run outside store transactions, at the precise boundaries where a
// different CLI process can win. Heartbeats are disabled for these schedules.
type findingStore struct {
	store.Store
	afterRead func(store.Run)
	afterStep func(store.Step)
}

func (s *findingStore) GetRun(ctx context.Context, id int64) (store.Run, error) {
	run, err := s.Store.GetRun(ctx, id)
	if err == nil && s.afterRead != nil {
		s.afterRead(run)
	}
	return run, err
}

func (s *findingStore) RecordStep(ctx context.Context, step store.Step) (store.Step, error) {
	saved, err := s.Store.RecordStep(ctx, step)
	if err == nil && s.afterStep != nil {
		s.afterStep(saved)
	}
	return saved, err
}

func findingRunner(t *testing.T, f *fixture, db store.Store, opts runner.Options) *runner.Runner {
	t.Helper()
	opts.HeartbeatInterval = time.Hour
	r, err := runner.New(t.Context(), f.cfg, db, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func otherProcess(t *testing.T, f *fixture) *runner.Runner {
	t.Helper()
	db, err := store.Open(t.Context(), f.cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return findingRunner(t, f, db, f.opts)
}

// Finding 1: a command's v1 snapshot must survive the call into approve/revise.
func TestGitHubDecisionCannotFollowUnseenSpec(t *testing.T) {
	for _, command := range []string{"/approve", "/revise improve it"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t, "true", specStep(), specStep())
			run := f.start("fix", false)
			other := otherProcess(t, f)
			f.addCommand("owner", command, false)
			var once sync.Once
			db := &findingStore{Store: f.db, afterRead: func(current store.Run) {
				if current.ID == run.ID && current.Status == store.StatusAwaitingApproval {
					once.Do(func() {
						d := draft()
						d.Summary = "unseen v2"
						if _, err := other.EditSpec(t.Context(), run.ID, d); err != nil {
							t.Fatal(err)
						}
					})
				}
			}}
			service := findingRunner(t, f, db, f.opts)
			err := service.SyncWaiting(t.Context())
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stale GitHub decision was not rejected: %v", err)
			}
			if got := f.get(run.ID); got.Status != store.StatusAwaitingApproval {
				t.Fatalf("unseen spec authorized: %s", got.Status)
			}
			actions, err := f.db.ListActions(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range actions {
				if action.Kind == "approve" || action.Kind == "revise" {
					t.Fatalf("stale decision persisted: %+v", action)
				}
			}
		})
	}
}

// Finding 2: another CLI can cancel while the local context is still live.
func TestPublicationRechecksCancellation(t *testing.T) {
	testPublicationBoundary(t, "cancel")
}

// Finding 3: approval expiry during work or push must stop publication.
func TestPublicationRechecksApprovalTTL(t *testing.T) {
	testPublicationBoundary(t, "expire")
}

func testPublicationBoundary(t *testing.T, change string) {
	t.Helper()
	for _, boundary := range []string{"before push", "before PR"} {
		t.Run(change+"/"+boundary, func(t *testing.T) {
			f := newFixture(t, "true", specStep(), writeStep(), submitStep())
			run := f.start("fix", false)
			f.approve(run.ID)
			other := otherProcess(t, f)
			var once sync.Once
			changed := false
			mutate := func() {
				once.Do(func() {
					changed = true
					if change == "cancel" {
						if err := other.Cancel(t.Context(), run.ID); err != nil {
							t.Fatal(err)
						}
					} else {
						f.mu.Lock()
						f.offset = 15 * 24 * time.Hour
						f.mu.Unlock()
					}
				})
			}
			db := &findingStore{Store: f.db}
			if boundary == "before push" {
				db.afterRead = func(current store.Run) {
					if current.HeadSHA != "" && current.Status == store.StatusRunning {
						mutate()
					}
				}
			} else {
				db.afterStep = func(step store.Step) {
					if step.Tool == "push" && step.Status == "ok" {
						mutate()
					}
				}
			}
			service := findingRunner(t, f, db, f.opts)
			err := service.Execute(t.Context(), run.ID)
			if !changed || err == nil {
				t.Fatalf("boundary reached: %t; publication error: %v", changed, err)
			}
			if change == "expire" && !errors.Is(err, agent.ErrApprovalDenied) {
				t.Fatalf("expired approval error: %v", err)
			}
			if change == "cancel" && f.get(run.ID).Status != store.StatusCancelled {
				t.Fatal("cancelled status overwritten")
			}
			if len(f.prs) != 0 || (boundary == "before push" && f.branches() != "") {
				t.Fatalf("publication after %s: branches=%s PRs=%d", change, f.branches(), len(f.prs))
			}
			if boundary == "before PR" && f.branches() == "" {
				t.Fatal("test did not reach the post-push boundary")
			}
			f.assertClean()
		})
	}
}

type issueHook struct {
	github.Client
	beforeIssue func()
}

func (g issueHook) GetIssue(ctx context.Context, repo github.RepoRef, number int) (github.Issue, error) {
	g.beforeIssue()
	return g.Client.GetIssue(ctx, repo, number)
}

// Finding 4: cancellation wins after EditSpec validates its source state.
func TestSpecEditCannotResurrectCancellation(t *testing.T) {
	f := newFixture(t, "true", specStep())
	run := f.start("fix", false)
	other := otherProcess(t, f)
	opts := f.opts
	opts.GitHub = issueHook{Client: opts.GitHub, beforeIssue: func() {
		if err := other.Cancel(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
	}}
	service := findingRunner(t, f, f.db, opts)
	if _, err := service.EditSpec(t.Context(), run.ID, draft()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cancel/edit race accepted: %v", err)
	}
	if got := f.get(run.ID); got.Status != store.StatusCancelled {
		t.Fatalf("cancelled run resurrected: %s", got.Status)
	}
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil || spec.Version != 1 {
		t.Fatalf("losing edit persisted content: v%d, %v", spec.Version, err)
	}
}

// Finding 5: hold the new revision inside the provider while the obsolete
// execution's clone fails. Channels establish the schedule without time waits.
func TestObsoleteCloneFailureCannotFailRevision(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	step := specStep()
	step.Expect = func(llm.ChatRequest) error {
		close(entered)
		<-release
		return nil
	}
	f := newFixture(t, "true", specStep(), step)
	run := f.start("fix", false)
	f.approve(run.ID)
	other := otherProcess(t, f)
	done := make(chan error, 1)
	opts := f.opts
	cloneErr := errors.New("obsolete clone failed")
	opts.Clone = func(context.Context, gitx.CloneOptions) (*gitx.Repo, error) {
		go func() { done <- other.Revise(t.Context(), run.ID, "new revision") }()
		<-entered
		return nil, cloneErr
	}
	service := findingRunner(t, f, f.db, opts)
	err := service.Execute(t.Context(), run.ID)
	current := f.get(run.ID)
	close(release)
	revisionErr := <-done
	if !errors.Is(err, cloneErr) {
		t.Fatalf("lost clone error: %v", err)
	}
	if current.Status != store.StatusRunning || current.Phase != store.PhaseRefine || revisionErr != nil {
		t.Fatalf("stale worker changed revision: %s/%s, %v", current.Status, current.Phase, revisionErr)
	}
	if f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatal("new revision could not finish")
	}
}

type executorFunc func(context.Context, execx.Spec) (execx.Result, error)

func (f executorFunc) Run(ctx context.Context, spec execx.Spec) (execx.Result, error) {
	return f(ctx, spec)
}

// Finding 6: reserve nearly all run time in persisted accounting, then inspect
// the context at the actual clone/executor boundary without waiting for expiry.
func TestPreAgentWorkUsesRemainingRunDeadline(t *testing.T) {
	for _, phase := range []string{"refine clone", "code clone", "baseline"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t, "true", specStep(), writeStep(), submitStep())
			f.cfg.Limits.RunTimeout = time.Hour
			var run store.Run
			if phase != "refine clone" {
				run = f.start("fix", false)
				f.approve(run.ID)
				if err := f.db.SaveArtifact(t.Context(), store.Artifact{RunID: run.ID, Kind: "accounting", Content: `{"Active":3540000000000}`}); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			check := func(ctx context.Context) {
				called = true
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				maximum := time.Minute
				if phase == "refine clone" {
					maximum = time.Hour
				}
				if !ok || remaining <= 0 || remaining > maximum {
					t.Errorf("%s has no remaining-run bound: deadline=%v remaining=%s", phase, ok, remaining)
				}
			}
			opts := f.opts
			if phase == "baseline" {
				opts.Executor = func(string) (execx.Executor, error) {
					return executorFunc(func(ctx context.Context, _ execx.Spec) (execx.Result, error) {
						check(ctx)
						return execx.Result{}, context.DeadlineExceeded
					}), nil
				}
			} else {
				opts.Clone = func(ctx context.Context, _ gitx.CloneOptions) (*gitx.Repo, error) {
					check(ctx)
					return nil, context.DeadlineExceeded
				}
			}
			service := findingRunner(t, f, f.db, opts)
			if phase == "refine clone" {
				_, _ = service.Start(t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, Mode: "fix"})
			} else {
				_ = service.Execute(t.Context(), run.ID)
			}
			if !called {
				t.Fatal("deadline boundary was not reached")
			}
		})
	}
}
