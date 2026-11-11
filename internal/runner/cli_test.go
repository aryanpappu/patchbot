package runner_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestCLIEndToEnd(t *testing.T) {
	f := newFixture(t, "true", specStep(), writeStep(), submitStep())
	invoke := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		app := cli.App{Stdout: &out, Stderr: &errOut, Config: config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }}, RunnerOptions: f.opts}
		code := app.Run(args)
		return code, out.String(), errOut.String()
	}
	code, out, errOut := invoke("run", "--repo", "owner/repo", "--issue", "12", "--dry-run")
	if code != 0 || !strings.Contains(out, "awaiting_approval") || !strings.Contains(out, "spec:") {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := invoke("run", "--repo", "owner/repo", "--issue", "12"); code != 3 || !strings.Contains(errOut, "active run already exists") {
		t.Fatalf("active conflict: %d %s", code, errOut)
	}
	code, out, errOut = invoke("approve", "1")
	if code != 0 || !strings.Contains(out, "diff:") || !strings.Contains(out, "pr_title:") || !strings.Contains(out, "Harness tests:") {
		t.Fatalf("approve: %d\n%s\n%s", code, out, errOut)
	}
	spec, err := f.db.LatestSpec(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := f.db.ValidApproval(t.Context(), 1, spec.Version, spec.Hash)
	if err != nil || approval.Actor != "local-owner" || approval.Channel != "cli" {
		t.Fatalf("approval: %+v, %v", approval, err)
	}
	code, out, errOut = invoke("runs", "--status", "succeeded", "--sync")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("runs: %d %s %s", code, out, errOut)
	}
	if f.posts != 0 || f.branches() != "" {
		t.Fatal("dry CLI published")
	}
}

func TestCLIControls(t *testing.T) {
	f := newFixture(t, "true", specStep(), specStep(), specStep())
	invoke := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		app := cli.App{Stdout: &out, Stderr: &errOut, Config: config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }}, RunnerOptions: f.opts}
		code := app.Run(args)
		return code, out.String(), errOut.String()
	}
	for _, args := range [][]string{{"run", "--repo", "owner/repo", "--issue", "12", "--mode", "refine"}, {"revise", "1", "--notes", "more detail"}, {"reject", "1"}, {"retry", "1", "--provider", "anthropic", "--model", "new-model"}, {"cancel", "2"}} {
		code, out, errOut := invoke(args...)
		if code != 0 && (args[0] != "cancel" || code != 130) {
			t.Fatalf("%v: %d %s %s", args, code, out, errOut)
		}
	}
	if got := f.get(2); got.RetryOf == nil || got.RefineModel != "new-model" || got.Status != store.StatusCancelled {
		t.Fatalf("retried: %+v", got)
	}
}
