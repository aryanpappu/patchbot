//go:build unix

package gitx

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func cleanupProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// Also terminate helpers left behind by an exited leader.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
