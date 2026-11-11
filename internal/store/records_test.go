package store

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
)

func TestStepsAndUsage(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	step, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseCode, Kind: "llm", Status: "running", Tool: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if step.Seq != 1 {
		t.Fatalf("first step seq = %d", step.Seq)
	}
	if got := mustGetRun(t, s, r.ID); got.CostMicros != nil || got.Turns != 0 {
		t.Fatal("unfinished step counted toward totals")
	}
	step.Status, step.FinishedAt = "ok", ptr(timestamp(time.Now()))
	step.InputTokens, step.OutputTokens = ptr(int64(100)), ptr(int64(10))
	step.CacheReadTokens, step.CacheWriteTokens, step.CostMicros = ptr(int64(0)), ptr(int64(0)), ptr(int64(12345))
	for range 2 {
		step, err = s.RecordStep(t.Context(), step)
		if err != nil {
			t.Fatal(err)
		}
		got := mustGetRun(t, s, r.ID)
		if got.Turns != 1 || got.CostMicros == nil || *got.CostMicros != 12345 || got.InputTokens == nil || *got.InputTokens != 100 {
			t.Fatalf("step update double counted or lost usage: %+v", got)
		}
	}
	unknown, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseRefine, Kind: "llm", Status: "ok", InputTokens: ptr(int64(20)), OutputTokens: ptr(int64(2))})
	if err != nil {
		t.Fatal(err)
	}
	got := mustGetRun(t, s, r.ID)
	if got.CostMicros != nil || got.Turns != 2 || got.InputTokens == nil || *got.InputTokens != 120 {
		t.Fatalf("unknown cost or totals: %+v", got)
	}
	if unknown.Seq != 2 {
		t.Fatalf("second step seq = %d", unknown.Seq)
	}
	// An explicitly free call is known zero, unlike an unpriced call.
	unknown.CostMicros = ptr(int64(0))
	if _, err := s.RecordStep(t.Context(), unknown); err != nil {
		t.Fatal(err)
	}
	got = mustGetRun(t, s, r.ID)
	if got.CostMicros == nil || *got.CostMicros != 12345 {
		t.Fatal("zero cost was treated as unknown")
	}
	testStep, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseVerify, Kind: "test", Status: "error", ExitCode: ptr(int64(-1)), DurationMS: ptr(int64(10)), OutputText: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustGetRun(t, s, r.ID); got.TestAttempts != 1 || got.Turns != 2 {
		t.Fatal("test attempt accounting")
	}
	steps, err := s.ListSteps(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Step{step, unknown, testStep}, steps); diff != "" {
		t.Fatal(diff)
	}
}

func TestStepAndTotalsRollbackTogether(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER reject_totals BEFORE UPDATE ON runs BEGIN SELECT RAISE(ABORT,'test totals failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseCode, Kind: "llm", Status: "ok"}); err == nil {
		t.Fatal("totals failure was ignored")
	}
	steps, err := s.ListSteps(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatal("step was committed without its run totals")
	}
}

func TestTranscriptCaps(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{ErrorMessage: strings.Repeat("界", 1000)})
	if len(r.ErrorMessage) > 2000 || !utf8.ValidString(r.ErrorMessage) {
		t.Fatal("error cap broke UTF-8 or exceeded limit")
	}
	step, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseCode, Kind: "tool", Status: "ok",
		InputJSON: strings.Repeat("界", 10000), OutputText: strings.Repeat("界", 30000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.InputJSON) > 16*1024 || len(step.OutputText) > 64*1024 || !utf8.ValidString(step.InputJSON) || !utf8.ValidString(step.OutputText) {
		t.Fatal("transcripts exceeded cap or broke UTF-8")
	}
}

