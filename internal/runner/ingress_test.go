package runner_test

import (
	"net/http"
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
	// The first sweep is a baseline; only labels applied afterwards start runs.
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
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

func TestFirstLabelSweepIsBaseline(t *testing.T) {
	f := newFixture(t, "true", specStep(), specStep())
	f.cfg.Webhook.Enabled = true
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-fix"}}
	f.mu.Unlock()
	// Enabling webhooks must not mass-start runs for already-labelled issues.
	for range 2 {
		if err := f.r.SyncLabels(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, true); err != nil {
		t.Fatal(err)
	}
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("baseline started runs: %+v %v", page, err)
	}
	// A later change is an observed label addition and starts exactly once.
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-refine"}}
	f.mu.Unlock()
	for range 2 {
		if err := f.r.SyncLabels(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	page, err = f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].Mode != "refine" {
		t.Fatalf("later label did not start once: %+v %v", page, err)
	}
}

func TestFirstSweepWithoutLabelsStillBaselines(t *testing.T) {
	f := newFixture(t, "true", specStep())
	f.cfg.Webhook.Enabled = true
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-refine"}}
	f.mu.Unlock()
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("label after empty baseline ignored: %+v %v", page, err)
	}
}

func TestLabelPollUsesListingAndSkipsHandledIssues(t *testing.T) {
	f := newFixture(t, "true", specStep())
	f.cfg.Webhook.Enabled = true
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.issue.Labels = []github.Label{{Name: "ai-refine"}}
	f.mu.Unlock()
	if err := f.r.SyncLabels(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := f.db.ListRuns(t.Context(), store.RunFilter{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("label did not start: %+v %v", page, err)
	}
	// Steady state: the handled label costs one repo read and one listing per
	// sweep, with no per-issue reads and no start attempts.
	f.mu.Lock()
	f.hits = nil
	f.mu.Unlock()
	for range 3 {
		if err := f.r.SyncLabels(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]int{"GET /repos/owner/repo": 3, "GET /repos/owner/repo/issues": 3}
	if len(f.hits) != len(want) || f.hits["GET /repos/owner/repo"] != 3 || f.hits["GET /repos/owner/repo/issues"] != 3 {
		t.Fatalf("steady-state label poll requests %v, want %v", f.hits, want)
	}
}

func TestGoneIssueIsDone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		f := newFixture(t, "true")
		f.mu.Lock()
		f.issueStatus = status
		f.mu.Unlock()
		for _, labels := range []bool{false, true} {
			if err := f.r.ReconcileIngress(t.Context(), "owner/repo", 12, labels); err != nil {
				t.Fatalf("status %d: %v", status, err)
			}
		}
	}
}
