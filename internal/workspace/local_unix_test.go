//go:build unix

package workspace_test

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/workspace"
)

// A FIFO must be refused without blocking: opening one for reading waits
// for a writer that never comes.
func TestSpecialFilesRefused(t *testing.T) {
	t.Parallel()
	dir, ws := openDir(t, workspace.Options{})
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := ws.ReadFile("pipe")
		done <- err
		done <- ws.WriteFile("pipe", []byte("x"))
	}()
	for _, op := range []string{"read", "write"} {
		select {
		case err := <-done:
			if !errors.Is(err, workspace.ErrNotRegular) {
				t.Errorf("%s of a FIFO = %v, want ErrNotRegular", op, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s of a FIFO blocked", op)
		}
	}
	entries, err := ws.ReadDir(".")
	if err != nil || len(entries) != 1 || entries[0].Type().IsRegular() {
		t.Errorf("ReadDir = %v, %v; want the FIFO listed as a special file", entries, err)
	}
}
