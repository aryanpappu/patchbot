package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
)

// CheckMigrations inspects the complete ledger without creating a database or
// applying migrations. Opening a store for normal use remains the migration step.
func CheckMigrations(ctx context.Context, path string) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	u := &url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SELECT version,name,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, errors.New("cannot read migration ledger")
	}
	defer func() { _ = rows.Close() }()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return count, err
		}
		if count >= len(entries) || version != count+1 || name != entries[count].Name() {
			return count, errors.New("migration ledger has gaps or unsupported versions")
		}
		body, err := migrationFiles.ReadFile("migrations/" + entries[count].Name())
		if err != nil {
			return count, err
		}
		if checksum != fmt.Sprintf("%x", sha256.Sum256(body)) {
			return count, errors.New("migration checksum mismatch")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	if count != len(entries) {
		return count, errors.New("database has pending migrations; normal startup applies them")
	}
	return count, nil
}