func TestQuestionsActionsArtifacts(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	q, err := s.SaveQuestion(t.Context(), Question{RunID: r.ID, Body: "Which behavior?", CommentID: ptr(int64(123))})
	if err != nil {
		t.Fatal(err)
	}
	if q.Answer != nil || q.AnsweredAt != nil {
		t.Fatal("unanswered question has answer metadata")
	}
	q.Answer, q.AnsweredBy, q.AnswerChannel, q.AnsweredAt = ptr("Use option A"), ptr("owner"), ptr("cli"), ptr(timestamp(time.Now()))
	q, err = s.SaveQuestion(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	questions, err := s.ListQuestions(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Question{q}, questions); diff != "" {
		t.Fatal(diff)
	}
	a, err := s.RecordAction(t.Context(), Action{RunID: r.ID, Kind: "answer", Actor: "owner", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := s.ListActions(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Action{a}, actions); diff != "" {
		t.Fatal(diff)
	}
	artifact := Artifact{RunID: r.ID, Kind: "test_output", Attempt: 1, Content: "first", CreatedAt: timestamp(time.Now())}
	if err := s.SaveArtifact(t.Context(), artifact); err != nil {
		t.Fatal(err)
	}
	artifact.Content = "replaced"
	if err := s.SaveArtifact(t.Context(), artifact); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArtifact(t.Context(), r.ID, artifact.Kind, artifact.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(artifact, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	_, runErr := s.GetRun(t.Context(), 999)
	_, specErr := s.LatestSpec(t.Context(), 999)
	_, approvalErr := s.ValidApproval(t.Context(), 999, 1, hashOf("none"))
	_, artifactErr := s.GetArtifact(t.Context(), 999, "none", 0)
	for i, err := range []error{runErr, specErr, approvalErr, artifactErr} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("missing read %d = %v", i, err)
		}
	}
}

func TestDatabaseConstraints(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	// These bypass Go validation to exercise the actual persistence boundary.
	for _, assignment := range []string{
		"status='unknown'", "phase='agent'", "mode='unknown'", "trigger='unknown'", "outcome='unknown'",
		"issue_number=0", "input_tokens=-1", "cost_micros=-1", "test_attempts=-1", "turns=-1", "dry_run=2",
		"pr_number=-1", "created_at=-1", "heartbeat_at=-1", "limits_json='invalid'", "cost_micros=0.5",
	} {
		//nolint:gosec // G202: assignments are fixed test cases; the run ID is bound.
		if _, err := s.db.ExecContext(t.Context(), "UPDATE runs SET "+assignment+" WHERE id=?", r.ID); err == nil {
			t.Errorf("accepted %s", assignment)
		}
	}
	if _, err := s.AppendEvent(t.Context(), Event{RunID: 999, Type: "orphan"}); err == nil {
		t.Error("foreign-key violation accepted")
	}
	if _, err := s.RecordApproval(t.Context(), Approval{RunID: r.ID, SpecVersion: 1, SpecHash: hashOf("none"), Decision: "approve", Actor: "owner", Channel: "cli"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("approval without spec = %v", err)
	}
	if err := s.SaveArtifact(t.Context(), Artifact{RunID: r.ID, Kind: "test", Attempt: -1}); err == nil {
		t.Error("negative attempt accepted")
	}
	if _, err := s.RecordStep(t.Context(), Step{RunID: r.ID, Phase: PhaseCode, Kind: "tool", Status: "ok", DurationMS: ptr(int64(-1))}); err == nil {
		t.Error("negative step duration accepted")
	}
	if _, err := s.SaveSpec(t.Context(), Spec{RunID: r.ID, BodyMD: "body", Hash: "not-sha256", Source: "agent"}); err == nil {
		t.Error("invalid spec hash accepted")
	}
}

func TestStaleRecoveryRollsBackOnEventFailure(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	old := timestamp(time.Now().Add(-time.Hour))
	r := mustRun(t, s, Run{Status: StatusRunning, HeartbeatAt: &old})
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER reject_event BEFORE INSERT ON run_events BEGIN SELECT RAISE(ABORT,'test recovery event failure'); END`); err != nil {
		t.Fatal(err)
	}
	count, err := s.FailStaleRuns(t.Context(), time.Now().Add(-time.Minute))
	if err == nil || count != 0 {
		t.Fatalf("failed recovery = %d, %v", count, err)
	}
	if diff := cmp.Diff(r, mustGetRun(t, s, r.ID)); diff != "" {
		t.Fatalf("stale recovery partially committed:\n%s", diff)
	}
}

func TestMigrationRollback(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	m := migration{version: 3, name: "0003_test.sql", checksum: hashOf("test"), sql: `CREATE TABLE migration_probe(id INTEGER); INSERT INTO missing_table VALUES(1);`}
	if err := s.applyMigration(t.Context(), m, 3); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var tables, ledger int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE name='migration_probe'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations WHERE version=3`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || ledger != 0 {
		t.Fatal("failed migration left a table or ledger entry behind")
	}
	m.sql = `CREATE TABLE migration_probe(id INTEGER);`
	if err := s.applyMigration(t.Context(), m, 3); err != nil {
		t.Fatal(err)
	}
}

func TestReactivationConstraintRollsBack(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	old := mustRun(t, s, Run{Status: StatusFailed})
	mustRun(t, s, Run{})
	if err := s.SetRunStatus(t.Context(), old.ID, StatusFailed, StatusQueued, RunPatch{Phase: PhasePreflight}); !errors.Is(err, ErrActiveRunExists) {
		t.Fatalf("active-slot transition collision: %v", err)
	}
	if diff := cmp.Diff(old, mustGetRun(t, s, old.ID)); diff != "" {
		t.Fatal(diff)
	}
	if len(mustEvents(t, s, old.ID, 0)) != 0 {
		t.Fatal("conflicting transition committed an event")
	}
}
