package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Clock is the time source for retries and caches. Tests replace it.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx is done, and then returns ctx's error.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryPolicy says how WithRetry retries. Zero fields take the defaults of
// DefaultRetryPolicy.
type RetryPolicy struct {
	// MaxAttempts counts the first try. The default is 4.
	MaxAttempts int
	// BaseDelay is the backoff ceiling before the second attempt. It is
	// multiplied by Factor for each later one and never exceeds MaxDelay.
	BaseDelay time.Duration
	Factor    float64
	MaxDelay  time.Duration
	// MaxRetryAfter is the longest wait a provider may ask for. When
	// RetryAfter is longer, the error is returned at once: a run should fail
	// with a clear message, not sleep for an hour.
	MaxRetryAfter time.Duration
	// Jitter returns a random duration in [0, ceiling]. The default is
	// uniform, which is "full jitter". Tests replace it.
	Jitter func(ceiling time.Duration) time.Duration
	// OnRetry, if set, is called before each wait.
	OnRetry func(RetryInfo)
}

// RetryInfo describes one retry that is about to happen.
type RetryInfo struct {
	Provider string
	// Attempt is the attempt that just failed, starting at 1.
	Attempt int
	Delay   time.Duration
	Err     error
}

// DefaultRetryPolicy returns the policy from the plan: 4 attempts, 1s base,
// factor 2, 30s cap, full jitter.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   4,
		BaseDelay:     time.Second,
		Factor:        2,
		MaxDelay:      30 * time.Second,
		MaxRetryAfter: 2 * time.Minute,
		Jitter:        fullJitter,
	}
}

func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return rand.N(ceiling + 1) //nolint:gosec // backoff jitter needs no cryptographic randomness
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	d := DefaultRetryPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = d.MaxAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = d.BaseDelay
	}
	if p.Factor < 1 {
		p.Factor = d.Factor
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = d.MaxDelay
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = d.MaxRetryAfter
	}
	if p.Jitter == nil {
		p.Jitter = d.Jitter
	}
	return p
}

// delay returns how long to wait after the given failed attempt.
func (p RetryPolicy) delay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter // the provider knows best; no jitter is taken off
	}
	ceiling := float64(p.BaseDelay)
	for range attempt - 1 {
		ceiling *= p.Factor
		if ceiling >= float64(p.MaxDelay) {
			break
		}
	}
	return min(max(p.Jitter(time.Duration(min(ceiling, float64(p.MaxDelay)))), 0), p.MaxDelay)
}

// WithRetry wraps a provider so that Chat and ListModels retry failures that
// are worth retrying: rate limits, overload and unavailability. Every other
// error, and a done context, is returned at once. A nil clock means the
// system clock.
//
// An adapter wrapped by WithRetry must turn off any retries of its own, so
// that there is exactly one retry policy.
func WithRetry(p Provider, policy RetryPolicy, clock Clock) Provider {
	if clock == nil {
		clock = SystemClock{}
	}
	return &retrier{inner: p, policy: policy.withDefaults(), clock: clock}
}

type retrier struct {
	inner  Provider
	policy RetryPolicy
	clock  Clock
}

func (r *retrier) Name() string { return r.inner.Name() }

func (r *retrier) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return retry(ctx, r, func(ctx context.Context) (*ChatResponse, error) {
		return r.inner.Chat(ctx, req)
	})
}

func (r *retrier) ListModels(ctx context.Context) ([]ModelInfo, error) {
	return retry(ctx, r, r.inner.ListModels)
}

// Unwrap returns the wrapped provider.
func (r *retrier) Unwrap() Provider { return r.inner }

func retry[T any](ctx context.Context, r *retrier, call func(context.Context) (T, error)) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := call(ctx)
		if err == nil {
			return result, nil
		}
		// A done context explains the failure better than whatever the
		// transport made of it, and must never be retried.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		perr, ok := errors.AsType[*Error](err)
		if !ok || !perr.Retryable() {
			return zero, err
		}
		if attempt >= r.policy.MaxAttempts {
			return zero, fmt.Errorf("gave up after %d attempts: %w", attempt, err)
		}
		if perr.RetryAfter > r.policy.MaxRetryAfter {
			return zero, fmt.Errorf("provider asked to wait %s, more than the %s allowed: %w",
				perr.RetryAfter.Round(time.Second), r.policy.MaxRetryAfter, err)
		}
		delay := r.policy.delay(attempt, perr.RetryAfter)
		if r.policy.OnRetry != nil {
			r.policy.OnRetry(RetryInfo{Provider: r.inner.Name(), Attempt: attempt, Delay: delay, Err: err})
		}
		if err := r.clock.Sleep(ctx, delay); err != nil {
			return zero, err
		}
	}
}
