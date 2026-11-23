package runner_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/gitx"
	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/llm/fake"
	"github.com/aryanpappu/patchbot/internal/runner"
	"github.com/aryanpappu/patchbot/internal/store"
)

const ladder = `
[escalation]
enabled = true
levels = [{ provider = "anthropic", model = "test-model" }, { provider = "anthropic", model = "big-model" }]
`

// withEscalation reloads the fixture with the ladder and a synchronous
// dispatcher, the server-mode path that runs afterRun.
func withEscalation(t *testing.T, f *fixture) {
	t.Helper()
	data, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, append(data, ladder...), 0600); err != nil { //nolint:gosec // The fixture owns this temp path.
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Path: f.configPath, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	f.opts.Dispatch = func(ctx context.Context, _ int64, work func(context.Context) error) error { return work(ctx) }
	f.r, err = runner.New(t.Context(), cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

// startFailing starts a run that is expected to fail and returns it.
func startFailing(f *fixture) store.Run {
	f.t.Helper()
	run, _ := f.r.Start(f.t.Context(), runner.Request{Repo: "owner/repo", Issue: 12, Mode: "fix", DryRun: true})
	if run.ID == 0 {
		f.t.Fatal("run was not created")
	}
	return f.get(run.ID)
}

func badRequest() fake.Step { return fake.Step{Err: &llm.Error{Kind: llm.ErrBadRequest}} }

func TestEscalationRetriesOneLevelUpThenStops(t *testing.T) {
	// Level 1 fails, level 2 writes a spec, so the chain stops at approval.
	f := newFixture(t, "true", badRequest(), specStep())
	withEscalation(t, f)
	first := startFailing(f)
	if first.Status != store.StatusFailed || first.ErrorKind != "provider_bad_request" {
		t.Fatalf("first run: %s %s", first.Status, first.ErrorKind)
	}
	info, err := f.r.Escalation(t.Context(), first)
	if err != nil || info.To == nil || info.Level != 1 || info.Levels != 2 {
		t.Fatalf("first escalation info: %+v %v", info, err)
	}
	second := f.get(*info.To)
	if second.RetryOf == nil || *second.RetryOf != first.ID || second.CodeModel != "big-model" || second.Status != store.StatusAwaitingApproval {
		t.Fatalf("second run: %+v", second)
	}
	info, err = f.r.Escalation(t.Context(), second)
	if err != nil || info.From == nil || *info.From != first.ID || info.Trigger != "provider_bad_request" || info.Level != 2 || info.Next != nil {
		t.Fatalf("second escalation info: %+v %v", info, err)
	}
	if caps := f.r.ActionCapabilities(second, nil); caps.CanEscalate {
		t.Fatal("top level offers escalation")
	}
}

func TestEscalationTopLevelFailureIsLeftForAPerson(t *testing.T) {
	f := newFixture(t, "true", badRequest(), badRequest())
	withEscalation(t, f)
	first := startFailing(f)
	info, err := f.r.Escalation(t.Context(), first)
	if err != nil || info.To == nil {
		t.Fatalf("no escalation: %+v %v", info, err)
	}
	second := f.get(*info.To)
	if second.Status != store.StatusFailed {
		t.Fatal(second.Status)
	}
	if again, _ := f.r.Escalation(t.Context(), second); again.To != nil {
		t.Fatal("escalated past the top level")
	}
	note, err := f.db.GetArtifact(t.Context(), second.ID, "escalation_note", 0)
	if err != nil || !strings.Contains(note.Content, "top level") {
		t.Fatalf("note: %+v %v", note, err)
	}
	if f.p.Calls() != 2 {
		t.Fatalf("provider calls = %d", f.p.Calls())
	}
}

func TestEscalationIgnoresUnlistedKindsAndDisabledLadder(t *testing.T) {
	f := newFixture(t, "true", fake.Step{Err: &llm.Error{Kind: llm.ErrAuth}})
	withEscalation(t, f)
	run := startFailing(f)
	if run.ErrorKind != "provider_auth" {
		t.Fatal(run.ErrorKind)
	}
	if info, _ := f.r.Escalation(t.Context(), run); info.To != nil {
		t.Fatal("auth failure escalated")
	}

	g := newFixture(t, "true", badRequest())
	g.opts.Dispatch = func(ctx context.Context, _ int64, work func(context.Context) error) error { return work(ctx) }
	r, err := runner.New(t.Context(), g.cfg, g.db, g.opts)
	if err != nil {
		t.Fatal(err)
	}
	g.r = r
	run = startFailing(g)
	if info, _ := g.r.Escalation(t.Context(), run); info.To != nil || info.Enabled {
		t.Fatal("disabled ladder escalated")
	}
}

func TestManualEscalate(t *testing.T) {
	f := newFixture(t, "true", fake.Step{Err: &llm.Error{Kind: llm.ErrAuth}}, specStep())
	withEscalation(t, f)
	first := startFailing(f)
	if !f.r.ActionCapabilities(first, nil).CanEscalate {
		t.Fatal("failed level-1 run cannot escalate")
	}
	next, err := f.r.Escalate(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.CodeModel != "big-model" || f.get(next.ID).Status != store.StatusAwaitingApproval {
		t.Fatalf("manual escalation: %+v", next)
	}
	if _, err := f.r.Escalate(t.Context(), next.ID); err == nil {
		t.Fatal("escalated a waiting run")
	}
	// The failed run already escalated; a second click must not start another run.
	if _, err := f.r.Escalate(t.Context(), first.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second escalation: %v", err)
	}
}

// A run that reaches its working-time limit is reported as run_timeout (an
// escalation trigger), not as an internal error.
func TestRunDeadlineIsRunTimeout(t *testing.T) {
	f := newFixture(t, "true", specStep())
	f.opts.Clone = func(context.Context, gitx.CloneOptions) (*gitx.Repo, error) { return nil, context.DeadlineExceeded }
	r, err := runner.New(t.Context(), f.cfg, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	run := startFailing(f)
	if run.ErrorKind != "run_timeout" || !strings.Contains(run.ErrorMessage, "run time limit") {
		t.Fatalf("kind=%q message=%q", run.ErrorKind, run.ErrorMessage)
	}
}
