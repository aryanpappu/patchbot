// Package cli implements the patchbot command-line interface.
package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/aryanpappu/patchbot/internal/version"
)

const usage = `Usage: patchbot <command> [options]

Commands:
  run      Start a run for a GitHub issue (planned)
  approve  Approve a run's spec (planned)
  revise   Request changes to a run's spec (planned)
  reject   Reject a run (planned)
  cancel   Cancel a run (planned)
  runs     List runs (planned)
  models   List available models (planned)
  config   Check or show configuration (planned)
  serve    Start the web interface (planned)
  version  Print version and build information

Use patchbot help, -h or --help to show this usage.
`

// Main runs the CLI and returns its process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("patchbot", flag.ContinueOnError)
	flags.SetOutput(stderr)
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
		return 2
	}

	switch name := flags.Arg(0); name {
	case "version":
		_, _ = fmt.Fprintln(stdout, "patchbot "+version.String())
		return 0
	case "run", "approve", "revise", "reject", "cancel", "runs", "models", "config", "serve":
		_, _ = fmt.Fprintf(stderr, "patchbot %s: not implemented yet\n", name)
		return 1
	default:
		flags.Usage()
		return 2
	}
}
