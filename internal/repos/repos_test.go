package repos

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aryanpappu/patchbot/internal/config"
	"github.com/aryanpappu/patchbot/internal/store"
)

const digest = "example.test/img:1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testConfig(container bool) *config.Config {
	cfg := config.Default()
	cfg.Defaults.Approvers = []string{"owner"}
	cfg.Repos = map[string]*config.Repo{"owner/fixed": {Commands: map[string][]string{"test": {"go", "test", "./..."}}}}
	if container {
		cfg.Executor.Kind, cfg.Executor.Runtime, cfg.Executor.Image = config.ExecutorContainer, "podman", digest
	}
	return cfg
}

func testRegistry(t *testing.T, cfg *config.Config) (*Registry, *store.SQLite) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "p.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := New(t.Context(), cfg, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg, db
}

func goSettings() Settings {
	return Settings{Commands: map[string][]string{"test": {"go", "test", "./..."}}, TestCommand: "test"}
}

// The dashboard must accept and refuse exactly what config.Validate accepts
// and refuses for a [repos."owner/name"] table.
func TestValidationParityWithConfig(t *testing.T) {
	cases := []struct {
		name      string
		repo      string
		container bool
		s         Settings
	}{
		{"valid", "acme/widgets", false, goSettings()},
		{"no commands", "acme/widgets", false, Settings{}},
		{"bad name", "widgets", false, goSettings()},
		{"bad name dots", "acme/..", false, goSettings()},
		{"bad id", "acme/widgets", false, Settings{Commands: map[string][]string{"Bad ID": {"go"}}}},
		{"empty argv", "acme/widgets", false, Settings{Commands: map[string][]string{"test": {}}}},
		{"shell string as program", "acme/widgets", false, Settings{Commands: map[string][]string{"test": {"go test ./..."}}}},
		{"shell metachar", "acme/widgets", false, Settings{Commands: map[string][]string{"test": {"sh;rm"}}}},
		{"dash program", "acme/widgets", false, Settings{Commands: map[string][]string{"test": {"-x"}}}},
		{"nul arg", "acme/widgets", false, Settings{Commands: map[string][]string{"test": {"go", "a\x00b"}}}},
		{"test command missing", "acme/widgets", false, Settings{Commands: map[string][]string{"build": {"make"}}, TestCommand: "test"}},
		{"protected absolute", "acme/widgets", false, Settings{Protected: []string{"/etc"}}},
		{"protected dotdot", "acme/widgets", false, Settings{Protected: []string{"a/../b"}}},
		{"protected ok", "acme/widgets", false, Settings{Protected: []string{"secrets", "deploy/prod"}}},
		{"container needs digest", "acme/widgets", true, Settings{Image: "example.test/img:latest"}},
		{"container digest ok", "acme/widgets", true, Settings{Image: digest}},
		{"container no image uses global", "acme/widgets", true, goSettings()},
		{"container bad path", "acme/widgets", true, Settings{Path: []string{"relative/bin"}}},
		{"container ok path", "acme/widgets", true, Settings{Path: []string{"/usr/local/bin", "/usr/bin"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(tc.container)
			reg, _ := testRegistry(t, cfg)
			dashboardErr := reg.Validate(tc.repo, tc.s)

			fileCfg := testConfig(tc.container)
			fileCfg.Repos[tc.repo] = tc.s.Repo()
			fileErr := fileCfg.Validate()
			if (dashboardErr == nil) != (fileErr == nil) {
				t.Fatalf("dashboard: %v\nconfig file: %v", dashboardErr, fileErr)
			}
			if dashboardErr != nil && !errors.Is(dashboardErr, ErrInvalid) {
				t.Fatalf("not an ErrInvalid: %v", dashboardErr)
			}
		})
	}
}

func TestCollisionWithConfigRepoRefused(t *testing.T) {
	reg, _ := testRegistry(t, testConfig(false))
	if _, err := reg.Add(t.Context(), "Owner/Fixed", goSettings(), "me", "ui"); !errors.Is(err, ErrCollision) {
		t.Fatalf("add: %v", err)
	}
	if _, err := reg.Update(t.Context(), "owner/fixed", goSettings(), 1, "me", "ui"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("update: %v", err)
	}
	if err := reg.Remove(t.Context(), "owner/fixed", 0, "me", "ui"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("remove: %v", err)
	}
	e, ok := reg.Get("owner/fixed")
	if !ok || e.Source != SourceConfig || e.Version != 0 {
		t.Fatalf("config repo: %+v", e)
	}
}

// A row that collides with a config repository (added to the file later) stays
// inert: the config file wins and the allowlist is not widened.
func TestStoredRowCollidingWithLaterConfigIsInert(t *testing.T) {
	cfg := testConfig(false)
	reg, db := testRegistry(t, cfg)
	if _, err := reg.Add(t.Context(), "acme/widgets", goSettings(), "me", "ui"); err != nil {
		t.Fatal(err)
	}
	cfg2 := testConfig(false)
	cfg2.Repos["acme/widgets"] = &config.Repo{Commands: map[string][]string{"test": {"make", "check"}}}
	reg2, err := New(t.Context(), cfg2, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := reg2.Get("acme/widgets")
	if e.Source != SourceConfig || e.Settings.Commands["test"][0] != "make" {
		t.Fatalf("config must win: %+v", e)
	}
}

func TestAddEditRemoveAndLimits(t *testing.T) {
	reg, _ := testRegistry(t, testConfig(false))
	ctx := t.Context()
	e, err := reg.Add(ctx, " Acme/Widgets ", goSettings(), "me@example.com", "ui")
	if err != nil || e.Name != "acme/widgets" || e.Version != 1 || e.Source != SourceDashboard || len(e.BuiltinProtected) == 0 {
		t.Fatalf("add: %+v %v", e, err)
	}
	if _, err := reg.Add(ctx, "acme/widgets", goSettings(), "me", "ui"); !errors.Is(err, store.ErrRepoExists) {
		t.Fatalf("duplicate: %v", err)
	}
	s := goSettings()
	s.Protected = []string{"secrets"}
	e, err = reg.Update(ctx, "acme/widgets", s, 1, "me", "ui")
	if err != nil || e.Version != 2 || e.Settings.Protected[0] != "secrets" {
		t.Fatalf("edit: %+v %v", e, err)
	}
	if _, err := reg.Update(ctx, "acme/widgets", s, 1, "me", "ui"); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if got, ok := reg.Lookup("acme/widgets"); !ok || got.Protected[0] != "secrets" {
		t.Fatalf("lookup: %+v", got)
	}
	// Lookup returns a copy: mutating it cannot change the registry.
	got, _ := reg.Lookup("acme/widgets")
	got.Commands["test"][0] = "evil"
	if again, _ := reg.Lookup("acme/widgets"); again.Commands["test"][0] != "go" {
		t.Fatal("registry state was mutated through a lookup")
	}
	tooMany := Settings{Commands: map[string][]string{}}
	for i := range maxCommands + 1 {
		tooMany.Commands[fmt.Sprintf("c%d", i)] = []string{"go"}
	}
	if _, err := reg.Update(ctx, "acme/widgets", tooMany, 2, "me", "ui"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("limit: %v", err)
	}
	if err := reg.Remove(ctx, "acme/widgets", 2, "me", "ui"); err != nil {
		t.Fatal(err)
	}
	if reg.Has("acme/widgets") {
		t.Fatal("removed repository is still allowlisted")
	}
	rows, err := reg.Audit(ctx, "acme/widgets")
	if err != nil || len(rows) != 3 || rows[0].Actor != "me" {
		t.Fatalf("audit: %+v %v", rows, err)
	}
}

func TestCredentialShapedSettingsRefused(t *testing.T) {
	reg, _ := testRegistry(t, testConfig(false))
	s := goSettings()
	s.Commands["test"] = []string{"go", "test", "ghp_" + strings.Repeat("a1B2", 9)}
	if _, err := reg.Add(t.Context(), "acme/widgets", s, "me", "ui"); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("credential-shaped argument: %v", err)
	}
}

func TestBuiltinProtectedCannotBeRemoved(t *testing.T) {
	reg, _ := testRegistry(t, testConfig(false))
	e, err := reg.Add(t.Context(), "acme/widgets", Settings{Protected: []string{}}, "me", "ui")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.BuiltinProtected) != 1 || e.BuiltinProtected[0] != ".github/workflows" {
		t.Fatalf("builtin protected: %v", e.BuiltinProtected)
	}
}

