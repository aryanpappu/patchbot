package openai

import (
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

var update = flag.Bool("update", false, "rewrite OpenAI request golden fixtures")

const testKey = "synthetic-private-key"

type family struct {
	name, base, prefix string
	quirks             Quirks
	key                bool
	usage              *llm.Usage
}

func families() []family {
	usage := &llm.Usage{InputTokens: 16, OutputTokens: 5, CacheReadTokens: 4}
	return []family{
		{"openai", "https://api.openai.com/v1", "/v1", Quirks{MaxTokensField: "max_completion_tokens"}, true, usage},
		{"openrouter", "https://openrouter.ai/api/v1", "/api/v1", Quirks{MaxTokensField: "max_tokens"}, true, usage},
		{"ollama", "http://localhost:11434/v1", "/v1", Quirks{MaxTokensField: "max_tokens", OmitParallelToolCalls: true}, false, nil},
		{"groq", "https://api.groq.com/openai/v1", "/openai/v1", Quirks{MaxTokensField: "max_tokens", OmitParallelToolCalls: true}, true, usage},
	}
}

func baseRequest(tools bool) llm.ChatRequest {
	temperature := 0.0
	req := llm.ChatRequest{Model: "fixture-model", System: "Be precise.", Messages: []llm.Message{llm.UserText("Inspect a.txt and b.txt.")}, MaxTokens: 128, Temperature: &temperature}
	if tools {
		req.Tools = []llm.ToolSpec{{Name: "read_file", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}
	}
	return req
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // Fixture names are hardcoded by these tests.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func golden(t *testing.T, name string, data []byte) {
	t.Helper()
	var got any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Errorf("invalid request JSON: %v", err)
		return
	}
	path := filepath.Join("testdata", name+".req.golden.json")
	if *update {
		formatted, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, append(formatted, '\n'), 0o600); err != nil {
			t.Error(err)
		}
	}
	wantData, err := os.ReadFile(path) //nolint:gosec // Golden paths are hardcoded by these tests.
	if err != nil {
		t.Error(err)
		return
	}
	var want any
	if err := json.Unmarshal(wantData, &want); err != nil {
		t.Error(err)
		return
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}

func writeBody(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	client := server.Client()
	p, err := NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: server.URL + "/v1/", APIKey: secret.New(testKey)}, Options{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFamilyGoldenConversations(t *testing.T) {
	t.Parallel()
	for _, f := range families() {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			for _, scenario := range []string{"text", "one_tool", "two_tools", "mixed"} {
				t.Run(scenario, func(t *testing.T) {
					t.Parallel()
					var requests atomic.Int32
					response := readFixture(t, f.name+"_"+scenario+".resp.json")
					results := readFixture(t, f.name+"_results.resp.json")
					server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						step := requests.Add(1)
						name := f.name + "_" + scenario
						if step == 2 {
							name = f.name + "_results"
						}
						if r.Method != http.MethodPost || r.URL.Path != f.prefix+"/chat/completions" || r.URL.RawQuery != "" {
							t.Errorf("wrong request endpoint: %s %s", r.Method, r.URL.Path)
						}
						wantAuth := ""
						if f.key {
							wantAuth = "Bearer " + testKey
						}
						if r.Header.Get("Authorization") != wantAuth || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
							t.Error("incorrect request headers")
						}
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						golden(t, name, data)
						w.Header().Set("X-Request-ID", "req_synthetic")
						if step == 1 {
							writeBody(t, w, response)
						} else {
							writeBody(t, w, results)
						}
					}))
					client := server.Client()
					cfg := llm.ProviderConfig{Name: f.name, BaseURL: server.URL + f.prefix + "/"}
					if f.key {
						cfg.APIKey = secret.New(testKey)
					}
					p, err := NewWithOptions(cfg, Options{HTTPClient: client, Quirks: &f.quirks})
					if err != nil {
						t.Fatal(err)
					}
					req := baseRequest(scenario != "text")
					before := req.Clone()
					got, err := p.Chat(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					want := &llm.ChatResponse{Model: "fixture-model", RequestID: "req_synthetic", RawStop: "tool_calls", StopReason: llm.StopToolUse, Usage: f.usage,
						Message: llm.Message{Role: llm.RoleAssistant, Origin: f.name}}
					switch scenario {
					case "text":
						want.Message.Parts = []llm.Part{llm.Text("Done.")}
						want.RawStop, want.StopReason = "stop", llm.StopEndTurn
					case "one_tool":
						want.Message.Parts = []llm.Part{llm.Call("first", "read_file", json.RawMessage(`{"path":"a.txt"}`))}
					case "two_tools":
						want.Message.Parts = []llm.Part{llm.Call("first", "read_file", json.RawMessage(`{"path":"a.txt"}`)), llm.Call("second", "read_file", json.RawMessage(`{"path":"b.txt"}`))}
					case "mixed":
						want.RawStop = "stop"
						want.Message.Parts = []llm.Part{llm.Text("I will read it."), llm.Call("first", "read_file", json.RawMessage(`{"path":"a.txt"}`))}
					}
					if diff := cmp.Diff(want, got); diff != "" {
						t.Errorf("normalized response (-want +got):\n%s", diff)
					}
					if diff := cmp.Diff(before, req); diff != "" {
						t.Errorf("request mutated: %s", diff)
					}
					if err := llm.Validate(append(req.Messages, got.Message)); err != nil {
						t.Errorf("invalid normalized conversation: %v", err)
					}
					if scenario != "two_tools" {
						return
					}
					// Both results remain in call order, including an empty result
					// and a failed result, and are sent as separate wire messages.
					req.Messages = append(req.Messages, got.Message, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("first", "read_file", "", false), llm.Result("second", "read_file", "missing", true)}})
					got, err = p.Chat(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					want.RawStop, want.StopReason = "stop", llm.StopEndTurn
					want.Message.Parts = []llm.Part{llm.Text("Finished reading.")}
					if diff := cmp.Diff(want, got); diff != "" {
						t.Errorf("continuation (-want +got):\n%s", diff)
					}
					if requests.Load() != 2 {
						t.Error("unexpected number of HTTP requests")
					}
				})
			}
		})
	}
}

