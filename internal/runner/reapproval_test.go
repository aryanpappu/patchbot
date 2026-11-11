package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/agent"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

// Clone/baseline invalidations must release the execution claim as well as
// return to waiting. A plain Approve followed by Execute must remain usable.
func TestReapprovalAfterCloneOrBaselineDenial(t *testing.T) {
	for _, boundary := range []string{"clone", "baseline"} {
		for _, denial := range []string{"expiry", "issue freshness"} {
			t.Run(boundary+"/"+denial, func(t *testing.T) {
				f := newFixture(t, "true", specStep(), writeStep(), submitStep())
				run := f.start("fix", true)
				f.approve(run.ID)
				mutate := func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if denial == "expiry" {
						f.offset = 15 * 24 * time.Hour
					} else {
						f.issue.Body = "Changed during execution"
					}
				}
				opts := f.opts
				var queued func(context.Context) error
				opts.Dispatch = func(_ context.Context, _ int64, work func(context.Context) error) error {
					queued = work
					return nil
				}
				db := &findingStore{Store: f.db}
				if boundary == "clone" {
					clone := opts.Clone
					opts.Clone = func(ctx context.Context, options gitx.CloneOptions) (*gitx.Repo, error) {
						repo, err := clone(ctx, options)
						if err == nil {
							mutate()
						}
						return repo, err
					}
				} else {
					db.afterStep = func(step store.Step) {
						if step.Kind == "test" && step.Phase == store.PhaseBaseline && step.Status != "running" {
							mutate()
						}
					}
				}
				service := findingRunner(t, f, db, opts)
				if err := service.Execute(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				if queued == nil {
					t.Fatal("execution was not dispatched")
				}
				if err := queued(t.Context()); !errors.Is(err, agent.ErrApprovalDenied) {
					t.Fatalf("expected approval denial: %v", err)
				}
				current := f.get(run.ID)
				if current.Status != store.StatusAwaitingApproval || current.Phase != store.PhaseRefine {
					t.Fatalf("denial retained execution claim: %s/%s", current.Status, current.Phase)
				}
				f.mu.Lock()
				f.offset, f.issue.Body = 0, "Create fixed.txt"
				f.mu.Unlock()
				// Remove the fault and dispatch a second execution of the same run.
				opts.Clone, db.afterStep = f.opts.Clone, nil
				service = findingRunner(t, f, db, opts)
				if err := service.Approve(t.Context(), run.ID, "local-owner"); err != nil {
					t.Fatal(err)
				}
				if err := service.Execute(t.Context(), run.ID); err != nil {
					t.Fatalf("reapproval could not dispatch: %v", err)
				}
				if err := queued(t.Context()); err != nil {
					t.Fatalf("reapproved execution failed: %v", err)
				}
				if f.get(run.ID).Status != store.StatusSucceeded {
					t.Fatal("reapproved execution did not finish")
				}
			})
		}
	}
}

// Admission failures are checked before a decision is made.
func TestApprovalAdmissionFailureLeavesDecisionUnchanged(t *testing.T) {
	f := newFixture(t, "true", specStep())
	run := f.start("fix", true)
	opts := f.opts
	full := errors.New("queue full")
	opts.Admit = func(ctx context.Context) (context.Context, func(), error) { return ctx, nil, full }
	opts.Dispatch = func(context.Context, int64, func(context.Context) error) error {
		t.Fatal("unadmitted work dispatched")
		return nil
	}
	service, err := runner.New(t.Context(), f.cfg, f.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ApproveAndExecute(t.Context(), run.ID, "operator"); !errors.Is(err, full) {
		t.Fatalf("admission error lost: %v", err)
	}
	approvals, err := f.db.ListApprovals(t.Context(), run.ID)
	if err != nil || len(approvals) != 0 || f.get(run.ID).Generation != run.Generation {
		t.Fatalf("undispatchable decision committed: %+v, %v", approvals, err)
	}
}
