package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

var update = flag.Bool("update", false, "rewrite Gemini request golden files")

const testKey = "fixture-gemini-key-do-not-expose"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".resp.json")) //nolint:gosec // Names are fixed test cases in this package.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func baseRequest() llm.ChatRequest {
	return llm.ChatRequest{Model: "fixture-model", System: "Be precise.", Messages: []llm.Message{llm.UserText("Hello")}, MaxTokens: 100, Tools: []llm.ToolSpec{{Name: "lookup", Description: "Look up a value.", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`)}}}
}

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != testKey {
			t.Error("missing API key header")
		}
		if strings.Contains(r.URL.String(), testKey) || r.URL.Query().Has("key") {
			t.Error("key in request URL")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing JSON accept header")
		}
		handler(w, r)
	}))
	client, err := NewWithClient(llm.ProviderConfig{Name: "test-gemini", APIKey: secret.New(testKey), BaseURL: server.URL + "/v1beta/"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".req.golden.json")
	if *update {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, got, "", "  "); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(pretty.Bytes(), '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // Path is a fixed testdata golden selected by a test case.
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(normalJSON(t, want), normalJSON(t, got)); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}

func normalJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func expected(parts []llm.Part, stop llm.StopReason, raw string, usage *llm.Usage) *llm.ChatResponse {
	return &llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Origin: "test-gemini", Parts: parts}, StopReason: stop, RawStop: raw, Model: "fixture-model", RequestID: "fixture-response", Usage: usage}
}

func rawParts(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	var r generateResponse
	if err := json.Unmarshal(fixture(t, name), &r); err != nil {
		t.Fatal(err)
	}
	return r.Candidates[0].Content.Parts
}

func callPart(t *testing.T, name string, index int, id, query string) llm.Part {
	t.Helper()
	part := llm.Call(id, "lookup", json.RawMessage(`{"q":"`+query+`"}`))
	part.ProviderMeta = rawParts(t, name)[index]
	var wire struct {
		Call functionCall `json:"functionCall"`
	}
	if err := json.Unmarshal(part.ProviderMeta, &wire); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(normalJSON(t, part.ToolCall.Args), normalJSON(t, wire.Call.Args)); diff != "" {
		t.Fatal(diff)
	}
	part.ToolCall.Args = wire.Call.Args
	return part
}

func TestChatGoldens(t *testing.T) {
	usage := &llm.Usage{InputTokens: 16, OutputTokens: 8, CacheReadTokens: 4}
	partial := rawParts(t, "max_tokens")[1]
	for _, tt := range []struct {
		name   string
		status int
		kind   llm.ErrorKind
		want   *llm.ChatResponse
	}{
		{name: "text", want: expected([]llm.Part{llm.Text("Hello.")}, llm.StopEndTurn, "STOP", usage)},
		{name: "one_call", want: expected([]llm.Part{callPart(t, "one_call", 0, "call_0_0", "one")}, llm.StopToolUse, "STOP", usage)},
		{name: "two_calls", want: expected([]llm.Part{callPart(t, "two_calls", 0, "call_0_0", "one"), callPart(t, "two_calls", 1, "call_0_1", "two")}, llm.StopToolUse, "STOP", usage)},
		{name: "mixed", want: expected([]llm.Part{llm.Text("Checking."), callPart(t, "mixed", 1, "wire-1", "one"), llm.Text("Please wait.")}, llm.StopToolUse, "STOP", usage)},
		{name: "malformed_args", kind: llm.ErrMalformed},
		{name: "max_tokens", want: expected([]llm.Part{llm.Text("Partial."), {Type: llm.PartOpaque, ProviderMeta: partial}}, llm.StopMaxTokens, "MAX_TOKENS", usage)},
		{name: "refusal", want: expected(nil, llm.StopRefusal, "SAFETY", usage)},
		{name: "blocked", want: &llm.ChatResponse{Message: llm.Message{Role: llm.RoleAssistant, Origin: "test-gemini"}, StopReason: llm.StopRefusal, RawStop: "BLOCKLIST", Model: "fixture-model", Usage: &llm.Usage{InputTokens: 20}}},
		{name: "missing_usage", want: expected([]llm.Part{llm.Text("Hello.")}, llm.StopEndTurn, "STOP", nil)},
		{name: "rate_limit", status: 429, kind: llm.ErrRateLimit},
		{name: "unavailable", status: 500, kind: llm.ErrUnavailable},
		{name: "overloaded", status: 503, kind: llm.ErrOverloaded},
		{name: "auth", status: 401, kind: llm.ErrAuth},
		{name: "forbidden", status: 403, kind: llm.ErrAuth},
		{name: "context", status: 400, kind: llm.ErrContextTooLong},
		{name: "bad_request", status: 400, kind: llm.ErrBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1beta/models/fixture-model:generateContent" || r.URL.RawQuery != "" {
					t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing content type")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				golden(t, tt.name, body)
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				if _, err := w.Write(fixture(t, tt.name)); err != nil {
					t.Error(err)
				}
			})
			got, err := p.Chat(context.Background(), baseRequest())
			if tt.kind != "" {
				assertKind(t, err, tt.kind)
				e, _ := errors.AsType[*llm.Error](err)
				if e.Status != tt.status {
					t.Errorf("HTTP status = %d, want %d", e.Status, tt.status)
				}
				if tt.name == "rate_limit" && e.RetryAfter != 2500*time.Millisecond {
					t.Errorf("retry delay = %s", e.RetryAfter)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
		})
	}
}

func assertKind(t *testing.T, err error, want llm.ErrorKind) {
	t.Helper()
	if kind, ok := llm.KindOf(err); !ok || kind != want {
		t.Fatalf("error = %v, want kind %s", err, want)
	}
}

func TestToolResultsGoldens(t *testing.T) {
	for _, name := range []string{"two_results", "empty_result", "error_result", "signature"} {
		t.Run(name, func(t *testing.T) {
			responseName := "two_calls"
			if name != "two_results" {
				responseName = "mixed"
			}
			if name == "signature" {
				responseName = "signature"
			}
			calls := 0
			var retained []json.RawMessage
			if name == "signature" {
				retained = rawParts(t, "signature")
			}
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				response := fixture(t, responseName)
				if calls == 2 {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					golden(t, name, body)
					for _, raw := range retained {
						if !bytes.Contains(body, raw) {
							t.Error("raw metadata part bytes or position were lost")
						}
					}
					var wire struct {
						Contents []content `json:"contents"`
					}
					if err := json.Unmarshal(body, &wire); err != nil {
						t.Error(err)
					}
					if name == "signature" {
						if diff := cmp.Diff(retained, wire.Contents[1].Parts); diff != "" {
							t.Errorf("signature part order changed: %s", diff)
						}
					}
					response = fixture(t, "text")
				}
				if _, err := w.Write(response); err != nil {
					t.Error(err)
				}
			})
			req := baseRequest()
			first, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if name == "signature" {
				want := expected([]llm.Part{{Type: llm.PartOpaque, ProviderMeta: retained[0]}, callPart(t, "signature", 1, "call_0_0", "one"), {Type: llm.PartOpaque, ProviderMeta: retained[2]}}, llm.StopToolUse, "STOP", nil)
				if diff := cmp.Diff(want, first); diff != "" {
					t.Errorf("signature response (-want +got):\n%s", diff)
				}
			}
			results := llm.Message{Role: llm.RoleTool}
			for i, c := range first.Message.ToolCalls() {
				text := fmt.Sprintf("result %d", i)
				if name == "empty_result" {
					text = ""
				}
				results.Parts = append(results.Parts, llm.Result(c.ID, c.Name, text, name == "error_result"))
			}
			req.Messages = append(req.Messages, first.Message, results)
			if _, err := p.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Errorf("HTTP calls = %d", calls)
			}
		})
	}
}
