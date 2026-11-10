// Package providers lists the provider adapters built into patchbot. It is
// the one place that imports every adapter, which keeps package llm free of
// them and gives each new adapter a single line to add.
package providers

import (
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
)

// Options select which adapters Constructors returns.
type Options struct {
	// Fake adds the scripted fake provider under kind "fake". It is for
	// tests and demos and is off by default.
	Fake bool
}

// Constructors returns the adapters by provider kind, for llm.NewRegistry.
//
// To add an adapter, write a function with the llm.Constructor signature in
// the adapter's package and add its kind here. A kind that is valid in the
// config but missing from this map yields llm.ErrNoAdapter for that provider
// only, so the other providers keep working.
func Constructors(opts Options) llm.Constructors {
	c := llm.Constructors{
		// M7: "anthropic": anthropic.New,
		// M8: "openai":    openai.New,
		// M9: "gemini":    gemini.New,
	}
	if opts.Fake {
		c[fake.Kind] = fake.Constructor
	}
	return c
}
