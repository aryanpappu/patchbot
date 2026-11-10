package gitx

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

type diffFile struct {
	paths  []string
	binary bool
}

// Let Git classify binary data; never open blobs or worktree content in Go.
func (r *Repo) diffNumstat(ctx context.Context, index string) (DiffStat, []diffFile, error) {
	out, truncated, err := r.runIndexLimit(ctx, false, index, max(r.outputCap, DefaultOutputCap),
		"diff", "--cached", "--numstat", "-z", "--find-renames", "--no-ext-diff", "--no-textconv", r.base, "--")
	if err != nil {
		return DiffStat{}, nil, err
	}
	if truncated {
		return DiffStat{}, nil, ErrOutputLimit
	}
	return parseDiffNumstat(out)
}

func parseDiffNumstat(out string) (DiffStat, []diffFile, error) {
	var stat DiffStat
	var files []diffFile
	for out != "" {
		entry, rest, found := strings.Cut(out, "\x00")
		fields := strings.SplitN(entry, "\t", 3)
		if !found || len(fields) != 3 {
			return DiffStat{}, nil, errors.New("invalid git diff stat record")
		}
		file := diffFile{paths: []string{fields[2]}, binary: fields[0] == "-" && fields[1] == "-"}
		if fields[2] == "" {
			// With -z, renames use an empty path followed by old\0new\0.
			file.paths = nil
			for range 2 {
				var path string
				path, rest, found = strings.Cut(rest, "\x00")
				if !found || path == "" {
					return DiffStat{}, nil, errors.New("invalid git diff stat rename")
				}
				file.paths = append(file.paths, path)
			}
		}
		stat.Files++
		if !file.binary {
			added, e1 := strconv.Atoi(fields[0])
			deleted, e2 := strconv.Atoi(fields[1])
			if e1 != nil || e2 != nil || added < 0 || deleted < 0 {
				return DiffStat{}, nil, errors.New("invalid git diff stat count")
			}
			stat.Insertions += added
			stat.Deletions += deleted
		}
		files = append(files, file)
		out = rest
	}
	return stat, files, nil
}
