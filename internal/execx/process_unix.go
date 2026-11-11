//go:build unix

package execx

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// RunProcessGroup owns Start and Wait for an explicit command. Callers set
// bounded output writers, WaitDelay and a private environment. The leader must
// NEVER be reaped while any group signal remains possible: an exited, unreaped
// leader pins its PID/PGID, so TERM/KILL cannot address a reused process group.
// waitExit observes exit without consuming wait status. Only after the final
// group signal do we call cmd.Wait (which reaps and bounds pipe draining).
func RunProcessGroup(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	interrupted, waitErr, processErr := runProcessGroup(ctx, cmd, grace)
	if interrupted {
		processErr = errors.Join(processErr, ctx.Err())
	}
	return errors.Join(processErr, waitErr)
}

func runProcessGroup(ctx context.Context, cmd *exec.Cmd, grace time.Duration) (interrupted bool, waitErr, processErr error) {
	if ctx.Err() != nil {
		return true, nil, nil
	}
	if grace < 0 || cmd.WaitDelay < 0 {
		return false, nil, errors.New("process limits must not be negative")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	if err := cmd.Start(); err != nil {
		return false, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- waitExit(cmd.Process.Pid) }()
	var observeErr, signalErr error
	select {
	case observeErr = <-exited:
		// Normal exits also clean up descendants. No signal may follow Wait.
		signalErr = finalSignalError(signalGroup(cmd.Process.Pid, syscall.SIGKILL), observeErr)
	case <-ctx.Done():
		interrupted = true
		signalErr = signalGroup(cmd.Process.Pid, syscall.SIGTERM)
		if grace == 0 {
			grace = DefaultGracePeriod
		}
		// The zombie anchor keeps the group present. Existence polling cannot tell
		// whether live descendants remain, so preserve the full grace period.
		timer := time.NewTimer(grace)
		<-timer.C
		killErr := signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		observeErr = <-exited
		signalErr = errors.Join(signalErr, finalSignalError(killErr, observeErr))
	}
	return interrupted, cmd.Wait(), errors.Join(observeErr, signalErr)
}

func signalGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func finalSignalError(err, observeErr error) error {
	// Darwin returns EPERM for a pinned group with only zombie members. Ignore
	// it only after non-reaping observation confirms that our leader exited.
	if observeErr == nil && runtime.GOOS == "darwin" && errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
