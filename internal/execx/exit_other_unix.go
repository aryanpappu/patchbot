//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package execx

func waitExit(_ int) error { return ErrUnsupported }
