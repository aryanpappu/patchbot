package workspace_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/workspace"
	"github.com/aryanpappu/patchbot/internal/workspace/wstest"
)

func openDir(t testing.TB, opts workspace.Options) (string, *workspace.Local) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	return dir, ws
}

func mustWrite(t testing.TB, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// hostFile reads a file by host path, around the workspace. A missing file
// reads as empty.
func hostFile(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path inside the test's temp dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return data
}

func modeOf(t testing.TB, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

// leftovers returns the temporary files a write left behind.
func leftovers(t testing.TB, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.Base(p), ".patchbot-") {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestWriteFileCreatesAndReplaces(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{})

	if err := ws.WriteFile("a/b/c/new.txt", []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "a", "b", "c", "new.txt")
	if got := hostFile(t, p); string(got) != "one\n" {
		t.Errorf("content = %q", got)
	}
	if got := modeOf(t, p).Perm(); got != 0o600 {
		t.Errorf("new file mode = %v, want 0600", got)
	}
	if got := modeOf(t, filepath.Dir(p)).Perm(); got != 0o700 {
		t.Errorf("new directory mode = %v, want 0700", got)
	}
	if err := ws.WriteFile("a/b/c/new.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := hostFile(t, p); len(got) != 0 {
		t.Errorf("content after empty write = %q", got)
	}
	if left := leftovers(t, dir); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestWriteFileKeepsMode(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{})
	for _, mode := range []fs.FileMode{0o755, 0o600, 0o640, 0o444} {
		name := "script-" + mode.String()
		p := filepath.Join(dir, name)
		mustWrite(t, p, "old\n", mode)
		if err := ws.WriteFile(name, []byte("new\n")); err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
		if got := modeOf(t, p).Perm(); got != mode&0o700 {
			t.Errorf("mode after write = %v, want %v", got, mode&0o700)
		}
		if got := hostFile(t, p); string(got) != "new\n" {
			t.Errorf("mode %v: content = %q", mode, got)
		}
	}
}

func TestWriteFileIsAtomic(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{})
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "old\n", 0o644)

	// A reader that opened the file before the write keeps the old content:
	// the write makes a new file and renames it, and never truncates.
	old, err := os.Open(p) //nolint:gosec // a path inside the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	before, _ := os.Stat(p)
	if err := ws.WriteFile("f.txt", []byte("new content\n")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if os.SameFile(before, after) {
		t.Error("the file was rewritten in place, want a rename of a new file")
	}
	buf := make([]byte, 64)
	n, _ := old.Read(buf)
	if string(buf[:n]) != "old\n" {
		t.Errorf("open handle now reads %q, want the old content", buf[:n])
	}
}

func TestWriteFileFailureLeavesNothing(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{MaxWriteBytes: 16})
	mustWrite(t, filepath.Join(dir, "keep.txt"), "keep\n", 0o644)
	if err := os.Mkdir(filepath.Join(dir, "ro"), 0o500); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // a directory needs its search bit back to be removed
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "ro"), 0o700) })

	err := ws.WriteFile("keep.txt", bytes.Repeat([]byte("x"), 17))
	if !errors.Is(err, workspace.ErrTooLarge) {
		t.Errorf("oversized write = %v, want ErrTooLarge", err)
	}
	if err := ws.WriteFile("big/new.txt", bytes.Repeat([]byte("x"), 17)); !errors.Is(err, workspace.ErrTooLarge) {
		t.Errorf("oversized new file = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a refused write created its parent directory")
	}
	if err := ws.WriteFile("keep.txt", bytes.Repeat([]byte("x"), 16)); err != nil {
		t.Errorf("write at the limit: %v", err)
	}
	if os.Getuid() != 0 {
		if err := ws.WriteFile("ro/new.txt", []byte("x")); err == nil {
			t.Error("write into a read-only directory succeeded")
		}
	}
	if err := ws.WriteFile("keep.txt/child", []byte("x")); err == nil {
		t.Error("write below a regular file succeeded")
	}
	if left := leftovers(t, dir); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestWriteThroughSymlinkKeepsTheLink(t *testing.T) {
	t.Parallel()
	fx, ws := openFixture(t)
	if err := ws.WriteFile("links/ok-file", []byte("via link\n")); err != nil {
		t.Fatal(err)
	}
	if modeOf(t, filepath.Join(fx.Repo, "links", "ok-file"))&fs.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if got := hostFile(t, filepath.Join(fx.Repo, "README.md")); string(got) != "via link\n" {
		t.Errorf("target content = %q", got)
	}
	if err := ws.WriteFile("links/ok-dir/added.go", []byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fx.Repo, "src", "added.go")); err != nil {
		t.Errorf("file written through a symlinked directory: %v", err)
	}
}

func TestReadFileLimit(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{MaxReadBytes: 10})
	mustWrite(t, filepath.Join(dir, "ok.txt"), "0123456789", 0o644)
	mustWrite(t, filepath.Join(dir, "big.txt"), "0123456789X", 0o644)
	if got, err := ws.ReadFile("ok.txt"); err != nil || string(got) != "0123456789" {
		t.Errorf("ReadFile at the limit = %q, %v", got, err)
	}
	if got, err := ws.ReadFile("big.txt"); !errors.Is(err, workspace.ErrTooLarge) || got != nil {
		t.Errorf("ReadFile over the limit = %q, %v; want ErrTooLarge", got, err)
	}
}

