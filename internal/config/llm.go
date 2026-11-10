package config

import (
	"errors"
	"fmt"
	"slices"

	"github.com/aryanpappu/patchbot/internal/llm"
)

// LLMProviders converts the [providers.*] tables into the form the llm
// registry takes, in name order. Prices are converted from dollars to whole
// micro-USD per million tokens here, once, so that no float reaches the cost
// math. Call it on a config that passed Validate.
func (c *Config) LLMProviders() ([]llm.ProviderConfig, error) {
	out := make([]llm.ProviderConfig, 0, len(c.Providers))
	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		pricing := llm.Pricing{Free: p.PriceFree}
		for _, model := range sortedKeys(p.Prices) {
			price, err := p.Prices[model].micro()
			if err != nil {
				return nil, fmt.Errorf("providers.%s.prices.%s: %w", name, model, err)
			}
			if pricing.Models == nil {
				pricing.Models = make(map[string]llm.Price, len(p.Prices))
			}
			pricing.Models[model] = price
		}
		out = append(out, llm.ProviderConfig{
			Name:         name,
			Kind:         p.Kind,
			BaseURL:      p.BaseURL,
			APIKey:       p.APIKey,
			APIKeyEnv:    p.APIKeyEnv,
			Models:       slices.Clone(p.Models),
			DefaultModel: p.DefaultModel,
			Pricing:      pricing,
		})
	}
	return out, nil
}

// MaxCostMicro returns defaults.max_cost_usd and limits.hard_max_cost_usd in
// micro-USD.
func (c *Config) MaxCostMicro() (budget, hardMax int64, err error) {
	budget, ok := llm.USDToMicro(c.Defaults.MaxCostUSD)
	if !ok {
		return 0, 0, fmt.Errorf("defaults.max_cost_usd: %v is not a valid amount", c.Defaults.MaxCostUSD)
	}
	hardMax, ok = llm.USDToMicro(c.Limits.HardMaxCostUSD)
	if !ok {
		return 0, 0, fmt.Errorf("limits.hard_max_cost_usd: %v is not a valid amount", c.Limits.HardMaxCostUSD)
	}
	return budget, hardMax, nil
}

func (p *Price) micro() (llm.Price, error) {
	var out llm.Price
	var ok bool
	// A missing rate is an error here too, not a zero: this function is the
	// last step before the budget math, and Validate may not have been run.
	if p.Input == nil || p.Output == nil {
		return out, errors.New("input and output are both required; write 0 for a rate that is free")
	}
	if out.Input, ok = llm.USDToMicro(*p.Input); !ok {
		return out, fmt.Errorf("input: %v is not a valid price", *p.Input)
	}
	if out.Output, ok = llm.USDToMicro(*p.Output); !ok {
		return out, fmt.Errorf("output: %v is not a valid price", *p.Output)
	}
	for _, f := range []struct {
		name string
		src  *float64
		dst  **int64
	}{
		{"cache_read", p.CacheRead, &out.CacheRead},
		{"cache_write", p.CacheWrite, &out.CacheWrite},
	} {
		if f.src == nil {
			continue
		}
		micro, ok := llm.USDToMicro(*f.src)
		if !ok {
			return out, fmt.Errorf("%s: %v is not a valid price", f.name, *f.src)
		}
		*f.dst = &micro
	}
	return out, nil
}
