package llm_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
)

var chatReq = llm.ChatRequest{Model: "m", MaxTokens: 16, Messages: []llm.Message{llm.UserText("hi")}}

func fail(kind llm.ErrorKind) fake.Step {
	return fake.Step{Err: &llm.Error{Kind: kind, Provider: "p"}}
}

func failAfter(kind llm.ErrorKind, retryAfter time.Duration) fake.Step {
	return fake.Step{Err: &llm.Error{Kind: kind, Provider: "p", RetryAfter: retryAfter}}
}

// maxJitter always picks the ceiling, which shows the backoff schedule.
func maxJitter(ceiling time.Duration) time.Duration { return ceiling }

func TestWithRetry(t *testing.T) {
	t.Parallel()
	ok := fake.Step{Text: "done"}
	const s = time.Second

	tests := []struct {
		name       string
		steps      []fake.Step
		policy     llm.RetryPolicy
		wantCalls  int
		wantSleeps []time.Duration
		wantKind   llm.ErrorKind // "" means success
		wantErr    string
	}{
		{name: "first try succeeds", steps: []fake.Step{ok}, wantCalls: 1},
		{name: "rate limit then success", steps: []fake.Step{fail(llm.ErrRateLimit), ok}, wantCalls: 2, wantSleeps: []time.Duration{1 * s}},
		{name: "overloaded then success", steps: []fake.Step{fail(llm.ErrOverloaded), ok}, wantCalls: 2, wantSleeps: []time.Duration{1 * s}},
		{name: "unavailable then success", steps: []fake.Step{fail(llm.ErrUnavailable), ok}, wantCalls: 2, wantSleeps: []time.Duration{1 * s}},
		{
			name:       "exponential backoff, success on the last attempt",
			steps:      []fake.Step{fail(llm.ErrUnavailable), fail(llm.ErrOverloaded), fail(llm.ErrRateLimit), ok},
			wantCalls:  4,
			wantSleeps: []time.Duration{1 * s, 2 * s, 4 * s},
		},
		{
			name:       "gives up after four attempts",
			steps:      []fake.Step{fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrRateLimit)},
			wantCalls:  4,
			wantSleeps: []time.Duration{1 * s, 2 * s, 4 * s},
			wantKind:   llm.ErrRateLimit,
			wantErr:    "gave up after 4 attempts",
		},
		{name: "auth is never retried", steps: []fake.Step{fail(llm.ErrAuth)}, wantCalls: 1, wantKind: llm.ErrAuth},
		{name: "bad request is never retried", steps: []fake.Step{fail(llm.ErrBadRequest)}, wantCalls: 1, wantKind: llm.ErrBadRequest},
		{name: "context too long is never retried", steps: []fake.Step{fail(llm.ErrContextTooLong)}, wantCalls: 1, wantKind: llm.ErrContextTooLong},
		{name: "malformed is never retried", steps: []fake.Step{fail(llm.ErrMalformed)}, wantCalls: 1, wantKind: llm.ErrMalformed},
		{name: "refusal is never retried", steps: []fake.Step{fail(llm.ErrRefusal)}, wantCalls: 1, wantKind: llm.ErrRefusal},
		{name: "non-retryable after a retry", steps: []fake.Step{fail(llm.ErrOverloaded), fail(llm.ErrAuth)}, wantCalls: 2, wantSleeps: []time.Duration{1 * s}, wantKind: llm.ErrAuth},
		{name: "a plain error is never retried", steps: []fake.Step{{Err: io.ErrUnexpectedEOF}}, wantCalls: 1, wantErr: "unexpected EOF"},
		{
			name:       "retry-after wins over backoff",
			steps:      []fake.Step{failAfter(llm.ErrRateLimit, 17*s), failAfter(llm.ErrRateLimit, 500*time.Millisecond), ok},
			wantCalls:  3,
			wantSleeps: []time.Duration{17 * s, 500 * time.Millisecond},
		},
		{
			name:       "retry-after may exceed the backoff cap",
			steps:      []fake.Step{failAfter(llm.ErrOverloaded, 90*s), ok},
			wantCalls:  2,
			wantSleeps: []time.Duration{90 * s},
		},
		{
			name:      "retry-after beyond the limit fails at once",
			steps:     []fake.Step{failAfter(llm.ErrRateLimit, time.Hour)},
			wantCalls: 1,
			wantKind:  llm.ErrRateLimit,
			wantErr:   "provider asked to wait 1h0m0s, more than the 2m0s allowed",
		},
		{
			name:       "backoff is capped",
			steps:      []fake.Step{fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), ok},
			policy:     llm.RetryPolicy{MaxAttempts: 6, BaseDelay: 10 * s, Factor: 3, MaxDelay: 60 * s},
			wantCalls:  6,
			wantSleeps: []time.Duration{10 * s, 30 * s, 60 * s, 60 * s, 60 * s},
		},
		{
			name:      "one attempt means no retry",
			steps:     []fake.Step{fail(llm.ErrUnavailable)},
			policy:    llm.RetryPolicy{MaxAttempts: 1},
			wantCalls: 1,
			wantKind:  llm.ErrUnavailable,
			wantErr:   "gave up after 1 attempts",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inner := fake.New("p", tt.steps...)
			clock := newFakeClock()
			policy := tt.policy
			policy.Jitter = maxJitter
			var infos []llm.RetryInfo
			policy.OnRetry = func(info llm.RetryInfo) { infos = append(infos, info) }
			p := llm.WithRetry(inner, policy, clock)

			resp, err := p.Chat(context.Background(), chatReq)

			if inner.Calls() != tt.wantCalls {
				t.Errorf("%d calls, want %d", inner.Calls(), tt.wantCalls)
			}
			if diff := cmp.Diff(tt.wantSleeps, clock.Sleeps()); diff != "" {
				t.Errorf("sleeps (-want +got):\n%s", diff)
			}
			if len(infos) != len(tt.wantSleeps) {
				t.Errorf("OnRetry ran %d times, want %d", len(infos), len(tt.wantSleeps))
			}
			for i, info := range infos {
				if info.Attempt != i+1 || info.Delay != tt.wantSleeps[i] || info.Provider != "p" || info.Err == nil {
					t.Errorf("OnRetry call %d = %+v", i, info)
				}
			}
			if tt.wantKind == "" && tt.wantErr == "" {
				if err != nil || resp == nil || resp.Message.Text() != "done" {
					t.Fatalf("Chat = %v, %v; want the scripted answer", resp, err)
				}
				inner.AssertConsumed(t)
				return
			}
			if err == nil {
				t.Fatal("Chat succeeded, want an error")
			}
			if kind, _ := llm.KindOf(err); kind != tt.wantKind {
				t.Errorf("error kind = %q, want %q (%v)", kind, tt.wantKind, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestWithRetryFullJitter(t *testing.T) {
	t.Parallel()
	// The injected random source sees the exponential ceilings, and its
	// answer is the wait.
	var ceilings []time.Duration
	policy := llm.RetryPolicy{Jitter: func(ceiling time.Duration) time.Duration {
		ceilings = append(ceilings, ceiling)
		return ceiling / 4
	}}
	inner := fake.New("p", fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fail(llm.ErrUnavailable), fake.Step{Text: "done"})
	clock := newFakeClock()
	if _, err := llm.WithRetry(inner, policy, clock).Chat(context.Background(), chatReq); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, ceilings); diff != "" {
		t.Errorf("jitter ceilings (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}, clock.Sleeps()); diff != "" {
		t.Errorf("sleeps (-want +got):\n%s", diff)
	}
}

func TestDefaultJitterStaysInRange(t *testing.T) {
	t.Parallel()
	jitter := llm.DefaultRetryPolicy().Jitter
	seen := map[time.Duration]bool{}
	for range 2000 {
		d := jitter(10 * time.Millisecond)
		if d < 0 || d > 10*time.Millisecond {
			t.Fatalf("jitter(10ms) = %v, outside [0, 10ms]", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("jitter produced only %d distinct values in 2000 draws", len(seen))
	}
	if jitter(0) != 0 || jitter(-time.Second) != 0 {
		t.Error("jitter of a non-positive ceiling is not 0")
	}
}

func TestWithRetryStopsOnContext(t *testing.T) {
	t.Parallel()

	t.Run("cancelled before the first call", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		inner := fake.New("p", fake.Step{Text: "never"})
		_, err := llm.WithRetry(inner, llm.RetryPolicy{}, newFakeClock()).Chat(ctx, chatReq)
		if !errors.Is(err, context.Canceled) || inner.Calls() != 0 {
			t.Errorf("err = %v after %d calls; want context.Canceled and no call", err, inner.Calls())
		}
	})

	t.Run("cancelled during the wait", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		clock := newFakeClock()
		clock.onSleep = func(time.Duration) { cancel() }
		inner := fake.New("p", fail(llm.ErrRateLimit), fake.Step{Text: "never"})
		_, err := llm.WithRetry(inner, llm.RetryPolicy{}, clock).Chat(ctx, chatReq)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if inner.Calls() != 1 || len(clock.Sleeps()) != 1 {
			t.Errorf("%d calls and %d sleeps; want 1 and 1", inner.Calls(), len(clock.Sleeps()))
		}
	})

	t.Run("a failure caused by cancellation is not retried", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		inner := fake.NewFunc("p", func(context.Context, int, llm.ChatRequest) (*llm.ChatResponse, error) {
			cancel() // the transport reports a cancelled request as a network error
			return nil, (&llm.Error{Kind: llm.ErrUnavailable, Provider: "p"}).WithCause(context.Canceled)
		})
		clock := newFakeClock()
		_, err := llm.WithRetry(inner, llm.RetryPolicy{}, clock).Chat(ctx, chatReq)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if _, isLLM := llm.KindOf(err); isLLM {
			t.Errorf("err = %v; want the bare context error, not an *llm.Error", err)
		}
		if inner.Calls() != 1 || len(clock.Sleeps()) != 0 {
			t.Errorf("%d calls and %d sleeps; want 1 and 0", inner.Calls(), len(clock.Sleeps()))
		}
	})

	t.Run("deadline during the wait", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		inner := fake.New("p", failAfter(llm.ErrRateLimit, time.Minute), fake.Step{Text: "never"})
		// The real clock: the wait must end at the deadline, not after a minute.
		start := time.Now()
		_, err := llm.WithRetry(inner, llm.RetryPolicy{}, nil).Chat(ctx, chatReq)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("Chat took %v; the wait ignored the deadline", elapsed)
		}
	})
}

