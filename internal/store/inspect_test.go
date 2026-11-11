package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckMigrationsReadOnly(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"current", ""},
		{"pending", "DELETE FROM schema_migrations WHERE version=2"},
		{"gap", "DELETE FROM schema_migrations WHERE version=1"},
		{"checksum", "UPDATE schema_migrations SET checksum='changed' WHERE version=1"},
		{"ahead", "UPDATE schema_migrations SET version=99 WHERE version=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.sqlite")
			db, err := Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.query != "" {
				if _, err := db.db.ExecContext(t.Context(), tc.query); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			n, err := CheckMigrations(t.Context(), path)
			if (err != nil) != (tc.query != "") {
				t.Fatalf("count %d, error %v", n, err)
			}
			if tc.query == "" && n != 2 {
				t.Fatalf("count %d", n)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := CheckMigrations(t.Context(), missing); err == nil {
		t.Fatal("accepted absent database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("inspection created a database")
	}
}

func TestOpenRejectsPublicDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- deliberately unsafe permissions exercise rejection.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(t.Context(), path); err == nil {
		_ = db.Close()
		t.Fatal("accepted public database")
	}
}
