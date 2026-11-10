package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
)

func TestNonJSONHTTPErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		kind   llm.ErrorKind
	}{
		{"gateway HTML", 502, "<html>Bad Gateway " + testKey + "</html>", llm.ErrUnavailable},
		{"empty rate limit", 429, "", llm.ErrRateLimit},
		{"truncated overload", 503, `{"error":`, llm.ErrOverloaded},
		{"invalid envelope types", 500, `{"error":{"code":"private diagnostic"}}`, llm.ErrUnavailable},
		{"plain text auth", 401, "private diagnostic", llm.ErrAuth},
		{"empty bad request", 400, "", llm.ErrBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(tt.status)
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Error(err)
				}
			})
			_, err := p.Chat(context.Background(), baseRequest())
			assertKind(t, err, tt.kind)
			e, _ := errors.AsType[*llm.Error](err)
			if e.Status != tt.status || e.Provider != p.Name() {
				t.Errorf("error status/provider = %d/%q, want %d/%q", e.Status, e.Provider, tt.status, p.Name())
			}
			wantRetry := tt.status >= 500 || tt.status == 429
			if e.Retryable() != wantRetry {
				t.Errorf("retryable = %v, want %v", e.Retryable(), wantRetry)
			}
			wantDelay := time.Duration(0)
			if wantRetry {
				wantDelay = time.Minute
			}
			if e.RetryAfter != wantDelay {
				t.Errorf("retry delay = %s, want %s", e.RetryAfter, wantDelay)
			}
			if e.Unwrap() == nil {
				t.Error("error-body decoding cause was lost")
			}
			if safe := llm.SafeText(err); strings.Contains(safe, testKey) || strings.Contains(safe, "private diagnostic") || strings.Contains(safe, "<html>") {
				t.Errorf("unsafe error text: %s", safe)
			}
		})
	}
}

type retryClock struct {
	delays []time.Duration
}

func (*retryClock) Now() time.Time { return time.Unix(0, 0) }

func (c *retryClock) Sleep(ctx context.Context, delay time.Duration) error {
	c.delays = append(c.delays, delay)
	return ctx.Err()
}

func TestRetryableHTTPErrorDelays(t *testing.T) {
	for _, tt := range []struct {
		name, header, delay string
		status              int
		kind                llm.ErrorKind
		want                time.Duration
	}{
		{"overload header", "60", "", 503, llm.ErrOverloaded, time.Minute},
		{"overload RPC", "", "65s", 503, llm.ErrOverloaded, 65 * time.Second},
		{"overload longer header", "60", "2.5s", 503, llm.ErrOverloaded, time.Minute},
		{"overload longer RPC", "60", "65s", 503, llm.ErrOverloaded, 65 * time.Second},
		{"unavailable header", "60", "bad", 502, llm.ErrUnavailable, time.Minute},
		{"unavailable RPC", "invalid", "65s", 500, llm.ErrUnavailable, 65 * time.Second},
		{"timeout header", "60", "-2s", 408, llm.ErrUnavailable, time.Minute},
		{"overload 529", "60", "0s", 529, llm.ErrOverloaded, time.Minute},
		{"rate limit", "60", "65s", 429, llm.ErrRateLimit, 65 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls > 1 {
					if _, err := w.Write(fixture(t, "text")); err != nil {
						t.Error(err)
					}
					return
				}
				w.Header().Set("Retry-After", tt.header)
				w.WriteHeader(tt.status)
				if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
					"code": tt.status,
					"details": []any{
						map[string]string{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": tt.delay},
						map[string]string{"@type": "unrelated", "retryDelay": "100s"},
					},
				}}); err != nil {
					t.Error(err)
				}
			})
			clock := &retryClock{}
			policy := llm.RetryPolicy{MaxAttempts: 2, Jitter: func(time.Duration) time.Duration { return time.Millisecond }, OnRetry: func(info llm.RetryInfo) {
				assertKind(t, info.Err, tt.kind)
				e, _ := errors.AsType[*llm.Error](info.Err)
				if e.Status != tt.status || e.RetryAfter != tt.want {
					t.Errorf("retry error status/delay = %d/%s, want %d/%s", e.Status, e.RetryAfter, tt.status, tt.want)
				}
			}}
			response, err := llm.WithRetry(p, policy, clock).Chat(context.Background(), baseRequest())
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || response.Message.Text() != "Hello." {
				t.Errorf("calls/answer = %d/%q", calls, response.Message.Text())
			}
			if diff := cmp.Diff([]time.Duration{tt.want}, clock.delays); diff != "" {
				t.Errorf("retry waits (-want +got):\n%s", diff)
			}
		})
	}
}

func TestThoughtPartsStayOpaque(t *testing.T) {
	for _, tt := range []struct {
		name, thought, answer string
	}{
		{"thought only", `{ "text" : "Thinking.", "thought":true, "thoughtSignature":"c2ln", "future" : [1, 2] }`, ""},
		{"thought and answer", `{ "text" : "Thinking.", "thought":true }`, `,{"text":"Answer.","thought":false}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 1 {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					if !bytes.Contains(body, []byte(tt.thought)) {
						t.Error("thought part was not replayed byte for byte")
					}
				}
				if _, err := io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[`+tt.thought+tt.answer+`]}}]}`); err != nil {
					t.Error(err)
				}
			})
			req := baseRequest()
			response, err := p.Chat(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			wantText := ""
			if tt.answer != "" {
				wantText = "Answer."
			}
			if text := response.Message.Text(); text != wantText {
				t.Errorf("answer text = %q, want %q", text, wantText)
			}
			want := llm.Part{Type: llm.PartOpaque, ProviderMeta: json.RawMessage(tt.thought)}
			if diff := cmp.Diff(want, response.Message.Parts[0]); diff != "" {
				t.Errorf("thought part (-want +got):\n%s", diff)
			}
			req.Messages = append(req.Messages, response.Message, llm.UserText("Continue"))
			if _, err := p.Chat(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			// A different provider instance reconstructs only normalized parts.
			foreign := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if bytes.Contains(body, []byte("Thinking.")) || bytes.Contains(body, []byte("thought")) {
					t.Error("thought leaked into another provider's request")
				}
				var wire struct {
					Contents []content `json:"contents"`
				}
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Error(err)
					return
				}
				if diff := cmp.Diff([]json.RawMessage{json.RawMessage(`{"text":"Answer."}`)}, wire.Contents[1].Parts); diff != "" {
					t.Errorf("foreign answer parts (-want +got):\n%s", diff)
				}
				if _, err := w.Write(fixture(t, "text")); err != nil {
					t.Error(err)
				}
			})
			foreign.name = "other-gemini"
			_, err = foreign.Chat(context.Background(), req)
			if tt.answer == "" {
				assertKind(t, err, llm.ErrBadRequest) // No portable assistant parts remain.
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
