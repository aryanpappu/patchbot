package gitx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
)

// ConfigHash returns SHA-256 of the literal .git/config bytes after checking
// metadata types and ensuring .git/hooks contains only regular *.sample files.
func (r *Repo) ConfigHash() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", errors.New("git repo is closed")
	}
	return r.hashConfig()
}

// VerifyConfig checks the captured hash and the hook directory without invoking
// git, so a modified configuration cannot execute helpers during verification.
func (r *Repo) VerifyConfig(hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("git repo is closed")
	}
	actual, err := r.hashConfig()
	if err != nil || actual != hash {
		return ErrTampered
	}
	return nil
}

func (r *Repo) verify() error {
	if r.closed {
		return errors.New("git repo is closed")
	}
	hash, err := r.hashConfig()
	if err != nil || hash != r.configHash {
		return ErrTampered
	}
	return nil
}

func (r *Repo) hashConfig() (string, error) {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return "", ErrTampered
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(".git")
	if err != nil || !info.IsDir() {
		return "", ErrTampered
	}
	git, err := root.OpenRoot(".git")
	if err != nil {
		return "", ErrTampered
	}
	defer func() { _ = git.Close() }()
	info, err = git.Lstat("config")
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", ErrTampered
	}
	file, err := git.Open("config")
	if err != nil {
		return "", ErrTampered
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (1<<20)+1))
	if err != nil || n > 1<<20 {
		return "", ErrTampered
	}
	// A missing hooks directory is fine (clone uses an empty template).
	if info, err := git.Lstat("hooks"); err == nil {
		if !info.IsDir() {
			return "", ErrTampered
		}
		hooks, err := git.Open("hooks")
		if err != nil {
			return "", ErrTampered
		}
		defer func() { _ = hooks.Close() }()
		for {
			entries, readErr := hooks.ReadDir(64)
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(entry.Name(), ".sample") {
					return "", ErrTampered
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return "", ErrTampered
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", ErrTampered
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
