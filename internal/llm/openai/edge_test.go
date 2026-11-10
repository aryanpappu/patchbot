package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

func TestConstructorAndQuirks(t *testing.T) {
	t.Parallel()
	for _, f := range families() {
		p, err := NewWithOptions(llm.ProviderConfig{Name: f.name, BaseURL: f.base}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != f.name {
			t.Error("instance name lost")
		}
		if diff := cmp.Diff(f.quirks, p.quirks); diff != "" {
			t.Errorf("%s quirks: %s", f.name, diff)
		}
	}
	for _, base := range []string{"http://127.0.0.1:1234/v1", "http://[::1]:1234/v1", "http://ollama:11434/v1", "http://lmstudio:1234/v1"} {
		p, err := NewWithOptions(llm.ProviderConfig{BaseURL: base}, Options{})
		if err != nil || !p.quirks.OmitParallelToolCalls {
			t.Errorf("local quirks: %v", err)
		}
	}
	for _, base := range []string{"", "/relative", "ftp://example.invalid/v1", "http://%", "https://user:" + testKey + "@example.invalid/v1", "https://example.invalid/v1?token=query-secret", "https://example.invalid/v1?", "https://example.invalid/v1#private-request-body"} {
		_, err := New(llm.ProviderConfig{Name: "test", BaseURL: base})
		_ = assertKind(t, err, llm.ErrBadRequest)
	}
	_, err := New(llm.ProviderConfig{Name: "test", BaseURL: "https://example.invalid/v1", APIKey: secret.New(testKey + "\n")})
	_ = assertKind(t, err, llm.ErrAuth)
	_, err = NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: "https://example.invalid/v1"}, Options{Quirks: &Quirks{MaxTokensField: "private-request-body"}})
	_ = assertKind(t, err, llm.ErrBadRequest)
	q := Quirks{MaxTokensField: "max_completion_tokens"}
	client := &http.Client{Timeout: time.Second}
	p, err := NewWithOptions(llm.ProviderConfig{BaseURL: "http://localhost:1234/prefix%2Fsegment/v1"}, Options{HTTPClient: client, Quirks: &q})
	if err != nil {
		t.Fatal(err)
	}
	q.MaxTokensField = "changed"
	if p.quirks.MaxTokensField != "max_completion_tokens" || p.quirks.OmitParallelToolCalls || client.CheckRedirect != nil || client.Timeout != time.Second {
		t.Error("constructor settings share mutable state or changed caller's client")
	}
	if got := p.endpoint("chat/completions").EscapedPath(); got != "/prefix%2Fsegment/v1/chat/completions" {
		t.Errorf("escaped prefix = %s", got)
	}
}

func TestMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, response := range []string{
		`private-response-body`, `{}`, `null`, `{} {}`,
		`{"choices":[]}`,
		`{"choices":[{"message":{"role":"user","content":"Done."},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":[]},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"},{"message":{}}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"custom","function":{"name":"read_file"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"bad name"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"read_file"}}],"function_call":{"name":"read_file"}},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":-1,"completion_tokens":0}}`,
		`{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2}}}`,
	} {
		t.Run(fmt.Sprintf("case_%d", len(response))+response[:min(len(response), 15)], func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) { writeBody(t, w, []byte(response)) })
			_, err := p.Chat(context.Background(), baseRequest(true))
			_ = assertKind(t, err, llm.ErrMalformed)
		})
	}
}

func TestArgumentNormalizationAndIDs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, raw, want string
		invalid         bool
	}{
		{"missing", "", `{}`, false}, {"null", `null`, `{}`, false},
		{"empty string", `""`, `{}`, false}, {"blank string", `"  "`, `{}`, false},
		{"string object", `"{\"x\":1}"`, `{"x":1}`, false}, {"object", ` { "x": 1 } `, `{"x":1}`, false},
		{"bad string", `"{x:1}"`, "", true}, {"array", `"[]"`, "", true},
		{"number", `1`, "", true}, {"trailing JSON", `"{}{}"`, "", true},
		{"string null", `"null"`, "", true}, {"string scalar", `"true"`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeArguments(json.RawMessage(tt.raw))
			if (err != nil) != tt.invalid || string(got) != tt.want {
				t.Errorf("got %s, %v", got, err)
			}
		})
	}
	// IDs must remain unique across history and this batch. Reserve supplied
	// IDs even when a synthetic ID would otherwise collide with a later call.
	req := baseRequest(true)
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Call("old", "read_file", json.RawMessage(`{}`))}},
		llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("old", "read_file", "", false)}})
	response := `{"model":"fixture-model","choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"old","type":"function","function":{"name":"read_file","arguments":""}},
		{"id":"call_3_0_0","type":"function","function":{"name":"read_file","arguments":{}}},
		{"id":"","type":"function","function":{"name":"read_file"}},
		{"id":"call_3_0_0","type":"function","function":{"name":"read_file","arguments":null}}
	]},"finish_reason":"tool_calls"}]}`
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) { writeBody(t, w, []byte(response)) })
	got, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"call_3_0_1", "call_3_0_0", "call_3_2_0", "call_3_3_0"}
	for i, call := range got.Message.ToolCalls() {
		if call.ID != wantIDs[i] || string(call.Args) != `{}` {
			t.Errorf("call %d = %+v", i, call)
		}
	}
	if err := llm.Validate(append(req.Messages, got.Message)); err != nil {
		t.Error(err)
	}
}

