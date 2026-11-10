//go:build unix

package execx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Run starts one process group. Timeout/cancel signals TERM, then KILL after the
// grace period, even if the leader has already exited. Wait reaps the direct
// child; descendant parents (or the OS reaper) reap their own children.
func (e *LocalExecutor) Run(ctx context.Context, spec Spec) (result Result, runErr error) {
	started := time.Now()
	result.ExitCode = -1
	defer func() { result.Duration = time.Since(started) }()
	if err := e.validate(&spec); err != nil {
		return result, err
	}
	env, err := scrubEnv(spec.Env)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		setCancellation(ctx, &result)
		return result, nil
	}
	output := newCapWriter(spec.OutputCap)
	// Equal Writer values make os/exec use a single shared pipe. This preserves
	// write order across stdout/stderr rather than merging two reader goroutines.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) // #nosec G204 -- argv is operator-owned, never a shell string.
	cmd.Dir, cmd.Env = spec.Dir, env
	cmd.Stdout, cmd.Stderr = output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = e.WaitDelay
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start command: %w", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
		// Also clean descendants of a leader that returns before its children.
		if groupExists(cmd.Process.Pid) {
			runErr = e.stopGroup(cmd.Process.Pid)
		}
	case <-ctx.Done():
		setCancellation(ctx, &result)
		runErr = e.stopGroup(cmd.Process.Pid)
		waitErr = <-waited
	}
	result.ExitCode = cmd.ProcessState.ExitCode()
	result.Output, result.TotalBytes, result.Truncated = output.snapshot()
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		if !errors.Is(waitErr, exec.ErrWaitDelay) || (!result.Cancelled && !result.TimedOut) {
			runErr = errors.Join(runErr, fmt.Errorf("wait for command: %w", waitErr))
		}
	}
	return result, runErr
}

func (e *LocalExecutor) validate(spec *Spec) error {
	if e.root == "" {
		return errors.New("use NewLocalExecutor to set a workspace root")
	}
	if e.GracePeriod < 0 || e.WaitDelay < 0 || spec.Timeout < 0 || spec.OutputCap < 0 {
		return errors.New("execution limits must not be negative")
	}
	if spec.Timeout == 0 {
		spec.Timeout = DefaultTimeout
	}
	if spec.OutputCap == 0 {
		spec.OutputCap = DefaultOutputCap
	}
	if spec.OutputCap < MinOutputCap {
		return fmt.Errorf("output cap must be at least %d bytes", MinOutputCap)
	}
	if len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) {
		return errors.New("an absolute executable is required")
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("argv must not contain NUL")
		}
	}
	if !filepath.IsAbs(spec.Dir) || strings.ContainsRune(spec.Dir, 0) {
		return errors.New("working directory must be absolute and inside the workspace")
	}
	resolved, err := canonicalRoot(spec.Dir)
	if err != nil {
		return err
	}
	if !inside(e.root, resolved) {
		return errors.New("working directory symlink escapes workspace")
	}
	// OpenRoot checks containment using the same primitive as workspace tools.
	root, err := os.OpenRoot(e.root)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	rel, err := filepath.Rel(e.root, resolved)
	if err != nil {
		return err
	}
	dir, err := root.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("open working directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close working directory: %w", err)
	}
	spec.Dir = resolved
	return nil
}

func setCancellation(ctx context.Context, result *Result) {
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	result.Cancelled = errors.Is(ctx.Err(), context.Canceled)
}

func groupExists(pid int) bool {
	return !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

func (e *LocalExecutor) stopGroup(pid int) error {
	termErr := syscall.Kill(-pid, syscall.SIGTERM)
	if errors.Is(termErr, syscall.ESRCH) {
		return nil
	}
	grace := e.GracePeriod
	if grace == 0 {
		grace = DefaultGracePeriod
	}
	// Do not stop this timer when Wait returns: a descendant may ignore TERM
	// after the leader exits, including when it has closed its output handles.
	timer := time.NewTimer(grace)
	defer timer.Stop()
	<-timer.C
	killErr := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(killErr, syscall.ESRCH) {
		killErr = nil
	}
	return errors.Join(termErr, killErr)
}
