//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrivateCreationMask(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	privateUmask()
	dir := filepath.Join(t.TempDir(), "directory")
	// #nosec G301 -- verify that startup masks permissive subprocess defaults.
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	// #nosec G306 -- verify that startup masks permissive subprocess defaults.
	if err := os.WriteFile(file, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, file: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %o, want %o", info.Mode().Perm(), want)
		}
	}
}
