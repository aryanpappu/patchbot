package anthropic_test

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
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/anthropic"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/testhttp"
)

var update = flag.Bool("update", false, "rewrite request golden files")

const thinking = `{"type":"thinking","thinking":"Check the file first.","signature":"synthetic-signature","future_field":{"keep":true}}`
const redacted = `{"type":"redacted_thinking","data":"synthetic-redacted-data"}`
const unknown = `{"type":"future_required_block","payload":{"signature":"keep-me","values":[1,2]}}`
const partial = `{"type":"tool_use","id":"tool_partial","name":"read_file","input":"{\"path\":"}`

func op(raw string) llm.Part {
	return llm.Part{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(raw)}
}
func call1() llm.Part {
	return llm.Call("tool_1", "read_file", json.RawMessage(`{"path":"README.md"}`))
}
func call2() llm.Part { return llm.Call("tool_2", "search", json.RawMessage(`{"query":"TODO"}`)) }
func assistant(parts ...llm.Part) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Origin: "test", Parts: parts}
}
func request() llm.ChatRequest {
	return llm.ChatRequest{Model: "claude-test", System: "Be helpful.", MaxTokens: 1024, Messages: []llm.Message{llm.UserText("request-private-text")}}
}
func tools() []llm.ToolSpec {
	return []llm.ToolSpec{
		{Name: "read_file", Description: "Read a file", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		{Name: "search", Description: "Search text", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)},
	}
}
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".resp.json")) //nolint:gosec // names come from fixed test tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func canonical(t *testing.T, data []byte) []byte {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}
func golden(t *testing.T, name string, body []byte) {
	t.Helper()
	body = canonical(t, body)
	path := filepath.Join("testdata", name+".req.golden.json")
	if *update {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Error(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path names a request golden in testdata
	if err != nil {
		t.Error(err)
		return
	}
	if diff := cmp.Diff(string(want), string(body)); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}
func checkHTTP(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method || r.URL.Path != path {
		t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, method, path)
	}
	if r.Header.Get("x-api-key") != "dummy-key" {
		t.Error("wrong API key")
	}
	if r.Header.Get("anthropic-version") != "2023-06-01" {
		t.Error("wrong API version")
	}
}
func provider(t *testing.T, server *httptest.Server) *anthropic.Provider {
	t.Helper()
	p, err := anthropic.NewWithOptions(llm.ProviderConfig{Name: "test", Kind: anthropic.Kind, BaseURL: server.URL, APIKey: secret.New("dummy-key")}, anthropic.Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func equalResponse(t *testing.T, got, want *llm.ChatResponse) {
	t.Helper()
	// Compare opaque JSON structurally: the stored bytes are separately checked
	// against the exact raw block extracted from the fixture below.
	normalize := func(r *llm.ChatResponse) *llm.ChatResponse {
		r = r.Clone()
		for i := range r.Message.Parts {
			if c := r.Message.Parts[i].ToolCall; c != nil {
				c.Args = bytes.TrimSpace(canonical(t, c.Args))
			}
			if len(r.Message.Parts[i].ProviderMeta) > 0 {
				r.Message.Parts[i].ProviderMeta = bytes.TrimSpace(canonical(t, r.Message.Parts[i].ProviderMeta))
			}
		}
		return r
	}
	if diff := cmp.Diff(normalize(want), normalize(got)); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
}

func TestChatGoldens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		parts   []llm.Part
		stop    llm.StopReason
		rawStop string
		noUsage bool
	}{
		{name: "text", parts: []llm.Part{llm.Text("Hello.")}},
		{name: "one_tool", parts: []llm.Part{call1()}, stop: llm.StopToolUse},
		{name: "two_tools", parts: []llm.Part{call1(), call2()}, stop: llm.StopToolUse},
		{name: "two_results", parts: []llm.Part{llm.Text("Hello.")}},
		{name: "mixed", parts: []llm.Part{llm.Text("First."), call1(), llm.Text("Then."), call2()}, stop: llm.StopToolUse},
		{name: "thinking", parts: []llm.Part{op(thinking), op(redacted), call1()}, stop: llm.StopToolUse},
		{name: "unknown", parts: []llm.Part{op(unknown), llm.Text("Hello.")}},
		{name: "max_tokens", parts: []llm.Part{llm.Text("Working."), op(partial)}, stop: llm.StopMaxTokens},
		{name: "refusal", parts: []llm.Part{llm.Text("I cannot help.")}, stop: llm.StopRefusal},
		{name: "missing_usage", parts: []llm.Part{llm.Text("Hello.")}, noUsage: true},
		{name: "null_usage", parts: []llm.Part{llm.Text("Hello.")}, noUsage: true},
		{name: "stop_sequence", parts: []llm.Part{llm.Text("Hello.")}, rawStop: "stop_sequence"},
		{name: "context_window", parts: []llm.Part{llm.Text("Hello.")}, stop: llm.StopMaxTokens, rawStop: "model_context_window_exceeded"},
		{name: "unknown_stop", parts: []llm.Part{llm.Text("Hello.")}, stop: llm.StopOther, rawStop: "future_stop"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, tc.name)
			var calls atomic.Int32
			server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				checkHTTP(t, r, http.MethodPost, "/v1/messages")
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				golden(t, tc.name, data)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("request-id", "req_synthetic")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			req := request()
			if tc.name == "one_tool" || tc.name == "two_tools" || tc.name == "two_results" || tc.name == "mixed" || tc.name == "thinking" || tc.name == "max_tokens" {
				req.Tools = tools()
			}
			if tc.name == "text" {
				v := 0.2
				req.Temperature = &v
			}
			if tc.name == "two_results" {
				req.Messages = append(req.Messages, assistant(call1(), call2()),
					llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("tool_1", "read_file", "file contents", false)}},
					llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("tool_2", "search", "search failed", true)}}, llm.UserText("Please continue."))
			}
			got, err := provider(t, server).Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			stop := tc.stop
			if stop == "" {
				stop = llm.StopEndTurn
			}
			raw := tc.rawStop
			if raw == "" {
				raw = string(stop)
			}
			want := &llm.ChatResponse{Message: assistant(tc.parts...), StopReason: stop, RawStop: raw, Model: "claude-test", RequestID: "req_synthetic"}
			if !tc.noUsage {
				want.Usage = &llm.Usage{InputTokens: 12, OutputTokens: 7, CacheWriteTokens: 20, CacheReadTokens: 30}
			}
			equalResponse(t, got, want)
			var wire struct {
				Content []json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			for i, p := range got.Message.Parts {
				if p.Type == llm.PartOpaque && !bytes.Equal(p.ProviderMeta, wire.Content[i]) {
					t.Error("opaque block was changed while decoding")
				}
			}
			if calls.Load() != 1 {
				t.Errorf("SDK made %d calls, want 1", calls.Load())
			}
		})
	}
}

