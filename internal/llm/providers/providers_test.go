package providers_test

import (
	"context"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
)

func TestConstructors(t *testing.T) {
	t.Parallel()
	if _, ok := providers.Constructors(providers.Options{})[fake.Kind]; ok {
		t.Error("the fake provider is registered without being asked for")
	}
	c := providers.Constructors(providers.Options{Fake: true})
	if _, ok := c[fake.Kind]; !ok {
		t.Fatal("Options.Fake did not register the fake provider")
	}

	for _, kind := range []string{config.KindAnthropic, config.KindOpenAI} {
		if _, ok := c[kind]; !ok {
			t.Errorf("kind %q is not registered", kind)
		}
	}

	// Until M9 lands, Gemini has no adapter.
	for _, kind := range []string{config.KindGemini} {
		if _, ok := c[kind]; ok {
			t.Errorf("kind %q has an adapter; update this test and the registry wiring notes", kind)
		}
	}
}

func TestOpenAIInstances(t *testing.T) {
	t.Parallel()
	reg, err := llm.NewRegistry(providers.Constructors(providers.Options{}), []llm.ProviderConfig{
		{Name: "local", Kind: config.KindOpenAI, BaseURL: "http://localhost:11434/v1"},
		{Name: "router", Kind: config.KindOpenAI, BaseURL: "https://openrouter.ai/api/v1"},
	}, llm.RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"local", "router"} {
		p, err := reg.Provider(name)
		if err != nil || p.Name() != name {
			t.Fatalf("provider %q: %v", name, err)
		}
	}
}

// TestRegistryFromDefaultConfig wires the real pieces together: the built-in
// config, its conversion to provider configs and the constructor map.
func TestRegistryFromDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	configs, err := cfg.LLMProviders()
	if err != nil {
		t.Fatal(err)
	}
	configs = append(configs, llm.ProviderConfig{Name: "demo", Kind: fake.Kind, Models: []string{"demo-model"}, DefaultModel: "demo-model"})

	reg, err := llm.NewRegistry(providers.Constructors(providers.Options{Fake: true}), configs, llm.RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// The Anthropic adapter is registered, and missing credentials produce
	// a config model fallback without making an HTTP request.
	if p, err := reg.Provider("anthropic"); err != nil || p.Name() != "anthropic" {
		t.Fatalf("Provider(anthropic) = %v, %v", p, err)
	}
	model, err := reg.ResolveModel("anthropic", "")
	if err != nil || model != "claude-sonnet-5-5" {
		t.Errorf("ResolveModel(anthropic) = %q, %v", model, err)
	}
	list, err := llm.NewModelLister(reg, nil).List(context.Background(), "anthropic")
	if err != nil || len(list.Models) != 4 || list.Live || list.Warning == "" {
		t.Errorf("anthropic models = %+v, %v; want the four config models and a warning", list, err)
	}
	if _, known := reg.Pricing("anthropic").Cost(model, &llm.Usage{InputTokens: 1000}); known {
		t.Error("a cost is known although no prices are shipped")
	}

	demo, err := reg.Provider("demo")
	if err != nil || demo.Name() != "demo" {
		t.Fatalf("Provider(demo) = %v, %v", demo, err)
	}
	list, err = llm.NewModelLister(reg, nil).List(context.Background(), "demo")
	if err != nil || !list.Live || len(list.Models) != 1 || list.Models[0].ID != "demo-model" {
		t.Errorf("demo models = %+v, %v", list, err)
	}
}
