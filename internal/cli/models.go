package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
)

const modelsUsage = `Usage: patchbot models [--provider NAME] [--config FILE]

List model IDs, display names and sources (live or config).
The provider defaults to defaults.provider. If listing fails, configured models
are shown with a warning.
`

func (a *App) runModels(args []string) int {
	flags := flag.NewFlagSet("patchbot models", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	opts := a.Config
	var provider string
	flags.StringVar(&opts.Path, "config", opts.Path, "config file")
	flags.StringVar(&provider, "provider", "", "provider instance")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, modelsUsage)
			return 0
		}
		_, _ = fmt.Fprintf(a.Stderr, "patchbot models: %v\n\n%s", err, modelsUsage)
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprint(a.Stderr, modelsUsage)
		return 2
	}
	if provider != "" {
		opts.Flags = append(append([]config.Override(nil), opts.Flags...), config.Override{Key: "defaults.provider", Value: provider, Flag: "--provider"})
	}
	cfg, err := config.Load(opts)
	if err != nil {
		a.printProblems(nil, err)
		return 1
	}
	// Listing needs provider settings, not a runnable repository or an API key.
	// In particular a missing key must still allow the config fallback.
	configs, err := cfg.LLMProviders()
	if err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	registry, err := llm.NewRegistry(providers.Constructors(a.Providers), configs, llm.RegistryOptions{})
	if err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := llm.NewModelLister(registry, nil).List(ctx, cfg.Defaults.Provider)
	if err != nil {
		a.printProblems(cfg.Redactor(), err)
		return 1
	}
	redactor := cfg.Redactor()
	if list.Warning != "" {
		_, _ = fmt.Fprintf(a.Stderr, "warning: %s\n", redactor.Redact(list.Warning))
	}
	table := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "MODEL ID\tDISPLAY NAME\tSOURCE")
	for _, m := range list.Models {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", redactor.Redact(m.ID), redactor.Redact(m.DisplayName), m.Source)
	}
	if err := table.Flush(); err != nil {
		_, _ = fmt.Fprintln(a.Stderr, "patchbot models: could not write model list")
		return 1
	}
	return 0
}