func TestReplayGoldens(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"thinking", "unknown"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			initial, follow := fixture(t, name), fixture(t, name+"_roundtrip")
			var count atomic.Int32
			server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkHTTP(t, r, http.MethodPost, "/v1/messages")
				w.Header().Set("Content-Type", "application/json")
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				switch count.Add(1) {
				case 1:
					_, _ = w.Write(initial)
				case 2:
					golden(t, name+"_roundtrip", data)
					var wire struct {
						Messages []struct {
							Content []json.RawMessage `json:"content"`
						} `json:"messages"`
					}
					if err := json.Unmarshal(data, &wire); err != nil {
						t.Error(err)
					} else {
						var original struct {
							Content []json.RawMessage `json:"content"`
						}
						if err := json.Unmarshal(initial, &original); err != nil {
							t.Error(err)
						}
						for i, b := range original.Content {
							if i >= len(wire.Messages[1].Content) {
								t.Error("lost block")
								break
							}
							if !bytes.Equal(canonical(t, b), canonical(t, wire.Messages[1].Content[i])) {
								t.Errorf("replay block %d changed", i)
							}
						}
					}
					_, _ = w.Write(follow)
				default:
					t.Error("unexpected SDK retry")
				}
			}))
			defer server.Close()
			p := provider(t, server)
			req := request()
			req.Tools = tools()
			first, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, first.Message)
			if name == "thinking" {
				req.Messages = append(req.Messages, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("tool_1", "read_file", "file contents", false)}})
			} else {
				req.Messages = append(req.Messages, llm.UserText("Continue."))
			}
			second, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if second.Message.Text() != "Hello." || count.Load() != 2 {
				t.Errorf("unexpected followup: %+v, calls=%d", second, count.Load())
			}
		})
	}
}

