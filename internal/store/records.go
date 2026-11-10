package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const specColumns = `run_id,version,body_md,hash,issue_hash,source,notes,comment_id,created_at`

func scanSpec(row scanner) (Spec, error) {
	var spec Spec
	var created int64
	err := row.Scan(&spec.RunID, &spec.Version, &spec.BodyMD, &spec.Hash, &spec.IssueHash,
		&spec.Source, &spec.Notes, &spec.CommentID, &created)
	spec.CreatedAt = time.UnixMilli(created).UTC()
	return spec, mapError(err)
}

// SaveSpec appends an immutable version. Any older approval immediately stops
// matching LatestSpec; an approved run returns to awaiting_approval with an
// event in this same transaction. Explicit stale version numbers conflict.
func (s *SQLite) SaveSpec(ctx context.Context, spec Spec) (Spec, error) {
	var saved Spec
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var next int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM specs WHERE run_id=?`, spec.RunID).Scan(&next); err != nil {
			return err
		}
		if spec.Version != 0 && spec.Version != next {
			return ErrConflict
		}
		spec.Version = next
		spec.CreatedAt = timestamp(spec.CreatedAt)
		var err error
		saved, err = scanSpec(tx.QueryRowContext(ctx, `INSERT INTO specs(`+specColumns+`) VALUES(?,?,?,?,?,?,?,?,?) RETURNING `+specColumns,
			spec.RunID, spec.Version, spec.BodyMD, spec.Hash, spec.IssueHash, spec.Source, spec.Notes, spec.CommentID, spec.CreatedAt.UnixMilli()))
		if err != nil {
			return err
		}
		now := time.Now().UTC().UnixMilli()
		result, err := tx.ExecContext(ctx, `UPDATE runs SET status='awaiting_approval',waiting_since=?,updated_at=?
			WHERE id=? AND status='approved'`, now, now, spec.RunID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 0 {
			_, err = appendEvent(ctx, tx, Event{RunID: spec.RunID, Type: "run.status",
				PayloadJSON: []byte(`{"from":"approved","status":"awaiting_approval","reason":"spec_changed"}`)})
		}
		return err
	})
	if err != nil {
		return Spec{}, err
	}
	return saved, nil
}

// LatestSpec returns the highest spec version for a run.
func (s *SQLite) LatestSpec(ctx context.Context, runID int64) (Spec, error) {
	return scanSpec(s.db.QueryRowContext(ctx, "SELECT "+specColumns+" FROM specs WHERE run_id=? ORDER BY version DESC LIMIT 1", runID))
}

const approvalColumns = `id,run_id,spec_version,spec_hash,decision,actor,channel,created_at`

func scanApproval(row scanner) (Approval, error) {
	var a Approval
	var created int64
	err := row.Scan(&a.ID, &a.RunID, &a.SpecVersion, &a.SpecHash, &a.Decision, &a.Actor, &a.Channel, &created)
	a.CreatedAt = time.UnixMilli(created).UTC()
	return a, mapError(err)
}

// RecordApproval records a decision only for the current version and hash.
// Status transitions and actor authorization remain the runner's responsibility.
func (s *SQLite) RecordApproval(ctx context.Context, a Approval) (Approval, error) {
	var saved Approval
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var version int64
		var hash string
		err := tx.QueryRowContext(ctx, `SELECT version,hash FROM specs WHERE run_id=? ORDER BY version DESC LIMIT 1`, a.RunID).Scan(&version, &hash)
		if err != nil {
			return err
		}
		if version != a.SpecVersion || hash != a.SpecHash {
			return ErrConflict
		}
		saved, err = scanApproval(tx.QueryRowContext(ctx, `INSERT INTO approvals(run_id,spec_version,spec_hash,decision,actor,channel,created_at)
			VALUES(?,?,?,?,?,?,?) RETURNING `+approvalColumns,
			a.RunID, a.SpecVersion, a.SpecHash, a.Decision, a.Actor, a.Channel, timestamp(a.CreatedAt).UnixMilli()))
		return err
	})
	if err != nil {
		return Approval{}, err
	}
	return saved, nil
}

// ValidApproval requires both the requested version/hash and the latest spec
// to match the newest decision. Revisions, edits, and subsequent revise/reject
// decisions invalidate earlier approvals without deleting audit history.
func (s *SQLite) ValidApproval(ctx context.Context, runID, specVersion int64, specHash string) (Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE run_id=? AND spec_version=? AND spec_hash=? AND decision='approve'
		AND spec_version=(SELECT MAX(version) FROM specs WHERE run_id=?)
		AND spec_hash=(SELECT hash FROM specs WHERE run_id=? AND version=?)
		AND id=(SELECT MAX(id) FROM approvals WHERE run_id=? AND spec_version=?)`,
		runID, specVersion, specHash, runID, runID, specVersion, runID, specVersion))
}

