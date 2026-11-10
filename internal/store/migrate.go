package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version             int64
	name, checksum, sql string
}

func (s *SQLite) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if !ok || parseErr != nil || version != int64(len(migrations)+1) {
			return fmt.Errorf("invalid migration filename %q", name)
		}
		body, readErr := migrationFiles.ReadFile("migrations/" + name)
		if readErr != nil {
			return readErr
		}
		migrations = append(migrations, migration{version, name, fmt.Sprintf("%x", sha256.Sum256(body)), string(body)})
	}
	_, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY CHECK (version > 0), name TEXT NOT NULL,
		checksum TEXT NOT NULL, applied_at INTEGER NOT NULL CHECK (applied_at >= 0)
	) STRICT`)
	if err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	// Each transaction re-reads the ledger while holding the immediate write
	// lock, so separate processes can safely open the same fresh database.
	for _, m := range migrations {
		if err := s.applyMigration(ctx, m, int64(len(migrations))); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) applyMigration(ctx context.Context, m migration, latest int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var maxVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&maxVersion); err != nil {
			return err
		}
		if maxVersion > latest {
			return fmt.Errorf("database migration %d is newer than supported version %d", maxVersion, latest)
		}
		var name, checksum string
		err := tx.QueryRowContext(ctx, `SELECT name, checksum FROM schema_migrations WHERE version=?`, m.version).Scan(&name, &checksum)
		if err == nil {
			if name != m.name || checksum != m.checksum {
				return fmt.Errorf("migration %d (%s) name or checksum changed", m.version, m.name)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if maxVersion != m.version-1 {
			return fmt.Errorf("migration ledger has a gap before version %d", m.version)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(?,?,?,?)`,
			m.version, m.name, m.checksum, time.Now().UTC().UnixMilli())
		return err
	})
}
