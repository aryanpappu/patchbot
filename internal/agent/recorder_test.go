package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

type emitter struct {
	events []store.Event
	db     *store.SQLite
	t      *testing.T
}

func (e *emitter) Emit(ctx context.Context, event store.Event) {
	rows, err := e.db.EventsAfter(ctx, event.RunID, event.Seq-1, 1)
	if err != nil || len(rows) != 1 || rows[0].Seq != event.Seq {
		e.t.Error("event emitted before persistence")
	}
	e.events = append(e.events, event)
}

func TestStoreRecorderPersistsBeforeEmitAndRedacts(t *testing.T) {
	t.Parallel()
	db, run := database(t)
	em := &emitter{db: db, t: t}
	const value = "private\nmulti-line-secret"
	r := &StoreRecorder{Store: db, RunID: run.ID, Phase: store.PhaseRefine, Emitter: em, Redactor: secret.NewRedactor(value)}
	args, _ := json.Marshal(map[string]string{"value": value})
	if err := r.Record(t.Context(), store.Step{Kind: "tool", Tool: "read_file", Status: "ok", InputJSON: string(args), OutputText: value}); err != nil {
		t.Fatal(err)
	}
	if err := r.Event(t.Context(), "tool.result", args); err != nil {
		t.Fatal(err)
	}
	steps, err := db.ListSteps(t.Context(), run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("%v", err)
	}
	if steps[0].Phase != store.PhaseRefine || strings.Contains(steps[0].InputJSON, "multi-line-secret") || strings.Contains(steps[0].OutputText, "multi-line-secret") {
		t.Fatal("unsafe record")
	}
	if len(em.events) != 1 || strings.Contains(string(em.events[0].PayloadJSON), "multi-line-secret") {
		t.Fatal("unsafe event")
	}
}

func TestLoopStoreRecorderUnknownUsage(t *testing.T) {
	t.Parallel()
	db, run := database(t)
	l, _, _, _ := setup(t, fake.Step{ToolCalls: reportStep().ToolCalls})
	l.Recorder = &StoreRecorder{Store: db, RunID: run.ID, Phase: store.PhaseRefine}
	s := session()
	if _, err := l.Run(t.Context(), s, Limits{}); err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.InputTokens != nil || saved.CostMicros != nil || saved.Turns != 1 {
		t.Fatalf("unknown accounting lost: %+v", saved)
	}
	steps, err := db.ListSteps(t.Context(), run.ID)
	if err != nil || len(steps) != 2 {
		t.Fatalf("steps=%d err=%v", len(steps), err)
	}
	events, err := db.EventsAfter(t.Context(), run.ID, 0, 100)
	if err != nil || len(events) < 4 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}
