package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/aryanpappu/patchbot/internal/config"
)

const configUsage = `Usage: patchbot config <check|show> [--config FILE]

Commands:
  check  Validate the configuration; exit 0 when valid, 1 when not
  show   Print the effective configuration and the source of every value

Options:
  --config FILE  Read this file instead of searching for one

Without --config the file is the first of: $PATCHBOT_CONFIG, ./patchbot.toml,
patchbot/config.toml in the user config directory. Values are layered as
flag > PATCHBOT_* environment variable > file > built-in default.
`

// runConfig implements `patchbot config check|show`.
func (a *App) runConfig(args []string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(a.Stderr, configUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(a.Stdout, configUsage)
		return 0
	case "check", "show":
	default:
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config: unknown command %q\n\n%s", sub, configUsage)
		return 2
	}

	flags := flag.NewFlagSet("patchbot config "+sub, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	opts := a.Config
	flags.StringVar(&opts.Path, "config", "", "config file")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, configUsage)
			return 0
		}
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config %s: %v\n\n%s", sub, err, configUsage)
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config %s: unexpected argument %q\n\n%s", sub, flags.Arg(0), configUsage)
		return 2
	}

	cfg, err := config.Load(opts)
	if err != nil {
		a.printProblems("error", err)
		return 1
	}
	if sub == "show" {
		return a.configShow(cfg)
	}
	return a.configCheck(cfg)
}

func (a *App) configCheck(cfg *config.Config) int {
	source := "built-in defaults (no config file found)"
	if cfg.Path() != "" {
		source = cfg.Path()
	}
	warnings := cfg.Warnings()
	for _, w := range warnings {
		_, _ = fmt.Fprintf(a.Stderr, "warning: %s\n", w)
	}
	if err := cfg.Validate(); err != nil {
		n := a.printProblems("error", err)
		_, _ = fmt.Fprintf(a.Stderr, "config invalid: %s: %s\n", source, plural(n, "error"))
		return 1
	}
	_, _ = fmt.Fprintf(a.Stdout, "config OK: %s (%s)\n", source, plural(len(warnings), "warning"))
	return 0
}

func (a *App) configShow(cfg *config.Config) int {
	if err := cfg.WriteEffective(a.Stdout); err != nil {
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config show: %v\n", err)
		return 1
	}
	// An invalid config is still shown, which helps to fix it, but the
	// command says so and fails.
	if err := cfg.Validate(); err != nil {
		n := a.printProblems("error", err)
		_, _ = fmt.Fprintf(a.Stderr, "config invalid: %s; run `patchbot config check`\n", plural(n, "error"))
		return 1
	}
	return 0
}

// printProblems prints each error joined into err on its own line and
// returns how many there were.
func (a *App) printProblems(label string, err error) int {
	lines := strings.Split(err.Error(), "\n")
	for _, line := range lines {
		_, _ = fmt.Fprintf(a.Stderr, "%s: %s\n", label, line)
	}
	return len(lines)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
