package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestNotificationsFollowCommitsAndNeverRollbacks(t *testing.T) {
	db := openTestStore(t)
	run, err := db.CreateRun(t.Context(), Run{Repo: "owner/repo", IssueNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []Event
	stop := db.SubscribeEvents(func(event Event) {
		// Reading here also proves notification happens outside the write lock.
		events, err := db.EventsAfter(context.Background(), event.RunID, event.Seq-1, 1)
		if err != nil || len(events) != 1 || events[0].Seq != event.Seq {
			t.Errorf("event not committed: %+v: %v", events, err)
		}
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	})
	if _, err := db.AppendEvent(t.Context(), Event{RunID: run.ID, Type: "test", PayloadJSON: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRunStatus(t.Context(), run.ID, StatusQueued, StatusRunning, RunPatch{Phase: PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRunStatus(t.Context(), run.ID, StatusQueued, StatusFailed, RunPatch{Phase: PhaseRefine}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale transition: %v", err)
	}
	mu.Lock()
	count := len(received)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("received %d events, want 2", count)
	}
	stop()
	stop()
	if _, err := db.AppendEvent(t.Context(), Event{RunID: run.ID, Type: "test"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != count {
		t.Fatal("unregistered sink received event")
	}
}
