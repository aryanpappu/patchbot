// Package cli implements the patchbot command-line interface.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/llm/providers"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/version"
)

const usage = `Usage: patchbot <command> [options]

Commands:
  run      Start a run for a GitHub issue
  approve  Approve a run's spec and continue
  revise   Refine a run's spec with notes
  reject   Reject a waiting run
  cancel   Cancel an active run
  runs     List runs and optionally sync GitHub comments
  retry    Start a new run linked to a failed run
  models   List available models
  doctor   Check local setup (offline unless --online)
  config   Check or show configuration
  serve    Serve the local JSON API and event streams
  version  Print version and build information

Use patchbot help, -h or --help to show this usage.
`

// Main runs the CLI against the real environment and returns its process exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	return (&App{Stdout: stdout, Stderr: stderr}).Run(args)
}

// App is the CLI with its surroundings made explicit, so tests can run it
// without reading the real environment, working directory or user config.
type App struct {
	// Context optionally controls the server lifetime in embedded use and tests.
	Context context.Context
	Stdout  io.Writer
	Stderr  io.Writer
	// Config holds the config search settings. Its Path and Flags are set
	// from the command line. The zero value reads the real environment.
	Config config.Options
	// Providers injects optional transports and test providers.
	Providers providers.Options
	// RunnerOptions injects local git remotes, GitHub and fake LLMs in tests.
	RunnerOptions  runner.Options
	outputRedactor *secret.Redactor
}

// Run runs one command and returns the process exit code: 0 for success, 1
// for failure and 2 for a usage error.
func (a *App) Run(args []string) int {
	lookup := a.Config.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := secret.NewRedactor()
	for _, name := range []string{"GITHUB_TOKEN", "PATCHBOT_UI_TOKEN"} {
		value, _ := lookup(name)
		r.AddExact(value)
	}
	previousRedactor := a.outputRedactor
	a.outputRedactor = r
	defer func() { a.outputRedactor = previousRedactor }()
	stdout, stderr := a.Stdout, a.Stderr
	flags := flag.NewFlagSet("patchbot", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, usage)
	}

	if len(args) == 0 {
		flags.Usage()
		return 2
	}

	switch args[0] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}

	if err := flags.Parse(args); err != nil {
		flags.Usage()
		return 2
	}

	switch name := flags.Arg(0); name {
	case "version":
		_, _ = fmt.Fprintln(stdout, "patchbot "+version.String())
		return 0
	case "config":
		return a.runConfig(flags.Args()[1:])
	case "doctor":
		return a.runDoctor(flags.Args()[1:])
	case "models":
		return a.runModels(flags.Args()[1:])
	case "run", "approve", "revise", "reject", "cancel", "runs", "retry":
		return a.runControl(name, flags.Args()[1:])
	case "serve":
		return a.runServe(flags.Args()[1:])
	default:
		flags.Usage()
		return 2
	}
}