func TestWithRetryListModels(t *testing.T) {
	t.Parallel()
	inner := fake.New("p")
	clock := newFakeClock()
	p := llm.WithRetry(inner, llm.RetryPolicy{Jitter: maxJitter}, clock)

	inner.SetModels(nil, &llm.Error{Kind: llm.ErrAuth, Provider: "p"})
	if _, err := p.ListModels(context.Background()); !errors.Is(err, &llm.Error{Kind: llm.ErrAuth}) {
		t.Errorf("ListModels error = %v, want auth", err)
	}
	if inner.ListCalls() != 1 {
		t.Errorf("%d ListModels calls for an auth error, want 1", inner.ListCalls())
	}

	inner.SetModels(nil, &llm.Error{Kind: llm.ErrUnavailable, Provider: "p"})
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Error("ListModels succeeded")
	}
	if inner.ListCalls() != 5 || len(clock.Sleeps()) != 3 {
		t.Errorf("%d ListModels calls and %d sleeps; want 4 more calls and 3 sleeps", inner.ListCalls(), len(clock.Sleeps()))
	}

	inner.SetModels([]llm.ModelInfo{{ID: "m1"}}, nil)
	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("ListModels = %v, %v", models, err)
	}
	if p.Name() != "p" {
		t.Errorf("Name() = %q, want the inner provider's name", p.Name())
	}
}

func TestSystemClockSleep(t *testing.T) {
	t.Parallel()
	var clock llm.SystemClock
	if err := clock.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("Sleep: %v", err)
	}
	if err := clock.Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v", err)
	}
	if err := clock.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep(0) on a cancelled context = %v", err)
	}
	if clock.Now().IsZero() {
		t.Error("Now() is zero")
	}
}
