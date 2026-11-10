package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ModelCacheTTL is how long a live model list is reused.
const ModelCacheTTL = 5 * time.Minute

// ModelList is the answer of ModelLister.List.
type ModelList struct {
	// Models is the live list followed by config entries the live list lacks.
	Models []ModelInfo
	// Live reports whether the provider's own list is included.
	Live bool
	// Warning is set when the live list could not be fetched and Models is
	// the config list alone. It is safe to show to the user.
	Warning string
	// FetchedAt is when the live part was fetched; zero when Live is false.
	FetchedAt time.Time
}

// ModelLister lists the models of a provider: the live list from the
// provider's API, cached for a while, merged with the config list. When the
// live list fails it falls back to the config list and says so. It is safe
// for concurrent use.
type ModelLister struct {
	registry *Registry
	clock    Clock
	ttl      time.Duration

	mu    sync.Mutex
	cache map[string]cachedModels
}

type cachedModels struct {
	models    []ModelInfo
	fetchedAt time.Time
}

// NewModelLister returns a lister over the registry's providers. A nil clock
// means the system clock.
func NewModelLister(registry *Registry, clock Clock) *ModelLister {
	if clock == nil {
		clock = SystemClock{}
	}
	return &ModelLister{registry: registry, clock: clock, ttl: ModelCacheTTL, cache: map[string]cachedModels{}}
}

// List returns the models of a provider. It fails only when the provider is
// unknown, the context is done, or there is neither a live nor a config list.
func (l *ModelLister) List(ctx context.Context, provider string) (ModelList, error) {
	cfg, ok := l.registry.Config(provider)
	if !ok {
		return ModelList{}, fmt.Errorf("%w %q (configured: %s)", ErrUnknownProvider, provider, orNone(l.registry.Names()))
	}
	fromConfig := configModels(cfg)

	if cached, ok := l.cached(provider); ok {
		return ModelList{Models: mergeModels(cached.models, fromConfig), Live: true, FetchedAt: cached.fetchedAt}, nil
	}

	live, err := l.fetch(ctx, provider)
	if err == nil {
		fetchedAt := l.clock.Now()
		l.mu.Lock()
		l.cache[provider] = cachedModels{models: live, fetchedAt: fetchedAt}
		l.mu.Unlock()
		return ModelList{Models: mergeModels(live, fromConfig), Live: true, FetchedAt: fetchedAt}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ModelList{}, ctxErr
	}
	if len(fromConfig) == 0 {
		return ModelList{}, fmt.Errorf("list models of %q: the live list failed and the config lists none: %w", provider, err)
	}
	return ModelList{
		Models:  fromConfig,
		Warning: fmt.Sprintf("could not fetch the live model list of %q, showing the models from the config: %v", provider, err),
	}, nil
}

// Invalidate drops the cached list of a provider, or of every provider when
// the name is empty.
func (l *ModelLister) Invalidate(provider string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if provider == "" {
		clear(l.cache)
		return
	}
	delete(l.cache, provider)
}

func (l *ModelLister) cached(provider string) (cachedModels, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.cache[provider]
	if !ok || l.clock.Now().Sub(entry.fetchedAt) >= l.ttl {
		return cachedModels{}, false
	}
	return entry, true
}

// fetch gets the live list and normalizes it: entries without an ID and
// repeated IDs are dropped, and every entry is marked live.
func (l *ModelLister) fetch(ctx context.Context, provider string) ([]ModelInfo, error) {
	p, err := l.registry.Provider(provider)
	if err != nil {
		return nil, err
	}
	raw, err := p.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(raw))
	live := make([]ModelInfo, 0, len(raw))
	for _, m := range raw {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		m.Source = SourceLive
		live = append(live, m)
	}
	if len(live) == 0 {
		return nil, errors.New("the provider returned an empty model list")
	}
	return live, nil
}

// configModels returns the config's model IDs, with the default model first
// in line if the list does not already have it.
func configModels(cfg ProviderConfig) []ModelInfo {
	ids := slices.Clone(cfg.Models)
	if cfg.DefaultModel != "" && !slices.Contains(ids, cfg.DefaultModel) {
		ids = append(ids, cfg.DefaultModel)
	}
	out := make([]ModelInfo, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, Source: SourceConfig})
	}
	return out
}

// mergeModels returns the live list followed by the config entries it lacks.
// The result shares no memory with the cache.
func mergeModels(live, fromConfig []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(live)+len(fromConfig))
	seen := make(map[string]bool, len(live))
	for _, m := range live {
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range fromConfig {
		if !seen[m.ID] {
			out = append(out, m)
		}
	}
	return out
}
