package store

import "context"

// Delivery is a durable wake-up hint, containing no command or approval data.
type Delivery struct {
	ID, Event, Repo string
	Issue           int64
}

// RecordDelivery preserves the original hint when GitHub retries a delivery.
func (s *SQLite) RecordDelivery(ctx context.Context, d Delivery) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries(delivery_id,event,repo,issue) VALUES(?,?,?,?) ON CONFLICT(delivery_id) DO NOTHING`, d.ID, d.Event, d.Repo, d.Issue)
	return err
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
	_, err := s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET processed=1 WHERE delivery_id=?`, id)
	return err
}

// ObserveTriggerLabel latches the current trigger. Only an observed removal or
// change rearms it, so bot edits and repeated deliveries cannot retrigger work.
func (s *SQLite) ObserveTriggerLabel(ctx context.Context, repo string, issue int64, label string) (int64, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO webhook_labels(repo,issue,label,generation) VALUES(?,?,?,1)
 ON CONFLICT(repo,issue) DO UPDATE SET generation=webhook_labels.generation + CASE WHEN webhook_labels.label != excluded.label THEN 1 ELSE 0 END, label=excluded.label RETURNING generation`, repo, issue, label).Scan(&generation)
	return generation, err
}
