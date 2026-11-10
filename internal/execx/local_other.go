//go:build !unix

package execx

import "context"

// Run is unavailable without Unix process groups.
func (*LocalExecutor) Run(context.Context, Spec) (Result, error) {
	return Result{ExitCode: -1}, ErrUnsupported
}
