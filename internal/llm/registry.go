package llm

import (
	"errors"
	"fmt"
	"slices"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// ProviderConfig is what an adapter needs to build one provider instance. It
// mirrors a [providers.<name>] config table, so this package does not import
// the config package.
type ProviderConfig struct {
	// Name is the instance name: the key used by --provider, the UI and the
	// runs table.
	Name string
	// Kind selects the adapter: "anthropic", "gemini", "openai" or "fake".
	Kind    string
	BaseURL string
	// APIKey may be unset. APIKeyEnv names the variable it came from, so an
	// error can say which variable to set without ever showing a value.
	APIKey    secret.Secret
	APIKeyEnv string
	// Models are the model IDs listed in the config.
	Models       []string
	DefaultModel string
	Pricing      Pricing
}

// Constructor builds a provider from its config. It must not call the
// network. The provider it returns must report cfg.Name as its name and must
// not retry on its own, because the registry wraps it in WithRetry.
type Constructor func(cfg ProviderConfig) (Provider, error)

// Constructors maps a provider kind to its Constructor. This is the hook for
// adapters: the package that wires the program together builds the map (see
// internal/llm/providers), so this package depends on no adapter.
type Constructors map[string]Constructor

// Errors a Registry returns. Match them with errors.Is.
var (
	// ErrUnknownProvider means no provider has the given instance name.
	ErrUnknownProvider = errors.New("unknown provider")
	// ErrNoAdapter means the provider's kind has no adapter in this build.
	ErrNoAdapter = errors.New("no adapter for this provider kind")
)

// RegistryOptions tune NewRegistry. The zero value is ready to use.
type RegistryOptions struct {
	// Retry is the policy every provider is wrapped with.
	Retry RetryPolicy
	// Clock is used for retry waits. Nil means the system clock.
	Clock Clock
}

// Registry holds the configured providers by instance name.
type Registry struct {
	names   []string
	entries map[string]*registryEntry
}

type registryEntry struct {
	cfg      ProviderConfig
	provider Provider
	err      error
}

// NewRegistry builds every provider in configs. A provider that cannot be
// built (its kind has no adapter, or its constructor fails) does not stop the
// others: the error is kept and returned by Provider for that name, so the
// UI can list it as unavailable. NewRegistry itself fails only when the
// configs are inconsistent.
func NewRegistry(constructors Constructors, configs []ProviderConfig, opts RegistryOptions) (*Registry, error) {
	r := &Registry{entries: make(map[string]*registryEntry, len(configs))}
	for _, cfg := range configs {
		if cfg.Name == "" {
			return nil, errors.New("llm: a provider has no name")
		}
		if _, dup := r.entries[cfg.Name]; dup {
			return nil, fmt.Errorf("llm: provider %q is configured twice", cfg.Name)
		}
		cfg.Models = slices.Clone(cfg.Models)
		entry := &registryEntry{cfg: cfg}
		entry.provider, entry.err = build(constructors, cfg, opts)
		r.entries[cfg.Name] = entry
		r.names = append(r.names, cfg.Name)
	}
	slices.Sort(r.names)
	return r, nil
}

func build(constructors Constructors, cfg ProviderConfig, opts RegistryOptions) (Provider, error) {
	construct, ok := constructors[cfg.Kind]
	if !ok || construct == nil {
		return nil, fmt.Errorf("provider %q: kind %q: %w", cfg.Name, cfg.Kind, ErrNoAdapter)
	}
	p, err := construct(cfg)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", cfg.Name, err)
	}
	if p == nil {
		return nil, fmt.Errorf("provider %q: the %s adapter returned no provider", cfg.Name, cfg.Kind)
	}
	if p.Name() != cfg.Name {
		return nil, fmt.Errorf("provider %q: the %s adapter named it %q", cfg.Name, cfg.Kind, p.Name())
	}
	return WithRetry(p, opts.Retry, opts.Clock), nil
}

// Names returns the instance names in sorted order, including providers that
// could not be built.
func (r *Registry) Names() []string { return slices.Clone(r.names) }

// Provider returns the provider with the given instance name, wrapped in the
// retry policy.
func (r *Registry) Provider(name string) (Provider, error) {
	entry, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("%w %q (configured: %s)", ErrUnknownProvider, name, orNone(r.names))
	}
	if entry.err != nil {
		return nil, entry.err
	}
	return entry.provider, nil
}

// Config returns the config of a provider.
func (r *Registry) Config(name string) (ProviderConfig, bool) {
	entry, ok := r.entries[name]
	if !ok {
		return ProviderConfig{}, false
	}
	cfg := entry.cfg
	cfg.Models = slices.Clone(cfg.Models)
	return cfg, true
}

// Pricing returns the price table of a provider. An unknown provider has an
// empty table, so every cost is unknown.
func (r *Registry) Pricing(name string) Pricing {
	if entry, ok := r.entries[name]; ok {
		return entry.cfg.Pricing
	}
	return Pricing{}
}

// ResolveModel returns the model a call should use: the requested one, or the
// provider's default when none was requested. The ID is not checked against
// any list, because local servers and routers accept IDs they do not list; a
// wrong ID fails on the first call with the provider's own message.
func (r *Registry) ResolveModel(provider, requested string) (string, error) {
	entry, ok := r.entries[provider]
	if !ok {
		return "", fmt.Errorf("%w %q (configured: %s)", ErrUnknownProvider, provider, orNone(r.names))
	}
	if requested != "" {
		return requested, nil
	}
	if entry.cfg.DefaultModel != "" {
		return entry.cfg.DefaultModel, nil
	}
	return "", fmt.Errorf("provider %q has no default model: pass --model or set providers.%s.default_model "+
		"(`patchbot models --provider %s` lists the choices)", provider, provider, provider)
}
