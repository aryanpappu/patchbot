//go:build !unix

package gitx

import "os/exec"

func configureProcess(_ *exec.Cmd) {}
func cleanupProcess(_ *exec.Cmd)   {}