const questionColumns = `id,run_id,body,answer,answered_by,answer_channel,comment_id,created_at,answered_at`

func scanQuestion(row scanner) (Question, error) {
	var q Question
	var created int64
	var answered sql.NullInt64
	err := row.Scan(&q.ID, &q.RunID, &q.Body, &q.Answer, &q.AnsweredBy, &q.AnswerChannel, &q.CommentID, &created, &answered)
	q.CreatedAt, q.AnsweredAt = time.UnixMilli(created).UTC(), optionalTime(answered)
	return q, mapError(err)
}

// SaveQuestion inserts a question, or updates the answer of an existing question
// owned by the same run. Existing question bodies and provenance are immutable.
func (s *SQLite) SaveQuestion(ctx context.Context, q Question) (Question, error) {
	if q.ID != 0 {
		return scanQuestion(s.db.QueryRowContext(ctx, `UPDATE questions SET answer=?,answered_by=?,answer_channel=?,answered_at=?
			WHERE id=? AND run_id=? RETURNING `+questionColumns, q.Answer, q.AnsweredBy, q.AnswerChannel, optionalMillis(q.AnsweredAt), q.ID, q.RunID))
	}
	return scanQuestion(s.db.QueryRowContext(ctx, `INSERT INTO questions(run_id,body,answer,answered_by,answer_channel,comment_id,created_at,answered_at)
		VALUES(?,?,?,?,?,?,?,?) RETURNING `+questionColumns,
		q.RunID, q.Body, q.Answer, q.AnsweredBy, q.AnswerChannel, q.CommentID, timestamp(q.CreatedAt).UnixMilli(), optionalMillis(q.AnsweredAt)))
}

// ListQuestions returns questions in insertion order.
func (s *SQLite) ListQuestions(ctx context.Context, runID int64) ([]Question, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+questionColumns+" FROM questions WHERE run_id=? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	questions := []Question{}
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		questions = append(questions, q)
	}
	return questions, rows.Err()
}

const actionColumns = `id,run_id,kind,actor,channel,payload_json,created_at`

func scanAction(row scanner) (Action, error) {
	var a Action
	var created int64
	var payload string
	err := row.Scan(&a.ID, &a.RunID, &a.Kind, &a.Actor, &a.Channel, &payload, &created)
	a.PayloadJSON = json.RawMessage(payload)
	a.CreatedAt = time.UnixMilli(created).UTC()
	return a, mapError(err)
}

// RecordAction appends a redacted action to the audit log.
func (s *SQLite) RecordAction(ctx context.Context, a Action) (Action, error) {
	return scanAction(s.db.QueryRowContext(ctx, `INSERT INTO actions(run_id,kind,actor,channel,payload_json,created_at)
		VALUES(?,?,?,?,?,?) RETURNING `+actionColumns,
		a.RunID, a.Kind, a.Actor, a.Channel, jsonText(a.PayloadJSON), timestamp(a.CreatedAt).UnixMilli()))
}

// ListActions returns the run's audit log in insertion order.
func (s *SQLite) ListActions(ctx context.Context, runID int64) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE run_id=? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	actions := []Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// SaveArtifact inserts or replaces an artifact with the same run/kind/attempt.
func (s *SQLite) SaveArtifact(ctx context.Context, a Artifact) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_artifacts(run_id,kind,attempt,content,created_at)
		VALUES(?,?,?,?,?) ON CONFLICT(run_id,kind,attempt) DO UPDATE SET content=excluded.content,created_at=excluded.created_at`,
		a.RunID, a.Kind, a.Attempt, a.Content, timestamp(a.CreatedAt).UnixMilli())
	return mapError(err)
}

// GetArtifact retrieves a particular run artifact.
func (s *SQLite) GetArtifact(ctx context.Context, runID int64, kind string, attempt int64) (Artifact, error) {
	var a Artifact
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT run_id,kind,attempt,content,created_at FROM run_artifacts WHERE run_id=? AND kind=? AND attempt=?`,
		runID, kind, attempt).Scan(&a.RunID, &a.Kind, &a.Attempt, &a.Content, &created)
	a.CreatedAt = time.UnixMilli(created).UTC()
	return a, mapError(err)
}

