package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/secret"
)

// fakes is a Constructors map whose "fake" kind hands out scripted providers
// and remembers them by name.
type fakes map[string]*fake.Provider

func (f fakes) constructors() llm.Constructors {
	return llm.Constructors{
		fake.Kind: func(cfg llm.ProviderConfig) (llm.Provider, error) {
			p := fake.New(cfg.Name)
			f[cfg.Name] = p
			return p, nil
		},
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	built := fakes{}
	constructors := built.constructors()
	constructors["broken"] = func(llm.ProviderConfig) (llm.Provider, error) {
		return nil, errors.New("environment variable BROKEN_KEY is not set")
	}
	constructors["misnamed"] = func(llm.ProviderConfig) (llm.Provider, error) { return fake.New("other"), nil }
	constructors["nil"] = func(llm.ProviderConfig) (llm.Provider, error) { return nil, nil }

	configs := []llm.ProviderConfig{
		{Name: "zeta", Kind: fake.Kind, DefaultModel: "z-default", Models: []string{"z1"}, Pricing: llm.Pricing{Free: true}},
		{Name: "alpha", Kind: fake.Kind, APIKey: secret.New("alpha-key-0123456789"), APIKeyEnv: "ALPHA_KEY"},
		{Name: "claude", Kind: "anthropic"}, // valid in the config, no adapter yet
		{Name: "bad", Kind: "broken"},
		{Name: "wrong-name", Kind: "misnamed"},
		{Name: "nothing", Kind: "nil"},
	}
	reg, err := llm.NewRegistry(constructors, configs, llm.RegistryOptions{Clock: newFakeClock()})
	if err != nil {
		t.Fatal(err)
	}

	wantNames := []string{"alpha", "bad", "claude", "nothing", "wrong-name", "zeta"}
	if diff := cmp.Diff(wantNames, reg.Names()); diff != "" {
		t.Errorf("Names() (-want +got):\n%s", diff)
	}

	t.Run("built providers work and are wrapped in retry", func(t *testing.T) {
		p, err := reg.Provider("zeta")
		if err != nil {
			t.Fatal(err)
		}
		if p.Name() != "zeta" {
			t.Errorf("Name() = %q", p.Name())
		}
		built["zeta"].Script(fail(llm.ErrOverloaded), fake.Step{Text: "ok"})
		resp, err := p.Chat(context.Background(), chatReq)
		if err != nil || resp.Message.Text() != "ok" {
			t.Errorf("Chat = %v, %v; want the retry wrapper to recover from one overload", resp, err)
		}
		if built["zeta"].Calls() != 2 {
			t.Errorf("%d calls, want 2", built["zeta"].Calls())
		}
	})

	errorCases := []struct {
		name    string
		wantIs  error
		wantErr string
	}{
		{"nope", llm.ErrUnknownProvider, `unknown provider "nope" (configured: alpha, bad, claude, nothing, wrong-name, zeta)`},
		{"claude", llm.ErrNoAdapter, `provider "claude": kind "anthropic": no adapter for this provider kind`},
		{"bad", nil, `provider "bad": environment variable BROKEN_KEY is not set`},
		{"wrong-name", nil, `provider "wrong-name": the misnamed adapter named it "other"`},
		{"nothing", nil, `provider "nothing": the nil adapter returned no provider`},
	}
	for _, tc := range errorCases {
		t.Run("provider "+tc.name, func(t *testing.T) {
			_, err := reg.Provider(tc.name)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Provider(%q) error = %v, want it to contain %q", tc.name, err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(err, %v) is false", tc.wantIs)
			}
		})
	}

	t.Run("config and pricing", func(t *testing.T) {
		cfg, ok := reg.Config("zeta")
		if !ok || cfg.DefaultModel != "z-default" {
			t.Fatalf("Config(zeta) = %+v, %v", cfg, ok)
		}
		cfg.Models[0] = "changed"
		if again, _ := reg.Config("zeta"); again.Models[0] != "z1" {
			t.Error("Config returns a slice shared with the registry")
		}
		if _, ok := reg.Config("nope"); ok {
			t.Error("Config found an unknown provider")
		}
		// A provider that failed to build still has its config.
		if _, ok := reg.Config("claude"); !ok {
			t.Error("Config lost a provider that has no adapter")
		}
		if cost, known := reg.Pricing("zeta").Cost("any", &llm.Usage{InputTokens: 10}); cost != 0 || !known {
			t.Errorf("zeta cost = %d, %v; want a known 0", cost, known)
		}
		if _, known := reg.Pricing("alpha").Cost("any", &llm.Usage{InputTokens: 10}); known {
			t.Error("alpha has no prices but its cost is known")
		}
		if _, known := reg.Pricing("nope").Cost("any", &llm.Usage{}); known {
			t.Error("an unknown provider has a known cost")
		}
	})

	t.Run("resolve model", func(t *testing.T) {
		tests := []struct {
			provider, requested string
			want                string
			wantErr             string
		}{
			{"zeta", "", "z-default", ""},
			{"zeta", "typed-by-hand", "typed-by-hand", ""},
			{"alpha", "m", "m", ""},
			{"alpha", "", "", `provider "alpha" has no default model: pass --model or set providers.alpha.default_model`},
			{"nope", "m", "", `unknown provider "nope"`},
		}
		for _, tt := range tests {
			got, err := reg.ResolveModel(tt.provider, tt.requested)
			if got != tt.want || (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("ResolveModel(%q, %q) = %q, %v; want %q, error containing %q", tt.provider, tt.requested, got, err, tt.want, tt.wantErr)
			}
		}
	})

	t.Run("errors never show a key", func(t *testing.T) {
		for _, name := range reg.Names() {
			if _, err := reg.Provider(name); err != nil && strings.Contains(err.Error(), "alpha-key") {
				t.Errorf("error for %s leaks a key: %v", name, err)
			}
		}
	})
}

func TestNewRegistryRejectsBadConfigs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		configs []llm.ProviderConfig
		wantErr string
	}{
		{"no name", []llm.ProviderConfig{{Kind: fake.Kind}}, "a provider has no name"},
		{"duplicate name", []llm.ProviderConfig{{Name: "a", Kind: fake.Kind}, {Name: "a", Kind: fake.Kind}}, `provider "a" is configured twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := llm.NewRegistry(fakes{}.constructors(), tt.configs, llm.RegistryOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewRegistry error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	// No providers and no constructors is a valid, empty registry.
	reg, err := llm.NewRegistry(nil, nil, llm.RegistryOptions{})
	if err != nil || len(reg.Names()) != 0 {
		t.Errorf("empty registry = %v, %v", reg.Names(), err)
	}
	if _, err := reg.Provider("x"); !errors.Is(err, llm.ErrUnknownProvider) || !strings.Contains(err.Error(), "configured: none") {
		t.Errorf("Provider on an empty registry = %v", err)
	}
}
