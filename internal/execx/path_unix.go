//go:build unix

package execx

import (
	"errors"
	"os"
	"syscall"
)

func trustedOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && int64(stat.Uid) != int64(os.Getuid())) {
		return errors.New("must be owned by root or the current user")
	}
	return nil
}
func trustedDirectory(info os.FileInfo) error {
	if err := trustedOwner(info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("must not be group or world writable (including sticky directories)")
	}
	return nil
}