func TestErrorGoldens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		kind   llm.ErrorKind
	}{
		{"rate_limit", 429, llm.ErrRateLimit}, {"overloaded", 529, llm.ErrOverloaded}, {"auth", 401, llm.ErrAuth},
		{"forbidden", 403, llm.ErrAuth}, {"service_unavailable", 503, llm.ErrOverloaded}, {"server_error", 500, llm.ErrUnavailable},
		{"context_too_long", 400, llm.ErrContextTooLong}, {"bad_request", 400, llm.ErrBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixtureName := tc.name
			switch tc.name {
			case "forbidden":
				fixtureName = "auth"
			case "service_unavailable", "server_error":
				fixtureName = "overloaded"
			}
			body := fixture(t, fixtureName)
			var count atomic.Int32
			server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				checkHTTP(t, r, http.MethodPost, "/v1/messages")
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				golden(t, tc.name, data)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("request-id", "req_error")
				w.Header().Set("retry-after", "3")
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			}))
			defer server.Close()
			_, err := provider(t, server).Chat(context.Background(), request())
			e, ok := errors.AsType[*llm.Error](err)
			if !ok {
				t.Fatalf("error = %T %v", err, err)
			}
			if e.Kind != tc.kind || e.Status != tc.status || e.RetryAfter != 3*time.Second || e.RequestID != "req_error" {
				t.Errorf("error = %+v", e)
			}
			if _, ok := errors.AsType[*sdk.Error](err); !ok {
				t.Error("SDK cause was lost")
			}
			encoded, jsonErr := json.Marshal(e)
			if jsonErr != nil {
				t.Fatal(jsonErr)
			}
			for _, s := range []string{err.Error(), fmt.Sprintf("%+v %#v", e, e), string(encoded), llm.SafeText(err)} {
				if strings.Contains(s, "dummy-key") || strings.Contains(s, "request-private-text") {
					t.Errorf("sensitive error: %s", s)
				}
			}
			if count.Load() != 1 {
				t.Errorf("SDK retried %d times", count.Load())
			}
		})
	}
}

func TestListModels(t *testing.T) {
	t.Parallel()
	pages := [][]byte{fixture(t, "models_page1"), fixture(t, "models_page2")}
	var count atomic.Int32
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkHTTP(t, r, http.MethodGet, "/v1/models")
		i := int(count.Add(1)) - 1
		if i >= len(pages) {
			t.Error("extra page")
			w.WriteHeader(500)
			return
		}
		wantQuery := ""
		if i == 1 {
			wantQuery = "after_id=claude-a"
		}
		if r.URL.RawQuery != wantQuery {
			t.Errorf("query=%q, want %q", r.URL.RawQuery, wantQuery)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if len(body) != 0 {
			t.Error("GET has a body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pages[i])
	}))
	defer server.Close()
	got, err := provider(t, server).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ModelInfo{{ID: "claude-a", DisplayName: "Claude A", Source: llm.SourceLive}, {ID: "claude-b", DisplayName: "Claude B", Source: llm.SourceLive}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
	if count.Load() != 2 {
		t.Errorf("calls=%d", count.Load())
	}
}

func TestMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing_id", "malformed_args", "duplicate_ids", "invalid_json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := []byte(`{"content":`)
			if name != "invalid_json" {
				body = fixture(t, name)
			}
			server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			_, err := provider(t, server).Chat(context.Background(), request())
			if !errors.Is(err, &llm.Error{Kind: llm.ErrMalformed}) {
				t.Errorf("error=%v, want malformed", err)
			}
		})
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNoUnexpectedCredentialsOrNetwork(t *testing.T) {
	// Even when the SDK environment holds credentials or a URL, only config wins.
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://ambient.invalid")
	t.Setenv("ANTHROPIC_PROFILE", "missing-profile")
	var count atomic.Int32
	transportErr := errors.New("transport leaked dummy-key request-private-text")
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		if r.URL.Host != "configured.invalid" || r.Header.Get("x-api-key") != "dummy-key" || r.Header.Get("Authorization") != "" {
			t.Error("SDK used ambient config")
		}
		return nil, transportErr
	})}
	cfg := llm.ProviderConfig{Name: "test", APIKey: secret.New("dummy-key"), BaseURL: "http://configured.invalid"}
	p, err := anthropic.NewWithOptions(cfg, anthropic.Options{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), request())
	if !errors.Is(err, transportErr) || !errors.Is(err, &llm.Error{Kind: llm.ErrUnavailable}) {
		t.Errorf("error=%v", err)
	}
	if strings.Contains(err.Error(), "dummy-key") || strings.Contains(err.Error(), "request-private-text") {
		t.Error("transport error leaked")
	}
	if count.Load() != 1 {
		t.Errorf("SDK transport attempts=%d", count.Load())
	}

	cfg.APIKey = secret.Secret{}
	p, err = anthropic.NewWithOptions(cfg, anthropic.Options{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), request())
	if !errors.Is(err, &llm.Error{Kind: llm.ErrAuth}) {
		t.Errorf("missing key: %v", err)
	}
	_, err = p.ListModels(context.Background())
	if !errors.Is(err, &llm.Error{Kind: llm.ErrAuth}) {
		t.Errorf("missing key models: %v", err)
	}
	if count.Load() != 1 {
		t.Error("missing key called HTTP")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Chat(ctx, request())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
	_, err = p.ListModels(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel models: %v", err)
	}
}

