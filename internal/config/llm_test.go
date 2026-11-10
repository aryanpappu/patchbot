package config_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/secret"
)

func TestLLMProviders(t *testing.T) {
	t.Parallel()
	const key = "key-value-0123456789"
	c := loadString(t, `
[defaults]
approvers = ["alice"]
max_cost_usd = 2.5

[providers.anthropic.prices."claude-sonnet-5-5"]
input = 1.25
output = 0.1
cache_read = 0.0
checked = 2026-11-01

[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
models = ["local-model"]
price_free = true
`, map[string]string{"ANTHROPIC_API_KEY": key})

	got, err := c.LLMProviders()
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	want := []llm.ProviderConfig{
		{ //nolint:gosec // APIKeyEnv is the name of a variable, not a credential
			Name:         "anthropic",
			Kind:         "anthropic",
			APIKeyEnv:    "ANTHROPIC_API_KEY",
			Models:       []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2"},
			DefaultModel: "claude-sonnet-5-5",
			Pricing: llm.Pricing{Models: map[string]llm.Price{
				// 0.1 is not exact in binary; the conversion must still give exactly 100000.
				"claude-sonnet-5-5": {Input: 1_250_000, Output: 100_000, CacheRead: &zero},
			}},
		},
		{
			Name:    "ollama",
			Kind:    "openai",
			BaseURL: "http://localhost:11434/v1",
			Models:  []string{"local-model"},
			Pricing: llm.Pricing{Free: true},
		},
	}
	want[0].APIKey = secret.New(key)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("LLMProviders (-want +got):\n%s", diff)
	}

	// The result shares no slice with the config.
	got[0].Models[0] = "changed"
	if c.Providers["anthropic"].Models[0] != "claude-opus-5-5" {
		t.Error("LLMProviders shares the models slice with the config")
	}

	budget, hardMax, err := c.MaxCostMicro()
	if err != nil || budget != 2_500_000 || hardMax != 10_000_000 {
		t.Errorf("MaxCostMicro() = %d, %d, %v; want 2500000, 10000000", budget, hardMax, err)
	}
}

func TestLLMProvidersRejectsBadAmounts(t *testing.T) {
	t.Parallel()
	c := loadString(t, "[providers.x]\nkind = \"gemini\"\n[providers.x.prices.m]\ninput = -1.0\noutput = 1.0\n", nil)
	if _, err := c.LLMProviders(); err == nil || !strings.Contains(err.Error(), "providers.x.prices.m: input") {
		t.Errorf("LLMProviders error = %v, want it to name the bad price", err)
	}
	c = loadString(t, "[defaults]\nmax_cost_usd = nan\n", nil)
	if _, _, err := c.MaxCostMicro(); err == nil {
		t.Error("MaxCostMicro accepted NaN")
	}
}
