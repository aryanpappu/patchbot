package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
)

func TestRunCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "--repo", "owner/repo", "--issue", "-1"}, {"run", "--repo", "owner/repo", "--issue", "1", "--mode", "all"}, {"run", "--repo", "owner/repo", "--issue", "1", "--pre-approve"}, {"approve", "0"}, {"approve", "abc"}, {"revise", "1"}, {"revise", "1", "--notes", " "}, {"reject"}, {"cancel", "1", "extra"}, {"runs", "--status", "unknown"}, {"retry", "-1"}, {"approve", "1", "--model", "x"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			app := cli.App{Stdout: &out, Stderr: &errOut}
			if code := app.Run(args); code != 2 || !strings.Contains(errOut.String(), "Usage:") {
				t.Fatalf("code %d, error %s", code, errOut.String())
			}
		})
	}
	for _, command := range []string{"run", "approve", "revise", "reject", "cancel", "runs", "retry"} {
		var out, errOut bytes.Buffer
		app := cli.App{Stdout: &out, Stderr: &errOut}
		if code := app.Run([]string{command, "--help"}); code != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("help %s: %d %s", command, code, errOut.String())
		}
	}
}
