package llm_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
)

// listerFixture is a registry with one fake provider "p" and a lister on a
// fake clock.
type listerFixture struct {
	provider *fake.Provider
	clock    *fakeClock
	lister   *llm.ModelLister
}

func newListerFixture(t *testing.T, cfg llm.ProviderConfig) *listerFixture {
	t.Helper()
	built := fakes{}
	cfg.Name, cfg.Kind = "p", fake.Kind
	clock := newFakeClock()
	// One attempt, so a failing live list is asked exactly once per List.
	reg, err := llm.NewRegistry(built.constructors(), []llm.ProviderConfig{cfg, {Name: "noadapter", Kind: "anthropic", Models: []string{"cfg-only"}}},
		llm.RegistryOptions{Clock: clock, Retry: llm.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return &listerFixture{provider: built["p"], clock: clock, lister: llm.NewModelLister(reg, clock)}
}

func live(id string) llm.ModelInfo { return llm.ModelInfo{ID: id, Source: llm.SourceLive} }
func conf(id string) llm.ModelInfo { return llm.ModelInfo{ID: id, Source: llm.SourceConfig} }

func TestModelListerMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cfg         llm.ProviderConfig
		live        []llm.ModelInfo
		liveErr     error
		want        []llm.ModelInfo
		wantLive    bool
		wantWarning string
		wantErr     string
	}{
		{
			name:     "live only",
			live:     []llm.ModelInfo{{ID: "m1", DisplayName: "Model One", ContextWindow: 1000}, {ID: "m2"}},
			want:     []llm.ModelInfo{{ID: "m1", DisplayName: "Model One", ContextWindow: 1000, Source: llm.SourceLive}, live("m2")},
			wantLive: true,
		},
		{
			name:     "live first, then config entries the live list lacks",
			cfg:      llm.ProviderConfig{Models: []string{"c1", "m2", "c2"}},
			live:     []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}},
			want:     []llm.ModelInfo{live("m1"), live("m2"), conf("c1"), conf("c2")},
			wantLive: true,
		},
		{
			name:     "default model is part of the config list",
			cfg:      llm.ProviderConfig{Models: []string{"c1"}, DefaultModel: "d1"},
			live:     []llm.ModelInfo{{ID: "m1"}},
			want:     []llm.ModelInfo{live("m1"), conf("c1"), conf("d1")},
			wantLive: true,
		},
		{
			name:     "live list is cleaned: no empty IDs, no repeats, source forced to live",
			cfg:      llm.ProviderConfig{Models: []string{"c1", "c1", ""}},
			live:     []llm.ModelInfo{{ID: "m1", Source: llm.SourceConfig}, {ID: ""}, {ID: "m1", DisplayName: "again"}},
			want:     []llm.ModelInfo{live("m1"), conf("c1")},
			wantLive: true,
		},
		{
			name:        "live fails: config list with a warning",
			cfg:         llm.ProviderConfig{Models: []string{"c1", "c2"}, DefaultModel: "c2"},
			liveErr:     &llm.Error{Kind: llm.ErrAuth, Provider: "p", Status: 401, Message: "invalid x-api-key"},
			want:        []llm.ModelInfo{conf("c1"), conf("c2")},
			wantWarning: `could not fetch the live model list of "p", showing the models from the config: p: auth (HTTP 401): invalid x-api-key`,
		},
		{
			name:        "live list is empty: config list with a warning",
			cfg:         llm.ProviderConfig{Models: []string{"c1"}},
			live:        []llm.ModelInfo{},
			want:        []llm.ModelInfo{conf("c1")},
			wantWarning: "the provider returned an empty model list",
		},
		{
			name:    "live fails and the config lists nothing",
			liveErr: &llm.Error{Kind: llm.ErrUnavailable, Provider: "p"},
			wantErr: `list models of "p": the live list failed and the config lists none: gave up after 1 attempts: p: unavailable`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newListerFixture(t, tt.cfg)
			f.provider.SetModels(tt.live, tt.liveErr)

			got, err := f.lister.List(context.Background(), "p")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("List error = %v, want it to contain %q", err, tt.wantErr)
				}
				if !errors.Is(err, &llm.Error{Kind: llm.ErrUnavailable}) {
					t.Error("the provider error is not in the chain")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got.Models); diff != "" {
				t.Errorf("models (-want +got):\n%s", diff)
			}
			if got.Live != tt.wantLive {
				t.Errorf("Live = %v, want %v", got.Live, tt.wantLive)
			}
			if !strings.Contains(got.Warning, tt.wantWarning) || (tt.wantWarning == "") != (got.Warning == "") {
				t.Errorf("Warning = %q, want it to contain %q", got.Warning, tt.wantWarning)
			}
			if got.Live != !got.FetchedAt.IsZero() {
				t.Errorf("FetchedAt = %v with Live = %v", got.FetchedAt, got.Live)
			}
		})
	}
}

