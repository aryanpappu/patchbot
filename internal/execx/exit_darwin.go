package execx

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

// Darwin exposes waitid(2), but syscall and x/sys do not provide a wrapper.
// The first four siginfo_t fields are 32-bit signo, errno, code and PID;
// 128 bytes also cover the remaining native siginfo_t fields and padding.
// WNOWAIT retains the zombie. Check si_code explicitly because Darwin can
// report stopped children even with WEXITED (Go issue 19314).
func waitExit(pid int) error {
	const pPID = 1
	for {
		var info struct {
			Signo, Errno, Code, PID int32
			Rest                    [112]byte
		}
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0) // #nosec G103 -- native waitid buffer, valid for the duration of the syscall.
		if errors.Is(errno, syscall.EINTR) {
			continue
		}
		if errno != 0 {
			return errno
		}
		if int(info.PID) == pid && info.Code >= 1 && info.Code <= 3 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
