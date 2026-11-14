package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Errors of the dashboard repository store.
var (
	// ErrRepoExists means a dashboard repository with that name exists.
	ErrRepoExists = errors.New("repository already exists")
	// ErrVersionConflict means the stored version is not the expected one.
	ErrVersionConflict = errors.New("repository changed since it was read")
	// ErrRepoBusy means the repository has active runs and cannot be removed.
	ErrRepoBusy = errors.New("repository has active runs")
)

// Repo audit actions.
const (
	RepoActionAdd    = "add"
	RepoActionEdit   = "edit"
	RepoActionRemove = "remove"
)

// RepoRecord is one dashboard-managed repository. SettingsJSON is the redacted,
// validated settings document; the repos package owns its schema.
type RepoRecord struct {
	Name         string
	SettingsJSON string
	Version      int64
	CreatedAt    time.Time
	CreatedBy    string
	UpdatedAt    time.Time
	UpdatedBy    string
}

// RepoAudit is one append-only audit row.
type RepoAudit struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Action     string    `json:"action"`
	Actor      string    `json:"actor"`
	Channel    string    `json:"channel"`
	BeforeJSON string    `json:"before"`
	AfterJSON  string    `json:"after"`
	At         time.Time `json:"at"`
}

// RepoStore persists dashboard repositories. Every mutation writes its audit
// row in the same transaction.
type RepoStore interface {
	ListRepos(ctx context.Context) ([]RepoRecord, error)
	// AddRepo inserts at version 1; ErrRepoExists when the name is taken.
	AddRepo(ctx context.Context, rec RepoRecord, audit RepoAudit) (RepoRecord, error)
	// UpdateRepo replaces the settings and bumps the version when the stored
	// version is expectedVersion; ErrNotFound or ErrVersionConflict otherwise.
	UpdateRepo(ctx context.Context, rec RepoRecord, expectedVersion int64, audit RepoAudit) (RepoRecord, error)
	// RemoveRepo deletes the row (history stays) unless a run of the
	// repository is still active (ErrRepoBusy). expectedVersion 0 skips the
	// version check.
	RemoveRepo(ctx context.Context, name string, expectedVersion int64, audit RepoAudit) error
	// ListRepoAudit returns the newest rows first, also for removed repositories.
	ListRepoAudit(ctx context.Context, name string, limit int) ([]RepoAudit, error)
}

const repoColumns = `name,settings_json,version,created_at,created_by,updated_at,updated_by`

func scanRepo(row interface{ Scan(...any) error }) (RepoRecord, error) {
	var rec RepoRecord
	var created, updated int64
	if err := row.Scan(&rec.Name, &rec.SettingsJSON, &rec.Version, &created, &rec.CreatedBy, &updated, &rec.UpdatedBy); err != nil {
		return RepoRecord{}, err
	}
	rec.CreatedAt, rec.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return rec, nil
}

// ListRepos returns every dashboard repository ordered by name.
func (s *SQLite) ListRepos(ctx context.Context) ([]RepoRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM dashboard_repos ORDER BY name`)
	if err != nil {
		return nil, mapError(err)
	}
	return collectRepos(rows)
}

func collectRepos(rows *sql.Rows) ([]RepoRecord, error) {
	defer func() { _ = rows.Close() }()
	var out []RepoRecord
	for rows.Next() {
		rec, err := scanRepo(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, rec)
	}
	return out, mapError(rows.Err())
}

func insertRepoAudit(ctx context.Context, tx *eventTx, a RepoAudit) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO repo_audit(repo,action,actor,channel,before_json,after_json,at) VALUES(?,?,?,?,?,?,?)`,
		a.Repo, a.Action, a.Actor, a.Channel, a.BeforeJSON, a.AfterJSON, timestamp(a.At).UnixMilli())
	return err
}

// AddRepo implements RepoStore.
func (s *SQLite) AddRepo(ctx context.Context, rec RepoRecord, audit RepoAudit) (RepoRecord, error) {
	now := timestamp(rec.CreatedAt)
	var saved RepoRecord
	err := s.withTx(ctx, func(tx *eventTx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM dashboard_repos WHERE name=?`, rec.Name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return ErrRepoExists
		}
		var err error
		saved, err = scanRepo(tx.QueryRowContext(ctx, `INSERT INTO dashboard_repos(`+repoColumns+`) VALUES(?,?,1,?,?,?,?) RETURNING `+repoColumns,
			rec.Name, rec.SettingsJSON, now.UnixMilli(), rec.CreatedBy, now.UnixMilli(), rec.CreatedBy))
		if err != nil {
			return err
		}
		audit.At = now
		return insertRepoAudit(ctx, tx, audit)
	})
	return saved, err
}

// UpdateRepo implements RepoStore.
func (s *SQLite) UpdateRepo(ctx context.Context, rec RepoRecord, expectedVersion int64, audit RepoAudit) (RepoRecord, error) {
	now := timestamp(rec.UpdatedAt)
	var saved RepoRecord
	err := s.withTx(ctx, func(tx *eventTx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM dashboard_repos WHERE name=?`, rec.Name).Scan(&current); err != nil {
			return err
		}
		if current != expectedVersion {
			return ErrVersionConflict
		}
		var err error
		saved, err = scanRepo(tx.QueryRowContext(ctx, `UPDATE dashboard_repos SET settings_json=?, version=version+1, updated_at=?, updated_by=?
			WHERE name=? RETURNING `+repoColumns, rec.SettingsJSON, now.UnixMilli(), rec.UpdatedBy, rec.Name))
		if err != nil {
			return err
		}
		audit.At = now
		return insertRepoAudit(ctx, tx, audit)
	})
	return saved, err
}

// RemoveRepo implements RepoStore.
func (s *SQLite) RemoveRepo(ctx context.Context, name string, expectedVersion int64, audit RepoAudit) error {
	return s.withTx(ctx, func(tx *eventTx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM dashboard_repos WHERE name=?`, name).Scan(&current); err != nil {
			return err
		}
		if expectedVersion != 0 && current != expectedVersion {
			return ErrVersionConflict
		}
		// Same immediate transaction as the delete: no run can become active
		// between the check and the removal.
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE repo=? AND status IN ('queued','running','needs_info','awaiting_approval','approved')`, name).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return ErrRepoBusy
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dashboard_repos WHERE name=?`, name); err != nil {
			return err
		}
		audit.At = timestamp(audit.At)
		return insertRepoAudit(ctx, tx, audit)
	})
}

// ListRepoAudit implements RepoStore.
func (s *SQLite) ListRepoAudit(ctx context.Context, name string, limit int) ([]RepoAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,repo,action,actor,channel,before_json,after_json,at FROM repo_audit WHERE repo=? ORDER BY id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []RepoAudit{}
	for rows.Next() {
		var a RepoAudit
		var at int64
		if err := rows.Scan(&a.ID, &a.Repo, &a.Action, &a.Actor, &a.Channel, &a.BeforeJSON, &a.AfterJSON, &at); err != nil {
			return nil, mapError(err)
		}
		a.At = time.UnixMilli(at).UTC()
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}

// ReadDashboardRepos reads the dashboard repositories from a database file
// opened read-only (doctor and config show use it; it never creates or
// migrates the file). A database without the table reads as empty.
func ReadDashboardRepos(ctx context.Context, path string) ([]RepoRecord, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT `+repoColumns+` FROM dashboard_repos ORDER BY name`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "unable to open") {
			return nil, nil
		}
		return nil, err
	}
	return collectRepos(rows)
}
