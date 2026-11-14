package runner

import (
	"os"
	"testing"

	"github.com/aryanpappu/patchbot/internal/repos"
	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/store"
)

func TestRunKeepsFrozenRepoSettingsAfterEdit(t *testing.T) {
	cfg := loadRepoConfig(t, "/usr/bin", repoTables)
	if err := mkData(cfg.DataDir()); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := testRegistry(t, cfg, db)
	r := &Runner{cfg: cfg, db: db, redactor: secret.NewRedactor(), repos: reg}
	ctx := t.Context()
	settings := repos.Settings{Commands: map[string][]string{"test": {"go", "test"}}, TestCommand: "test", Protected: []string{"secrets"}}
	if _, err := reg.Add(ctx, "acme/widgets", settings, "me", "ui"); err != nil {
		t.Fatal(err)
	}
	newRun := func(issue int64) store.Run {
		run, err := db.CreateRun(ctx, store.Run{Repo: "acme/widgets", IssueNumber: issue, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	inFlight := newRun(1)
	first, err := r.runRepo(ctx, inFlight)
	if err != nil || first.Protected[0] != "secrets" {
		t.Fatalf("freeze: %+v %v", first, err)
	}
	settings.Commands = map[string][]string{"test": {"make", "check"}}
	settings.Protected = []string{"deploy"}
	if _, err := reg.Update(ctx, "acme/widgets", settings, 1, "me", "ui"); err != nil {
		t.Fatal(err)
	}
	// The run in flight (and a resumed run in a fresh Runner) keeps the old settings.
	again, err := r.runRepo(ctx, inFlight)
	if err != nil || again.Commands["test"][0] != "go" || again.Protected[0] != "secrets" {
		t.Fatalf("in-flight run changed: %+v %v", again, err)
	}
	reg2 := testRegistry(t, cfg, db)
	resumed := &Runner{cfg: cfg, db: db, redactor: secret.NewRedactor(), repos: reg2}
	if got, err := resumed.runRepo(ctx, inFlight); err != nil || got.Commands["test"][0] != "go" {
		t.Fatalf("resume lost frozen settings: %+v %v", got, err)
	}
	// A new run sees the edit, and still works if the repo is later removed.
	fresh := newRun(2)
	if got, err := r.runRepo(ctx, fresh); err != nil || got.Commands["test"][0] != "make" || got.Protected[0] != "deploy" {
		t.Fatalf("new run: %+v %v", got, err)
	}
	if !r.repos.Has("acme/widgets") {
		t.Fatal("repo should be allowlisted")
	}
}

func TestRunRepoRefusesUnknownRepo(t *testing.T) {
	cfg := loadRepoConfig(t, "/usr/bin", repoTables)
	if err := mkData(cfg.DataDir()); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := &Runner{cfg: cfg, db: db, redactor: secret.NewRedactor(), repos: testRegistry(t, cfg, db)}
	run, _ := db.CreateRun(t.Context(), store.Run{Repo: "nobody/nothing", IssueNumber: 1, Mode: "fix", Provider: "anthropic", Status: store.StatusRunning})
	if _, err := r.runRepo(t.Context(), run); err == nil {
		t.Fatal("unlisted repository was frozen")
	}
}

func mkData(dir string) error { return os.MkdirAll(dir, 0o700) }
