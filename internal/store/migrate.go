package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
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

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if !ok || parseErr != nil || version != int64(len(migrations)+1) {
			return nil, fmt.Errorf("invalid migration filename %q", name)
		}
		body, readErr := migrationFiles.ReadFile("migrations/" + name)
		if readErr != nil {
			return nil, readErr
		}
		migrations = append(migrations, migration{version, name, fmt.Sprintf("%x", sha256.Sum256(body)), string(body)})
	}
	return migrations, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
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

// foreignKeysOff marks a migration that rebuilds a referenced table. SQLite
// ignores PRAGMA foreign_keys inside a transaction and DROP TABLE would
// otherwise cascade into child rows, so the migrator switches enforcement off
// on its single connection, applies the migration in a transaction, verifies
// foreign_key_check before commit and always switches enforcement back on.
const foreignKeysOff = "-- patchbot:foreign_keys=off"

// needsForeignKeysOff reads the marker from the first line, tolerating a BOM,
// CR line endings and surrounding whitespace. A table rebuild without the
// marker would cascade deletes, so DROP TABLE without it is refused.
func needsForeignKeysOff(m migration) (bool, error) {
	first, _, _ := strings.Cut(strings.TrimPrefix(m.sql, "\uFEFF"), "\n")
	if strings.TrimSpace(first) == foreignKeysOff {
		return true, nil
	}
	if strings.Contains(strings.ToUpper(m.sql), "DROP TABLE") {
		return false, fmt.Errorf("migration %s drops a table without %s", m.name, foreignKeysOff)
	}
	return false, nil
}

func (s *SQLite) applyMigration(ctx context.Context, m migration, latest int64) error {
	off, err := needsForeignKeysOff(m)
	if err != nil {
		return err
	}
	if !off {
		return s.withTx(ctx, func(tx *eventTx) error { return applyMigrationTx(ctx, tx, m, latest, false) })
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	applyErr := func() error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := applyMigrationTx(ctx, &eventTx{Tx: tx}, m, latest, true); err != nil {
			return mapError(err)
		}
		return tx.Commit()
	}()
	_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`)
	if restoreErr != nil {
		// Never return a pooled connection without enforcement.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return errors.Join(applyErr, restoreErr)
}

func applyMigrationTx(ctx context.Context, tx *eventTx, m migration, latest int64, checkKeys bool) error {
	{
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
		if checkKeys {
			rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
			if err != nil {
				return err
			}
			violation := rows.Next()
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if violation {
				return fmt.Errorf("apply migration %s: foreign key check failed", m.name)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(?,?,?,?)`,
			m.version, m.name, m.checksum, time.Now().UTC().UnixMilli())
		return err
	}
}
