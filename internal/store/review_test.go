package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// The 0004 rebuild runs with foreign keys off. Child rows must survive the
// DROP of the referenced runs table rather than cascading away.
func TestReviewMigrationPreservesChildRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "old.sqlite")
	query := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "foreign_keys(ON)"} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db}
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	var review migration
	for _, m := range migrations {
		if m.name == "0004_review.sql" {
			review = m
			break
		}
		if err := s.applyMigration(t.Context(), m, int64(len(migrations))); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO runs(id,repo,issue_number,mode,trigger,provider,refine_model,code_model,dry_run,status,phase,limits_json,created_at,updated_at) VALUES(1,'o/r',1,'fix','cli','p','m','m',0,'succeeded','publish','{}',1,1)`,
		`INSERT INTO runs(id,repo,issue_number,mode,retry_of,trigger,provider,refine_model,code_model,dry_run,status,phase,limits_json,created_at,updated_at) VALUES(2,'o/r',1,'fix',1,'cli','p','m','m',0,'running','verify','{}',1,1)`,
		`INSERT INTO specs(run_id,version,body_md,hash,source,created_at) VALUES(1,1,'b','` + hashOf("b") + `','agent',1)`,
		`INSERT INTO approvals(run_id,spec_version,spec_hash,decision,actor,channel,created_at) VALUES(1,1,'` + hashOf("b") + `','approve','a','cli',1)`,
		`INSERT INTO steps(run_id,seq,phase,kind,status,started_at) VALUES(1,1,'verify','test','ok',1)`,
		`INSERT INTO run_events(run_id,seq,type,payload_json,created_at) VALUES(1,1,'x','{}',1)`,
		`INSERT INTO run_artifacts(run_id,kind,attempt,content,created_at) VALUES(1,'diff',0,'d',1)`,
		// A deleted newer run leaves the sequence above MAX(id).
		`INSERT INTO runs(id,repo,issue_number,mode,trigger,provider,refine_model,code_model,dry_run,status,phase,limits_json,created_at,updated_at) VALUES(9,'o/r',2,'fix','cli','p','m','m',0,'failed','publish','{}',1,1)`,
		`DELETE FROM runs WHERE id=9`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.applyMigration(t.Context(), review, int64(len(migrations))); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"runs": 2, "specs": 1, "approvals": 1, "steps": 1, "run_events": 1, "run_artifacts": 1, "reviews": 0} {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
	var seq int
	if err := db.QueryRowContext(t.Context(), `SELECT seq FROM sqlite_sequence WHERE name='runs'`).Scan(&seq); err != nil || seq != 9 {
		t.Fatalf("run ID sequence = %d, %v; IDs could be reused", seq, err)
	}
	var fk int
	if err := db.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys not restored: %d %v", fk, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE runs SET phase='review' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO steps(run_id,seq,phase,kind,status,started_at) VALUES(2,1,'review','llm','ok',1)`); err != nil {
		t.Fatal(err)
	}
	// Cascades still work after the rebuild.
	if _, err := db.ExecContext(t.Context(), `DELETE FROM runs WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	var steps int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM steps WHERE run_id=2`).Scan(&steps); err != nil || steps != 0 {
		t.Fatalf("cascade after rebuild: %d %v", steps, err)
	}
}

