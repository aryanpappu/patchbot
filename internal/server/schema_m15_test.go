package server

import (
	"encoding/json"
	"testing"

	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestResponseRedactionPreservesSchema(t *testing.T) {
	s := &Server{redactor: secret.NewRedactor("input_tokens", "baseline", "succeeded")}
	raw, err := s.safeJSON(map[string]any{"input_tokens": "input_tokens", "phase": store.PhaseBaseline, "status": store.StatusSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["input_tokens"] != secret.Mask || got["phase"] != "baseline" || got["status"] != "succeeded" {
		t.Fatalf("schema corrupted: %s", raw)
	}
}
