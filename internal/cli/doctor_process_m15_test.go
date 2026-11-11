//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestDoctorVersionBoundsInheritedPipeAndKillsChildren(t *testing.T) {
	bin := testpath.Dir(t)
	pids := filepath.Join(t.TempDir(), "pids")
	script := "#!/bin/sh\ntrap '' TERM\n/bin/sleep 60 &\nprintf '%s %s' \"$$\" \"$!\" > '" + pids + "'\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	} // #nosec G306 -- executable fixture.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	root := t.TempDir()
	go func() { _, err := doctorVersion(ctx, root, []string{bin}, "go", secret.NewRedactor()); done <- err }()
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(pids); err == nil && len(strings.Fields(string(data))) == 2 { // #nosec G304 -- readiness file belongs to this test.
			break
		}
		select {
		case err := <-done:
			t.Fatalf("version fixture did not start: %v", err)
		default:
		}
		if time.Now().After(readyDeadline) {
			cancel()
			cleanupErr := <-done
			t.Logf("fixture cleanup: %v", cleanupErr)
			t.Fatal("version fixture did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("accepted timed out version command")
		}
	case <-time.After(5 * time.Second):
		t.Error("doctor exceeded deadline while a descendant held its output pipe")
		data, _ := os.ReadFile(pids) // #nosec G304 -- readiness file belongs to this test.
		for _, s := range strings.Fields(string(data))[1:] {
			pid, _ := strconv.Atoi(s)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		cleanupErr := <-done
		t.Logf("fixture cleanup: %v", cleanupErr)
	}
	if time.Since(started) > 5*time.Second {
		t.Error("version command exceeded bounded cleanup time")
	}
	data, err := os.ReadFile(pids) // #nosec G304 -- readiness file belongs to this test.
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range strings.Fields(string(data)) {
		pid, _ := strconv.Atoi(s)
		if pid <= 0 {
			t.Fatal("invalid helper PID")
		}
		// The leader must be reaped. Orphan reaping for the child belongs to the OS.
		if s == strings.Fields(string(data))[0] && syscall.Kill(pid, 0) == nil {
			t.Error("leader still exists")
		}
	}
}