// TestModelListerWarningIsSafe checks that the warning, which the UI shows
// next to the model picker, never quotes a transport error.
func TestModelListerWarningIsSafe(t *testing.T) {
	t.Parallel()
	const key = "QUERYKEY0123456789abcdef"
	transport := &url.Error{Op: "Get", URL: "https://api.example.com/v1/models?key=" + key, Err: errors.New("refused")}
	tests := []struct {
		name    string
		liveErr error
		want    string
	}{
		{"llm error with a transport cause", (&llm.Error{Kind: llm.ErrUnavailable, Provider: "p"}).WithCause(transport), "p: unavailable: network or transport failure"},
		{"bare transport error from a careless adapter", transport, "the provider could not be reached or set up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newListerFixture(t, llm.ProviderConfig{Models: []string{"c1"}})
			f.provider.SetModels(nil, tt.liveErr)
			got, err := f.lister.List(context.Background(), "p")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got.Warning, key) || strings.Contains(got.Warning, "api.example.com") {
				t.Errorf("the warning exposes the request URL: %s", got.Warning)
			}
			if !strings.Contains(got.Warning, tt.want) {
				t.Errorf("Warning = %q, want it to contain %q", got.Warning, tt.want)
			}
		})
	}
}

func TestModelListerCache(t *testing.T) {
	t.Parallel()
	f := newListerFixture(t, llm.ProviderConfig{Models: []string{"c1"}})
	ctx := context.Background()
	list := func() llm.ModelList {
		t.Helper()
		got, err := f.lister.List(ctx, "p")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ids := func(l llm.ModelList) string {
		var out []string
		for _, m := range l.Models {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}

	f.provider.SetModels([]llm.ModelInfo{{ID: "m1"}}, nil)
	first := list()
	if ids(first) != "m1,c1" || f.provider.ListCalls() != 1 {
		t.Fatalf("first list = %s after %d calls", ids(first), f.provider.ListCalls())
	}

	// Within the TTL the provider is not asked again, even though its list changed.
	f.provider.SetModels([]llm.ModelInfo{{ID: "m2"}}, nil)
	f.clock.Advance(llm.ModelCacheTTL - time.Second)
	cached := list()
	if ids(cached) != "m1,c1" || f.provider.ListCalls() != 1 {
		t.Errorf("cached list = %s after %d calls; want the cached list and no new call", ids(cached), f.provider.ListCalls())
	}
	if !cached.FetchedAt.Equal(first.FetchedAt) || !cached.Live {
		t.Errorf("cached list FetchedAt = %v, Live = %v", cached.FetchedAt, cached.Live)
	}

	// A caller that edits the result does not damage the cache.
	cached.Models[0].ID = "changed"
	if got := list(); ids(got) != "m1,c1" {
		t.Errorf("list after the caller edited a result = %s", ids(got))
	}

	// At the TTL the list is fetched again.
	f.clock.Advance(time.Second)
	if got := list(); ids(got) != "m2,c1" || f.provider.ListCalls() != 2 {
		t.Errorf("list after the TTL = %s after %d calls", ids(got), f.provider.ListCalls())
	}

	// A failure after the TTL falls back to the config and is not cached.
	f.clock.Advance(llm.ModelCacheTTL)
	f.provider.SetModels(nil, &llm.Error{Kind: llm.ErrUnavailable, Provider: "p"})
	if got := list(); ids(got) != "c1" || got.Warning == "" || got.Live {
		t.Errorf("list during an outage = %s, warning %q", ids(got), got.Warning)
	}
	f.provider.SetModels([]llm.ModelInfo{{ID: "m3"}}, nil)
	if got := list(); ids(got) != "m3,c1" || got.Warning != "" {
		t.Errorf("list after the outage = %s, warning %q; want a fresh live list at once", ids(got), got.Warning)
	}

	// Invalidate forces a fetch inside the TTL.
	calls := f.provider.ListCalls()
	f.lister.Invalidate("p")
	list()
	f.lister.Invalidate("")
	list()
	if f.provider.ListCalls() != calls+2 {
		t.Errorf("%d fetches after two invalidations, want 2", f.provider.ListCalls()-calls)
	}
}

func TestModelListerErrors(t *testing.T) {
	t.Parallel()
	f := newListerFixture(t, llm.ProviderConfig{Models: []string{"c1"}})

	if _, err := f.lister.List(context.Background(), "nope"); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Errorf("List of an unknown provider = %v", err)
	}

	// A provider with no adapter still shows its config models.
	got, err := f.lister.List(context.Background(), "noadapter")
	if err != nil || len(got.Models) != 1 || got.Models[0] != conf("cfg-only") || !strings.Contains(got.Warning, "no adapter") {
		t.Errorf("List of a provider with no adapter = %+v, %v", got, err)
	}

	// A done context is an error, not a reason to fall back.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.lister.List(ctx, "p"); !errors.Is(err, context.Canceled) {
		t.Errorf("List on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestModelListerConcurrentUse(t *testing.T) {
	t.Parallel()
	f := newListerFixture(t, llm.ProviderConfig{Models: []string{"c1"}})
	f.provider.SetModels([]llm.ModelInfo{{ID: "m1"}}, nil)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 50 {
				if i%4 == 0 {
					f.lister.Invalidate("p")
				}
				got, err := f.lister.List(context.Background(), "p")
				if err != nil || len(got.Models) != 2 {
					t.Errorf("List = %+v, %v", got, err)
					return
				}
				got.Models[0].ID = "scribble"
			}
		})
	}
	wg.Wait()
}

