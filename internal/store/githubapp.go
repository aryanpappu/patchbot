package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"time"
)

// GitHubApp is the App created by the Connect GitHub flow. It holds no secret.
type GitHubApp struct {
	AppID          int64
	Slug           string
	InstallationID int64
	CreatedAt      time.Time
}

// GitHubApp returns the connected App, or false when none was recorded.
func (s *SQLite) GitHubApp(ctx context.Context) (GitHubApp, bool, error) {
	return scanGitHubApp(s.db.QueryRowContext(ctx, `SELECT app_id,slug,installation_id,created_at FROM github_app WHERE id=1`))
}

// SaveGitHubApp records the App once. A second call returns ErrConflict so a
// replayed callback can never replace a connected App.
func (s *SQLite) SaveGitHubApp(ctx context.Context, app GitHubApp) error {
	if app.AppID <= 0 || app.Slug == "" || len(app.Slug) > 100 || app.InstallationID < 0 {
		return errors.New("invalid GitHub App record")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO github_app(id,app_id,slug,installation_id,created_at)
		VALUES(1,?,?,?,?) ON CONFLICT(id) DO NOTHING`, app.AppID, app.Slug, app.InstallationID, timestamp(app.CreatedAt).UnixMilli())
	if err != nil {
		return mapError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// ReadGitHubApp reads the connected App from a database file opened read-only
// (doctor uses it; it never creates or migrates the file). A database without
// the table, or without a row, reports false.
func ReadGitHubApp(ctx context.Context, path string) (GitHubApp, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return GitHubApp{}, false, err
	}
	u := &url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return GitHubApp{}, false, err
	}
	defer func() { _ = db.Close() }()
	app, found, err := scanGitHubApp(db.QueryRowContext(ctx, `SELECT app_id,slug,installation_id,created_at FROM github_app WHERE id=1`))
	if err != nil {
		// An older database has no table yet: not connected.
		return GitHubApp{}, false, nil
	}
	return app, found, nil
}

func scanGitHubApp(row *sql.Row) (GitHubApp, bool, error) {
	var app GitHubApp
	var created int64
	err := row.Scan(&app.AppID, &app.Slug, &app.InstallationID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, false, nil
	}
	if err != nil {
		return GitHubApp{}, false, mapError(err)
	}
	app.CreatedAt = time.UnixMilli(created).UTC()
	return app, true, nil
}