func TestResponseBodyLimitsAndCutStream(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"chat", "models"} {
		for _, status := range []int{200, 500} {
			t.Run(op+fmt.Sprint(status), func(t *testing.T) {
				t.Parallel()
				p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					writeBody(t, w, []byte(strings.Repeat(" ", maxResponseBody+1)))
				})
				var err error
				if op == "chat" {
					_, err = p.Chat(context.Background(), baseRequest(false))
				} else {
					_, err = p.ListModels(context.Background())
				}
				_ = assertKind(t, err, llm.ErrMalformed)
			})
		}
	}
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10000")
		writeBody(t, w, []byte(`{"choices":[`))
	})
	_, err := p.Chat(context.Background(), baseRequest(false))
	_ = assertKind(t, err, llm.ErrUnavailable)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error("cut stream lost read cause")
	}
}

func TestSafeErrorsAndContextDetection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		status int
		body   string
		kind   llm.ErrorKind
	}{
		{400, `{"error":{"code":400,"message":"Maximum context exceeded"}}`, llm.ErrContextTooLong},
		{400, `{"error":{"message":"too many tokens"}}`, llm.ErrContextTooLong},
		{400, `{"error":{"message":"context window exceeded"}}`, llm.ErrContextTooLong},
		{400, `{"error":{"message":"unsupported model private-request-body"}}`, llm.ErrBadRequest},
		{401, `private-request-body ` + testKey + ` query-secret`, llm.ErrAuth},
		{500, `{"error":{"message":"private-response-body ` + testKey + ` query-secret"}}`, llm.ErrUnavailable},
		{200, `{"choices":[{"message":{"role": "` + testKey + `", "content":"private-response-body"}}]}`, llm.ErrMalformed},
	} {
		t.Run(fmt.Sprint(tt.status)+tt.body[:8], func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Request-ID", testKey)
				w.WriteHeader(tt.status)
				writeBody(t, w, []byte(tt.body))
			})
			_, err := p.Chat(context.Background(), baseRequest(false))
			got := assertKind(t, err, tt.kind)
			if got.RequestID != "" {
				t.Error("echoed API key in request ID")
			}
		})
	}
}

func TestRedirectsRejected(t *testing.T) {
	t.Parallel()
	var foreignCalls atomic.Int32
	other := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreignCalls.Add(1) }))
	_ = other.Client()
	for _, relative := range []bool{true, false} {
		p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			target := other.URL + "/" + testKey
			if relative {
				target = "/" + testKey
			}
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		_, err := p.Chat(context.Background(), baseRequest(false))
		_ = assertKind(t, err, llm.ErrBadRequest)
	}
	if foreignCalls.Load() != 0 {
		t.Error("redirect forwarded credentials")
	}
}

func TestInvalidRequestsAndMetadata(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	response := readFixture(t, "openai_text.resp.json")
	p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeBody(t, w, response)
	})
	for _, mutate := range []func(*llm.ChatRequest){
		func(r *llm.ChatRequest) { r.Model = "" },
		func(r *llm.ChatRequest) { r.MaxTokens = 0 },
		func(r *llm.ChatRequest) { r.Messages = nil },
		func(r *llm.ChatRequest) { r.Tools[0].Schema = json.RawMessage(`{"type":"private-request-body"}`) },
		func(r *llm.ChatRequest) {
			r.Messages = append(r.Messages,
				llm.Message{Role: llm.RoleAssistant, Origin: "test", Parts: []llm.Part{{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(`{"private-request-body":true}`)}}}, llm.UserText("Continue."))
		},
		func(r *llm.ChatRequest) {
			r.Messages = append(r.Messages,
				llm.Message{Role: llm.RoleAssistant, Origin: "test", Parts: []llm.Part{{Type: llm.PartText, Text: "Hello.", ProviderMeta: json.RawMessage(`{}`)}}}, llm.UserText("Continue."))
		},
	} {
		req := baseRequest(true)
		mutate(&req)
		_, err := p.Chat(context.Background(), req)
		_ = assertKind(t, err, llm.ErrBadRequest)
	}
	if calls.Load() != 0 {
		t.Error("invalid request reached transport")
	}
	// Foreign opaque blocks are dropped. Foreign text is still sent.
	req := baseRequest(false)
	req.Temperature = nil
	req.System = ""
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Origin: "other", Parts: []llm.Part{llm.Text("Hello."), {Type: llm.PartOpaque, ProviderMeta: json.RawMessage(`{}`)}}}, llm.UserText("Continue."))
	data, err := p.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "optional_fields", data)
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownFinishAndIncompleteUsage(t *testing.T) {
	t.Parallel()
	for _, usage := range []string{`null`, `{}`, `{"prompt_tokens":0}`, `{"completion_tokens":0}`} {
		p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			writeBody(t, w, []byte(`{"choices":[{"message":{"role":"assistant","content":"Hello."},"finish_reason":"private-response-body"}],"usage":`+usage+`}`))
		})
		got, err := p.Chat(context.Background(), baseRequest(false))
		if err != nil {
			t.Fatal(err)
		}
		if got.StopReason != llm.StopOther || got.RawStop != "" || got.Usage != nil {
			t.Errorf("response = %+v", got)
		}
	}
}

func TestRequestIDSanitization(t *testing.T) {
	t.Parallel()
	p := &Provider{key: secret.New(testKey)}
	for _, id := range []string{"req_good", testKey, "req_" + testKey, "req invalid", strings.Repeat("x", 129), "sk-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		header := http.Header{"X-Request-Id": {id}}
		got := p.requestID(header)
		if got == testKey || (id != "req_good" && got == id) {
			t.Error("unsafe request ID retained")
		}
	}
}

func TestEndpointURL(t *testing.T) {
	t.Parallel()
	base, err := url.Parse("https://example.invalid/api/v1/")
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{base: base}
	if got := p.endpoint("models").String(); got != "https://example.invalid/api/v1/models" {
		t.Error(got)
	}
	if base.Path != "/api/v1/" {
		t.Error("endpoint mutated shared base URL")
	}
}
