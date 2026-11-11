//go:build unix

package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestTrustedPathRejectsWritableAncestorsAndMissingEntries(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o777, os.ModeSticky | 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := testpath.Dir(t)
			bin := filepath.Join(parent, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, mode); err != nil {
				t.Fatal(err)
			} // #nosec G302 -- deliberately unsafe fixture.
			if err := ValidateTrustedPath([]string{bin}); err == nil {
				t.Fatal("accepted writable ancestor")
			}
			alias := filepath.Join(parent, "alias")
			if err := os.Symlink("/usr/bin", alias); err != nil {
				t.Fatal(err)
			}
			if err := ValidateTrustedPath([]string{alias}); err == nil {
				t.Fatal("accepted replaceable symlink location")
			}
		})
	}
	if err := ValidateTrustedPath([]string{filepath.Join(t.TempDir(), "missing", "bin")}); err == nil {
		t.Fatal("accepted nonexistent trusted directory")
	}
}

func TestProcessLeaderRemainsUnreapedDuringGroupSignaling(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	e, spec := helperSpec(t, "sleep", pidFile)
	e.GracePeriod = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, spec); done <- err }()
	pid := waitPIDFile(t, pidFile)[0]
	cancel()
	time.Sleep(100 * time.Millisecond)
	// A zombie still answers kill(0), pinning its PID and therefore its PGID.
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		t.Error("leader reaped before the group signaling grace period ended")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertGone(t, []int{pid})
}

type foreignOwnerInfo struct{ os.FileInfo }

func (foreignOwnerInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Getuid() + 1)} } // #nosec G115 -- test fixture UID fits all supported Unix UID ranges.
func TestTrustedDirectoryRejectsForeignOwner(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if trustedDirectory(foreignOwnerInfo{info}) == nil {
		t.Fatal("accepted directory owned by another user")
	}
}
