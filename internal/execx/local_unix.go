//go:build unix

package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Run starts one process group. Timeout/cancel signals TERM, then KILL after the
// grace period, even if the leader has already exited. The leader is not reaped
// until all group signaling finishes: its zombie pins the PGID against reuse.
// Descendant parents (or the OS reaper) reap their own children.
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
	var sink io.Writer = output
	var filtered *redactedOutput
	if spec.Redactor != nil {
		filtered = &redactedOutput{writer: secret.NewWriter(output, spec.Redactor)}
		sink = filtered
	}
	// Equal Writer values make os/exec use a single shared pipe. This preserves
	// write order across stdout/stderr rather than merging two reader goroutines.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) // #nosec G204 -- argv is operator-owned, never a shell string.
	cmd.Dir, cmd.Env = spec.Dir, env
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.WaitDelay = e.WaitDelay
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	interrupted, waitErr, processErr := runProcessGroup(ctx, cmd, e.GracePeriod)
	runErr = processErr
	if interrupted {
		setCancellation(ctx, &result)
	}
	if cmd.ProcessState == nil {
		return result, runErr
	}
	result.ExitCode = cmd.ProcessState.ExitCode()
	if filtered != nil {
		runErr = errors.Join(runErr, filtered.writer.Flush())
	}
	result.Output, result.TotalBytes, result.Truncated = output.snapshot()
	if filtered != nil {
		result.TotalBytes = filtered.total
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		if !errors.Is(waitErr, exec.ErrWaitDelay) || (!result.Cancelled && !result.TimedOut) {
			runErr = errors.Join(runErr, fmt.Errorf("wait for command: %w", waitErr))
		}
	}
	return result, runErr
}

// os/exec uses one copy goroutine for the shared stdout/stderr writer.
type redactedOutput struct {
	writer *secret.Writer
	total  int64
}

func (w *redactedOutput) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	return w.writer.Write(p)
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
