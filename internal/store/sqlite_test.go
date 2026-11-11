package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func openTestStore(t *testing.T) *SQLite {
	t.Helper()
	return openTestPath(t, filepath.Join(t.TempDir(), "patchbot.db"))
}

func openTestPath(t *testing.T, path string) *SQLite {
	t.Helper()
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func mustRun(t *testing.T, s *SQLite, run Run) Run {
	t.Helper()
	if run.Repo == "" {
		run.Repo = "owner/repo"
	}
	if run.IssueNumber == 0 {
		run.IssueNumber = 12
	}
	r, err := s.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustGetRun(t *testing.T, s *SQLite, id int64) Run {
	t.Helper()
	r, err := s.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustEvents(t *testing.T, s *SQLite, runID, after int64) []Event {
	t.Helper()
	events, err := s.EventsAfter(t.Context(), runID, after, 500)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func ptr[T any](v T) *T { return &v }

func hashOf(body string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(body))) }

func TestMigrations(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "db with ? and #.sqlite")
	s := openTestPath(t, path)
	var version, applied int64
	var name, checksum string
	if err := s.db.QueryRowContext(t.Context(), `SELECT version,name,checksum,applied_at FROM schema_migrations WHERE version=1`).Scan(&version, &name, &checksum, &applied); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFiles.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "0001_init.sql" || checksum != hashOf(string(body)) || applied <= 0 {
		t.Fatalf("unexpected migration ledger: %d %q %q %d", version, name, checksum, applied)
	}
	var count int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		('runs','specs','approvals','questions','steps','run_events','run_artifacts','actions')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatalf("domain tables = %d, want 8", count)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("database mode = %o, want 600", info.Mode().Perm())
	}
	mustRun(t, s, Run{})

	second := openTestPath(t, path)
	var reapplied int64
	if err := second.db.QueryRowContext(t.Context(), `SELECT applied_at FROM schema_migrations WHERE version=1`).Scan(&reapplied); err != nil {
		t.Fatal(err)
	}
	if reapplied != applied {
		t.Error("second open reapplied migration")
	}
	if r := mustGetRun(t, second, 1); r.IssueNumber != 12 {
		t.Fatal("second open lost data")
	}

	if _, err := s.db.ExecContext(t.Context(), `UPDATE schema_migrations SET checksum='tampered'`); err != nil {
		t.Fatal(err)
	}
	broken, err := Open(t.Context(), path)
	if err == nil {
		_ = broken.Close()
		t.Fatal("tampered checksum was accepted")
	}
	if !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("unexpected checksum error: %v", err)
	}
}

