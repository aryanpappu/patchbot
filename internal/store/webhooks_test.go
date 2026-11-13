package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWebhookDurabilityAndAtomicStartKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	original := Delivery{ID: "delivery", Event: "issues", Repo: "owner/repo", Issue: 12}
	if err := db.RecordDelivery(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDelivery(t.Context(), Delivery{ID: "delivery", Repo: "other/repo"}); err != nil {
		t.Fatal(err)
	}
	generation, err := db.ObserveTriggerLabel(t.Context(), "owner/repo", 12, "ai-refine")
	if err != nil || generation != 1 {
		t.Fatalf("generation %d: %v", generation, err)
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
	generation, err = db.ObserveTriggerLabel(t.Context(), "owner/repo", 12, "ai-refine")
	if err != nil || generation != 1 {
		t.Fatalf("lost generation: %d %v", generation, err)
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
	if err := db.RecordDelivery(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingDeliveries(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed delivery replayed: %+v %v", pending, err)
	}
}
