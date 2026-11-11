package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
)

func TestCLI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantStdout: "patchbot dev (commit none, built unknown)\n"},
		{name: "help", args: []string{"help"}, wantStdout: "Usage: patchbot"},
		{name: "short help", args: []string{"-h"}, wantStdout: "Usage: patchbot"},
		{name: "long help", args: []string{"--help"}, wantStdout: "Usage: patchbot"},
		{name: "no arguments", wantCode: 2, wantStderr: "Usage: patchbot"},
		{name: "unknown command", args: []string{"unknown"}, wantCode: 2, wantStderr: "Usage: patchbot"},
		{name: "unknown flag", args: []string{"--unknown"}, wantCode: 2, wantStderr: "Usage: patchbot"},
		{name: "empty command", args: []string{""}, wantCode: 2, wantStderr: "Usage: patchbot"},
		{name: "option terminator", args: []string{"--"}, wantCode: 2, wantStderr: "Usage: patchbot"},
		{name: "run", args: []string{"run"}, wantCode: 2, wantStderr: "--repo and a positive --issue are required"},
		{name: "approve", args: []string{"approve"}, wantCode: 2, wantStderr: "run ID is required"},
		{name: "revise", args: []string{"revise"}, wantCode: 2, wantStderr: "run ID is required"},
		{name: "reject", args: []string{"reject"}, wantCode: 2, wantStderr: "run ID is required"},
		{name: "cancel", args: []string{"cancel"}, wantCode: 2, wantStderr: "run ID is required"},
		{name: "config", args: []string{"config"}, wantCode: 2, wantStderr: "Usage: patchbot config <check|show>"},
		{name: "serve help", args: []string{"serve", "--help"}, wantStdout: "Usage: patchbot serve"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if got := cli.Main(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("Main() = %d, want %d", got, tt.wantCode)
			}
			for _, output := range []struct {
				name string
				got  string
				want string
			}{
				{name: "stdout", got: stdout.String(), want: tt.wantStdout},
				{name: "stderr", got: stderr.String(), want: tt.wantStderr},
			} {
				if output.want == "" {
					if output.got != "" {
						t.Errorf("%s = %q, want empty output", output.name, output.got)
					}
				} else if !strings.Contains(output.got, output.want) {
					t.Errorf("%s = %q, want substring %q", output.name, output.got, output.want)
				}
			}
			if tt.wantStdout == "Usage: patchbot" {
				for _, command := range []string{"run", "approve", "revise", "reject", "cancel", "runs", "models", "config", "serve", "version"} {
					found := false
					for line := range strings.SplitSeq(stdout.String(), "\n") {
						fields := strings.Fields(line)
						if len(fields) > 1 && fields[0] == command {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("help is missing command %q and its description", command)
					}
				}
			}
		})
	}
}
