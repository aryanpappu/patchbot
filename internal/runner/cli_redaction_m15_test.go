package runner_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/cli"
	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestCLIRunnersMaskUICredentialBeforeModelAndPersistence(t *testing.T) {
	const token = "ui-credential-regression-value" // #nosec G101 -- dummy disclosure fixture.
	f := newFixture(t, "true", specStep(), toolStep("read_file", map[string]string{"path": "README.md"}), writeStep(), submitStep())
	f.issue.Title = "Issue containing " + token
	f.issue.Body = "Read README.md; " + token
	if err := os.WriteFile(filepath.Join(f.seed, "README.md"), []byte("private credential "+token), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "add", "README.md")
	git(t, f.seed, "commit", "-m", "credential fixture")
	git(t, f.seed, "push", f.remote, "main")
	for _, args := range [][]string{{"run", "--repo", "owner/repo", "--issue", "12", "--dry-run"}, {"approve", "1"}} {
		var out, errs bytes.Buffer
		app := cli.App{Stdout: &out, Stderr: &errs, Config: config.Options{Path: f.configPath, LookupEnv: func(name string) (string, bool) {
			if name == "PATCHBOT_UI_TOKEN" {
				return token, true
			}
			return "", false
		}}, RunnerOptions: f.opts}
		if code := app.Run(args); code != 0 {
			t.Fatalf("%v: %d %s %s", args, code, out.String(), errs.String())
		}
		if strings.Contains(out.String()+errs.String(), token) {
			t.Fatal("CLI output leaked UI credential")
		}
	}
	requests, err := json.Marshal(f.p.Requests())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requests), token) || !strings.Contains(string(requests), "redacted") {
		t.Fatal("UI credential reached model or masking was not exercised")
	}
	run := f.get(1)
	if run.Status != store.StatusSucceeded {
		t.Fatalf("run: %+v", run)
	}
	steps, err := f.db.ListSteps(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(t.Context(), 1, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]any{run, steps, events})
	if strings.Contains(string(raw), token) {
		t.Fatal("UI credential persisted")
	}
	for _, kind := range []string{"spec", "report", "diff", "pr_body"} {
		artifact, err := f.db.GetArtifact(t.Context(), 1, kind, 0)
		if err == nil && strings.Contains(artifact.Content, token) {
			t.Fatalf("UI credential persisted in %s", kind)
		}
	}
	f.p.AssertConsumed(t)
}
