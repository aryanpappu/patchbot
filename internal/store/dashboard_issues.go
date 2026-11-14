package store

import (
	"context"
	"errors"
	"time"
)

// DashboardIssue records an issue created from the dashboard. Actor is the
// dashboard user who asked; GitHub shows the bot or PAT user as the author.
type DashboardIssue struct {
	IdempotencyKey string
	RequestHash    string
	Repo           string
	Actor          string
	// Pending is true until GitHub confirmed the issue (Number is then zero).
	Pending   bool
	Number    int64
	NodeID    string
	URL       string
	RunID     int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const dashboardIssueColumns = `idempotency_key,request_hash,repo,actor,state,coalesce(issue_number,0),issue_node_id,issue_url,coalesce(run_id,0),created_at,updated_at`

func scanDashboardIssue(row interface{ Scan(...any) error }) (DashboardIssue, error) {
	var d DashboardIssue
	var state string
	var created, updated int64
	if err := row.Scan(&d.IdempotencyKey, &d.RequestHash, &d.Repo, &d.Actor, &state, &d.Number, &d.NodeID, &d.URL, &d.RunID, &created, &updated); err != nil {
		return DashboardIssue{}, mapError(err)
	}
	d.Pending = state == "pending"
	d.CreatedAt, d.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return d, nil
}

// BeginDashboardIssue inserts a pending row for the key. When the key already
// exists nothing changes: the stored row is returned with inserted false.
func (s *SQLite) BeginDashboardIssue(ctx context.Context, d DashboardIssue) (DashboardIssue, bool, error) {
	if d.IdempotencyKey == "" || len(d.RequestHash) != 64 || d.Repo == "" || d.Actor == "" {
		return DashboardIssue{}, false, errors.New("invalid dashboard issue")
	}
	now := timestamp(d.CreatedAt).UnixMilli()
	res, err := s.db.ExecContext(ctx, `INSERT INTO dashboard_issues(idempotency_key,request_hash,repo,actor,state,created_at,updated_at)
		VALUES(?,?,?,?,'pending',?,?) ON CONFLICT(idempotency_key) DO NOTHING`, d.IdempotencyKey, d.RequestHash, d.Repo, d.Actor, now, now)
	if err != nil {
		return DashboardIssue{}, false, mapError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return DashboardIssue{}, false, mapError(err)
	}
	got, err := s.DashboardIssue(ctx, d.IdempotencyKey)
	return got, n == 1, err
}

// DashboardIssue returns the row for an idempotency key.
func (s *SQLite) DashboardIssue(ctx context.Context, key string) (DashboardIssue, error) {
	return scanDashboardIssue(s.db.QueryRowContext(ctx, `SELECT `+dashboardIssueColumns+` FROM dashboard_issues WHERE idempotency_key=?`, key))
}

// DashboardIssueByNumber finds the dashboard record of a GitHub issue, so an
// issue patchbot opened itself is recognised and attributed to its requester.
func (s *SQLite) DashboardIssueByNumber(ctx context.Context, repo string, number int64) (DashboardIssue, bool, error) {
	d, err := scanDashboardIssue(s.db.QueryRowContext(ctx, `SELECT `+dashboardIssueColumns+` FROM dashboard_issues WHERE repo=? AND issue_number=?`, repo, number))
	if errors.Is(err, ErrNotFound) {
		return DashboardIssue{}, false, nil
	}
	return d, err == nil, err
}

// ClaimPendingDashboardIssue lets exactly one retry reconcile a pending row.
// It succeeds only when the row is pending and untouched since staleBefore, so
// a request that is still talking to GitHub is never raced.
func (s *SQLite) ClaimPendingDashboardIssue(ctx context.Context, key string, staleBefore, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE dashboard_issues SET updated_at=? WHERE idempotency_key=? AND state='pending' AND updated_at<=?`,
		timestamp(now).UnixMilli(), key, staleBefore.UnixMilli())
	if err != nil {
		return false, mapError(err)
	}
	n, err := res.RowsAffected()
	return n == 1, mapError(err)
}

// CompleteDashboardIssue records the GitHub issue behind a pending row. A
// repeat with the same issue number succeeds; a different number conflicts.
func (s *SQLite) CompleteDashboardIssue(ctx context.Context, key string, number int64, nodeID, url string, now time.Time) error {
	if number <= 0 {
		return errors.New("invalid issue number")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE dashboard_issues SET state='created',issue_number=?,issue_node_id=?,issue_url=?,updated_at=?
		WHERE idempotency_key=? AND (state='pending' OR issue_number=?)`, number, nodeID, url, timestamp(now).UnixMilli(), key, number)
	if err != nil {
		return mapError(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// SetDashboardIssueRun links the run started for the issue.
func (s *SQLite) SetDashboardIssueRun(ctx context.Context, key string, runID int64) error {
	if runID <= 0 {
		return errors.New("invalid run ID")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE dashboard_issues SET run_id=? WHERE idempotency_key=? AND state='created' AND (run_id IS NULL OR run_id=?)`, runID, key, runID)
	if err != nil {
		return mapError(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// DeletePendingDashboardIssue forgets a row whose GitHub call definitely did
// not create an issue, so the same key can be submitted again.
func (s *SQLite) DeletePendingDashboardIssue(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dashboard_issues WHERE idempotency_key=? AND state='pending'`, key)
	return mapError(err)
}

// RunByIngressKey returns the run created under an ingress (idempotency) key.
func (s *SQLite) RunByIngressKey(ctx context.Context, key string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, "SELECT "+runColumns+" FROM runs WHERE ingress_key=?", key))
}
