// Package execx runs operator-configured commands without a shell.
//
// Local execution is not a sandbox: approved commands run repository code with
// the host user's privileges, filesystem access, and network access. Process
// groups cover ordinary descendants, not children that deliberately use setsid
// or change groups. Directory checks reject existing symlink escapes but cannot
// prevent another host process from renaming directories between check and exec.
package execx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultTimeout is the per-command limit from the merged plan.
	DefaultTimeout = 2 * time.Minute
	// DefaultOutputCap bounds stored combined output, including its cut marker.
	DefaultOutputCap = 256 << 10
	// DefaultGracePeriod allows a process group to handle SIGTERM before SIGKILL.
	DefaultGracePeriod = 3 * time.Second
	// DefaultWaitDelay bounds pipe draining after process exit.
	DefaultWaitDelay = time.Second
	// MinOutputCap leaves enough room for a complete omission marker.
	MinOutputCap = 64
)

// ErrUnsupported indicates that local process groups are unavailable.
var ErrUnsupported = errors.New("local execution is unsupported on this OS")

// Executor is the boundary between command policy and process execution.
type Executor interface {
	Run(context.Context, Spec) (Result, error)
}

// Spec describes a literal invocation. Callers should obtain Argv from Policy.
// Env is explicit; nil means empty, never the parent's environment. Secret-like
// names are also removed here as defense in depth. Stdin is always the null device.
// Zero Timeout and OutputCap select defaults; negative values are invalid.
type Spec struct {
	Argv      []string
	Dir       string
	Env       []string
	Timeout   time.Duration
	OutputCap int
}

// Result describes an attempted execution. ExitCode is -1 if no process exited
// normally (including cancellation before start). Nonzero exits are not errors.
// Output combines stdout and stderr in pipe order. TotalBytes counts all bytes
// drained, including omitted bytes, and excludes the generated cut marker.
type Result struct {
	ExitCode   int
	Output     string
	Truncated  bool
	TotalBytes int64
	Duration   time.Duration
	TimedOut   bool
	Cancelled  bool
}

// LocalExecutor runs in a fixed workspace. Configure durations before sharing
// it across goroutines; zero selects defaults. A group is cleaned up even when
// its leader exits successfully while descendants are still running.
type LocalExecutor struct {
	root        string
	GracePeriod time.Duration
	WaitDelay   time.Duration
}

var _ Executor = (*LocalExecutor)(nil)

// NewLocalExecutor validates and fixes the absolute workspace root.
func NewLocalExecutor(root string) (*LocalExecutor, error) {
	resolved, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	return &LocalExecutor{root: resolved}, nil
}

func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return "", errors.New("workspace root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(resolved) // #nosec G703 -- root is an operator-supplied path; containment is checked separately for command directories.
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workspace root must be a directory")
	}
	return resolved, nil
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}
