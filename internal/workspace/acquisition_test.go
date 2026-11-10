package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func acquisitionFixture(t *testing.T, opts Options) *Local {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"a", "b", ".git"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "config"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	return ws
}

func exchangeDirs(t *testing.T, dir, a, b string) {
	t.Helper()
	for _, pair := range [][2]string{{a, "parked"}, {b, a}, {"parked", b}} {
		if err := os.Rename(filepath.Join(dir, pair[0]), filepath.Join(dir, pair[1])); err != nil {
			t.Fatal(err)
		}
	}
}

// Finding 4: force the reviewed exploit's real-directory exchange after
// resolution. Swapping back after pinning cannot launder the retained handle.
func TestPinnedDirectoriesKeepApprovedIdentity(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"resolved", "before-open-dir", "pinned"} {
		for _, operation := range []string{"read", "write", "list", "stat"} {
			t.Run(stage+"/"+operation, func(t *testing.T) {
				t.Parallel()
				ws := acquisitionFixture(t, Options{})
				swapped := false
				ws.testHook = func(at string) {
					if at == stage && !swapped {
						exchangeDirs(t, ws.Dir(), "a", ".git")
						swapped = true
					} else if at == "pinned" && stage != "pinned" && swapped {
						exchangeDirs(t, ws.Dir(), "a", ".git")
					}
				}
				var err error
				switch operation {
				case "read":
					_, err = ws.ReadFile("a/config")
				case "write":
					err = ws.WriteFile("a/config", []byte("changed"))
				case "list":
					_, err = ws.ReadDir("a")
				case "stat":
					_, err = ws.Stat("a/config")
				}
				if !swapped || (!errors.Is(err, ErrSymlink) && !errors.Is(err, ErrPathProtected)) {
					t.Fatalf("swap=%v, %s returned %v; want changed/protected path", swapped, operation, err)
				}
				// The protected directory might still be parked at a.
				for _, name := range []string{"a", ".git"} {
					data, readErr := os.ReadFile(filepath.Join(ws.Dir(), name, "config")) //nolint:gosec // test fixture
					if readErr != nil || string(data) == "changed" {
						t.Fatalf("%s/config was modified: %q, %v", name, data, readErr)
					}
				}
			})
		}
	}
}

func TestPinnedDirectoryRechecksNewProtectedIdentity(t *testing.T) {
	t.Parallel()
	ws := acquisitionFixture(t, Options{Protected: []string{"sealed"}})
	ws.testHook = func(stage string) {
		if stage == "pinned" {
			if err := os.Symlink("a", filepath.Join(ws.Dir(), "sealed")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := ws.ReadFile("a/config"); !errors.Is(err, ErrPathProtected) {
		t.Fatalf("new protected alias was ignored: %v", err)
	}
}

// An unrelated real directory is also refused, even if it is not protected.
// This distinguishes binding approved identities from merely rescanning .git.
func TestDirectoryRecheckedAfterOpen(t *testing.T) {
	t.Parallel()
	ws := acquisitionFixture(t, Options{})
	ws.testHook = func(stage string) {
		if stage == "before-open-dir" {
			exchangeDirs(t, ws.Dir(), "a", "b")
		}
	}
	if _, err := ws.ReadFile("a/config"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("opened a different directory than approved: %v", err)
	}
}
