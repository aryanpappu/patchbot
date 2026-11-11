//go:build !unix

package execx

import (
	"context"
	"os/exec"
	"time"
)

// Run is unavailable without Unix process groups.
func (*LocalExecutor) Run(context.Context, Spec) (Result, error) {
	return Result{ExitCode: -1}, ErrUnsupported
}

// RunProcessGroup is unavailable without Unix process groups.
func RunProcessGroup(context.Context, *exec.Cmd, time.Duration) error { return ErrUnsupported }
