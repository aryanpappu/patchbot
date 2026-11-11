package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

func TestListModelsPagination(t *testing.T) {
	calls := 0
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1beta/models" {
			t.Errorf("unexpected models endpoint: %s %s", r.Method, r.URL)
		}
		name := "models_page1"
		wantQuery := ""
		if calls == 2 {
			name, wantQuery = "models_page2", url.Values{"pageToken": {"page +/2"}}.Encode()
		}
		if r.URL.RawQuery != wantQuery {
			t.Errorf("query = %q, want %q", r.URL.RawQuery, wantQuery)
		}
		if _, err := w.Write(fixture(t, name)); err != nil {
			t.Error(err)
		}
	})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ModelInfo{{ID: "fixture-a", DisplayName: "Fixture A", ContextWindow: 1000, Source: llm.SourceLive}, {ID: "fixture-b", DisplayName: "Fixture B", ContextWindow: 2000, Source: llm.SourceLive}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
	if calls != 2 {
		t.Errorf("models calls = %d", calls)
	}
}

func TestSchemaGolden(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		golden(t, "schema", body)
		if _, err := w.Write(fixture(t, "schema")); err != nil {
			t.Error(err)
		}
	})
	req := baseRequest()
	zero := 0.0
	req.Temperature = &zero
	req.Tools[0].Schema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"q":{"type":"string","enum":["one","two"],"default":"one","description":"Query"},"count":{"type":"integer","minimum":1,"maximum":9},"weight":{"type":"number","minimum":0},"enabled":{"type":"boolean","default":true},"tags":{"type":"array","items":{"type":"string","default":"tag"}},"nested":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}},"required":["q"]}`)
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestFinishReasons(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want llm.StopReason
	}{
		{"STOP", llm.StopToolUse}, {"MAX_TOKENS", llm.StopMaxTokens},
		{"SAFETY", llm.StopRefusal}, {"RECITATION", llm.StopRefusal}, {"BLOCKLIST", llm.StopRefusal}, {"PROHIBITED_CONTENT", llm.StopRefusal}, {"SPII", llm.StopRefusal},
		{"MALFORMED_FUNCTION_CALL", llm.StopOther}, {"UNEXPECTED_TOOL_CALL", llm.StopOther}, {"OTHER", llm.StopOther}, {"FUTURE_REASON", llm.StopOther},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				raw := strings.ReplaceAll(string(fixture(t, "one_call")), `"STOP"`, `"`+tt.raw+`"`)
				if _, err := io.WriteString(w, raw); err != nil {
					t.Error(err)
				}
			})
			got, err := p.Chat(context.Background(), baseRequest())
			if err != nil {
				t.Fatal(err)
			}
			if got.StopReason != tt.want || got.RawStop != tt.raw {
				t.Errorf("stop = %s / %s", got.StopReason, got.RawStop)
			}
			if tt.raw != "STOP" && len(got.Message.ToolCalls()) != 0 {
				t.Error("non-STOP call is executable")
			}
		})
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"null", `null`}, {"no candidates", `{}`},
		{"multiple candidates", `{"candidates":[{},{}]}`},
		{"no finish", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`},
		{"empty complete", `{"candidates":[{"finishReason":"STOP"}]}`},
		{"wrong role", `{"candidates":[{"finishReason":"STOP","content":{"role":"user","parts":[{"text":"hi"}]}}]}`},
		{"bad text", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":42}]}}]}`},
		{"null text", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":null}]}}]}`},
		{"empty part", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{}]}}]}`},
		{"array args", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"lookup","args":[]}}]}}]}`},
		{"null args", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"lookup","args":null}}]}}]}`},
		{"null call", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":null}]}}]}`},
		{"bad name", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"not valid"}}]}}]}`},
		{"duplicate IDs", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"id":"duplicate","name":"lookup"}},{"functionCall":{"id":"duplicate","name":"lookup"}}]}}]}`},
		{"ambiguous part", `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hi","functionCall":{"name":"lookup"}}]}}]}`},
		{"trailing JSON", string(fixture(t, "text")) + `{}`},
		{"truncated JSON", string(fixture(t, "body_cut"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Error(err)
				}
			})
			_, err := p.Chat(context.Background(), baseRequest())
			assertKind(t, err, llm.ErrMalformed)
		})
	}
}

func TestBodyFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		kind   llm.ErrorKind
	}{
		{"cut success", 200, llm.ErrUnavailable}, {"cut error", 500, llm.ErrUnavailable}, {"oversize success", 200, llm.ErrMalformed}, {"oversize error", 429, llm.ErrMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				if strings.HasPrefix(tt.name, "cut") {
					w.Header().Set("Content-Length", "1000")
					w.WriteHeader(tt.status)
					if _, err := w.Write(fixture(t, "body_cut")); err != nil {
						t.Error(err)
					}
				} else {
					w.WriteHeader(tt.status)
					// The client stops reading at the cap, so a closed pipe is expected.
					_, _ = io.CopyN(w, strings.NewReader(strings.Repeat(" ", maxResponseBody+2)), maxResponseBody+2)
				}
			})
			_, err := p.Chat(context.Background(), baseRequest())
			assertKind(t, err, tt.kind)
			if strings.HasPrefix(tt.name, "cut") && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("read cause not retained: %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSafeErrors(t *testing.T) {
	secretText := "prompt-body-private-marker" //nolint:gosec // Synthetic prompt marker for leak checks.
	querySecret := "query-private-marker"
	cause := fmt.Errorf("transport %s https://example.invalid/?secret=%s body=%s", testKey, querySecret, secretText)
	p, err := NewWithClient(llm.ProviderConfig{Name: "test-gemini", APIKey: secret.New(testKey)}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), baseRequest())
	assertKind(t, err, llm.ErrUnavailable)
	if !errors.Is(err, cause) {
		t.Error("transport cause was lost")
	}
	checkSafe(t, err, testKey, querySecret, secretText)
	p = newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 400, "status": testKey, "message": cause.Error()}}); err != nil {
			t.Error(err)
		}
	})
	_, err = p.Chat(context.Background(), baseRequest())
	checkSafe(t, err, testKey, querySecret, secretText)
	// Decoder type errors quote wire data; they must also stay behind WithCause.
	p = newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"candidates":"`+testKey+`"}`); err != nil {
			t.Error(err)
		}
	})
	_, err = p.Chat(context.Background(), baseRequest())
	assertKind(t, err, llm.ErrMalformed)
	checkSafe(t, err, testKey)
}

func checkSafe(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	e, ok := errors.AsType[*llm.Error](err)
	if !ok {
		t.Fatalf("not an llm.Error: %T", err)
	}
	data, marshalErr := json.Marshal(e)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, output := range []string{err.Error(), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e), string(data), llm.SafeText(err)} {
		for _, value := range secrets {
			if strings.Contains(output, value) {
				t.Error("error diagnostic exposed secret data")
			}
		}
	}
}

func TestCancellation(t *testing.T) {
	for _, operation := range []string{"chat", "models"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "chat" {
					_, err = p.Chat(ctx, baseRequest())
				} else {
					_, err = p.ListModels(ctx)
				}
				done <- err
			}()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancel error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("context cancellation was not respected")
			}
			if _, err := p.Chat(ctx, baseRequest()); !errors.Is(err, context.Canceled) {
				t.Errorf("pre-cancel = %v", err)
			}
		})
	}
}

func TestConstructorsAndValidation(t *testing.T) {
	for _, base := range []string{"ftp://example.invalid", "https://user:password@example.invalid", "https://example.invalid?key=private", "https://example.invalid#private", "https://example.invalid?", "https://example.invalid/%zz"} {
		_, err := NewWithClient(llm.ProviderConfig{Name: "test", BaseURL: base, APIKey: secret.New(testKey)}, nil)
		assertKind(t, err, llm.ErrBadRequest)
		checkSafe(t, err, "password", "private")
	}
	for _, key := range []string{"", "line\nbreak"} {
		_, err := New(llm.ProviderConfig{Name: "test", APIKey: secret.New(key)})
		assertKind(t, err, llm.ErrAuth)
	}
	p, err := NewWithClient(llm.ProviderConfig{Name: "instance", APIKey: secret.New(testKey)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "instance" || p.base.String() != defaultBaseURL {
		t.Error("constructor defaults")
	}
	p = newTestProvider(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached transport") })
	for _, mutate := range []func(*llm.ChatRequest){
		func(r *llm.ChatRequest) { r.Model = "../invalid?key=private" },
		func(r *llm.ChatRequest) { r.MaxTokens = 0 },
		func(r *llm.ChatRequest) { r.Tools[0].Schema = json.RawMessage(`{"type":"object","oneOf":[]}`) },
		func(r *llm.ChatRequest) {
			r.Tools[0].Schema = json.RawMessage(`{"type":"object","properties":{"x":{"type":["string","null"]}}}`)
		},
		func(r *llm.ChatRequest) { r.Temperature = new(math.NaN()) },
	} {
		req := baseRequest()
		mutate(&req)
		_, err := p.Chat(context.Background(), req)
		assertKind(t, err, llm.ErrBadRequest)
	}
}

func TestUsage(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want *llm.Usage
		bad  bool
	}{
		{`{}`, nil, false}, {`null`, nil, false}, {`{"promptTokenCount":20}`, nil, false},
		{`{"promptTokenCount":20,"candidatesTokenCount":5}`, &llm.Usage{InputTokens: 20, OutputTokens: 5}, false},
		{`{"promptTokenCount":0,"candidatesTokenCount":0}`, &llm.Usage{}, false},
		{`{"promptTokenCount":1,"candidatesTokenCount":5,"cachedContentTokenCount":2}`, nil, true},
		{`{"promptTokenCount":20,"candidatesTokenCount":-1}`, nil, true},
		{`{"promptTokenCount":20,"candidatesTokenCount":9223372036854775807,"thoughtsTokenCount":1}`, nil, true},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				body := strings.Replace(string(fixture(t, "missing_usage")), `"modelVersion"`, `"usageMetadata":`+tt.raw+`,"modelVersion"`, 1)
				if _, err := io.WriteString(w, body); err != nil {
					t.Error(err)
				}
			})
			got, err := p.Chat(context.Background(), baseRequest())
			if tt.bad {
				assertKind(t, err, llm.ErrMalformed)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got.Usage); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestConcurrentChats(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(fixture(t, "one_call")); err != nil {
			t.Error(err)
		}
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			response, err := p.Chat(context.Background(), baseRequest())
			if err != nil {
				t.Error(err)
				return
			}
			if response.Message.ToolCalls()[0].ID != "call_0_0" {
				t.Error("call ID depends on provider state")
			}
		})
	}
	wg.Wait()
}

func TestRedirectRefused(t *testing.T) {
	var redirected atomic.Int64
	other := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	_ = other.Client()
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", other.URL)
		w.WriteHeader(307)
		if _, err := io.WriteString(w, `{"error":{"code":307,"status":"REDIRECT"}}`); err != nil {
			t.Error(err)
		}
	})
	_, err := p.Chat(context.Background(), baseRequest())
	assertKind(t, err, llm.ErrBadRequest)
	if redirected.Load() != 0 {
		t.Error("credential-bearing redirect was followed")
	}
}

func TestPaginationCycle(t *testing.T) {
	calls := 0
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if _, err := io.WriteString(w, `{"models":[],"nextPageToken":"again"}`); err != nil {
			t.Error(err)
		}
	})
	_, err := p.ListModels(context.Background())
	assertKind(t, err, llm.ErrMalformed)
	if calls != 2 {
		t.Errorf("pagination calls=%d", calls)
	}
}

func TestMetadataAndResultValidation(t *testing.T) {
	p := newTestProvider(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid replay reached transport") })
	for _, name := range []string{"wrong result name", "wrong result order", "changed metadata", "invalid metadata"} {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			parts := []llm.Part{callPart(t, "two_calls", 0, "call_0_0", "one"), callPart(t, "two_calls", 1, "call_0_1", "two")}
			results := []llm.Part{llm.Result("call_0_0", "lookup", "one", false), llm.Result("call_0_1", "lookup", "two", false)}
			switch name {
			case "wrong result name":
				results[0].ToolResult.Name = "other"
			case "wrong result order":
				results[0], results[1] = results[1], results[0]
			case "changed metadata":
				parts[0].ToolCall.Args = json.RawMessage(`{"q":"changed"}`)
			case "invalid metadata":
				parts[0].ProviderMeta = json.RawMessage(`null`)
			}
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Origin: p.Name(), Parts: parts}, llm.Message{Role: llm.RoleTool, Parts: results})
			_, err := p.Chat(context.Background(), req)
			assertKind(t, err, llm.ErrBadRequest)
		})
	}
}

func TestSyntheticIDTurnsAndCollisions(t *testing.T) {
	for _, name := range []string{"next turn", "explicit collision", "missing arguments"} {
		t.Run(name, func(t *testing.T) {
			body := fixture(t, "one_call")
			want := "call_1_0"
			if name == "explicit collision" {
				body = []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"lookup"}},{"functionCall":{"id":"call_1_0","name":"lookup"}}]}}]}`)
				want = "call_1_0_"
			}
			if name == "missing arguments" {
				body = []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"lookup"}}]}}]}`)
			}
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(body); err != nil {
					t.Error(err)
				}
			})
			req := baseRequest()
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Call("prior", "lookup", json.RawMessage(`{}`))}}, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("prior", "lookup", "", false)}})
			got, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Message.ToolCalls()[0].ID != want {
				t.Errorf("ID = %s, want %s", got.Message.ToolCalls()[0].ID, want)
			}
			if name == "missing arguments" && string(got.Message.ToolCalls()[0].Args) != "{}" {
				t.Error("missing arguments were not normalized to an empty object")
			}
			if err := llm.Validate(append(req.Messages, got.Message)); err != nil {
				t.Errorf("invalid normalized transcript: %v", err)
			}
		})
	}
}

func TestRetryDelays(t *testing.T) {
	for _, tt := range []struct {
		header, delay string
		want          time.Duration
	}{
		{"4", "2.500s", 4 * time.Second}, {"1", "2.500s", 2500 * time.Millisecond}, {"", "0s", 0}, {"-1", "-2s", 0}, {"garbage", "bad", 0}, {"", "1.000000001s", time.Second + time.Nanosecond},
	} {
		t.Run(tt.header+"_"+tt.delay, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tt.header)
				w.WriteHeader(429)
				if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED", "details": []any{map[string]string{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": tt.delay}}}}); err != nil {
					t.Error(err)
				}
			})
			_, err := p.Chat(context.Background(), baseRequest())
			assertKind(t, err, llm.ErrRateLimit)
			e, _ := errors.AsType[*llm.Error](err)
			if e.RetryAfter != tt.want {
				t.Errorf("delay=%s want=%s", e.RetryAfter, tt.want)
			}
		})
	}
}

func TestMinimalRequest(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		golden(t, "minimal", body)
		if _, err := w.Write(fixture(t, "text")); err != nil {
			t.Error(err)
		}
	})
	req := baseRequest()
	req.System = ""
	req.Tools = nil
	req.Model = "models/fixture-model"
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestClientDeadlineIsUnavailable(t *testing.T) {
	p, err := NewWithClient(llm.ProviderConfig{Name: "test", APIKey: secret.New(testKey)}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), baseRequest())
	assertKind(t, err, llm.ErrUnavailable)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("timeout cause lost")
	}
}

func TestForeignMetadata(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(body), "foreign-marker") {
			t.Error("foreign metadata replayed")
		}
		if _, err := w.Write(fixture(t, "text")); err != nil {
			t.Error(err)
		}
	})
	req := baseRequest()
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Origin: "foreign", Parts: []llm.Part{
		{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(`{"thinking":"foreign-marker"}`)},
		{Type: llm.PartText, Text: "Answer", ProviderMeta: json.RawMessage(`{"thoughtSignature":"foreign-marker"}`)},
	}}, llm.UserText("Continue"))
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestInjectedClientStillGetsContextDeadline(t *testing.T) {
	p, err := NewWithClient(llm.ProviderConfig{Name: "test", APIKey: secret.New(testKey)}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("outbound HTTP has no deadline")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[]}`)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
}