func TestReviewSelectionFrozenAndReviewsBound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	run := mustRun(t, s, Run{Status: StatusAwaitingApproval, Phase: PhaseRefine})
	spec, err := s.SaveSpec(t.Context(), Spec{RunID: run.ID, BodyMD: "v1", Hash: hashOf("v1"), Source: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	run = mustGetRun(t, s, run.ID)
	selection := json.RawMessage(`{"enabled":true,"model":"m"}`)
	if err := s.DecideRun(t.Context(), run.Status, StatusApproved, Approval{RunID: run.ID, SpecVersion: spec.Version, SpecHash: spec.Hash, Decision: "approve", Actor: "a", Channel: "cli", Review: selection}, run.Generation); err != nil {
		t.Fatal(err)
	}
	run = mustGetRun(t, s, run.ID)
	if string(run.ReviewJSON) != string(selection) {
		t.Fatalf("selection not frozen: %s", run.ReviewJSON)
	}
	review := Review{RunID: run.ID, SpecVersion: spec.Version, SpecHash: spec.Hash, DiffHash: hashOf("diff"), Verdict: "approve", Provider: "p", Model: "m"}
	if _, err := s.SaveReview(t.Context(), review); !errors.Is(err, ErrConflict) {
		t.Fatalf("review saved before execution: %v", err)
	}
	if err := s.StartCode(t.Context(), run.ID, spec.Version, spec.Hash, run.Generation); err != nil {
		t.Fatal(err)
	}
	run = mustGetRun(t, s, run.ID)
	if err := s.SetRunStatus(t.Context(), run.ID, StatusRunning, StatusRunning, RunPatch{Phase: PhaseReview, ExpectedGeneration: &run.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSpec(t.Context(), Spec{RunID: run.ID, BodyMD: "v2", Hash: hashOf("v2"), Source: "ui_edit"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("spec changed during review: %v", err)
	}
	stale := review
	stale.SpecHash = hashOf("other")
	if _, err := s.SaveReview(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale spec review: %v", err)
	}
	bad := review
	bad.Verdict = "maybe"
	if _, err := s.SaveReview(t.Context(), bad); err == nil {
		t.Fatal("invalid verdict accepted")
	}
	first, err := s.SaveReview(t.Context(), review)
	if err != nil || first.Round != 1 {
		t.Fatalf("first review: %+v %v", first, err)
	}
	review.Round = 1
	if _, err := s.SaveReview(t.Context(), review); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate round: %v", err)
	}
	review.Round = 0
	review.Verdict = "incomplete"
	if second, err := s.SaveReview(t.Context(), review); err != nil || second.Round != 2 {
		t.Fatalf("second review: %+v %v", second, err)
	}
	list, err := s.ListReviews(t.Context(), run.ID)
	if err != nil || len(list) != 2 || list[0].Verdict != "approve" || string(list[1].FindingsJSON) != "[]" {
		t.Fatalf("reviews: %+v %v", list, err)
	}
	if !IsSchemaIdentifier("phase", "review") {
		t.Fatal("review phase is not a schema identifier")
	}
}

func TestStaleReviewPhaseRecovered(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	old := timestamp(time.Now().Add(-time.Hour))
	r := mustRun(t, s, Run{Status: StatusRunning, Phase: PhaseReview, HeartbeatAt: &old})
	if n, err := s.FailStaleRuns(t.Context(), time.Now().Add(-time.Minute)); err != nil || n != 1 {
		t.Fatalf("recovered %d, %v", n, err)
	}
	if got := mustGetRun(t, s, r.ID); got.Status != StatusFailed || got.ErrorKind != "interrupted" || got.Phase != PhaseReview {
		t.Fatalf("stale review run: %+v", got)
	}
}

func TestForeignKeysOffMarker(t *testing.T) {
	t.Parallel()
	for sql, want := range map[string]bool{
		"-- patchbot:foreign_keys=off\nDROP TABLE x;":          true,
		"\uFEFF-- patchbot:foreign_keys=off \r\nDROP TABLE x;": true,
		"CREATE TABLE x(id INTEGER);":                          false,
	} {
		got, err := needsForeignKeysOff(migration{name: "m", sql: sql})
		if err != nil || got != want {
			t.Errorf("%q: %t %v", sql, got, err)
		}
	}
	if _, err := needsForeignKeysOff(migration{name: "m", sql: "-- rebuild\nDROP TABLE runs;"}); err == nil {
		t.Fatal("unmarked DROP TABLE accepted")
	}
}
