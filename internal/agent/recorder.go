package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
	"github.com/aryanpappu/patchbot/internal/tools"
)

// Recorder receives bounded, redacted records only. A failed write stops the
// harness rather than letting paid or mutating work proceed without an audit.
type Recorder interface {
	Record(context.Context, store.Step) error
	Event(context.Context, string, json.RawMessage) error
}

// EventEmitter publishes an event after StoreRecorder has persisted its Seq.
// A live event bus should not block; SQLite remains the replay source.
type EventEmitter interface {
	Emit(context.Context, store.Event)
}

// RecordStore is the persistence subset used by StoreRecorder.
type RecordStore interface {
	RecordStep(context.Context, store.Step) (store.Step, error)
	AppendEvent(context.Context, store.Event) (int64, error)
}

// StoreRecorder adapts the store and an optional live emitter to Recorder.
type StoreRecorder struct {
	Store    RecordStore
	RunID    int64
	Phase    store.Phase
	Emitter  EventEmitter
	Redactor *secret.Redactor
}

// Record persists a step. The store also applies its own defense-in-depth caps.
func (r *StoreRecorder) Record(ctx context.Context, step store.Step) error {
	if r.Store == nil {
		return errors.New("recorder store is missing")
	}
	step.RunID = r.RunID
	if step.Phase == "" {
		step.Phase = r.Phase
	}
	step.InputJSON = redactJSON([]byte(step.InputJSON), r.Redactor)
	step.OutputText = redactJSON([]byte(step.OutputText), r.Redactor)
	_, err := r.Store.RecordStep(ctx, step)
	return err
}

// Event persists a replayable event before notifying the live emitter.
func (r *StoreRecorder) Event(ctx context.Context, kind string, data json.RawMessage) error {
	if r.Store == nil {
		return errors.New("recorder store is missing")
	}
	event := store.Event{RunID: r.RunID, Type: kind, PayloadJSON: json.RawMessage(redactJSON(data, r.Redactor)), CreatedAt: time.Now().UTC()}
	seq, err := r.Store.AppendEvent(ctx, event)
	if err != nil {
		return err
	}
	event.Seq = seq
	if r.Emitter != nil {
		r.Emitter.Emit(ctx, event)
	}
	return nil
}

func (l *Loop) record(ctx context.Context, step store.Step) error {
	if l.Recorder == nil {
		return nil
	}
	step.InputJSON = tools.CapOutput(redactJSON([]byte(step.InputJSON), l.redactor()), 16<<10)
	capBytes := 64 << 10
	if step.Kind == "test" || step.Tool == "run_cmd" {
		capBytes = 256 << 10
	}
	step.OutputText = tools.CapOutput(l.redactor().Redact(step.OutputText), capBytes)
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Recorder.Record(recordCtx, step); err != nil {
		return &Error{Kind: "internal", Message: "recording step failed"}
	}
	return nil
}

func (l *Loop) event(ctx context.Context, kind string, data any) error {
	if l.Recorder == nil {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return &Error{Kind: "internal", Message: "encoding event failed"}
	}
	safe := redactJSON(raw, l.redactor())
	// Keep a valid JSON payload when capping large response events.
	if len(safe) > 16<<10 {
		raw, _ = json.Marshal(tools.CapOutput(safe, 16<<10))
		safe = string(raw)
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Recorder.Event(recordCtx, kind, json.RawMessage(safe)); err != nil {
		return &Error{Kind: "internal", Message: "recording event failed"}
	}
	return nil
}

func redactJSON(raw []byte, redactor *secret.Redactor) string {
	value, err := jsonValue(raw)
	if err != nil {
		return redactor.Redact(string(raw))
	}
	value = redactStrings(value, redactor)
	safe, _ := json.Marshal(value)
	return string(safe)
}

func redactStrings(value any, redactor *secret.Redactor) any {
	switch x := value.(type) {
	case string:
		return redactor.Redact(x)
	case []any:
		for i := range x {
			x[i] = redactStrings(x[i], redactor)
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, v := range x {
			out[redactor.Redact(key)] = redactStrings(v, redactor)
		}
		return out
	}
	return value
}

// recordedMessages omits opaque provider data from records. It remains intact
// in the live conversation for provider signatures and thinking round trips.
func recordedMessages(messages []llm.Message) []llm.Message {
	out := llm.CloneMessages(messages)
	for i := range out {
		parts := out[i].Parts[:0]
		for _, part := range out[i].Parts {
			if part.Type == llm.PartOpaque {
				continue
			}
			part.ProviderMeta = nil
			if part.ToolCall != nil && !json.Valid(part.ToolCall.Args) {
				part.ToolCall.Args, _ = json.Marshal(string(part.ToolCall.Args))
			}
			parts = append(parts, part)
		}
		out[i].Parts = parts
	}
	return out
}

// Keep JSON numbers exact when redacting or hashing provider-controlled data.
func jsonValue(raw []byte) (any, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}