func TestInvalidRequestsAndForeignMeta(t *testing.T) {
	t.Parallel()
	var count atomic.Int32
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if bytes.Contains(data, []byte("foreign-block")) {
			t.Error("foreign opaque data was replayed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "text"))
	}))
	defer server.Close()
	p := provider(t, server)
	for _, name := range []string{"missing_model", "mismatched_result", "invalid_meta", "empty_after_foreign", "sdk_non_streaming_limit"} {
		t.Run(name, func(t *testing.T) {
			req := request()
			switch name {
			case "missing_model":
				req.Model = ""
			case "sdk_non_streaming_limit":
				req.MaxTokens = 128000
			case "mismatched_result":
				req.Messages = append(req.Messages, assistant(call1()), llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("wrong_id", "read_file", "contents", false)}})
			case "invalid_meta":
				req.Messages = append(req.Messages, assistant(op(`[]`)), llm.UserText("Continue."))
			case "empty_after_foreign":
				m := assistant(op(`{"type":"foreign-block"}`))
				m.Origin = "other"
				req.Messages = append(req.Messages, m, llm.UserText("Continue."))
			}
			_, err := p.Chat(context.Background(), req)
			if !errors.Is(err, &llm.Error{Kind: llm.ErrBadRequest}) {
				t.Errorf("error=%v", err)
			}
		})
	}
	if count.Load() != 0 {
		t.Error("invalid request reached HTTP")
	}
	req := request()
	m := assistant(op(`{"type":"foreign-block"}`), llm.Text("Retained."))
	m.Origin = "other"
	req.Messages = append(req.Messages, m, llm.UserText("Continue."))
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Error("valid request did not reach HTTP")
	}
}

type retryClock struct{ waits []time.Duration }

func (*retryClock) Now() time.Time { return time.Now() }
func (c *retryClock) Sleep(_ context.Context, d time.Duration) error {
	c.waits = append(c.waits, d)
	return nil
}

func TestRetryOwnedByLLM(t *testing.T) {
	t.Parallel()
	rateLimit, success := fixture(t, "rate_limit"), fixture(t, "text")
	var count atomic.Int32
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if count.Add(1) == 1 {
			w.Header().Set("retry-after", "3")
			w.WriteHeader(429)
			_, _ = w.Write(rateLimit)
			return
		}
		_, _ = w.Write(success)
	}))
	defer server.Close()
	clock := &retryClock{}
	p := llm.WithRetry(provider(t, server), llm.RetryPolicy{MaxAttempts: 2}, clock)
	got, err := p.Chat(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 || got.Message.Text() != "Hello." {
		t.Errorf("calls=%d, response=%+v", count.Load(), got)
	}
	if diff := cmp.Diff([]time.Duration{3 * time.Second}, clock.waits); diff != "" {
		t.Errorf("llm retry waits: %s", diff)
	}
}

func TestListModelsPageFailure(t *testing.T) {
	t.Parallel()
	first, fail := fixture(t, "models_page1"), fixture(t, "rate_limit")
	var count atomic.Int32
	server := testhttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if count.Add(1) == 1 {
			_, _ = w.Write(first)
			return
		}
		w.Header().Set("retry-after", "4")
		w.WriteHeader(429)
		_, _ = w.Write(fail)
	}))
	defer server.Close()
	got, err := provider(t, server).ListModels(context.Background())
	e, ok := errors.AsType[*llm.Error](err)
	if !ok || e.Kind != llm.ErrRateLimit || e.RetryAfter != 4*time.Second || got != nil || count.Load() != 2 {
		t.Errorf("models=%+v, error=%v, calls=%d", got, err, count.Load())
	}
}

func TestClientTimeoutIsSafeAndRetryable(t *testing.T) {
	t.Parallel()
	// A timeout in the HTTP client is distinct from the caller cancelling.
	cause := fmt.Errorf("dummy-key request-private-text: %w", context.DeadlineExceeded)
	p, err := anthropic.NewWithOptions(llm.ProviderConfig{Name: "test", APIKey: secret.New("dummy-key"), BaseURL: "http://timeout.invalid"}, anthropic.Options{HTTPClient: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return nil, cause })}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), request())
	if !llm.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error=%v", err)
	}
	if strings.Contains(err.Error(), "dummy-key") || strings.Contains(err.Error(), "request-private-text") {
		t.Errorf("unsafe error: %s", err)
	}
}
