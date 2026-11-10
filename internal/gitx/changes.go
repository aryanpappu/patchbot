package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Change contains a porcelain v1 two-character index/worktree Code and the
// literal path. A rename/copy also has OriginalPath; Path is the destination.
type Change struct {
	Code         string
	Path         string
	OriginalPath string
}

// Status returns tracked and individual untracked paths, including spaces and
// newlines literally. Truncated or malformed output fails closed.
func (r *Repo) Status(ctx context.Context) ([]Change, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verify(); err != nil {
		return nil, err
	}
	return r.status(ctx)
}

func (r *Repo) status(ctx context.Context) ([]Change, error) {
	out, err := r.complete(ctx, false, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

func parseStatus(out string) ([]Change, error) {
	var changes []Change
	for out != "" {
		entry, rest, found := strings.Cut(out, "\x00")
		if !found || len(entry) < 4 || entry[2] != ' ' {
			return nil, errors.New("invalid git status output")
		}
		change := Change{Code: entry[:2], Path: entry[3:]}
		out = rest
		if strings.ContainsAny(change.Code, "RC") {
			change.OriginalPath, out, found = strings.Cut(out, "\x00")
			if !found || change.OriginalPath == "" {
				return nil, errors.New("invalid git rename output")
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// HasChanges reports index, worktree, or untracked changes.
func (r *Repo) HasChanges(ctx context.Context) (bool, error) {
	changes, err := r.Status(ctx)
	return len(changes) != 0, err
}

func changedPaths(changes []Change) ([]string, error) {
	var paths []string
	seen := make(map[string]bool)
	for _, change := range changes {
		for _, path := range []string{change.Path, change.OriginalPath} {
			if path == "" {
				continue
			}
			if path == "." || !filepath.IsLocal(path) || strings.ContainsRune(path, 0) {
				return nil, ErrTampered
			}
			for _, part := range strings.Split(filepath.ToSlash(path), "/") {
				if part == "." || part == ".." || strings.EqualFold(part, ".git") {
					return nil, ErrTampered
				}
				if strings.EqualFold(part, ".gitmodules") {
					return nil, ErrSubmodules
				}
			}
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}
	return paths, nil
}

func (r *Repo) stage(ctx context.Context, index string, paths []string) error {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return ErrTampered
	}
	defer func() { _ = root.Close() }()
	var existing, removed []string
	for _, path := range paths {
		if _, err := root.Lstat(path); err == nil {
			existing = append(existing, path)
		} else if errors.Is(err, os.ErrNotExist) {
			removed = append(removed, path)
		} else {
			return errors.New("inspect changed path failed")
		}
	}
	// A staged rename's source is already absent from the real index. Force
	// removal handles that case as well as ordinary worktree deletions, without
	// passing nonexistent pathspecs to git add.
	if err := r.stageBatch(ctx, index, removed, []string{"update-index", "--force-remove", "--"}); err != nil {
		return err
	}
	return r.stageBatch(ctx, index, existing, []string{"add", "--all", "--"})
}

func (r *Repo) stageBatch(ctx context.Context, index string, paths, prefix []string) error {
	for len(paths) > 0 {
		batch := min(len(paths), 100)
		args := append(append([]string{}, prefix...), paths[:batch]...)
		_, truncated, err := r.runIndex(ctx, false, index, args...)
		if err != nil {
			return err
		}
		if truncated {
			return ErrOutputLimit
		}
		paths = paths[batch:]
	}
	return nil
}

// diffIndex snapshots the intended final worktree into a separate index. This
// includes new files without altering the caller's staged/unstaged state.
func (r *Repo) diffIndex(ctx context.Context) (string, func(), error) {
	changes, err := r.status(ctx)
	if err != nil {
		return "", nil, err
	}
	paths, err := changedPaths(changes)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(r.temp, "index-")
	if err != nil {
		return "", nil, errors.New("create temporary git index failed")
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, errors.New("close temporary git index failed")
	}
	cleanup() // read-tree expects a nonexistent index rather than an empty file.
	_, _, err = r.runIndex(ctx, false, path, "read-tree", "HEAD")
	if err == nil {
		err = r.stage(ctx, path, paths)
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// Diff returns the complete staged+worktree+untracked diff against the pinned
// base, up to OutputCap bytes. A final marker denotes a truncated diff.
func (r *Repo) Diff(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verify(); err != nil {
		return "", err
	}
	index, cleanup, err := r.diffIndex(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	out, truncated, err := r.runIndex(ctx, false, index, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=3", r.base, "--")
	if truncated {
		const marker = "\n[diff truncated]\n"
		out = out[:r.outputCap-len(marker)] + marker
	}
	return out, err
}

// DiffStat summarizes changes against the pinned base. Binary files count as
// changed files with no textual insertion/deletion count.
type DiffStat struct {
	Files      int
	Insertions int
	Deletions  int
}

// DiffStat includes untracked new files and never changes the real git index.
func (r *Repo) DiffStat(ctx context.Context) (DiffStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verify(); err != nil {
		return DiffStat{}, err
	}
	index, cleanup, err := r.diffIndex(ctx)
	if err != nil {
		return DiffStat{}, err
	}
	defer cleanup()
	out, truncated, err := r.runIndex(ctx, false, index, "diff", "--cached", "--numstat", "-z", "--find-renames", "--no-ext-diff", "--no-textconv", r.base, "--")
	if err != nil {
		return DiffStat{}, err
	}
	if truncated {
		return DiffStat{}, ErrOutputLimit
	}
	var stat DiffStat
	for out != "" {
		entry, rest, found := strings.Cut(out, "\x00")
		if !found {
			return DiffStat{}, errors.New("invalid git diff stat output")
		}
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 {
			return DiffStat{}, errors.New("invalid git diff stat record")
		}
		if fields[2] == "" {
			// With -z, renames use an empty path followed by old\0new\0.
			for range 2 {
				var path string
				path, rest, found = strings.Cut(rest, "\x00")
				if !found || path == "" {
					return DiffStat{}, errors.New("invalid git diff stat rename")
				}
			}
		}
		stat.Files++
		if fields[0] != "-" || fields[1] != "-" {
			added, e1 := strconv.Atoi(fields[0])
			deleted, e2 := strconv.Atoi(fields[1])
			if e1 != nil || e2 != nil || added < 0 || deleted < 0 {
				return DiffStat{}, errors.New("invalid git diff stat count")
			}
			stat.Insertions += added
			stat.Deletions += deleted
		}
		out = rest
	}
	return stat, nil
}
