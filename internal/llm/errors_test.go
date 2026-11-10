package llm_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		{"no status", &llm.Error{Kind: llm.ErrUnavailable, Provider: "ollama", Err: io.ErrUnexpectedEOF}, "ollama: unavailable: unexpected EOF"},
		{"message wins over cause", &llm.Error{Kind: llm.ErrMalformed, Provider: "gemini", Message: "no candidates", Err: io.EOF}, "gemini: malformed: no candidates"},
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
	err := fmt.Errorf("chat: %w", &llm.Error{Kind: llm.ErrUnavailable, Provider: "ollama", Status: 502, Err: io.ErrUnexpectedEOF})

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
