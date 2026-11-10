package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
)

func TestFamilyModels(t *testing.T) {
	t.Parallel()
	for _, f := range families() {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			response := readFixture(t, f.name+"_models.resp.json")
			server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != f.prefix+"/models" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "" || r.ContentLength != 0 {
					t.Error("incorrect models request")
				}
				writeBody(t, w, response)
			}))
			client := server.Client()
			p, err := NewWithOptions(llm.ProviderConfig{Name: f.name, BaseURL: server.URL + f.prefix}, Options{HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.ListModels(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []llm.ModelInfo{{ID: "fixture-model", Source: llm.SourceLive}}
			if f.name == "openrouter" {
				want[0].DisplayName, want[0].ContextWindow = "Fixture Model", 8192
			}
			if f.name == "groq" {
				want[0].ContextWindow = 4096
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestModelPagination(t *testing.T) {
	t.Parallel()
	first, second := readFixture(t, "models_page1.resp.json"), readFixture(t, "models_page2.resp.json")
	var calls atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		step := calls.Add(1)
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("incorrect pagination endpoint or authentication")
		}
		if step == 1 {
			if r.URL.RawQuery != "" {
				t.Error("unexpected initial query")
			}
			writeBody(t, w, first)
		} else {
			if diff := cmp.Diff(url.Values{"after": {"first"}}, r.URL.Query()); diff != "" {
				t.Error(diff)
			}
			writeBody(t, w, second)
		}
	})
	got, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []llm.ModelInfo{{ID: "first", DisplayName: "First", ContextWindow: 4096, Source: llm.SourceLive}, {ID: "second", ContextWindow: 8192, Source: llm.SourceLive}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
	if calls.Load() != 2 {
		t.Errorf("requests = %d, want 2", calls.Load())
	}
}

func TestInvalidModelPages(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, response string
		calls          int32
	}{
		{"missing data", `{}`, 1},
		{"null data", `{"data":null}`, 1},
		{"negative context", `{"data":[{"id":"a","context_length":-1}]}`, 1},
		{"no cursor", `{"data":[{"id":"a"}],"has_more":true}`, 1},
		{"empty page", `{"data":[],"has_more":true,"last_id":"a"}`, 1},
		{"repeated cursor", `{"data":[{"id":"a"}],"has_more":true,"last_id":"a"}`, 2},
		{"malformed JSON", `{"data":[`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			p := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeBody(t, w, []byte(tt.response))
			})
			_, err := p.ListModels(context.Background())
			_ = assertKind(t, err, llm.ErrMalformed)
			if calls.Load() != tt.calls {
				t.Errorf("requests = %d, want %d", calls.Load(), tt.calls)
			}
		})
	}
}

func TestModelPaginationBoundAndCursorEscaping(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		step := calls.Add(1)
		if step > 1 {
			want := fmt.Sprintf("cursor %d&token=synthetic", step-1)
			if diff := cmp.Diff(url.Values{"after": {want}}, r.URL.Query()); diff != "" {
				t.Error(diff)
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": fmt.Sprint(step)}}, "has_more": true, "last_id": fmt.Sprintf("cursor %d&token=synthetic", step)}); err != nil {
			t.Error(err)
		}
	})
	_, err := p.ListModels(context.Background())
	_ = assertKind(t, err, llm.ErrMalformed)
	if calls.Load() != 100 {
		t.Errorf("requests = %d, want pagination cap", calls.Load())
	}
}
