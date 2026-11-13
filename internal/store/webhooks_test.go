package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestWebhookDurabilityAndAtomicStartKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	original := Delivery{ID: "delivery", Event: "issues", Repo: "owner/repo", Issue: 12}
	if err := db.RecordDelivery(t.Context(), original, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDelivery(t.Context(), Delivery{ID: "delivery", Repo: "other/repo"}, 10); err != nil {
		t.Fatal(err)
	}
	state, err := db.ObserveTriggerLabel(t.Context(), "owner/repo", 12, "ai-refine")
	if err != nil || state.Generation != 1 || state.Handled {
		t.Fatalf("generation %+v: %v", state, err)
	}
	// Simulate a crash after Start committed, before delivery completion.
	first := mustRun(t, db, Run{Repo: "owner/repo", IssueNumber: 12, Status: StatusFailed, IngressKey: "label:owner/repo:12:1"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	pending, err := db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 1 || pending[0] != original {
		t.Fatalf("lost receipt: %+v %v", pending, err)
	}
	state, err = db.ObserveTriggerLabel(t.Context(), "owner/repo", 12, "ai-refine")
	if err != nil || state.Generation != 1 {
		t.Fatalf("lost generation: %+v %v", state, err)
	}
	if _, err := db.CreateRun(t.Context(), Run{Repo: "owner/repo", IssueNumber: 12, IngressKey: "label:owner/repo:12:1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed terminal run %d: %v", first.ID, err)
	}
	page, err := db.ListRuns(t.Context(), RunFilter{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("duplicate run: %+v %v", page, err)
	}
	if err := db.CompleteDelivery(t.Context(), original.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDelivery(t.Context(), original, 10); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed delivery replayed: %+v %v", pending, err)
	}
}

func TestDeliveryBacklogAttemptsAndPrune(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	for i := range 3 {
		if err := db.RecordDelivery(ctx, Delivery{ID: fmt.Sprint("d", i), Event: "issues", Repo: "owner/repo", Issue: 12}, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordDelivery(ctx, Delivery{ID: "over", Event: "issues", Repo: "owner/repo", Issue: 12}, 3); !errors.Is(err, ErrDeliveryBacklog) {
		t.Fatalf("backlog not enforced: %v", err)
	}
	// A redelivery of a known ID is still a harmless duplicate.
	if err := db.RecordDelivery(ctx, Delivery{ID: "d0"}, 3); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		abandoned, err := db.FailDeliveries(ctx, []string{"d0", "d1"}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(0); attempt == 2 {
			want = 2
			if abandoned != want {
				t.Fatalf("abandoned %d, want %d", abandoned, want)
			}
		} else if abandoned != want {
			t.Fatalf("abandoned early: %d", abandoned)
		}
	}
	if err := db.CompleteDeliveries(ctx, []string{"d2"}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingDeliveries(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after give-up: %+v %v", pending, err)
	}
	if n, err := db.PruneDeliveries(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("pruned fresh receipts: %d %v", n, err)
	}
	if n, err := db.PruneDeliveries(ctx, time.Now().Add(time.Hour)); err != nil || n != 3 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := db.RecordDelivery(ctx, Delivery{ID: "after", Event: "issues", Repo: "owner/repo", Issue: 12}, 3); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerHandledAndBaseline(t *testing.T) {
	db := openTestStore(t)
	ctx := t.Context()
	if ok, err := db.RepoBaselined(ctx, "owner/repo"); err != nil || ok {
		t.Fatalf("fresh repo baselined: %v %v", ok, err)
	}
	if err := db.MarkRepoBaselined(ctx, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRepoBaselined(ctx, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RepoBaselined(ctx, "owner/repo"); err != nil || !ok {
		t.Fatalf("baseline lost: %v %v", ok, err)
	}
	state, err := db.ObserveTriggerLabel(ctx, "owner/repo", 12, "ai-fix")
	if err != nil {
		t.Fatal(err)
	}
	// A stale generation cannot mark a newer label handled.
	if err := db.MarkTriggerHandled(ctx, "owner/repo", 12, state.Generation+1); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTriggerHandled(ctx, "owner/repo", 12, state.Generation); err != nil {
		t.Fatal(err)
	}
	if state, err = db.ObserveTriggerLabel(ctx, "owner/repo", 12, "ai-fix"); err != nil || !state.Handled || state.Generation != 1 {
		t.Fatalf("handled lost: %+v %v", state, err)
	}
	if state, err = db.ObserveTriggerLabel(ctx, "owner/repo", 12, "ai-refine"); err != nil || state.Handled || state.Generation != 2 {
		t.Fatalf("change did not rearm: %+v %v", state, err)
	}
	labels, err := db.TriggerLabels(ctx, "owner/repo")
	if err != nil || len(labels) != 1 || labels[12].Label != "ai-refine" || labels[12].Handled {
		t.Fatalf("trigger labels: %+v %v", labels, err)
	}
	if other, err := db.TriggerLabels(ctx, "other/repo"); err != nil || len(other) != 0 {
		t.Fatalf("labels leaked across repos: %+v %v", other, err)
	}
}
