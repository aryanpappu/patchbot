package execx

import (
	"errors"
	"golang.org/x/sys/unix"
)

// WNOWAIT observes the zombie while retaining the leader's identity anchor.
func waitExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
