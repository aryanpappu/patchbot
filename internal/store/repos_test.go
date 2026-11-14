package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestReposMigrationApplied(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	var name string
	if err := s.db.QueryRowContext(t.Context(), `SELECT name FROM schema_migrations WHERE version=10`).Scan(&name); err != nil || name != "0010_repos.sql" {
		t.Fatalf("migration 10: %q %v", name, err)
	}
	for _, table := range []string{"dashboard_repos", "repo_audit"} {
		var n int
		if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
}

func TestRepoLifecycleAuditAndConflicts(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	audit := func(action string) RepoAudit {
		return RepoAudit{Repo: "acme/widgets", Action: action, Actor: "me@example.com", Channel: "ui", AfterJSON: "{}"}
	}
	rec, err := s.AddRepo(ctx, RepoRecord{Name: "acme/widgets", SettingsJSON: `{"commands":{}}`, CreatedBy: "me@example.com"}, audit("add"))
	if err != nil || rec.Version != 1 || rec.CreatedBy != "me@example.com" {
		t.Fatalf("add: %+v %v", rec, err)
	}
	if _, err := s.AddRepo(ctx, RepoRecord{Name: "acme/widgets", SettingsJSON: `{}`}, audit("add")); !errors.Is(err, ErrRepoExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.UpdateRepo(ctx, RepoRecord{Name: "acme/widgets", SettingsJSON: `{"a":1}`, UpdatedBy: "x"}, 7, audit("edit")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	rec, err = s.UpdateRepo(ctx, RepoRecord{Name: "acme/widgets", SettingsJSON: `{"a":1}`, UpdatedBy: "x"}, 1, audit("edit"))
	if err != nil || rec.Version != 2 || rec.UpdatedBy != "x" {
		t.Fatalf("update: %+v %v", rec, err)
	}
	if _, err := s.UpdateRepo(ctx, RepoRecord{Name: "acme/none", SettingsJSON: `{}`}, 1, audit("edit")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// An active run blocks removal; a finished one does not.
	run := mustRun(t, s, Run{Repo: "acme/widgets", IssueNumber: 5, Status: StatusAwaitingApproval})
	if err := s.RemoveRepo(ctx, "acme/widgets", 0, audit("remove")); !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("busy: %v", err)
	}
	if err := s.SetRunStatus(ctx, run.ID, StatusAwaitingApproval, StatusCancelled, RunPatch{Phase: PhaseRefine}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepo(ctx, "acme/widgets", 99, audit("remove")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("remove with stale version: %v", err)
	}
	if err := s.RemoveRepo(ctx, "acme/widgets", 2, audit("remove")); err != nil {
		t.Fatal(err)
	}
	if recs, err := s.ListRepos(ctx); err != nil || len(recs) != 0 {
		t.Fatalf("list: %+v %v", recs, err)
	}
	rows, err := s.ListRepoAudit(ctx, "acme/widgets", 0)
	if err != nil || len(rows) != 3 || rows[0].Action != "remove" || rows[2].Action != "add" || rows[0].At.IsZero() {
		t.Fatalf("audit survives removal: %+v %v", rows, err)
	}
	// The history stays: a run row for the removed repository is untouched.
	if _, err := s.GetRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRepoAuditIsAppendOnly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	if _, err := s.AddRepo(t.Context(), RepoRecord{Name: "acme/widgets", SettingsJSON: `{}`, CreatedBy: "a"}, RepoAudit{Repo: "acme/widgets", Action: "add", Actor: "a", Channel: "ui"}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`UPDATE repo_audit SET actor='b'`, `DELETE FROM repo_audit`} {
		if _, err := s.db.ExecContext(t.Context(), statement); err == nil {
			t.Fatalf("%s succeeded", statement)
		}
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO repo_audit(repo,action,actor,channel,at) VALUES('x/y','nuke','a','ui',1)`); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestReadDashboardReposReadOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "patchbot.sqlite")
	if recs, err := ReadDashboardRepos(t.Context(), path); err != nil || recs != nil {
		t.Fatalf("missing database: %v %v", recs, err)
	}
	s := openTestPath(t, path)
	if _, err := s.AddRepo(t.Context(), RepoRecord{Name: "acme/widgets", SettingsJSON: `{}`, CreatedBy: "a", CreatedAt: time.Now()}, RepoAudit{Repo: "acme/widgets", Action: "add", Actor: "a", Channel: "ui"}); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadDashboardRepos(t.Context(), path)
	if err != nil || len(recs) != 1 || recs[0].Name != "acme/widgets" {
		t.Fatalf("read: %+v %v", recs, err)
	}
}
