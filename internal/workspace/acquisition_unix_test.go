//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Finding 3: force the swap both before the precheck and between the precheck
// and the actual open. The latter must use a directory-only kernel open.
func TestDirectoryAcquisitionDoesNotOpenFIFO(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"resolved", "before-open-dir"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ws := acquisitionFixture(t, Options{})
			pipe := filepath.Join(ws.Dir(), "a")
			ws.testHook = func(at string) {
				if at != stage {
					return
				}
				if err := os.Rename(pipe, filepath.Join(ws.Dir(), "parked")); err != nil {
					t.Error(err)
					return
				}
				if err := syscall.Mkfifo(pipe, 0o600); err != nil {
					t.Error(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := ws.ReadFile("a/config")
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO directory accepted")
				}
			case <-time.After(2 * time.Second):
				// Release a regressed blocking open so the test fails cleanly.
				f, err := os.OpenFile(pipe, os.O_RDWR|syscall.O_NONBLOCK, 0) //nolint:gosec // test FIFO
				if err == nil {
					defer func() { _ = f.Close() }()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Error("FIFO directory accepted after recovery")
					}
				case <-time.After(time.Second):
					t.Error("blocked open did not recover")
				}
				t.Fatal("directory acquisition blocked on a FIFO")
			}
		})
	}
}
