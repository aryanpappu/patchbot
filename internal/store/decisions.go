package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// EditSpec appends an operator edit only if both snapshots still match. A
// cancellation, code claim, revision, or competing edit rolls everything back.
func (s *SQLite) EditSpec(ctx context.Context, edit SpecEdit) (Spec, error) {
	if (edit.From != StatusAwaitingApproval && edit.From != StatusNeedsInfo && edit.From != StatusApproved) ||
		(edit.To != StatusAwaitingApproval && edit.To != StatusNeedsInfo) {
		return Spec{}, ErrConflict
	}
	spec := edit.Spec
	spec.Version = edit.PriorVersion + 1
	spec.CreatedAt = timestamp(spec.CreatedAt)
	var saved Spec
	err := s.withTx(ctx, func(tx *eventTx) error {
		now := time.Now().UTC().UnixMilli()
		result, err := tx.ExecContext(ctx, `UPDATE runs SET generation=generation+1,status=?,phase='refine',updated_at=?,
   waiting_since=?,heartbeat_at=NULL,finished_at=NULL WHERE id=? AND status=? AND generation=?
   AND EXISTS(SELECT 1 FROM specs WHERE run_id=? AND version=? AND hash=?
     AND version=(SELECT MAX(version) FROM specs WHERE run_id=?))`,
			edit.To, now, now, spec.RunID, edit.From, edit.Generation, spec.RunID, edit.PriorVersion, edit.PriorHash, spec.RunID)
		if err != nil {
			return err
		}
		if err := requireAffected(result, ErrConflict); err != nil {
			return err
		}
		saved, err = scanSpec(tx.QueryRowContext(ctx, `INSERT INTO specs(`+specColumns+`) VALUES(?,?,?,?,?,?,?,?,?) RETURNING `+specColumns,
			spec.RunID, spec.Version, spec.BodyMD, spec.Hash, spec.IssueHash, spec.Source, spec.Notes, spec.CommentID, spec.CreatedAt.UnixMilli()))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"from": edit.From, "status": edit.To, "phase": PhaseRefine,
			"spec_version": spec.Version, "spec_hash": spec.Hash, "reason": "spec_changed"})
		if err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, Event{RunID: spec.RunID, Type: "run.status", PayloadJSON: payload}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO actions(run_id,kind,actor,channel,payload_json,created_at) VALUES(?,'edit_spec',?,'ui',?,?)`,
			spec.RunID, edit.Actor, string(payload), now)
		return err
	})
	if err != nil {
		return Spec{}, err
	}
	return saved, nil
}

// DecideRun persists an exact-spec decision and lifecycle change together.
// An optional phase claims asynchronous execution in the same approval commit.
func (s *SQLite) DecideRun(ctx context.Context, from, to Status, a Approval, generation int64, phases ...Phase) error {
	return s.withTx(ctx, func(tx *eventTx) error {
		var version int64
		var hash string
		var phase Phase
		if err := tx.QueryRowContext(ctx, `SELECT specs.version,specs.hash,runs.phase FROM specs JOIN runs ON specs.run_id=runs.id
 WHERE specs.run_id=? ORDER BY specs.version DESC LIMIT 1`, a.RunID).Scan(&version, &hash, &phase); err != nil {
			return err
		}
		if version != a.SpecVersion || hash != a.SpecHash {
			return ErrConflict
		}
		now := time.Now().UTC().UnixMilli()
		decisionAt := timestamp(a.CreatedAt).UnixMilli()
		var finished, heartbeat any
		if !to.IsActive() {
			finished = now
		}
		if to == StatusApproved {
			phase = PhaseRefine
		}
		if to == StatusRunning {
			phase = PhasePreflight
		}
		if len(phases) > 0 {
			phase = phases[0]
		}
		if to == StatusRunning || (to == StatusApproved && (phase == PhaseClone || phase == PhaseBaseline)) {
			heartbeat = now
		}
		// An approve decision freezes the reviewer selection the approver saw.
		// Later configuration changes or retries of this worker cannot alter it.
		review := ""
		if to == StatusApproved && a.Decision == "approve" && len(a.Review) > 0 {
			if !json.Valid(a.Review) {
				return errors.New("invalid review selection")
			}
			review = string(a.Review)
		}
		result, err := tx.ExecContext(ctx, `UPDATE runs SET generation=generation+1,status=?,updated_at=?,finished_at=?,heartbeat_at=?,waiting_since=NULL,
   phase=?,
   started_at=CASE WHEN ?='running' THEN COALESCE(started_at,?) ELSE started_at END,
   outcome=CASE WHEN ?='rejected' THEN 'rejected' ELSE outcome END,
   review_json=CASE WHEN ?='approved' THEN ? ELSE review_json END
   WHERE id=? AND status=? AND generation=?`, to, now, finished, heartbeat, phase, to, now, to, to, review, a.RunID, from, generation)
		if err != nil {
			return err
		}
		if err := requireAffected(result, ErrConflict); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO approvals(run_id,spec_version,spec_hash,decision,actor,channel,created_at) VALUES(?,?,?,?,?,?,?)`, a.RunID, a.SpecVersion, a.SpecHash, a.Decision, a.Actor, a.Channel, decisionAt); err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"from": from, "status": to, "phase": phase, "spec_version": version, "spec_hash": hash})
		if err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, Event{RunID: a.RunID, Type: "run.status", PayloadJSON: payload, CreatedAt: time.UnixMilli(now)}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO actions(run_id,kind,actor,channel,payload_json,created_at) VALUES(?,?,?,?,?,?)`, a.RunID, a.Decision, a.Actor, a.Channel, string(payload), now)
		return err
	})
}

// SetSpecComment records the immutable spec's GitHub rendering exactly once.
func (s *SQLite) SetSpecComment(ctx context.Context, runID, version int64, hash string, commentID int64) error {
	if commentID <= 0 {
		return errors.New("invalid spec comment ID")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE specs SET comment_id=? WHERE run_id=? AND version=? AND hash=? AND comment_id IS NULL`, commentID, runID, version, hash)
	if err != nil {
		return mapError(err)
	}
	return requireAffected(result, ErrConflict)
}