func TestRegistryConcurrency(t *testing.T) {
	reg, _ := testRegistry(t, testConfig(false))
	ctx := t.Context()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("acme/r%d", i)
			if _, err := reg.Add(ctx, name, goSettings(), "me", "ui"); err != nil {
				t.Error(err)
				return
			}
			for v := int64(1); v <= 5; v++ {
				if _, err := reg.Update(ctx, name, goSettings(), v, "me", "ui"); err != nil {
					t.Error(err)
					return
				}
				_ = reg.List()
				reg.Has(name)
				reg.Lookup(name)
				reg.Names()
			}
			if i%2 == 0 {
				if err := reg.Remove(ctx, name, 0, "me", "ui"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	// Readers run while writers mutate.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = reg.List()
				reg.Has("acme/r3")
			}
		}()
	}
	wg.Wait()
	if n := len(reg.Names()); n != 1+4 {
		t.Fatalf("names: %v", reg.Names())
	}
}

func TestMergeReportsProblems(t *testing.T) {
	cfg := testConfig(false)
	recs := []store.RepoRecord{
		{Name: "acme/ok", SettingsJSON: `{"commands":{"test":["go","test"]}}`},
		{Name: "owner/fixed", SettingsJSON: `{}`},
		{Name: "acme/bad", SettingsJSON: `{"commands":{"test":["a b"]}}`},
		{Name: "acme/junk", SettingsJSON: `[]`},
	}
	merged, dashboard, problems := Merge(cfg, recs)
	if len(dashboard) != 1 || dashboard[0] != "acme/ok" || merged["acme/ok"] == nil || merged["owner/fixed"] == nil {
		t.Fatalf("merged: %v %v", merged, dashboard)
	}
	if !errors.Is(problems["owner/fixed"], ErrCollision) || problems["acme/bad"] == nil || problems["acme/junk"] == nil || len(cfg.Repos) != 1 {
		t.Fatalf("problems: %v", problems)
	}
}
