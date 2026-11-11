// Package testpath creates trusted tool fixtures outside shared temporary roots.
package testpath

import (
	"os"
	"path/filepath"
	"testing"
)

// Dir creates a private directory beneath the package's checkout. Unlike /tmp,
// its ancestors satisfy the production trusted-path policy on Linux and macOS.
func Dir(t *testing.T) string {
	t.Helper()
	base, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(base, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(base)
		if parent == base {
			t.Fatal("cannot find checkout root")
		}
		base = parent
	}
	path, err := os.MkdirTemp(base, ".patchbot-trusted-test-")
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	return path
}
