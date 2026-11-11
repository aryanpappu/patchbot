//go:build dragonfly || freebsd || netbsd || openbsd

package execx

import (
	"errors"
	"golang.org/x/sys/unix"
)

// NOTE_EXIT reports exit without reaping. A zombie that exited before EV_ADD
// can report ESRCH; since no Wait has occurred, its PID is still reserved.
func waitExit(pid int) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(queue) }()
	event := unix.Kevent_t{Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	event.Ident = uint64(pid)
	_, err = unix.Kevent(queue, []unix.Kevent_t{event}, nil, nil)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	var events [1]unix.Kevent_t
	for {
		_, err = unix.Kevent(queue, nil, events[:], nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
