package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// workspacesDir is the directory under the data dir that holds one
	// directory per run.
	workspacesDir = "workspaces"
	// runPrefix starts the name of every per-run directory.
	runPrefix = "run-"
	// checkoutDir is the checkout inside a per-run directory. The rest of
	// the per-run directory is free for the harness, for example for the
	// HOME and temp directories of test commands.
	checkoutDir = "repo"
	// privateMode keeps other users of the machine out of a run.
	privateMode fs.FileMode = 0o700
)

var runIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ErrRunExists means a per-run directory for the run ID is already there.
var ErrRunExists = errors.New("workspace for this run already exists")

// Manager creates and removes the per-run directories under the data
// directory:
//
//	<data dir>/workspaces/run-<id>/        mode 0700
//	<data dir>/workspaces/run-<id>/repo/   mode 0700, the checkout
//
// It is safe for concurrent use.
type Manager struct {
	base string // absolute path of <data dir>/workspaces
	root *os.Root

	mu     sync.Mutex
	active map[string]struct{} // names of the run directories in use
}

// NewManager prepares the workspaces directory under dataDir, creating it
// when missing.
func NewManager(dataDir string) (*Manager, error) {
	if dataDir == "" {
		return nil, errors.New("workspace: the data directory is not set")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	base := filepath.Join(abs, workspacesDir)
	if err := os.MkdirAll(base, privateMode); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	// MkdirAll accepts a symlink to a directory. Run directories are made
	// and deleted here, so the place must be a real directory of our own.
	info, err := os.Lstat(base)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %s is not a directory", base)
	}
	root, err := os.OpenRoot(base + string(os.PathSeparator) + ".")
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = changedPath()
	}
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("workspace: %w", err)
	}
	if err := root.Chmod(".", privateMode); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("workspace: %w", err)
	}
	return &Manager{base: base, root: root, active: map[string]struct{}{}}, nil
}

// Close releases the manager. Workspaces it created stay usable.
func (m *Manager) Close() error { return m.root.Close() }

// Create makes the directory of a run and returns a workspace on its empty
// checkout directory. The run ID is 1 to 64 letters, digits, "_" or "-".
// Create fails with ErrRunExists rather than reuse a directory left by an
// earlier run. Closing the workspace with keep false deletes the run
// directory.
func (m *Manager) Create(runID string, opts Options) (*Local, error) {
	if !runIDRE.MatchString(runID) {
		return nil, fmt.Errorf("workspace: %q is not a run ID: use 1 to 64 letters, digits, _ or -", runID)
	}
	name := runPrefix + runID

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.root.Mkdir(name, privateMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("workspace: run %s: %w", runID, ErrRunExists)
		}
		return nil, fmt.Errorf("workspace: run %s: %w", runID, err)
	}
	ws, err := m.open(name, opts)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("workspace: run %s: %w", runID, err), m.remove(name))
	}
	m.active[name] = struct{}{}
	return ws, nil
}

func (m *Manager) open(name string, opts Options) (*Local, error) {
	// Mkdir is subject to the umask, which can only take bits away, so the
	// directories are never more open than 0700.
	policy, err := NewPolicy(opts.Protected)
	if err != nil {
		return nil, err
	}
	approved, err := m.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	runRoot, err := openDirectory(m.root, name, approved, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = runRoot.Close() }()
	if err := runRoot.Mkdir(checkoutDir, privateMode); err != nil {
		return nil, err
	}
	checkoutInfo, err := runRoot.Lstat(checkoutDir)
	if err != nil {
		return nil, err
	}
	root, err := openDirectory(runRoot, checkoutDir, checkoutInfo, nil)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(m.base, name)
	ws := localFromRoot(filepath.Join(runDir, checkoutDir), root, policy, opts)
	ws.runDir = runDir
	ws.onClose = func(keep bool) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.active, name)
		if keep {
			return nil
		}
		return m.remove(name)
	}
	return ws, nil
}

// Sweep deletes run directories that are not in use by this Manager and
// were last changed more than olderThan ago. It is for directories left
// behind by a crash or kept for inspection. It returns how many it deleted.
//
// The age is that of the run directory itself, which changes when the run is
// created, not while it works. Pass an age well above the longest run, so
// that a run of another patchbot process on the same data directory is not
// swept while it works.
func (m *Manager) Sweep(ctx context.Context, olderThan time.Duration) (int, error) {
	dir, err := m.root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("workspace: sweep: %w", err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return 0, fmt.Errorf("workspace: sweep: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var (
		removed int
		errs    []error
	)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(errs, err)...)
		}
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, runPrefix) {
			continue
		}
		info, err := m.root.Lstat(name)
		if err != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		switch done, err := m.sweepOne(name); {
		case err != nil:
			errs = append(errs, err)
		case done:
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

// sweepOne deletes one run directory unless it is in use.
func (m *Manager) sweepOne(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.active[name]; busy {
		return false, nil
	}
	if err := m.remove(name); err != nil {
		return false, err
	}
	return true, nil
}

// remove deletes a run directory. Everything goes through the os.Root on
// the workspaces directory, so a symlink planted in a checkout cannot make
// it delete anything outside.
func (m *Manager) remove(name string) error {
	err := m.root.RemoveAll(name)
	if err == nil {
		return nil
	}
	// A read-only directory, such as one in a Go module cache, stops
	// RemoveAll. Make every directory writable and try once more.
	_ = fs.WalkDir(m.root.FS(), name, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = m.root.Chmod(p, privateMode)
		}
		return nil
	})
	if err := m.root.RemoveAll(name); err != nil {
		return fmt.Errorf("workspace: delete %s: %w", name, err)
	}
	return nil
}
