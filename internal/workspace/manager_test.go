package workspace_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/workspace"
)

func newManager(t *testing.T) (string, *workspace.Manager) {
	t.Helper()
	data, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := workspace.NewManager(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return data, m
}

func TestManagerCreate(t *testing.T) {
	t.Parallel()
	data, m := newManager(t)
	ws, err := m.Create("42", workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(data, "workspaces", "run-42")
	if ws.RunDir() != runDir || ws.Dir() != filepath.Join(runDir, "repo") {
		t.Errorf("RunDir = %q, Dir = %q", ws.RunDir(), ws.Dir())
	}
	for _, dir := range []string{filepath.Join(data, "workspaces"), runDir, ws.Dir()} {
		if got := modeOf(t, dir); !got.IsDir() || got.Perm() != 0o700 {
			t.Errorf("%s has mode %v, want a directory with 0700", dir, got)
		}
	}
	if err := ws.WriteFile("hello.txt", []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("42", workspace.Options{}); !errors.Is(err, workspace.ErrRunExists) {
		t.Errorf("second Create = %v, want ErrRunExists", err)
	}
	if err := ws.Close(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(runDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory still exists after Close(false): %v", err)
	}
	if err := ws.Close(false); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := ws.ReadFile("hello.txt"); err == nil {
		t.Error("ReadFile after Close succeeded")
	}
	// The ID is free again after cleanup.
	again, err := m.Create("42", workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(runDir, "repo")); err != nil {
		t.Errorf("Close(true) removed the checkout: %v", err)
	}
}

func TestManagerRejectsBadInput(t *testing.T) {
	t.Parallel()
	data, m := newManager(t)
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "-x", "a b", "a\x00", strings.Repeat("a", 65), "é"} {
		if ws, err := m.Create(id, workspace.Options{}); err == nil {
			_ = ws.Close(false)
			t.Errorf("Create(%q) succeeded", id)
		}
	}
	if _, err := m.Create("ok", workspace.Options{Protected: []string{"../x"}}); err == nil {
		t.Error("Create with a bad protected path succeeded")
	}
	if _, err := os.Lstat(filepath.Join(data, "workspaces", "run-ok")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a failed Create left its run directory behind")
	}
	if _, err := workspace.NewManager(""); err == nil {
		t.Error("NewManager with no data directory succeeded")
	}
	file := filepath.Join(data, "file")
	mustWrite(t, file, "", 0o644)
	if _, err := workspace.NewManager(file); err == nil {
		t.Error("NewManager on a regular file succeeded")
	}
	// A workspaces directory that is a symlink is refused: run directories
	// are deleted through it.
	elsewhere, linked := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(linked, "workspaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.NewManager(linked); err == nil {
		t.Error("NewManager on a symlinked workspaces directory succeeded")
	}
}

func TestManagerTightensExistingDirectory(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	base := filepath.Join(data, "workspaces")
	//nolint:gosec // too open on purpose: NewManager must tighten it
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := workspace.NewManager(data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if got := modeOf(t, base).Perm(); got != 0o700 {
		t.Errorf("mode = %v, want 0700", got)
	}
}

func TestManagerCleanupStaysInside(t *testing.T) {
	t.Parallel()
	_, m := newManager(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "precious.txt"), "keep\n", 0o644)

	ws, err := m.Create("r1", workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// What a test command could leave behind: a symlink to the outside and
	// a read-only tree, as in a Go module cache.
	if err := os.Symlink(outside, filepath.Join(ws.Dir(), "escape")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws.Dir(), "cache", "mod", "f.go"), "x", 0o400)
	for _, dir := range []string{filepath.Join(ws.Dir(), "cache", "mod"), filepath.Join(ws.Dir(), "cache")} {
		//nolint:gosec // a read-only directory, as in a Go module cache
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.Close(false); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(ws.RunDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory still exists: %v", err)
	}
	if got := hostFile(t, filepath.Join(outside, "precious.txt")); string(got) != "keep\n" {
		t.Errorf("cleanup reached outside the run directory: %q", got)
	}
}

func TestManagerSweep(t *testing.T) {
	t.Parallel()
	data, m := newManager(t)
	base := filepath.Join(data, "workspaces")
	old := time.Now().Add(-48 * time.Hour)
	age := func(name string) {
		t.Helper()
		if err := os.Chtimes(filepath.Join(base, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	kept, err := m.Create("kept", workspace.Options{}) // left by an earlier run
	if err != nil {
		t.Fatal(err)
	}
	if err := kept.Close(true); err != nil {
		t.Fatal(err)
	}
	age("run-kept")
	active, err := m.Create("active", workspace.Options{}) // old but in use
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close(false) }()
	age("run-active")
	fresh, err := m.Create("fresh", workspace.Options{}) // closed, but recent
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(true); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(base, "notes.txt"), "mine", 0o600) // not a run
	if err := os.Mkdir(filepath.Join(base, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	age("notes.txt")
	age("other")

	removed, err := m.Sweep(t.Context(), 24*time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 removed", removed, err)
	}
	for name, want := range map[string]bool{"run-kept": false, "run-active": true, "run-fresh": true, "notes.txt": true, "other": true} {
		_, err := os.Lstat(filepath.Join(base, name))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", name, exists, want)
		}
	}
	if err := active.WriteFile("still.txt", []byte("works")); err != nil {
		t.Errorf("the active workspace broke after Sweep: %v", err)
	}
}

// Finding 1: replacing the manager's absolute pathname between calls must not
// redirect a checkout away from the directory pinned by NewManager.
func TestManagerCreateUsesPinnedRoot(t *testing.T) {
	t.Parallel()
	data, m := newManager(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "precious"), "outside", 0o600)
	attacker := t.TempDir()
	if err := os.Mkdir(filepath.Join(attacker, "run-next"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(attacker, "run-next", "repo")); err != nil {
		t.Fatal(err)
	}
	base, parked := filepath.Join(data, "workspaces"), filepath.Join(data, "parked")
	if err := os.Rename(base, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, base); err != nil {
		t.Fatal(err)
	}
	ws, err := m.Create("next", workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(false) }()
	if got, err := ws.ReadFile("precious"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("poisoned checkout read %q, %v; want not found", got, err)
	}
	if err := ws.WriteFile("created", []byte("inside")); err != nil {
		t.Fatal(err)
	}
	if got := hostFile(t, filepath.Join(parked, "run-next", "repo", "created")); string(got) != "inside" {
		t.Errorf("write did not use pinned checkout: %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "created")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write reached outside: %v", err)
	}
}