func TestUnknownMigration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	s := openTestPath(t, path)
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO schema_migrations VALUES(999,'0999_future.sql','future',1)`); err != nil {
		t.Fatal(err)
	}
	future, err := Open(t.Context(), path)
	if err == nil {
		_ = future.Close()
		t.Fatal("accepted unknown migration")
	}
	if !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPragmasOnReconnection(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	for attempt := range 2 {
		if attempt == 1 {
			s.db.SetMaxIdleConns(0)
			s.db.SetMaxIdleConns(1)
		}
		for _, pragma := range []struct {
			name string
			want any
		}{
			{"journal_mode", "wal"}, {"foreign_keys", int64(1)}, {"busy_timeout", int64(5000)}, {"synchronous", int64(1)},
		} {
			var got any
			if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+pragma.name).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(pragma.want, got); diff != "" {
				t.Errorf("connection %d %s (-want +got):\n%s", attempt, pragma.name, diff)
			}
		}
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("max connections = %d, want 1", got)
	}
}

func TestActiveRunUniqueness(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusQueued, StatusRunning, StatusNeedsInfo, StatusAwaitingApproval, StatusApproved} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			r := mustRun(t, s, Run{Repo: "Owner/Repo", Status: status})
			if r.Repo != "owner/repo" || !status.IsActive() {
				t.Fatal("invalid normalized active run")
			}
			_, err := s.CreateRun(t.Context(), Run{Repo: "owner/repo", IssueNumber: 12})
			if !errors.Is(err, ErrActiveRunExists) {
				t.Fatalf("duplicate = %v, want ErrActiveRunExists", err)
			}
			if err := s.SetRunStatus(t.Context(), r.ID, status, StatusSucceeded, RunPatch{Phase: PhasePublish}); err != nil {
				t.Fatal(err)
			}
			next := mustRun(t, s, Run{Repo: "owner/repo"})
			if next.ID == r.ID {
				t.Fatal("new run reused old ID")
			}
		})
	}
	for _, status := range []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusRejected, "invalid"} {
		if status.IsActive() {
			t.Errorf("%s should not be active", status)
		}
	}
}

func TestRunRoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	now := time.Date(2026, time.November, 10, 13, 0, 0, 123456789, time.FixedZone("offset", 20700))
	r := mustRun(t, s, Run{
		IssueTitle: "repair", Mode: "fix", Trigger: "ui", Provider: "fake", RefineModel: "refine-model", CodeModel: "code-model",
		DryRun: true, Status: StatusRunning, Phase: PhaseCode, Branch: "patchbot/issue-12", BaseSHA: "base", HeadSHA: "head",
		PRNumber: ptr(int64(123)), PRURL: "https://example.test/pr/123", PRDraft: true, Summary: "summary", TestsStatus: "passed",
		TestAttempts: 2, Turns: 3, InputTokens: ptr(int64(0)), OutputTokens: ptr(int64(13)), CacheReadTokens: ptr(int64(1)),
		CacheWriteTokens: ptr(int64(2)), CostMicros: ptr(int64(42)), LimitsJSON: json.RawMessage(`{"max_turns":40}`), CreatedAt: now,
	})
	got := mustGetRun(t, s, r.ID)
	if diff := cmp.Diff(r, got); diff != "" {
		t.Fatalf("run round trip (-want +got):\n%s", diff)
	}
	if got.CreatedAt.Location() != time.UTC || got.CreatedAt.Nanosecond() != 123000000 {
		t.Fatal("timestamp is not UTC milliseconds")
	}
	unknown := mustRun(t, s, Run{IssueNumber: 13})
	if unknown.CostMicros != nil || unknown.InputTokens != nil || unknown.FinishedAt != nil || unknown.HeartbeatAt != nil {
		t.Fatal("unknown numeric/time fields did not round trip as NULL")
	}
}

func TestStatusCASAndAtomicEvent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	ctx := t.Context()
	if err := s.SetRunStatus(ctx, r.ID, StatusRunning, StatusSucceeded, RunPatch{Phase: PhasePublish}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong from: %v", err)
	}
	if got := mustGetRun(t, s, r.ID); got.Status != StatusQueued {
		t.Fatal("failed CAS changed status")
	}
	if got := mustEvents(t, s, r.ID, 0); len(got) != 0 {
		t.Fatal("failed CAS appended an event")
	}
	if err := s.SetRunStatus(ctx, r.ID, StatusQueued, StatusRunning, RunPatch{Phase: PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	got := mustGetRun(t, s, r.ID)
	if got.Status != StatusRunning || got.Phase != PhaseRefine || got.StartedAt == nil || got.HeartbeatAt == nil {
		t.Fatalf("unexpected running row: %+v", got)
	}
	events := mustEvents(t, s, r.ID, 0)
	if len(events) != 1 || events[0].Type != "run.status" {
		t.Fatalf("events: %+v", events)
	}
	var payload map[string]string
	if err := json.Unmarshal(events[0].PayloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]string{"from": "queued", "status": "running", "phase": "refine"}, payload); diff != "" {
		t.Fatal(diff)
	}

	// Make event insertion fail after the UPDATE has succeeded. The status and
	// all its timestamps must roll back, proving both writes share a transaction.
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_event BEFORE INSERT ON run_events BEGIN SELECT RAISE(ABORT,'test event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStatus(ctx, r.ID, StatusRunning, StatusFailed, RunPatch{Phase: PhaseVerify, ErrorKind: ptr("test")}); err == nil {
		t.Fatal("event failure did not fail transition")
	}
	if diff := cmp.Diff(got, mustGetRun(t, s, r.ID)); diff != "" {
		t.Fatalf("transition was not rolled back:\n%s", diff)
	}
	if len(mustEvents(t, s, r.ID, 0)) != 1 {
		t.Fatal("failed event was committed")
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_event`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStatus(ctx, r.ID, StatusRunning, StatusSucceeded, RunPatch{Phase: PhasePublish, Outcome: ptr("no_changes")}); err != nil {
		t.Fatal(err)
	}
	got = mustGetRun(t, s, r.ID)
	if got.FinishedAt == nil || got.HeartbeatAt != nil || got.Outcome != "no_changes" {
		t.Fatalf("terminal metadata: %+v", got)
	}
	if err := s.Heartbeat(ctx, r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal heartbeat: %v", err)
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{})
	other := mustRun(t, s, Run{IssueNumber: 13})
	for _, run := range []Run{r, other} {
		for want := int64(1); want <= 5; want++ {
			seq, err := s.AppendEvent(t.Context(), Event{RunID: run.ID, Type: "step.finished", PayloadJSON: json.RawMessage(`{"ok":true}`)})
			if err != nil {
				t.Fatal(err)
			}
			if seq != want {
				t.Fatalf("run %d seq = %d, want %d", run.ID, seq, want)
			}
		}
	}
	later := mustEvents(t, s, r.ID, 2)
	var seqs []int64
	for _, event := range later {
		seqs = append(seqs, event.Seq)
		if event.RunID != r.ID {
			t.Fatal("events leaked across runs")
		}
	}
	if diff := cmp.Diff([]int64{3, 4, 5}, seqs); diff != "" {
		t.Fatal(diff)
	}
	limited, err := s.EventsAfter(t.Context(), r.ID, 2, 1)
	if err != nil || len(limited) != 1 || limited[0].Seq != 3 {
		t.Fatalf("limited events: %+v, %v", limited, err)
	}
	if len(mustEvents(t, s, r.ID, 5)) != 0 {
		t.Fatal("last event included in exclusive after")
	}
	if _, err := s.AppendEvent(t.Context(), Event{RunID: r.ID, Type: "bad", PayloadJSON: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	seq, err := s.AppendEvent(t.Context(), Event{RunID: r.ID, Type: "valid"})
	if err != nil || seq != 6 {
		t.Fatalf("failed insert consumed a sequence: %d, %v", seq, err)
	}
}

func TestConcurrentEventAppends(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	stores := []*SQLite{openTestPath(t, path), openTestPath(t, path)}
	r := mustRun(t, stores[0], Run{})
	const workers, appends = 8, 25
	seqs := make(chan int64, workers*appends)
	errs := make(chan error, workers*appends)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for range appends {
				seq, err := stores[worker%len(stores)].AppendEvent(t.Context(), Event{RunID: r.ID, Type: "concurrent"})
				if err != nil {
					errs <- err
				} else {
					seqs <- seq
				}
			}
		})
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var got []int64
	for seq := range seqs {
		got = append(got, seq)
	}
	slices.Sort(got)
	want := make([]int64, workers*appends)
	for i := range want {
		want[i] = int64(i + 1)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("concurrent sequence allocation (-want +got):\n%s", diff)
	}
	events := mustEvents(t, stores[0], r.ID, 0)
	if len(events) != len(want) {
		t.Fatalf("persisted events = %d", len(events))
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatal("events were not persisted in sequence order")
		}
	}
}

