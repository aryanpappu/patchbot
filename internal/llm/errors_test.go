package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
)

func TestErrorRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind llm.ErrorKind
		want bool
	}{
		{llm.ErrAuth, false},
		{llm.ErrRateLimit, true},
		{llm.ErrOverloaded, true},
		{llm.ErrUnavailable, true},
		{llm.ErrContextTooLong, false},
		{llm.ErrBadRequest, false},
		{llm.ErrMalformed, false},
		{llm.ErrRefusal, false},
		{"", false},
		{"something_new", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()
			err := &llm.Error{Kind: tt.kind}
			if got := err.Retryable(); got != tt.want {
				t.Errorf("Retryable() = %v, want %v", got, tt.want)
			}
			wrapped := fmt.Errorf("turn 3: %w", err)
			if got := llm.IsRetryable(wrapped); got != tt.want {
				t.Errorf("IsRetryable(wrapped) = %v, want %v", got, tt.want)
			}
			if kind, ok := llm.KindOf(wrapped); !ok || kind != tt.kind {
				t.Errorf("KindOf(wrapped) = %q, %v", kind, ok)
			}
		})
	}
	if llm.IsRetryable(io.ErrUnexpectedEOF) || llm.IsRetryable(nil) || llm.IsRetryable(context.Canceled) {
		t.Error("IsRetryable is true for an error that is not an *llm.Error")
	}
	if _, ok := llm.KindOf(io.EOF); ok {
		t.Error("KindOf found a kind in a plain error")
	}
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  *llm.Error
		want string
	}{
		{"full", &llm.Error{Kind: llm.ErrRateLimit, Provider: "anthropic", Status: 429, Message: "slow down"}, "anthropic: rate_limit (HTTP 429): slow down"},
		{"cause is named by class, not quoted", (&llm.Error{Kind: llm.ErrUnavailable, Provider: "ollama"}).WithCause(io.ErrUnexpectedEOF), "ollama: unavailable: connection closed early"},
		{"timeout cause", (&llm.Error{Kind: llm.ErrUnavailable, Provider: "p"}).WithCause(fmt.Errorf("dial: %w", context.DeadlineExceeded)), "p: unavailable: timed out"},
		{"other cause", (&llm.Error{Kind: llm.ErrUnavailable}).WithCause(errors.New("dial tcp 10.0.0.1: refused")), "unavailable: network or transport failure"},
		{"token in a message is masked", &llm.Error{Kind: llm.ErrAuth, Message: "bad key sk-ant-" + strings.Repeat("a", 30)}, "auth: bad key [redacted]"},
		{"message wins over cause", (&llm.Error{Kind: llm.ErrMalformed, Provider: "gemini", Message: "no candidates"}).WithCause(io.EOF), "gemini: malformed: no candidates"},
		{"kind only", &llm.Error{Kind: llm.ErrRefusal}, "refusal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrorUnwrapAndIs(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("chat: %w", (&llm.Error{Kind: llm.ErrUnavailable, Provider: "ollama", Status: 502}).WithCause(io.ErrUnexpectedEOF))

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error("errors.Is does not reach the underlying error")
	}
	tests := []struct {
		name   string
		target *llm.Error
		want   bool
	}{
		{"same kind", &llm.Error{Kind: llm.ErrUnavailable}, true},
		{"other kind", &llm.Error{Kind: llm.ErrAuth}, false},
		{"kind and provider", &llm.Error{Kind: llm.ErrUnavailable, Provider: "ollama"}, true},
		{"kind and other provider", &llm.Error{Kind: llm.ErrUnavailable, Provider: "gemini"}, false},
		{"status", &llm.Error{Status: 502}, true},
		{"other status", &llm.Error{Status: 500}, false},
		{"any llm error", &llm.Error{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := errors.Is(err, tt.target); got != tt.want {
				t.Errorf("errors.Is = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKindFromStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   llm.ErrorKind
	}{
		{400, llm.ErrBadRequest},
		{401, llm.ErrAuth},
		{403, llm.ErrAuth},
		{404, llm.ErrBadRequest},
		{408, llm.ErrUnavailable},
		{413, llm.ErrContextTooLong},
		{422, llm.ErrBadRequest},
		{429, llm.ErrRateLimit},
		{500, llm.ErrUnavailable},
		{502, llm.ErrUnavailable},
		{503, llm.ErrOverloaded},
		{504, llm.ErrUnavailable},
		{529, llm.ErrOverloaded},
	}
	for _, tt := range tests {
		if got := llm.KindFromStatus(tt.status); got != tt.want {
			t.Errorf("KindFromStatus(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"7", 7 * time.Second},
		{" 30 ", 30 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
		{"1e30", 0},
		{"Tue, 10 Nov 2026 12:00:20 GMT", 20 * time.Second},
		{"Tue, 10 Nov 2026 11:59:00 GMT", 0},
	}
	for _, tt := range tests {
		if got := llm.ParseRetryAfter(tt.header, now); got != tt.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

// TestErrorNeverPrintsItsCause uses the realistic leak: net/http wraps a
// failed request in a url.Error, whose text is the full URL, and the Gemini
// and some OpenAI-compatible APIs accept the key as a query parameter.
func TestErrorNeverPrintsItsCause(t *testing.T) {
	t.Parallel()
	const key = "QUERYKEY0123456789abcdef"
	transport := &url.Error{Op: "Get", URL: "https://api.example.com/v1/models?key=" + key, Err: errors.New("connection refused by " + key)}
	e := (&llm.Error{Kind: llm.ErrUnavailable, Provider: "gemini", RequestID: "req_1"}).WithCause(transport)
	type holder struct {
		Public  *llm.Error
		private llm.Error
		ptr     *llm.Error
		iface   error
	}
	h := holder{Public: e, private: *e, ptr: e, iface: e}

	outputs := map[string]string{
		"Error()":       e.Error(),
		"wrapped":       fmt.Errorf("list models: %w", e).Error(),
		"SafeText":      llm.SafeText(fmt.Errorf("list models: %w", e)),
		"SafeText bare": llm.SafeText(transport),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		outputs[verb+" pointer"] = fmt.Sprintf(verb, e)
		outputs[verb+" value"] = fmt.Sprintf(verb, *e)
		outputs[verb+" holder"] = fmt.Sprintf(verb, h)
		outputs[verb+" holder pointer"] = fmt.Sprintf(verb, &h)
	}
	for name, v := range map[string]any{"pointer": e, "value": *e, "holder": h, "slice": []*llm.Error{e}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		outputs["json "+name] = string(raw)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Error("m", "err", e, "value", *e, "holder", h)
	slog.New(slog.NewTextHandler(&logged, nil)).Error("m", "err", e, "value", *e, "holder", h)
	outputs["slog"] = logged.String()

	for name, got := range outputs {
		if strings.Contains(got, key) || strings.Contains(got, "api.example.com") {
			t.Errorf("%s exposes the cause: %s", name, got)
		}
	}

	// The cause is still there for code that asks for it.
	if !errors.Is(e, transport) {
		t.Error("errors.Is does not find the cause")
	}
	if got, ok := errors.AsType[*url.Error](fmt.Errorf("x: %w", e)); !ok || got != transport {
		t.Error("errors.As does not find the cause")
	}
	if e.WithCause(nil).Unwrap() != nil {
		t.Error("WithCause(nil) did not clear the cause")
	}

	// JSON carries the sanitized fields.
	raw, _ := json.Marshal((&llm.Error{Kind: llm.ErrRateLimit, Provider: "p", Status: 429, RetryAfter: 1500 * time.Millisecond, Message: "slow down", RequestID: "r"}).WithCause(transport))
	const want = `{"kind":"rate_limit","provider":"p","status":429,"retry_after_ms":1500,"message":"slow down","request_id":"r"}`
	if string(raw) != want {
		t.Errorf("JSON = %s, want %s", raw, want)
	}
}

func TestSafeText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"llm error", fmt.Errorf("gave up: %w", &llm.Error{Kind: llm.ErrAuth, Provider: "p", Status: 401, Message: "invalid key"}), "p: auth (HTTP 401): invalid key"},
		{"no adapter", fmt.Errorf("provider x: %w", llm.ErrNoAdapter), "no adapter for this provider kind"},
		{"unknown provider", fmt.Errorf("%w %q", llm.ErrUnknownProvider, "x"), "unknown provider"},
		{"cancelled", fmt.Errorf("x: %w", context.Canceled), "cancelled"},
		{"foreign error", errors.New("open /home/u/.netrc: password hunter2"), "the provider could not be reached or set up"},
	}
	for _, tt := range tests {
		if got := llm.SafeText(tt.err); got != tt.want {
			t.Errorf("%s: SafeText = %q, want %q", tt.name, got, tt.want)
		}
	}
}
