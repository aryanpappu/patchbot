package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// SQLite implements Store with a single writer connection and WAL persistence.
type SQLite struct {
	db         *sql.DB
	eventMu    sync.RWMutex
	eventSinks map[*eventSink]struct{}
}

var _ Store = (*SQLite)(nil)

// Open creates a private on-disk database if needed, configures every new
// connection through the DSN, and applies embedded migrations before returning.
func Open(ctx context.Context, path string) (*SQLite, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("store requires an on-disk database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database path: %w", err)
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errors.New("database must be a regular file with mode 0600")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	//nolint:gosec // G304: opening the caller-selected database file is the purpose of Open.
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	dsn := &url.URL{Scheme: "file", Path: abs}
	query := url.Values{}
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(ON)", "synchronous(NORMAL)"} {
		query.Add("_pragma", pragma)
	}
	query.Set("_txlock", "immediate")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &SQLite{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return s, nil
}

// Close releases database connections.
func (s *SQLite) Close() error { return s.db.Close() }

// withTx only accepts internal operations; no caller-controlled code is run
// while holding the connection or SQLite write lock.
func (s *SQLite) withTx(ctx context.Context, operation func(*eventTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	tracked := &eventTx{Tx: tx}
	if err := operation(tracked); err != nil {
		return mapError(err)
	}
	if err := mapError(tx.Commit()); err != nil {
		return err
	}
	for _, event := range tracked.events {
		s.publishEvent(event)
	}
	return nil
}

func mapError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var sqliteErr *sqlite.Error
	// SQLITE_CONSTRAINT_UNIQUE (2067) identifies a unique-index collision.
	// Check the columns too: unrelated primary/unique constraints must not be
	// mislabeled as an active-run collision.
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == 2067 &&
		strings.Contains(sqliteErr.Error(), "runs.repo, runs.issue_number") {
		return fmt.Errorf("%w: %w", ErrActiveRunExists, err)
	}
	return err
}

func timestamp(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now()
	}
	return time.UnixMilli(t.UnixMilli()).UTC()
}

func optionalMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func optionalTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.UnixMilli(n.Int64).UTC()
	return &t
}