// StartCode atomically closes the edit/approval race before any model writes.
func (s *SQLite) StartCode(ctx context.Context, runID, version int64, hash string, generation int64) error {
	return s.withTx(ctx, func(tx *eventTx) error {
		now := time.Now().UTC().UnixMilli()
		result, err := tx.ExecContext(ctx, `UPDATE runs SET generation=generation+1,status='running',phase='code',updated_at=?,heartbeat_at=?,waiting_since=NULL,
   started_at=COALESCE(started_at,?) WHERE id=? AND status='approved' AND generation=?
   AND EXISTS(SELECT 1 FROM specs WHERE run_id=? AND version=? AND hash=? AND version=(SELECT MAX(version) FROM specs WHERE run_id=?))
   AND EXISTS(SELECT 1 FROM approvals WHERE run_id=? AND spec_version=? AND spec_hash=? AND decision='approve'
     AND id=(SELECT MAX(id) FROM approvals WHERE run_id=? AND spec_version=?))`, now, now, now, runID, generation, runID, version, hash, runID, runID, version, hash, runID, version)
		if err != nil {
			return err
		}
		if err := requireAffected(result, ErrConflict); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, Event{RunID: runID, Type: "run.status", PayloadJSON: []byte(`{"from":"approved","status":"running","phase":"code"}`)})
		return err
	})
}

const reviewColumns = `id,run_id,round,spec_version,spec_hash,diff_hash,base_sha,tree_id,verdict,reason,findings_json,criteria_json,provider,model,created_at`

func scanReview(row scanner) (Review, error) {
	var r Review
	var findings, criteria string
	var created int64
	err := row.Scan(&r.ID, &r.RunID, &r.Round, &r.SpecVersion, &r.SpecHash, &r.DiffHash, &r.BaseSHA, &r.TreeID,
		&r.Verdict, &r.Reason, &findings, &criteria, &r.Provider, &r.Model, &created)
	r.FindingsJSON, r.CriteriaJSON = json.RawMessage(findings), json.RawMessage(criteria)
	r.CreatedAt = time.UnixMilli(created).UTC()
	return r, mapError(err)
}

// SaveReview appends the next review round. The round must be the next one, the
// spec version/hash must be the run's latest spec, and the run must still be
// executing. A verdict for a stale spec or a finished run is refused.
func (s *SQLite) SaveReview(ctx context.Context, r Review) (Review, error) {
	switch r.Verdict {
	case "approve", "request_changes", "incomplete":
	default:
		return Review{}, errors.New("invalid review verdict")
	}
	if len(r.FindingsJSON) == 0 {
		r.FindingsJSON = json.RawMessage(`[]`)
	}
	if len(r.CriteriaJSON) == 0 {
		r.CriteriaJSON = json.RawMessage(`[]`)
	}
	if !json.Valid(r.FindingsJSON) || !json.Valid(r.CriteriaJSON) {
		return Review{}, errors.New("invalid review JSON")
	}
	r.Reason = cappedText(r.Reason, 2000)
	r.CreatedAt = timestamp(r.CreatedAt)
	var saved Review
	err := s.withTx(ctx, func(tx *eventTx) error {
		var status Status
		if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=?`, r.RunID).Scan(&status); err != nil {
			return err
		}
		if status != StatusRunning {
			return ErrConflict
		}
		var version int64
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT version,hash FROM specs WHERE run_id=? ORDER BY version DESC LIMIT 1`, r.RunID).Scan(&version, &hash); err != nil {
			return err
		}
		if version != r.SpecVersion || hash != r.SpecHash {
			return ErrConflict
		}
		var next int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(round),0)+1 FROM reviews WHERE run_id=?`, r.RunID).Scan(&next); err != nil {
			return err
		}
		if r.Round != 0 && r.Round != next {
			return ErrConflict
		}
		r.Round = next
		var err error
		saved, err = scanReview(tx.QueryRowContext(ctx, `INSERT INTO reviews(`+strings.TrimPrefix(reviewColumns, "id,")+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING `+reviewColumns,
			r.RunID, r.Round, r.SpecVersion, r.SpecHash, r.DiffHash, r.BaseSHA, r.TreeID, r.Verdict, r.Reason,
			string(r.FindingsJSON), string(r.CriteriaJSON), r.Provider, r.Model, r.CreatedAt.UnixMilli()))
		return err
	})
	if err != nil {
		return Review{}, err
	}
	return saved, nil
}

// ListReviews returns a run's review rounds in order.
func (s *SQLite) ListReviews(ctx context.Context, runID int64) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+reviewColumns+" FROM reviews WHERE run_id=? ORDER BY round", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Review{}
	for rows.Next() {
		review, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, review)
	}
	return out, rows.Err()
}
