package anthropic_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/anthropic"
	"github.com/aryanpappu/patchbot/internal/secret"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type deadlineBody struct{ ctx context.Context }

func (b deadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (deadlineBody) Close() error               { return nil }

var _ io.ReadCloser = deadlineBody{}

func TestAdapterDeadlineRetriesAndCallerCancellationDoesNot(t *testing.T) {
	for _, models := range []bool{false, true} {
		for _, body := range []bool{false, true} {
			for _, caller := range []string{"alive", "cancel", "deadline"} {
				t.Run(map[bool]string{true: "models", false: "chat"}[models]+"/"+map[bool]string{true: "body", false: "transport"}[body]+"/"+caller, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ctx := context.Background()
						if caller == "deadline" {
							var cancel context.CancelFunc
							ctx, cancel = context.WithTimeout(ctx, time.Second)
							defer cancel()
						}
						if caller == "cancel" {
							var cancel context.CancelFunc
							ctx, cancel = context.WithCancel(ctx)
							time.AfterFunc(time.Second, cancel)
							defer cancel()
						}
						calls := 0
						client := &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
							calls++
							if body {
								return &http.Response{StatusCode: 200, Header: make(http.Header), Body: deadlineBody{r.Context()}, Request: r}, nil
							}
							<-r.Context().Done()
							return nil, r.Context().Err()
						})}
						p, err := anthropic.NewWithOptions(llm.ProviderConfig{Name: "test", APIKey: secret.New("dummy-test-key")}, anthropic.Options{HTTPClient: client})
						if err != nil {
							t.Fatal(err)
						}
						wrapped := llm.WithRetry(p, llm.RetryPolicy{MaxAttempts: 2, Jitter: func(time.Duration) time.Duration { return 0 }}, nil)
						if models {
							_, err = wrapped.ListModels(ctx)
						} else {
							_, err = wrapped.Chat(ctx, request())
						}
						if caller == "alive" {
							if kind, _ := llm.KindOf(err); kind != llm.ErrUnavailable || !llm.IsRetryable(err) || calls != 2 || ctx.Err() != nil {
								t.Fatalf("adapter deadline: %v calls=%d caller=%v", err, calls, ctx.Err())
							}
						} else if !errors.Is(err, ctx.Err()) || llm.IsRetryable(err) || calls != 1 {
							t.Fatalf("caller cancellation: %v calls=%d", err, calls)
						}
					})
				})
			}
		}
	}
}
