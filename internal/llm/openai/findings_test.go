package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
)

func TestContinuationMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	fields := map[string]json.RawMessage{
		"reasoning_details": json.RawMessage(`[ { "type": "reasoning.encrypted", "data": "sig\u002b<>&", "index": 1e0 }, { "data": "second" } ]`),
		"reasoning_content": json.RawMessage(`"think\u0069ng"`),
		"future_signature":  json.RawMessage(`{ "opaque": [ null, 9007199254740993, 1.00 ] }`),
	}
	var requests atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			writeBody(t, w, []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"first","type":"function","function":{"name":"read_file","arguments":"{}"}}],"reasoning_details":`+string(fields["reasoning_details"])+`,"reasoning_content":`+string(fields["reasoning_content"])+`,"future_signature":`+string(fields["future_signature"])+`},"finish_reason":"tool_calls"}]}`))
			return
		}
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		} else {
			for name, want := range fields {
				if got := body.Messages[2][name]; string(got) != string(want) {
					t.Errorf("replayed %s = %s; want exact bytes %s", name, got, want)
				}
			}
		}
		writeBody(t, w, readFixture(t, "openai_text.resp.json"))
	})
	req := baseRequest(true)
	got, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	for _, part := range got.Message.Parts {
		if part.Type == llm.PartOpaque {
			if err := json.Unmarshal(part.ProviderMeta, &metadata); err != nil {
				t.Fatal(err)
			}
		}
	}
	if diff := cmp.Diff(fields, metadata); diff != "" {
		t.Fatalf("provider_meta lost bytes (-want +got):\n%s", diff)
	}
	req.Messages = append(req.Messages, got.Message, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.Result("first", "read_file", "done", false)}})
	before := req.Clone()
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, req); diff != "" {
		t.Errorf("continuation mutated: %s", diff)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d; want 2", requests.Load())
	}
}

func TestHTTP200ProviderErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		kind       llm.ErrorKind
		status     int
	}{
		{"top level", `{"error":{"code":503,"message":"private-response-body"}}`, llm.ErrOverloaded, 503},
		{"partial choice", `{"choices":[{"message":{"role":"assistant","content":"partial output"},"finish_reason":"error","error":{"code":502,"message":"private-response-body"}}]}`, llm.ErrUnavailable, 502},
		{"error despite stop", `{"choices":[{"message":{"role":"assistant","content":"partial output"},"finish_reason":"stop","error":{"code":429,"message":"private-response-body"}}]}`, llm.ErrRateLimit, 429},
		{"auth", `{"error":{"code":401,"message":"private-response-body"}}`, llm.ErrAuth, 401},
		{"context overflow", `{"error":{"code":400,"message":"maximum context length private-response-body"}}`, llm.ErrContextTooLong, 400},
		{"missing code", `{"choices":[{"message":{"role":"assistant","content":"partial output"},"finish_reason":"error","error":{"message":"private-response-body"}}]}`, llm.ErrMalformed, 0},
		{"missing error", `{"choices":[{"message":{"role":"assistant","content":"partial output"},"finish_reason":"error"}]}`, llm.ErrMalformed, 0},
		{"invalid code", `{"error":{"code":200,"message":"private-response-body"}}`, llm.ErrMalformed, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.Header().Set("X-Request-ID", "req_embedded_error")
				writeBody(t, w, []byte(tt.body))
			})
			out, err := p.Chat(context.Background(), baseRequest(false))
			got := assertKind(t, err, tt.kind)
			if out != nil || got.Status != tt.status {
				t.Errorf("response = %+v, error = %+v", out, got)
			}
			if tt.status != 0 && (got.RetryAfter != 2*time.Second || got.RequestID != "req_embedded_error") {
				t.Errorf("lost error metadata: %+v", got)
			}
		})
	}
}

// synctest advances the adapter's real two-minute timer without wall-clock
// delays. Every Chat attempt also checks the ordinary request golden.
func TestInternalTimeoutRetryGolden(t *testing.T) {
	for _, op := range []string{"chat", "models"} {
		for _, phase := range []string{"transport", "read", "after read"} {
			t.Run(op+"/"+phase, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var calls int
					client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						if op == "chat" {
							data, err := io.ReadAll(r.Body)
							if err != nil {
								t.Fatal(err)
							}
							golden(t, "openai_text", data)
						}
						body := `{"data":[]}`
						if op == "chat" {
							body = string(readFixture(t, "openai_text.resp.json"))
						}
						if calls == 1 && phase == "transport" {
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						reader := io.NopCloser(strings.NewReader(body))
						if calls == 1 {
							reader = &timeoutBody{ctx: r.Context(), data: body, readError: phase == "read"}
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
					})}
					p, err := NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: "https://api.openai.com/v1"}, Options{HTTPClient: client})
					if err != nil {
						t.Fatal(err)
					}
					retried := 0
					wrapped := llm.WithRetry(p, llm.RetryPolicy{MaxAttempts: 2, Jitter: func(time.Duration) time.Duration { return 0 }, OnRetry: func(info llm.RetryInfo) {
						retried++
						_ = assertKind(t, info.Err, llm.ErrUnavailable)
					}}, nil)
					if op == "chat" {
						_, err = wrapped.Chat(context.Background(), baseRequest(false))
					} else {
						_, err = wrapped.ListModels(context.Background())
					}
					if err != nil || calls != 2 || retried != 1 {
						t.Fatalf("error = %v, calls = %d, retries = %d", err, calls, retried)
					}
				})
			})
		}
	}
}

type timeoutBody struct {
	ctx       context.Context
	data      string
	readError bool
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	if b.readError {
		return 0, b.ctx.Err()
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, io.EOF
}

func (*timeoutBody) Close() error { return nil }

func TestCallerContextErrorsRemainContextErrors(t *testing.T) {
	for _, op := range []string{"chat", "models"} {
		for _, phase := range []string{"transport", "read", "after read"} {
			for _, mode := range []string{"cancel", "deadline"} {
				t.Run(op+"/"+phase+"/"+mode, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						cause := errors.New("private-request-body")
						var ctx context.Context
						if mode == "cancel" {
							var cancel context.CancelCauseFunc
							ctx, cancel = context.WithCancelCause(context.Background())
							defer cancel(nil)
							go func() {
								time.Sleep(time.Second)
								cancel(cause)
							}()
						} else {
							var cancel context.CancelFunc
							ctx, cancel = context.WithTimeoutCause(context.Background(), time.Second, cause)
							defer cancel()
						}
						calls := 0
						client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
							calls++
							if phase == "transport" {
								<-r.Context().Done()
								return nil, r.Context().Err()
							}
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &timeoutBody{ctx: r.Context(), data: `{}`, readError: phase == "read"}}, nil
						})}
						p, err := NewWithOptions(llm.ProviderConfig{Name: "test", BaseURL: "https://example.invalid/v1"}, Options{HTTPClient: client})
						if err != nil {
							t.Fatal(err)
						}
						if op == "chat" {
							_, err = p.Chat(ctx, baseRequest(false))
						} else {
							_, err = p.ListModels(ctx)
						}
						var providerError *llm.Error
						if !errors.Is(err, ctx.Err()) || errors.As(err, &providerError) || calls != 1 {
							t.Fatalf("error = %v; want caller error %v, calls = %d", err, ctx.Err(), calls)
						}
					})
				})
			}
		}
	}
}

func TestCacheWriteUsage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                  string
		input, cached, writes int64
		invalid               bool
	}{
		{"writes", 2000, 0, 1500, false},
		{"reads and writes", 2000, 250, 1500, false},
		{"all cached", 2000, 500, 1500, false},
		{"negative writes", 2000, 0, -1, true},
		{"excess writes", 2000, 0, 2001, true},
		{"excess combined cache", 2000, 501, 1500, true},
		{"overflow combined cache", math.MaxInt64, math.MaxInt64, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				writeBody(t, w, fmt.Appendf(nil, `{"model":"fixture-model","choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}}`, tt.input, tt.cached, tt.writes))
			})
			got, err := p.Chat(context.Background(), baseRequest(false))
			if tt.invalid {
				_ = assertKind(t, err, llm.ErrMalformed)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := &llm.Usage{InputTokens: tt.input - tt.cached - tt.writes, OutputTokens: 10, CacheReadTokens: tt.cached, CacheWriteTokens: tt.writes}
			if diff := cmp.Diff(want, got.Usage); diff != "" {
				t.Errorf("usage (-want +got):\n%s", diff)
			}
			readPrice, writePrice := int64(1_000_000), int64(8_000_000)
			pricing := llm.Pricing{Models: map[string]llm.Price{"fixture-model": {Input: 2_000_000, Output: 3_000_000, CacheRead: &readPrice, CacheWrite: &writePrice}}}
			wantCost := want.InputTokens*2 + 30 + tt.cached + tt.writes*8
			if cost, known := pricing.Cost(got.Model, got.Usage); !known || cost != wantCost {
				t.Errorf("cost = %d, known = %v; want %d", cost, known, wantCost)
			}
		})
	}
}