func TestNewModelListerDefaultClock(t *testing.T) {
	t.Parallel()
	built := fakes{}
	reg, err := llm.NewRegistry(built.constructors(), []llm.ProviderConfig{{Name: "p", Kind: fake.Kind}}, llm.RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	built["p"].SetModels([]llm.ModelInfo{{ID: "m1"}}, nil)
	got, err := llm.NewModelLister(reg, nil).List(context.Background(), "p")
	if err != nil || !got.Live || time.Since(got.FetchedAt) > time.Minute {
		t.Errorf("List with the system clock = %+v, %v", got, err)
	}
}

// slowLister is a Provider whose ListModels blocks until the test lets it
// finish, to hold a fetch open across an Invalidate.
type slowLister struct {
	started chan struct{}
	release chan struct{}

	mu     sync.Mutex
	models []llm.ModelInfo
	calls  int
}

func (s *slowLister) Name() string { return "slow" }

func (s *slowLister) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return nil, errors.New("not used")
}

func (s *slowLister) set(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = nil
	for _, id := range ids {
		s.models = append(s.models, llm.ModelInfo{ID: id})
	}
}

func (s *slowLister) ListModels(context.Context) ([]llm.ModelInfo, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	answer := append([]llm.ModelInfo(nil), s.models...) // what the provider had when asked
	s.mu.Unlock()
	if first {
		close(s.started)
		<-s.release
	}
	return answer, nil
}

// TestInvalidateDuringFetch: a fetch is in flight, the caller invalidates
// (say, after changing the API key), and the old fetch then finishes. Its
// result must not go into the cache, or the next List would serve the list
// the caller just declared stale for a full TTL.
func TestInvalidateDuringFetch(t *testing.T) {
	t.Parallel()
	slow := &slowLister{started: make(chan struct{}), release: make(chan struct{})}
	slow.set("old")
	reg, err := llm.NewRegistry(
		llm.Constructors{"slow": func(llm.ProviderConfig) (llm.Provider, error) { return slow, nil }},
		[]llm.ProviderConfig{{Name: "slow", Kind: "slow"}}, llm.RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lister := llm.NewModelLister(reg, newFakeClock())

	firstDone := make(chan llm.ModelList, 1)
	go func() {
		got, err := lister.List(context.Background(), "slow")
		if err != nil {
			t.Errorf("first List: %v", err)
		}
		firstDone <- got
	}()
	<-slow.started

	slow.set("new")
	lister.Invalidate("slow")
	close(slow.release)
	if first := <-firstDone; len(first.Models) != 1 || first.Models[0].ID != "old" {
		t.Errorf("the in-flight List = %+v; it should still return what it fetched", first.Models)
	}

	got, err := lister.List(context.Background(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "new" {
		t.Errorf("List after Invalidate = %+v, want the new list; the superseded fetch filled the cache", got.Models)
	}
	// The fresh result is cached as usual.
	if _, err := lister.List(context.Background(), "slow"); err != nil {
		t.Fatal(err)
	}
	slow.mu.Lock()
	calls := slow.calls
	slow.mu.Unlock()
	if calls != 2 {
		t.Errorf("%d fetches, want 2: one superseded, one fresh and then cached", calls)
	}
}
