package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/secret"
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
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config: unknown command\n\n%s", configUsage)
		return 2
	}

	flags := flag.NewFlagSet("patchbot config "+sub, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	opts := a.Config
	flags.StringVar(&opts.Path, "config", opts.Path, "config file")
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(a.Stdout, configUsage)
			return 0
		}
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config %s: invalid command options\n\n%s", sub, configUsage)
		return 2
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config %s: unexpected argument\n\n%s", sub, configUsage)
		return 2
	}

	cfg, err := a.loadConfig(opts)
	if err != nil {
		a.printProblems(nil, err)
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
	redactor := a.outputRedactor
	warnings := append(cfg.Warnings(), a.credentialWarnings()...)
	for _, w := range warnings {
		_, _ = fmt.Fprintf(a.Stderr, "warning: %s\n", redactor.Redact(w))
	}
	if err := cfg.Validate(); err != nil {
		n := a.printProblems(redactor, err)
		_, _ = fmt.Fprintf(a.Stderr, "config invalid: %s: %s\n", redactor.Redact(source), plural(n, "error"))
		return 1
	}
	_, _ = fmt.Fprintf(a.Stdout, "config OK: %s (%s)\n", redactor.Redact(source), plural(len(warnings), "warning"))
	return 0
}

func (a *App) configShow(cfg *config.Config) int {
	if err := cfg.WriteEffective(a.Stdout, a.outputRedactor); err != nil {
		_, _ = fmt.Fprintf(a.Stderr, "patchbot config show: %s\n", cfg.Redactor().Redact(err.Error()))
		return 1
	}
	// An invalid config is still shown, which helps to fix it, but the
	// command says so and fails.
	if err := cfg.Validate(); err != nil {
		n := a.printProblems(cfg.Redactor(), err)
		_, _ = fmt.Fprintf(a.Stderr, "config invalid: %s; run `patchbot config check`\n", plural(n, "error"))
		return 1
	}
	return 0
}

// printProblems prints each error joined into err on its own line and
// returns how many there were. Every line passes the redactor first: the
// config package already redacts its own messages, and this is the last
// check before the text reaches a terminal or a CI log. A nil redactor
// still masks token-shaped text.
func (a *App) printProblems(redactor *secret.Redactor, err error) int {
	r := secret.NewRedactor()
	r.Extend(redactor)
	r.Extend(a.outputRedactor)
	lines := strings.Split(r.Redact(err.Error()), "\n")
	for _, line := range lines {
		_, _ = fmt.Fprintf(a.Stderr, "error: %s\n", line)
	}
	return len(lines)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// loadConfig also registers provider credentials at the final CLI stream
// boundary, covering diagnostic summaries as well as individual errors.
func (a *App) loadConfig(opts config.Options) (*config.Config, error) {
	cfg, err := config.Load(opts)
	if err == nil && a.outputRedactor != nil {
		a.outputRedactor.Extend(cfg.Redactor())
	}
	return cfg, err
}

// Credential warnings name the variable only; short values are deliberately
// excluded from substring masking so ordinary words and schema names survive.
func (a *App) credentialWarnings() []string {
	lookup := a.Config.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var warnings []string
	for _, name := range []string{"GITHUB_TOKEN", "PATCHBOT_UI_TOKEN"} {
		value, _ := lookup(name)
		if len(value) > 0 && len(value) < secret.MinValueLen {
			warnings = append(warnings, fmt.Sprintf("credential in %s is shorter than %d characters and cannot be masked; use a longer credential", name, secret.MinValueLen))
		}
	}
	return warnings
}
