package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrDeliveryBacklog reports that too many receipts are still unprocessed to
// accept another one. The webhook answers 503 so GitHub can redeliver later.
var ErrDeliveryBacklog = errors.New("webhook delivery backlog is full")

// Delivery is a durable wake-up hint, containing no command or approval data.
type Delivery struct {
	ID, Event, Repo string
	Issue           int64
}

// TriggerState is the latched trigger label for one issue. Handled means the
// current generation already started a run or was recorded as a baseline.
type TriggerState struct {
	Label      string
	Generation int64
	Handled    bool
}

// RecordDelivery preserves the original hint when GitHub retries a delivery.
// A new receipt is refused with ErrDeliveryBacklog once maxPending unprocessed
// receipts exist; a known delivery ID is always accepted as a duplicate.
func (s *SQLite) RecordDelivery(ctx context.Context, d Delivery, maxPending int) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries(delivery_id,event,repo,issue)
 SELECT ?,?,?,? WHERE (SELECT COUNT(*) FROM webhook_deliveries WHERE processed=0) < ?
 ON CONFLICT(delivery_id) DO NOTHING`, d.ID, d.Event, d.Repo, d.Issue, maxPending)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var exists int
	switch err := s.db.QueryRowContext(ctx, `SELECT 1 FROM webhook_deliveries WHERE delivery_id=?`, d.ID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrDeliveryBacklog
	default:
		return err
	}
}

// PendingDeliveries includes receipts left unfinished by a process crash.
func (s *SQLite) PendingDeliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT delivery_id,event,repo,issue FROM webhook_deliveries WHERE processed=0 ORDER BY delivery_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Event, &d.Repo, &d.Issue); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CompleteDelivery acknowledges a successfully reconciled hint.
func (s *SQLite) CompleteDelivery(ctx context.Context, id string) error {
	return s.CompleteDeliveries(ctx, []string{id})
}

// CompleteDeliveries acknowledges a coalesced group of hints atomically.
func (s *SQLite) CompleteDeliveries(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET processed=1, processed_at=?
 WHERE processed=0 AND delivery_id IN (SELECT value FROM json_each(?))`, time.Now().UTC().UnixMilli(), string(list))
	return err
}

// FailDeliveries counts a failed reconcile attempt for each hint. Hints that
// reach maxAttempts are abandoned (marked processed); the count is returned.
// Label polling and comment polling still recover the underlying state.
func (s *SQLite) FailDeliveries(ctx context.Context, ids []string, maxAttempts int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `UPDATE webhook_deliveries SET attempts=attempts+1,
 processed=CASE WHEN attempts+1 >= ?1 THEN 1 ELSE 0 END,
 processed_at=CASE WHEN attempts+1 >= ?1 THEN ?2 ELSE NULL END
 WHERE processed=0 AND delivery_id IN (SELECT value FROM json_each(?3))
 RETURNING processed`, maxAttempts, time.Now().UTC().UnixMilli(), string(list))
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var abandoned int64
	for rows.Next() {
		var processed bool
		if err := rows.Scan(&processed); err != nil {
			return abandoned, err
		}
		if processed {
			abandoned++
		}
	}
	return abandoned, rows.Err()
}

// PruneDeliveries deletes processed receipts older than before. Their delivery
// IDs stop being deduplicated, which is harmless because a redelivered hint
// only reconciles current GitHub state.
func (s *SQLite) PruneDeliveries(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE processed=1 AND processed_at IS NOT NULL AND processed_at < ?`, before.UTC().UnixMilli())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ObserveTriggerLabel latches the current trigger. Only an observed removal or
// change rearms it, so bot edits and repeated deliveries cannot retrigger work.
func (s *SQLite) ObserveTriggerLabel(ctx context.Context, repo string, issue int64, label string) (TriggerState, error) {
	state := TriggerState{Label: label}
	err := s.db.QueryRowContext(ctx, `INSERT INTO webhook_labels(repo,issue,label,generation) VALUES(?,?,?,1)
 ON CONFLICT(repo,issue) DO UPDATE SET
  generation=webhook_labels.generation + CASE WHEN webhook_labels.label != excluded.label THEN 1 ELSE 0 END,
  handled=CASE WHEN webhook_labels.label != excluded.label THEN 0 ELSE webhook_labels.handled END,
  label=excluded.label
 RETURNING generation, handled`, repo, issue, label).Scan(&state.Generation, &state.Handled)
	return state, err
}

// MarkTriggerHandled records that generation needs no further start attempt.
// A concurrent label change has a newer generation and stays unhandled.
func (s *SQLite) MarkTriggerHandled(ctx context.Context, repo string, issue, generation int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webhook_labels SET handled=1 WHERE repo=? AND issue=? AND generation=?`, repo, issue, generation)
	return err
}

// TriggerLabels returns the latched trigger state of every known issue in repo.
func (s *SQLite) TriggerLabels(ctx context.Context, repo string) (map[int64]TriggerState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issue,label,generation,handled FROM webhook_labels WHERE repo=?`, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]TriggerState)
	for rows.Next() {
		var issue int64
		var state TriggerState
		if err := rows.Scan(&issue, &state.Label, &state.Generation, &state.Handled); err != nil {
			return nil, err
		}
		out[issue] = state
	}
	return out, rows.Err()
}

// RepoBaselined reports whether label polling has completed a first sweep of
// repo. Labels present before that sweep never start runs.
func (s *SQLite) RepoBaselined(ctx context.Context, repo string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM webhook_repos WHERE repo=?`, repo).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// MarkRepoBaselined records a completed first label sweep of repo.
func (s *SQLite) MarkRepoBaselined(ctx context.Context, repo string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_repos(repo,baselined_at) VALUES(?,?) ON CONFLICT(repo) DO NOTHING`, repo, time.Now().UTC().UnixMilli())
	return err
}