const stepColumns = `run_id,seq,phase,kind,tool,status,turn,input_json,output_text,stop_reason,input_tokens,output_tokens,
cache_read_tokens,cache_write_tokens,cost_micros,exit_code,duration_ms,started_at,finished_at`

func scanStep(row scanner) (Step, error) {
	var step Step
	var started int64
	var finished sql.NullInt64
	err := row.Scan(&step.RunID, &step.Seq, &step.Phase, &step.Kind, &step.Tool, &step.Status, &step.Turn,
		&step.InputJSON, &step.OutputText, &step.StopReason, &step.InputTokens, &step.OutputTokens,
		&step.CacheReadTokens, &step.CacheWriteTokens, &step.CostMicros, &step.ExitCode, &step.DurationMS, &started, &finished)
	step.StartedAt, step.FinishedAt = time.UnixMilli(started).UTC(), optionalTime(finished)
	return step, mapError(err)
}

// RecordStep inserts a new step or replaces an existing step's data, updating
// run totals in the same transaction. Re-recording cannot double-count usage.
func (s *SQLite) RecordStep(ctx context.Context, step Step) (Step, error) {
	if step.Seq < 0 {
		return Step{}, errors.New("step sequence must be non-negative")
	}
	step.InputJSON = cappedText(step.InputJSON, 16*1024)
	step.OutputText = cappedText(step.OutputText, 64*1024)
	var saved Step
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		args := []any{step.Phase, step.Kind, step.Tool, step.Status, step.Turn, step.InputJSON, step.OutputText,
			step.StopReason, step.InputTokens, step.OutputTokens, step.CacheReadTokens, step.CacheWriteTokens,
			step.CostMicros, step.ExitCode, step.DurationMS, timestamp(step.StartedAt).UnixMilli(), optionalMillis(step.FinishedAt)}
		var query string
		if step.Seq == 0 {
			query = `INSERT INTO steps(` + stepColumns + `) SELECT ?,COALESCE(MAX(seq),0)+1,` + placeholders(len(args)) + ` FROM steps WHERE run_id=? RETURNING ` + stepColumns
			args = append([]any{step.RunID}, args...)
			args = append(args, step.RunID)
		} else {
			query = `UPDATE steps SET phase=?,kind=?,tool=?,status=?,turn=?,input_json=?,output_text=?,stop_reason=?,
				input_tokens=?,output_tokens=?,cache_read_tokens=?,cache_write_tokens=?,cost_micros=?,exit_code=?,duration_ms=?,started_at=?,finished_at=?
				WHERE run_id=? AND seq=? RETURNING ` + stepColumns
			args = append(args, step.RunID, step.Seq)
		}
		var err error
		saved, err = scanStep(tx.QueryRowContext(ctx, query, args...))
		if err != nil {
			return err
		}
		// NULL propagates if any completed llm call lacks a token count or price.
		// SUM alone would silently ignore unknown values and under-report usage.
		_, err = tx.ExecContext(ctx, `UPDATE runs SET
			(input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,turns)=(
			SELECT CASE WHEN COUNT(input_tokens)=COUNT(*) THEN SUM(input_tokens) END,
			CASE WHEN COUNT(output_tokens)=COUNT(*) THEN SUM(output_tokens) END,
			CASE WHEN COUNT(cache_read_tokens)=COUNT(*) THEN SUM(cache_read_tokens) END,
			CASE WHEN COUNT(cache_write_tokens)=COUNT(*) THEN SUM(cache_write_tokens) END,
			CASE WHEN COUNT(cost_micros)=COUNT(*) THEN SUM(cost_micros) END,COUNT(*)
			FROM steps WHERE run_id=? AND kind='llm' AND status!='running'),
			test_attempts=(SELECT COUNT(*) FROM steps WHERE run_id=? AND kind='test' AND status!='running'),updated_at=?
			WHERE id=?`, step.RunID, step.RunID, time.Now().UTC().UnixMilli(), step.RunID)
		return err
	})
	if err != nil {
		return Step{}, err
	}
	return saved, nil
}

// ListSteps returns execution records in ascending per-run sequence order.
func (s *SQLite) ListSteps(ctx context.Context, runID int64) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+stepColumns+" FROM steps WHERE run_id=? ORDER BY seq", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	steps := []Step{}
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, rows.Err()
}
