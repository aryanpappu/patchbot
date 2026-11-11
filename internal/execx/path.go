package execx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateTrustedPath requires existing directories with trusted ownership and
// permissions along every ancestor, including unresolved symlink locations.
// Call again with the actual workspace and data roots when constructing a policy.
func ValidateTrustedPath(dirs []string, excluded ...string) error {
	if len(dirs) == 0 {
		return errors.New("at least one trusted directory is required")
	}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00"+string(filepath.ListSeparator)) {
			return errors.New("entries must be nonempty absolute directories without a PATH separator")
		}
		for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
			if part == ".." {
				return errors.New("entries must not contain parent traversal")
			}
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("resolve trusted directory: %w", err)
		}
		for _, root := range excluded {
			if root == "" {
				continue
			}
			canonical, err := resolveFuturePath(root)
			if err != nil {
				return fmt.Errorf("resolve excluded root: %w", err)
			}
			if inside(canonical, resolved) || inside(filepath.Clean(root), filepath.Clean(dir)) {
				return errors.New("trusted directory is inside the data directory or workspace")
			}
		}
		if err := validateDirectoryChain(dir, excluded...); err != nil {
			return fmt.Errorf("trusted directory %q: %w", dir, err)
		}
	}
	return nil
}

// Walk each unresolved component before following symlinks. Checking only the
// final EvalSymlinks result misses replaceable links and intermediate targets.
func validateDirectoryChain(path string, excluded ...string) error {
	pending := strings.Split(filepath.Clean(path), string(filepath.Separator))
	current := string(filepath.Separator)
	links := 0
	for {
		if excludedExecutable(excluded, current) {
			return errors.New("trusted path crosses the data directory or workspace")
		}
		info, err := os.Lstat(current) // #nosec G703 -- operator-owned path being validated.
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 255 {
				return errors.New("too many symlinks")
			}
			if err := trustedOwner(info); err != nil {
				return err
			}
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Dir(current) + string(filepath.Separator) + target
			}
			pending = append(strings.Split(target, string(filepath.Separator)), pending...)
			current = string(filepath.Separator)
			continue
		}
		if !info.IsDir() {
			return errors.New("every path component must be a directory")
		}
		if err := trustedDirectory(info); err != nil {
			return fmt.Errorf("%s: %w", current, err)
		}
		for len(pending) > 0 && pending[0] == "" {
			pending = pending[1:]
		}
		if len(pending) == 0 {
			return nil
		}
		part := pending[0]
		pending = pending[1:]
		if part == "." {
			continue
		}
		if part == ".." {
			current = filepath.Dir(current)
		} else {
			current = filepath.Join(current, part)
		}
	}
}

// Resolve the existing prefix too, so a not-yet-created data directory cannot
// hide beneath a symlink. EvalSymlinks alone cannot resolve such paths.
func resolveFuturePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	info, statErr := os.Lstat(abs)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		return resolveFuturePath(target)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	resolved, err = resolveFuturePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(abs)), nil
}
