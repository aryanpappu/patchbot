package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// MaxDiffFileSize bounds every version of a changed file before diffing.
	MaxDiffFileSize int64 = 5 << 20
	// MaxDiffTotalSize bounds changed paths, using each path's largest version.
	MaxDiffTotalSize int64 = 50 << 20
)

// ErrDiffTooLarge reports a file (Path set) or aggregate (Path empty) size limit.
type ErrDiffTooLarge struct {
	Path  string
	Size  int64
	Limit int64
}

func (e *ErrDiffTooLarge) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("diff too large: changed files total %d bytes exceeds %d bytes", e.Size, e.Limit)
	}
	return fmt.Sprintf("diff too large: file %q is %d bytes, exceeds %d bytes", e.Path, e.Size, e.Limit)
}

type diffBlob struct {
	mode string
	oid  string
}

// diffMetadata keeps metadata bounded even when a caller requests a tiny patch.
func (r *Repo) diffMetadata(ctx context.Context, index string, args ...string) (string, error) {
	out, truncated, err := r.runIndexLimit(ctx, false, index, max(r.outputCap, DefaultOutputCap), args...)
	if err == nil && truncated {
		err = ErrOutputLimit
	}
	return out, err
}

func (r *Repo) diffBlobs(ctx context.Context, index string, tree bool) (map[string]diffBlob, error) {
	args := []string{"ls-files", "-s", "-z"}
	if tree {
		args = []string{"ls-tree", "-r", "-z", r.base}
	}
	out, err := r.diffMetadata(ctx, index, args...)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]diffBlob)
	for out != "" {
		entry, rest, ok := strings.Cut(out, "\x00")
		metadata, path, tab := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || !tab || path == "" || len(fields) != 3 {
			return nil, errors.New("invalid git file metadata")
		}
		blob := diffBlob{mode: fields[0], oid: fields[1]}
		if tree {
			blob.oid = fields[2]
		} else if fields[2] != "0" {
			return nil, errors.New("cannot diff an unmerged index")
		}
		if !objectID.MatchString(blob.oid) {
			return nil, errors.New("invalid git blob ID")
		}
		blobs[path] = blob
		out = rest
	}
	return blobs, nil
}

// checkDiffSizes reads only metadata, both before staging and on the snapshot
// before any git diff. Compare the pinned base with the index as well as status:
// HEAD may have advanced since the base, even with an otherwise clean status.
func (r *Repo) checkDiffSizes(ctx context.Context, indexFile string, changes []Change) error {
	base, err := r.diffBlobs(ctx, indexFile, true)
	if err != nil {
		return err
	}
	index, err := r.diffBlobs(ctx, indexFile, false)
	if err != nil {
		return err
	}
	for path, blob := range base {
		if index[path] != blob {
			changes = append(changes, Change{Path: path})
		}
	}
	for path, blob := range index {
		if base[path] != blob {
			changes = append(changes, Change{Path: path})
		}
	}
	paths, err := changedPaths(changes)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return ErrTampered
	}
	defer func() { _ = root.Close() }()
	sizes := make(map[string]int64)
	var total int64
	for _, path := range paths {
		var size int64
		info, statErr := root.Lstat(path)
		if statErr == nil {
			if !info.IsDir() {
				if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
					return errors.New("unsupported changed file type")
				}
				size = info.Size()
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return errors.New("inspect changed file size failed")
		}
		if err := checkDiffFileSize(path, size); err != nil {
			return err
		}
		for _, blob := range []diffBlob{base[path], index[path]} {
			if blob.oid == "" {
				continue
			}
			if blob.mode != "100644" && blob.mode != "100755" && blob.mode != "120000" {
				return errors.New("unsupported changed blob type")
			}
			blobSize, found := sizes[blob.oid]
			if !found {
				out, err := r.diffMetadata(ctx, indexFile, "cat-file", "-s", blob.oid)
				if err != nil {
					return err
				}
				blobSize, err = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
				if err != nil || blobSize < 0 {
					return errors.New("invalid git blob size")
				}
				sizes[blob.oid] = blobSize
			}
			if err := checkDiffFileSize(path, blobSize); err != nil {
				return err
			}
			size = max(size, blobSize)
		}
		total += size
		if total > MaxDiffTotalSize {
			return &ErrDiffTooLarge{Size: total, Limit: MaxDiffTotalSize}
		}
	}
	return nil
}

func checkDiffFileSize(path string, size int64) error {
	if size > MaxDiffFileSize {
		return &ErrDiffTooLarge{Path: path, Size: size, Limit: MaxDiffFileSize}
	}
	return nil
}
