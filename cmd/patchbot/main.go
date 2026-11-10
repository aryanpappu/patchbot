// Command patchbot provides the patchbot CLI.
package main

import (
	"os"

	"github.com/aryanpappu/patchbot/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
