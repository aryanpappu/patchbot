//go:build smoke

package runner_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
)

// TestSmoke uses the production CLI dispatcher/loader with injected fake
// infrastructure. Git, command execution, approval and SQLite are real.
func TestSmoke(t *testing.T) {
	f := newFixture(t, "test", specStep(), writeStep(), submitStep())
	configBytes, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(string(configBytes), `api_key_env = ""`, `api_key_env = "PATCHBOT_SMOKE_KEY"`, 1)
	// setup-go and tool managers often install Go outside the portable defaults.
	dirs := append([]string{filepath.Join(runtime.GOROOT(), "bin")}, f.cfg.Limits.TrustedPath...)
	quoted := make([]string, len(dirs))
	for i, dir := range dirs {
		quoted[i] = strconv.Quote(dir)
	}
	content += "\n[limits]\ntrusted_path = [" + strings.Join(quoted, ", ") + "]\n"
	if err := os.WriteFile(f.configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		switch name {
		case "PATCHBOT_SMOKE_KEY":
			return "fake-provider-credential", true
		case "GITHUB_TOKEN":
			return "fixture-credential", true
		}
		return "", false
	}
	// Exercise the built binary too when invoked by scripts/smoke.sh.
	if binary := os.Getenv("PATCHBOT_SMOKE_BINARY"); binary != "" {
		cmd := exec.CommandContext(t.Context(), binary, "doctor", "--config", f.configPath) // #nosec G204 -- binary is built by the local smoke script.
		cmd.Env = []string{"PATCHBOT_SMOKE_KEY=fake-provider-credential", "GITHUB_TOKEN=fixture-credential"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("built doctor: %v\n%s", err, out)
		}
		fmt.Print(string(out))
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		app := cli.App{Context: t.Context(), Stdout: &out, Stderr: &errs, Config: config.Options{Path: f.configPath, LookupEnv: lookup}, RunnerOptions: f.opts}
		if code := app.Run(args); code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, out.String(), errs.String())
		}
		fmt.Print(out.String())
		return out.String()
	}
	invoke("doctor")
	out := invoke("run", "--repo", "owner/repo", "--issue", "12", "--dry-run")
	if !strings.Contains(out, "awaiting_approval") || !strings.Contains(out, "spec:") {
		t.Fatal("refinement did not wait for approval")
	}
	out = invoke("approve", "1")
	if !strings.Contains(out, "pr_body:") || !strings.Contains(out, "Harness tests: **passed**") || !strings.Contains(out, "diff:") {
		t.Fatal("missing dry-run publication artifacts")
	}
	run := f.get(1)
	if run.Status != store.StatusSucceeded || run.Outcome != "dry_run" || run.TestAttempts != 1 {
		t.Fatalf("run: %+v", run)
	}
	spec, err := f.db.LatestSpec(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ValidApproval(t.Context(), 1, spec.Version, spec.Hash); err != nil {
		t.Fatal(err)
	}
	if f.posts != 0 || len(f.prs) != 0 || f.branches() != "" {
		t.Fatal("dry run published")
	}
	for _, phase := range []store.Phase{store.PhaseBaseline, store.PhaseVerify} {
		steps, err := f.db.ListSteps(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, step := range steps {
			if step.Phase == phase && step.Kind == "test" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s harness test", phase)
		}
	}
	f.p.AssertConsumed(t)
	f.assertClean()
	if _, err := os.Stat(filepath.Join(f.cfg.DataDir(), "patchbot.sqlite")); err != nil {
		t.Fatal(err)
	}
	fmt.Println("SMOKE PASS: refine → approve → code → test; PR text printed; no writes or pushes")
}