// cappedText preserves UTF-8 while bounding persisted transcripts.
func cappedText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	for maxBytes > 0 && !utf8.RuneStart(text[maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

const runColumns = `id,repo,issue_number,issue_title,mode,retry_of,trigger,
provider,refine_model,code_model,dry_run,status,phase,outcome,base_sha,head_sha,branch,
pr_number,pr_url,pr_draft,summary,tests_status,test_attempts,turns,input_tokens,
output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,limits_json,
error_kind,error_message,created_at,updated_at,started_at,finished_at,heartbeat_at,waiting_since,generation`

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (Run, error) {
	var r Run
	var created, updated int64
	var limits string
	var started, finished, heartbeat, waiting sql.NullInt64
	err := row.Scan(&r.ID, &r.Repo, &r.IssueNumber, &r.IssueTitle, &r.Mode, &r.RetryOf, &r.Trigger,
		&r.Provider, &r.RefineModel, &r.CodeModel, &r.DryRun, &r.Status, &r.Phase, &r.Outcome,
		&r.BaseSHA, &r.HeadSHA, &r.Branch, &r.PRNumber, &r.PRURL, &r.PRDraft, &r.Summary,
		&r.TestsStatus, &r.TestAttempts, &r.Turns, &r.InputTokens, &r.OutputTokens,
		&r.CacheReadTokens, &r.CacheWriteTokens, &r.CostMicros, &limits,
		&r.ErrorKind, &r.ErrorMessage, &created, &updated, &started, &finished, &heartbeat, &waiting, &r.Generation)
	r.LimitsJSON = json.RawMessage(limits)
	r.CreatedAt, r.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	r.StartedAt, r.FinishedAt = optionalTime(started), optionalTime(finished)
	r.HeartbeatAt, r.WaitingSince = optionalTime(heartbeat), optionalTime(waiting)
	return r, mapError(err)
}

// CreateRun inserts a run, assigning its ID and default lifecycle metadata.
func (s *SQLite) CreateRun(ctx context.Context, r Run) (Run, error) {
	r.Repo = strings.ToLower(strings.TrimSpace(r.Repo))
	if r.Mode == "" {
		r.Mode = "fix"
	}
	if r.Status == "" {
		r.Status = StatusQueued
	}
	if r.Phase == "" {
		r.Phase = PhasePreflight
	}
	if r.Trigger == "" {
		r.Trigger = "cli"
	}
	r.ErrorMessage = cappedText(r.ErrorMessage, 2000)
	r.CreatedAt = timestamp(r.CreatedAt)
	r.UpdatedAt = r.CreatedAt
	if r.Status == StatusRunning {
		if r.StartedAt == nil {
			r.StartedAt = &r.CreatedAt
		}
		if r.HeartbeatAt == nil {
			r.HeartbeatAt = &r.CreatedAt
		}
	}
	if r.Status == StatusAwaitingApproval || r.Status == StatusNeedsInfo {
		if r.WaitingSince == nil {
			r.WaitingSince = &r.CreatedAt
		}
	}
	if !r.Status.IsActive() && r.FinishedAt == nil {
		r.FinishedAt = &r.CreatedAt
	}
	args := []any{r.Repo, r.IssueNumber, r.IssueTitle, r.Mode, r.RetryOf, r.Trigger,
		r.Provider, r.RefineModel, r.CodeModel, r.DryRun, r.Status, r.Phase, r.Outcome,
		r.BaseSHA, r.HeadSHA, r.Branch, r.PRNumber, r.PRURL, r.PRDraft, r.Summary,
		r.TestsStatus, r.TestAttempts, r.Turns, r.InputTokens, r.OutputTokens,
		r.CacheReadTokens, r.CacheWriteTokens, r.CostMicros, jsonText(r.LimitsJSON),
		r.ErrorKind, r.ErrorMessage, r.CreatedAt.UnixMilli(), r.UpdatedAt.UnixMilli(),
		optionalMillis(r.StartedAt), optionalMillis(r.FinishedAt), optionalMillis(r.HeartbeatAt), optionalMillis(r.WaitingSince), int64(0)}
	query := "INSERT INTO runs(" + strings.TrimPrefix(runColumns, "id,") + ") VALUES(" + placeholders(len(args)) + ") RETURNING " + runColumns
	return scanRun(s.db.QueryRowContext(ctx, query, args...))
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// GetRun retrieves a run by ID.
func (s *SQLite) GetRun(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE id=?", id))
}

type runCursor struct{ CreatedAt, ID int64 }

// ListRuns uses keyset pagination. Inserts at the front never shift later pages.
func (s *SQLite) ListRuns(ctx context.Context, f RunFilter) (RunPage, error) {
	page := RunPage{Runs: []Run{}}
	where := []string{"1=1"}
	args := []any{}
	if f.Repo != "" {
		where = append(where, "repo=?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Repo)))
	}
	if f.IssueNumber != 0 {
		where = append(where, "issue_number=?")
		args = append(args, f.IssueNumber)
	}
	if len(f.Statuses) != 0 {
		where = append(where, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, status := range f.Statuses {
			args = append(args, status)
		}
	}
	if f.Cursor != "" {
		var cursor runCursor
		data, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil {
			return page, fmt.Errorf("invalid run cursor: %w", err)
		}
		if err := json.Unmarshal(data, &cursor); err != nil {
			return page, fmt.Errorf("invalid run cursor: %w", err)
		}
		if cursor.ID <= 0 || cursor.CreatedAt < 0 {
			return page, errors.New("invalid run cursor values")
		}
		where = append(where, "(created_at,id) < (?,?)")
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	limit := pageLimit(f.Limit)
	args = append(args, limit+1)
	//nolint:gosec // G202: SQL fragments are constants; all filter and cursor values use placeholders.
	rows, err := s.db.QueryContext(ctx, "SELECT "+runColumns+" FROM runs WHERE "+strings.Join(where, " AND ")+" ORDER BY created_at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return page, err
		}
		page.Runs = append(page.Runs, r)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Runs) > limit {
		page.Runs = page.Runs[:limit]
		last := page.Runs[limit-1]
		data, err := json.Marshal(runCursor{last.CreatedAt.UnixMilli(), last.ID})
		if err != nil {
			return page, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

func pageLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// SetRunStatus changes status and phase via compare-and-set and appends a
// run.status event atomically. An unexpected prior status returns ErrConflict.
func (s *SQLite) SetRunStatus(ctx context.Context, id int64, from, to Status, patch RunPatch) error {
	if patch.ErrorMessage != nil {
		message := cappedText(*patch.ErrorMessage, 2000)
		patch.ErrorMessage = &message
	}
	return s.withTx(ctx, func(tx *eventTx) error {
		now := time.Now().UTC().UnixMilli()
		var finished, heartbeat, waiting any
		if !to.IsActive() {
			finished = now
		}
		if to == StatusRunning || (to == StatusApproved && (patch.Phase == PhaseClone || patch.Phase == PhaseBaseline)) {
			heartbeat = now
		}
		if to == StatusAwaitingApproval || to == StatusNeedsInfo {
			waiting = now
		}
		result, err := tx.ExecContext(ctx, `UPDATE runs SET generation=generation+1,status=?,phase=?,updated_at=?,issue_title=COALESCE(?,issue_title),
			outcome=COALESCE(?,outcome),error_kind=COALESCE(?,error_kind),error_message=COALESCE(?,error_message),
			base_sha=COALESCE(?,base_sha),head_sha=COALESCE(?,head_sha),branch=COALESCE(?,branch),
			pr_number=COALESCE(?,pr_number),pr_url=COALESCE(?,pr_url),pr_draft=COALESCE(?,pr_draft),
			summary=COALESCE(?,summary),tests_status=COALESCE(?,tests_status),
			started_at=CASE WHEN ?='running' THEN COALESCE(started_at,?) ELSE started_at END,
			finished_at=?,heartbeat_at=?,waiting_since=CASE WHEN status=? THEN waiting_since ELSE ? END
			WHERE id=? AND status=? AND (? IS NULL OR generation=?)`, to, patch.Phase, now, patch.IssueTitle, patch.Outcome, patch.ErrorKind, patch.ErrorMessage,
			patch.BaseSHA, patch.HeadSHA, patch.Branch, patch.PRNumber, patch.PRURL, patch.PRDraft, patch.Summary,
			patch.TestsStatus, to, now, finished, heartbeat, to, waiting, id, from, patch.ExpectedGeneration, patch.ExpectedGeneration)
		if err != nil {
			return err
		}
		if err := requireAffected(result, ErrConflict); err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			From      Status  `json:"from"`
			Status    Status  `json:"status"`
			Phase     Phase   `json:"phase"`
			ErrorKind *string `json:"error_kind,omitempty"`
		}{from, to, patch.Phase, patch.ErrorKind})
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, Event{RunID: id, Type: "run.status", PayloadJSON: payload})
		return err
	})
}

func requireAffected(result sql.Result, empty error) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return empty
	}
	return nil
}

// Heartbeat refreshes admitted queued runs and executing approved clone/baseline work
// before the agent gate permits the running code transition.
func (s *SQLite) Heartbeat(ctx context.Context, id int64) error {
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET heartbeat_at=?,updated_at=? WHERE id=? AND (status IN ('queued','running') OR (status='approved' AND phase IN ('clone','baseline')))", now, now, id)
	if err != nil {
		return err
	}
	return requireAffected(result, ErrConflict)
}

// FailStaleRuns fails executing runs with an expired heartbeat. Approved
// clone/baseline work keeps its status for the agent's gate, but still owns a
// heartbeat. Queued work left before its first transition is recovered too.
func (s *SQLite) FailStaleRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	var count int64
	err := s.withTx(ctx, func(tx *eventTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id,status FROM runs WHERE
   ((status='running' OR (status='approved' AND phase IN ('clone','baseline'))) AND heartbeat_at < ?)
   OR (status='queued' AND updated_at < ?)`, olderThan.UnixMilli(), olderThan.UnixMilli())
		if err != nil {
			return err
		}
		type stale struct {
			id     int64
			status Status
		}
		var runs []stale
		for rows.Next() {
			var run stale
			if err := rows.Scan(&run.id, &run.status); err != nil {
				_ = rows.Close()
				return err
			}
			runs = append(runs, run)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := time.Now().UTC().UnixMilli()
		for _, run := range runs {
			result, err := tx.ExecContext(ctx, `UPDATE runs SET generation=generation+1,status='failed',outcome='error',error_kind='interrupted',
    error_message='run heartbeat expired',finished_at=?,updated_at=? WHERE id=? AND status=?`, now, now, run.id, run.status)
			if err != nil {
				return err
			}
			if err := requireAffected(result, ErrConflict); err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"from": run.status, "status": StatusFailed, "error_kind": "interrupted"})
			if err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, Event{RunID: run.id, Type: "run.status", PayloadJSON: payload}); err != nil {
				return err
			}
		}
		count = int64(len(runs))
		return nil
	})
	return count, err
}

// AppendEvent allocates a sequence number with a single INSERT statement.
func (s *SQLite) AppendEvent(ctx context.Context, e Event) (int64, error) {
	seq, err := appendEvent(ctx, s.db, e)
	if err == nil {
		e.Seq = seq
		s.publishEvent(e)
	}
	return seq, err
}

type eventWriter interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func appendEvent(ctx context.Context, db eventWriter, e Event) (int64, error) {
	e.CreatedAt = timestamp(e.CreatedAt)
	var seq int64
	err := db.QueryRowContext(ctx, `INSERT INTO run_events(run_id,seq,type,payload_json,created_at)
		SELECT ?,COALESCE(MAX(seq),0)+1,?,?,? FROM run_events WHERE run_id=? RETURNING seq`,
		e.RunID, e.Type, jsonText(e.PayloadJSON), timestamp(e.CreatedAt).UnixMilli(), e.RunID).Scan(&seq)
	if err == nil {
		if tracked, ok := db.(*eventTx); ok {
			e.Seq = seq
			tracked.events = append(tracked.events, e)
		}
	}
	return seq, mapError(err)
}

// EventsAfter returns at most limit events with seq strictly greater than
// afterSeq, in ascending order. A nonpositive limit defaults to 50 (max 500).
func (s *SQLite) EventsAfter(ctx context.Context, runID, afterSeq int64, limit int) ([]Event, error) {
	if afterSeq < 0 {
		return nil, errors.New("event sequence must be non-negative")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,seq,type,payload_json,created_at FROM run_events
		WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?`, runID, afterSeq, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []Event{}
	for rows.Next() {
		var e Event
		var created int64
		var payload string
		if err := rows.Scan(&e.RunID, &e.Seq, &e.Type, &payload, &created); err != nil {
			return nil, err
		}
		e.PayloadJSON = json.RawMessage(payload)
		e.CreatedAt = time.UnixMilli(created).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}