func TestReadDirLimitAndOrder(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{MaxDirEntries: 5})
	for _, name := range []string{"b", "a", "c"} {
		mustWrite(t, filepath.Join(dir, "few", name), "", 0o644)
	}
	for i := range 9 {
		mustWrite(t, filepath.Join(dir, "many", string(rune('a'+i))), "", 0o644)
	}
	entries, err := ws.ReadDir("few")
	if err != nil || len(entries) != 3 || entries[0].Name() != "a" || entries[2].Name() != "c" {
		t.Errorf("ReadDir(few) = %v, %v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || !info.Mode().IsRegular() {
		t.Errorf("entry info = %v, %v", info, err)
	}
	entries, err = ws.ReadDir("many")
	if !errors.Is(err, workspace.ErrDirTruncated) || len(entries) != 5 {
		t.Errorf("ReadDir(many) = %d entries, %v; want 5 and ErrDirTruncated", len(entries), err)
	}
}

func TestStat(t *testing.T) {
	t.Parallel()
	_, ws := openFixture(t)
	if info, err := ws.Stat("."); err != nil || !info.IsDir() {
		t.Errorf("Stat(.) = %v, %v", info, err)
	}
	if info, err := ws.Stat("links/ok-file"); err != nil || !info.Mode().IsRegular() || info.Name() != "README.md" {
		t.Errorf("Stat(symlink) = %v, %v; want the target README.md", info, err)
	}
	if _, err := ws.Stat("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing) = %v", err)
	}
}

// TestProtectedByIdentity covers a name that leads to a protected path
// without any rule naming it: the protected path is itself a symlink, and
// the directory it points to is reached directly. The identity check
// refuses it.
func TestProtectedByIdentity(t *testing.T) {
	t.Parallel()
	dir, _ := openDir(t, workspace.Options{})
	mustWrite(t, filepath.Join(dir, "vault", "key.txt"), "KEY"+wstest.Marker, 0o644)
	if err := os.Symlink("vault", filepath.Join(dir, "sealed")); err != nil {
		t.Fatal(err)
	}
	// "sealed" is protected by name. "vault" is what it resolves to, and no
	// rule names it, but it is the same directory.
	ws, err := workspace.Open(dir, workspace.Options{Protected: []string{"sealed"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(true) }()
	if data, err := ws.ReadFile("vault/key.txt"); !errors.Is(err, workspace.ErrPathProtected) {
		t.Errorf("ReadFile(vault/key.txt) = %q, %v; want ErrPathProtected", data, err)
	}
	if err := ws.WriteFile("vault/new.txt", []byte("x")); !errors.Is(err, workspace.ErrPathProtected) {
		t.Errorf("WriteFile(vault/new.txt) = %v; want ErrPathProtected", err)
	}
}

func TestConcurrentWritesAreWhole(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{})
	a, b := bytes.Repeat([]byte("a"), 200_000), bytes.Repeat([]byte("b"), 150_000)
	if err := ws.WriteFile("f.bin", a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 25 {
				data := a
				if (w+i)%2 == 0 {
					data = b
				}
				if err := ws.WriteFile("f.bin", data); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 50 {
				got, err := ws.ReadFile("f.bin")
				if err != nil {
					t.Errorf("read: %v", err)
					continue
				}
				if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
					t.Errorf("read a torn file of %d bytes", len(got))
				}
			}
		})
	}
	wg.Wait()
	if left := leftovers(t, dir); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// TestSwappedSymlinkIsNotFollowed swaps a directory for a symlink to .git
// while it is being read and written. No read may return the protected
// content and no write may land in .git.
func TestSwappedSymlinkIsNotFollowed(t *testing.T) {
	t.Parallel()
	fx, ws := openFixture(t)
	repo := fx.Repo
	mustWrite(t, filepath.Join(repo, "swap", "config"), "INSIDE\n", 0o644)
	gitBefore := hostFile(t, filepath.Join(repo, ".git", "config"))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		swap, parked := filepath.Join(repo, "swap"), filepath.Join(repo, "parked")
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(swap, parked)
			_ = os.Symlink(".git", swap)
			_ = os.Remove(swap)
			_ = os.Rename(parked, swap)
		}
	})
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if data, err := ws.ReadFile("swap/config"); err == nil && string(data) != "INSIDE\n" && string(data) != "mine\n" {
			t.Fatalf("read through a swapped symlink returned %q", data)
		}
		_ = ws.WriteFile("swap/config", []byte("mine\n"))
		_ = ws.WriteFile("swap/planted", []byte("mine\n"))
	}
	close(stop)
	wg.Wait()

	if got := hostFile(t, filepath.Join(repo, ".git", "config")); !bytes.Equal(got, gitBefore) {
		t.Errorf(".git/config was changed to %q", got)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".git", "planted")); err == nil {
		t.Error("a file was planted in .git")
	}
}