func TestFailStaleRuns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	cutoff := timestamp(time.Now().Add(-time.Minute))
	old, fresh := cutoff.Add(-time.Millisecond), cutoff.Add(time.Millisecond)
	cases := []struct {
		status     Status
		heartbeat  *time.Time
		wantFailed bool
	}{
		{StatusRunning, &old, true}, {StatusRunning, &fresh, false}, {StatusRunning, &cutoff, false},
		{StatusQueued, &old, false}, {StatusAwaitingApproval, &old, false}, {StatusNeedsInfo, &old, false},
		{StatusApproved, &old, false}, {StatusFailed, &old, false}, {StatusCancelled, &old, false},
	}
	var runs []Run
	for i, tc := range cases {
		runs = append(runs, mustRun(t, s, Run{IssueNumber: int64(i + 1), Status: tc.status, HeartbeatAt: tc.heartbeat}))
	}
	unknown := mustRun(t, s, Run{IssueNumber: 100, Status: StatusRunning})
	if _, err := s.db.ExecContext(t.Context(), `UPDATE runs SET heartbeat_at=NULL WHERE id=?`, unknown.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(t.Context(), runs[1].ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.FailStaleRuns(t.Context(), cutoff)
	if err != nil || count != 1 {
		t.Fatalf("stale recovery = %d, %v", count, err)
	}
	for i, tc := range cases {
		got := mustGetRun(t, s, runs[i].ID)
		if tc.wantFailed {
			if got.Status != StatusFailed || got.ErrorKind != "interrupted" || got.FinishedAt == nil || got.Outcome != "error" {
				t.Errorf("stale run: %+v", got)
			}
			if len(mustEvents(t, s, got.ID, 0)) != 1 {
				t.Error("stale failure missing atomic event")
			}
		} else if got.Status != tc.status || len(mustEvents(t, s, got.ID, 0)) != 0 {
			t.Errorf("non-stale run changed: %+v", got)
		}
	}
	if got := mustGetRun(t, s, unknown.ID); got.Status != StatusRunning {
		t.Error("unknown heartbeat was treated as old")
	}
	count, err = s.FailStaleRuns(t.Context(), cutoff)
	if err != nil || count != 0 {
		t.Fatalf("second recovery = %d, %v", count, err)
	}
}

func TestApprovalGate(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	r := mustRun(t, s, Run{Status: StatusAwaitingApproval, Phase: PhaseRefine})
	spec, err := s.SaveSpec(t.Context(), Spec{RunID: r.ID, BodyMD: "spec v1", Hash: hashOf("v1"), Source: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != 1 {
		t.Fatalf("first version = %d", spec.Version)
	}
	if _, err := s.ValidApproval(t.Context(), r.ID, 1, spec.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unapproved spec: %v", err)
	}
	a, err := s.RecordApproval(t.Context(), Approval{RunID: r.ID, SpecVersion: 1, SpecHash: spec.Hash, Decision: "approve", Actor: "owner", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := s.ValidApproval(t.Context(), r.ID, 1, spec.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, valid); diff != "" {
		t.Fatal(diff)
	}
	if _, err := s.ValidApproval(t.Context(), r.ID, 1, hashOf("wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong hash: %v", err)
	}
	if _, err := s.ValidApproval(t.Context(), r.ID, 2, spec.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong version: %v", err)
	}
	if err := s.SetRunStatus(t.Context(), r.ID, StatusAwaitingApproval, StatusApproved, RunPatch{Phase: PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	// Even identical content needs approval again when its version changes.
	spec2, err := s.SaveSpec(t.Context(), Spec{RunID: r.ID, BodyMD: spec.BodyMD, Hash: spec.Hash, Source: "ui_edit"})
	if err != nil {
		t.Fatal(err)
	}
	if spec2.Version != 2 {
		t.Fatalf("second version = %d", spec2.Version)
	}
	for _, version := range []int64{1, 2} {
		if _, err := s.ValidApproval(t.Context(), r.ID, version, spec.Hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stale/unapproved version %d: %v", version, err)
		}
	}
	if got := mustGetRun(t, s, r.ID); got.Status != StatusAwaitingApproval {
		t.Fatal("spec edit left run approved")
	}
	if _, err := s.RecordApproval(t.Context(), a); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval of superseded version: %v", err)
	}
	a.SpecVersion, a.SpecHash = 2, hashOf("wrong")
	if _, err := s.RecordApproval(t.Context(), a); !errors.Is(err, ErrConflict) {
		t.Fatalf("record wrong hash: %v", err)
	}
	a.SpecHash = spec2.Hash
	if _, err := s.RecordApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	a.Decision = "revise"
	if _, err := s.RecordApproval(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidApproval(t.Context(), r.ID, 2, spec2.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revise did not invalidate approval: %v", err)
	}
	latest, err := s.LatestSpec(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(spec2, latest); diff != "" {
		t.Fatal(diff)
	}
	if _, err := s.SaveSpec(t.Context(), Spec{RunID: r.ID, Version: 2, BodyMD: "stale", Hash: spec.Hash, Source: "ui_edit"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale spec edit: %v", err)
	}
}

func TestRunCursorPagination(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	tie := timestamp(time.Now().Add(-time.Hour))
	var expected []int64
	for i := range 7 {
		r := mustRun(t, s, Run{IssueNumber: int64(i + 1), CreatedAt: tie, Status: StatusSucceeded})
		expected = append(expected, r.ID)
	}
	slices.Reverse(expected)
	mustRun(t, s, Run{Repo: "other/repo", IssueNumber: 20, CreatedAt: tie})
	mustRun(t, s, Run{IssueNumber: 30, CreatedAt: tie})
	filter := RunFilter{Repo: "OWNER/REPO", Statuses: []Status{StatusSucceeded, StatusFailed}, Limit: 3}
	var got []int64
	for pageNumber := 0; ; pageNumber++ {
		page, err := s.ListRuns(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Runs) > filter.Limit {
			t.Fatal("page exceeds limit")
		}
		for _, r := range page.Runs {
			got = append(got, r.ID)
		}
		if pageNumber == 0 {
			// A concurrent insertion at the front, even with the same timestamp,
			// must not shift or duplicate any entry in subsequent pages.
			mustRun(t, s, Run{IssueNumber: 99, CreatedAt: tie, Status: StatusSucceeded})
		}
		if page.NextCursor == "" {
			break
		}
		if pageNumber > 5 {
			t.Fatal("pagination did not terminate")
		}
		filter.Cursor = page.NextCursor
	}
	if diff := cmp.Diff(expected, got); diff != "" {
		t.Fatalf("stable pages (-want +got):\n%s", diff)
	}
	page, err := s.ListRuns(ctx, RunFilter{Repo: "owner/repo", IssueNumber: 2})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].IssueNumber != 2 {
		t.Fatalf("issue filter: %+v %v", page, err)
	}
	for _, cursor := range []string{"not-a-cursor", "e30", "bnVsbA"} {
		if _, err := s.ListRuns(ctx, RunFilter{Cursor: cursor}); err == nil {
			t.Errorf("invalid cursor %q accepted", cursor)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.CreateRun(ctx, Run{Repo: "owner/repo", IssueNumber: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create: %v", err)
	}
}
