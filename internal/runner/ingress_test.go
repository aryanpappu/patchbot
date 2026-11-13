package runner_test

import (
	"sync"
	"testing"

	"github.com/aryanpappu/patchbot/internal/github"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestLabelPollingRaceAndReorderedHints(t *testing.T) {
	f := newFixture(t, "true", specStep(), specStep())
	f.cfg.Webhook.Enabled = true
	f.cfg.Webhook.Secret = secret.New("fixture-webhook-secret")
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-refine"}}
	f.mu.Unlock()
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for n := range 12 {
		wg.Go(func() {
			if n%2 == 0 {
				errs <- f.r.SyncLabels(t.Context())
			} else {
				errs <- f.r.ReconcileIngress(t.Context(), "owner/repo", 12, true)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("race runs: %+v: %v", page, err)
	}
	run := page.Runs[0]
	if run.Mode != "refine" || run.Status != store.StatusAwaitingApproval {
		t.Fatalf("label bypassed gate: %+v", run)
	}
	// A label hint cannot consume a valid approval comment.
	f.addCommand("owner", "/approve", false)
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, true); err != nil {
		t.Fatal(err)
	}
	if f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatal("label approved run")
	}
	// Polling recovers the missed issue_comment delivery through the original gate.
	if err := f.r.SyncWaiting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.get(run.ID).Status != store.StatusSucceeded {
		t.Fatal("poll missed approval")
	}
	// Older comment/label deliveries now reconcile current state, without replay.
	for _, labels := range []bool{false, true, false, true} {
		if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, labels); err != nil {
			t.Fatal(err)
		}
	}
	page, err = f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("reordered hints replayed run: %+v %v", page, err)
	}
	// Observe removal before reapplication to deliberately rearm the trigger.
	f.mu.Lock()
	f.issue.Labels = nil
	f.mu.Unlock()
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-refine"}}
	f.mu.Unlock()
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err = f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 2 {
		t.Fatalf("reapplication did not rearm: %+v %v", page, err)
	}
}

func TestIngressCurrentStateAndScope(t *testing.T) {
	f := newFixture(t, "true", specStep())
	// Old labeled delivery arrives after the label has already been removed.
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, true); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ReconcileIngress(t.Context(), "other/repo", 12, true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-fix"}}
	f.meta.FullName = "other/repo"
	f.mu.Unlock()
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, true); err != nil {
		t.Fatal(err)
	}
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("ignored hints started run: %+v %v", page, err)
	}
}

func TestCommentHintsReuseRevisionAndApprovalGate(t *testing.T) {
	f := newFixture(t, "true", specStep(), specStep())
	run := f.start("refine", false)
	f.addCommand("outsider", "/approve", false)
	f.addCommand("owner", "/approve", true)
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, false); err != nil {
		t.Fatal(err)
	}
	if f.get(run.ID).Status != store.StatusAwaitingApproval {
		t.Fatal("ineligible comment approved")
	}
	f.addCommand("owner", "/revise improve description", false)
	for range 2 {
		if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, false); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := f.db.LatestSpec(t.Context(), run.ID)
	if err != nil || spec.Version != 2 {
		t.Fatalf("revision replayed: %+v %v", spec, err)
	}
	f.addCommand("owner", "/approve", false)
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, false); err != nil {
		t.Fatal(err)
	}
	if f.get(run.ID).Status != store.StatusSucceeded {
		t.Fatal("current eligible comment not reconciled")
	}
}
