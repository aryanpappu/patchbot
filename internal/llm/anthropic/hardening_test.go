package anthropic_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/anthropic"
	"github.com/aryanpappu/patchbot/internal/secret"
)

func TestResponseBoundAndRequestDeadline(t *testing.T) {
	for _, status := range []int{200, 400} {
		for _, models := range []bool{false, true} {
			t.Run(strings.Join([]string{http.StatusText(status), map[bool]string{true: "models", false: "chat"}[models]}, "/"), func(t *testing.T) {
				calls := 0
				client := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					if _, ok := req.Context().Deadline(); !ok {
						t.Error("outbound request has no deadline")
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat(" ", 10<<20)), strings.NewReader("oversized"))), Request: req}, nil
				})}
				p, err := anthropic.NewWithOptions(llm.ProviderConfig{Name: "test", APIKey: secret.New("dummy-key")}, anthropic.Options{HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				if models {
					_, err = p.ListModels(t.Context())
				} else {
					_, err = p.Chat(t.Context(), request())
				}
				var failure *llm.Error
				if !errors.As(err, &failure) || failure.Kind != llm.ErrMalformed || calls != 1 {
					t.Fatalf("response: %v, calls %d", err, calls)
				}
			})
		}
	}
}