func TestResponseFixtures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		stop llm.StopReason
		text string
		kind llm.ErrorKind
	}{
		{"malformed_arguments", "", "", llm.ErrMalformed},
		{"partial_call", llm.StopMaxTokens, "Working.", ""},
		{"refusal", llm.StopRefusal, "I cannot help with that.", ""},
		{"blocked", llm.StopRefusal, "", ""},
		{"missing_usage", llm.StopEndTurn, "Done.", ""},
		{"legacy_call", llm.StopToolUse, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			response := readFixture(t, tt.name+".resp.json")
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) { writeBody(t, w, response) })
			got, err := p.Chat(context.Background(), baseRequest(true))
			if tt.kind != "" {
				_ = assertKind(t, err, tt.kind)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.StopReason != tt.stop || got.Message.Text() != tt.text {
				t.Errorf("response = %+v", got)
			}
			if tt.name == "legacy_call" {
				want := []llm.ToolCall{{ID: "call_1_0_0", Name: "read_file", Args: json.RawMessage(`{"path":"a.txt"}`)}}
				if diff := cmp.Diff(want, got.Message.ToolCalls()); diff != "" {
					t.Error(diff)
				}
			} else if len(got.Message.ToolCalls()) != 0 {
				t.Error("blocked/truncated response contains executable calls")
			}
			if (tt.name == "missing_usage" || tt.name == "legacy_call") && got.Usage != nil {
				t.Error("absent usage is known")
			}
		})
	}
}

func assertKind(t *testing.T, err error, kind llm.ErrorKind) *llm.Error {
	t.Helper()
	var got *llm.Error
	if !errors.As(err, &got) || got.Kind != kind || got.Provider != "test" {
		t.Fatalf("error = %v; want %s from test", err, kind)
	}
	assertSafe(t, err)
	return got
}

func assertSafe(t *testing.T, err error) {
	t.Helper()
	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	texts := []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), string(data), llm.SafeText(err)}
	for _, text := range texts {
		for _, private := range []string{testKey, "query-secret", "private-request-body", "private-response-body"} {
			if strings.Contains(text, private) {
				t.Error("observable error leaked private data")
			}
		}
	}
}

func TestHTTPFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		status  int
		fixture string
		kind    llm.ErrorKind
	}{
		{401, "auth_error", llm.ErrAuth}, {403, "auth_error", llm.ErrAuth},
		{429, "rate_limit", llm.ErrRateLimit}, {500, "server_error", llm.ErrUnavailable},
		{502, "server_error", llm.ErrUnavailable}, {503, "server_error", llm.ErrOverloaded},
		{400, "context_too_long", llm.ErrContextTooLong}, {400, "auth_error", llm.ErrBadRequest},
		{404, "auth_error", llm.ErrBadRequest},
	} {
		t.Run(fmt.Sprint(tt.status)+tt.fixture, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			data := readFixture(t, tt.fixture+".resp.json")
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "2.5")
				w.Header().Set("X-Request-ID", "req_synthetic")
				w.WriteHeader(tt.status)
				writeBody(t, w, data)
			})
			_, err := p.Chat(context.Background(), baseRequest(false))
			got := assertKind(t, err, tt.kind)
			if got.Status != tt.status || got.RetryAfter != 2500*time.Millisecond || got.RequestID != "req_synthetic" || requests.Load() != 1 {
				t.Errorf("HTTP metadata or retry behavior: %+v, requests %d", got, requests.Load())
			}
			_, err = p.ListModels(context.Background())
			_ = assertKind(t, err, tt.kind)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{ closed bool }

func (*brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *brokenBody) Close() error           { b.closed = true; return nil }

func TestTransportAndReadFailures(t *testing.T) {
	t.Parallel()
	cause := errors.New("query-secret " + testKey + " private-request-body")
	for _, status := range []int{0, 200, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			body := &brokenBody{}
			p, err := NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: "https://example.invalid/v1", APIKey: secret.New(testKey)}, Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if status == 0 {
					return nil, cause
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Chat(context.Background(), baseRequest(false))
			_ = assertKind(t, err, llm.ErrUnavailable)
			if status == 0 {
				if !errors.Is(err, cause) {
					t.Error("transport cause not preserved")
				}
			} else if !errors.Is(err, io.ErrUnexpectedEOF) || !body.closed {
				t.Error("read cause or body cleanup lost")
			}
		})
	}
}

func TestCancellationAndConcurrentCalls(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"chat", "models"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			defer close(release)
			p := newTestProvider(t, func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			var err error
			if op == "chat" {
				_, err = p.Chat(ctx, baseRequest(false))
			} else {
				_, err = p.ListModels(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	response := readFixture(t, "openai_text.resp.json")
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) { writeBody(t, w, response) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Chat(ctx, baseRequest(false)); !errors.Is(err, context.Canceled) {
		t.Errorf("pre-canceled Chat = %v", err)
	}
	if _, err := p.ListModels(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("pre-canceled ListModels = %v", err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := p.Chat(context.Background(), baseRequest(false)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
