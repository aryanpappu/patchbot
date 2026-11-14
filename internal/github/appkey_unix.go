//go:build unix

package github

import (
	"errors"
	"os"
	"syscall"
)

func keyOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return errors.New("private_key_file: must be owned by the patchbot service user")
	}
	return nil
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- operator-configured, validated key path.
}
