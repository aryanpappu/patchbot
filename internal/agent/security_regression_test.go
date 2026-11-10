package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/anthropic"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

func TestTruncatedOpaqueActionIsNotReplayed(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("../llm/anthropic/testdata/max_tokens.resp.json")
	if err != nil {
		t.Fatal(err)
	}
	const thinking = `{"type":"thinking","thinking":"Keep this.","signature":"signed-thinking"}`
	const metadata = `{"type":"redacted_thinking","data":"signed-redacted-thinking"}`
	requests := make(chan []byte, 2)
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			// Exercise the real adapter's opaque representation of the partial call,
			// alongside metadata which must survive the continuation byte-for-byte.
			var wire map[string]json.RawMessage
			if decodeErr := json.Unmarshal(fixture, &wire); decodeErr != nil {
				t.Error(decodeErr)
			}
			var blocks []json.RawMessage
			if decodeErr := json.Unmarshal(wire["content"], &blocks); decodeErr != nil {
				t.Error(decodeErr)
			}
			blocks = append([]json.RawMessage{json.RawMessage(thinking), json.RawMessage(metadata)}, blocks...)
			wire["content"], _ = json.Marshal(blocks)
			_ = json.NewEncoder(w).Encode(wire)
			return
		}
		_, _ = io.WriteString(w, `{"id":"done","type":"message","role":"assistant","model":"claude-test","content":[{"type":"tool_use","id":"report","name":"submit_report","input":{"report_md":"Recovered."}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer server.Close()
	p, err := anthropic.NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: server.URL, APIKey: secret.New("fixture-key")}, anthropic.Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	l, _, _, ex := setup(t)
	l.Provider, l.Model = p, "claude-test"
	s := &Session{}
	result, err := l.Investigate(t.Context(), InvestigateRequest{Title: "title", Session: s, Limits: Limits{MaxTurns: 2}})
	if err != nil || result.ReportMD != "Recovered." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	<-requests
	continuation := string(<-requests)
	if strings.Contains(continuation, "tool_partial") || strings.Contains(continuation, `"type":"tool_use"`) {
		t.Fatal("truncated opaque action was replayed")
	}
	for _, block := range []string{thinking, metadata} {
		if !strings.Contains(continuation, block) {
			t.Fatalf("provider metadata changed: %s", continuation)
		}
	}
	if len(ex.specs) != 0 || llm.Validate(s.Messages) != nil {
		t.Fatal("truncation dispatched a command or broke the conversation")
	}
}

func TestSynthesizedSubmissionsAreRedacted(t *testing.T) {
	t.Parallel()
	const credential = "registered-private-value"
	const criterion = "README says after"
	for _, code := range []bool{false, true} {
		t.Run(map[bool]string{false: "investigate", true: "code"}[code], func(t *testing.T) {
			l, p, _, _ := setup(t, fake.Step{Text: "done"}, fake.Step{Text: "Result: " + credential})
			l.Env.Redactor.Add(credential)
			var out Outcome
			if code {
				l.Env.Redactor.Add(criterion)
				token, _, run := approved(t)
				result, err := l.Code(t.Context(), token, codeRequest(l, run.ID))
				if err != nil {
					t.Fatal(err)
				}
				out = result.Outcome
			} else {
				result, err := l.Investigate(t.Context(), InvestigateRequest{Title: "title"})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(result.ReportMD, credential) {
					t.Error("synthesized report leaked a secret")
				}
				out = result.Outcome
			}
			if !out.Synthesized || out.Submission == nil {
				t.Fatalf("no synthesized submission: %+v", out)
			}
			raw, _ := json.Marshal(out.Submission)
			if strings.Contains(string(raw), credential) || strings.Contains(string(raw), criterion) || !strings.Contains(string(raw), "redacted") {
				t.Fatalf("unsafe synthesized submission: %s", raw)
			}
			validRequests(t, p)
		})
	}
}

func TestToolIdentifierSecretsNeverReachRecordsOrRequests(t *testing.T) {
	t.Parallel()
	const token = "ghp_0123456789abcdefghij"
	const registered = "registered-private-value"
	l, p, r, _ := setup(t,
		turn(llm.ToolCall{ID: "unknown", Name: token, Args: json.RawMessage(`{}`)}),
		turn(llm.ToolCall{ID: token, Name: "read_file", Args: json.RawMessage(`{"path":"README.md"}`)},
			llm.ToolCall{ID: registered, Name: "list_dir", Args: json.RawMessage(`{}`)}),
		reportStep(),
	)
	l.Env.Redactor.Add(registered)
	s := session()
	if _, err := l.Run(t.Context(), s, Limits{}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{r.steps, r.events, p.Requests(), s.Messages} {
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), token) || strings.Contains(string(raw), registered) {
			t.Errorf("tool identifier secret leaked: %s", raw)
		}
	}
	validRequests(t, p)
	if err := llm.Validate(s.Messages); err != nil {
		t.Fatalf("redaction broke call/result pairing: %v", err)
	}
	var callIDs, resultIDs, recordedIDs []string
	for _, event := range r.events {
		kind, data, _ := strings.Cut(event, " ")
		var payload struct {
			CallID string         `json:"call_id"`
			Result llm.ToolResult `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "tool.call":
			callIDs = append(callIDs, payload.CallID)
		case "tool.result":
			resultIDs = append(resultIDs, payload.Result.CallID)
		}
	}
	for _, step := range r.steps {
		if step.Kind != "llm" || step.Status != "ok" {
			continue
		}
		var messages []llm.Message
		if err := json.Unmarshal([]byte(step.OutputText), &messages); err != nil {
			t.Fatal(err)
		}
		for _, msg := range messages {
			for _, call := range msg.ToolCalls() {
				recordedIDs = append(recordedIDs, call.ID)
			}
		}
	}
	if len(callIDs) != 3 || callIDs[0] == callIDs[1] || !slices.Equal(callIDs, resultIDs) || !slices.Equal(callIDs, recordedIDs) {
		t.Fatalf("audit pairing changed: calls=%v results=%v recorded=%v", callIDs, resultIDs, recordedIDs)
	}
}

func TestOversizedToolIDIsRejectedBeforeDispatch(t *testing.T) {
	t.Parallel()
	l, p, r, _ := setup(t, turn(llm.ToolCall{ID: strings.Repeat("x", 257), Name: "read_file", Args: json.RawMessage(`{"path":"README.md"}`)}), reportStep())
	if _, err := l.Run(t.Context(), session(), Limits{}); err != nil {
		t.Fatal(err)
	}
	if r.steps[0].Status != "error" || len(p.Requests()[1].Messages) != 1 {
		t.Fatal("oversized tool ID was accepted and replayed")
	}
}
