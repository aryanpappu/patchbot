package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// ListDir is the list_dir tool.
type ListDir struct{}

type listDirArgs struct {
	Path  string `json:"path"`
	Depth *int   `json:"depth"`
}

// Spec describes list_dir.
func (ListDir) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "list_dir",
		Description: "List a directory of the repository. Directories end with a slash and files show their size. " +
			"Lists one level by default; pass depth 2 or 3 to include subdirectories. " +
			"Protected paths such as .git are not listed, and symlinks are not followed.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory path relative to the repository root. Leave out, or pass \".\", for the root."},
    "depth": {"type": "integer", "minimum": 1, "maximum": 3, "description": "How many levels to list. The default is 1."}
  },
  "additionalProperties": false
}`),
	}
}

// Execute runs list_dir.
func (ListDir) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	var args listDirArgs
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	lim := env.Limits.withDefaults()
	depth := 1
	if args.Depth != nil {
		depth = *args.Depth
	}
	if depth < 1 {
		return failed("depth must be 1 or more")
	}
	if args.Path == "" {
		args.Path = "."
	}
	l := lister{ctx: ctx, env: env, maxDepth: min(depth, lim.ListMaxDepth), maxEntries: lim.ListMaxEntries, maxBytes: lim.MaxOutputBytes}
	// The path is checked by the workspace as the model gave it. Only then
	// is it made canonical, so that the entries can be passed straight to
	// other tools: path.Clean would resolve a ".." before the jail saw it.
	if _, err := env.WS.Stat(args.Path); err != nil {
		return failure(ctx, relabel(err, "list"))
	}
	if err := l.walk(path.Clean(args.Path), 1); err != nil {
		return failure(ctx, err)
	}
	if l.count == 0 && len(l.notes) == 0 {
		return Result{Content: "[the directory is empty]"}, nil
	}
	if l.full {
		l.notes = append(l.notes, fmt.Sprintf("stopped at %d entries; list a subdirectory to see more", l.count))
	}
	if len(l.notes) > 0 {
		fmt.Fprintf(&l.out, "[%s]", strings.Join(l.notes, "; "))
	}
	return Result{Content: strings.TrimSuffix(l.out.String(), "\n")}, nil
}

type lister struct {
	ctx        context.Context
	env        *Env
	maxDepth   int
	maxEntries int
	maxBytes   int

	out   strings.Builder
	count int
	full  bool
	notes []string
}

// walk lists one directory and, while depth allows, its subdirectories.
// Only an error on the starting directory is returned; a subdirectory that
// cannot be listed is marked in the output.
func (l *lister) walk(dir string, depth int) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	entries, err := l.env.WS.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, workspace.ErrDirTruncated) {
			return err
		}
		l.notes = append(l.notes, fmt.Sprintf("%s has too many entries; only the first %d were read", dir, len(entries)))
	}
	for _, e := range entries {
		if l.count >= l.maxEntries || l.out.Len() >= l.maxBytes {
			l.full = true
			return nil
		}
		rel := path.Join(dir, e.Name())
		l.count++
		switch {
		case e.IsDir():
			l.dir(rel, e.Name(), depth)
		case e.Type()&fs.ModeSymlink != 0:
			fmt.Fprintf(&l.out, "%s (symlink)\n", rel)
		case e.Type().IsRegular():
			size := "unknown size"
			if info, err := e.Info(); err == nil {
				size = plural(int(info.Size()), "byte", "bytes")
			}
			fmt.Fprintf(&l.out, "%s (%s)\n", rel, size)
		default:
			fmt.Fprintf(&l.out, "%s (special file)\n", rel)
		}
		if l.full {
			return nil
		}
	}
	return nil
}

func (l *lister) dir(rel, name string, depth int) {
	switch {
	case depth >= l.maxDepth:
		fmt.Fprintf(&l.out, "%s/\n", rel)
	case skippedDirs[name]:
		fmt.Fprintf(&l.out, "%s/ (not expanded; list it directly)\n", rel)
	default:
		fmt.Fprintf(&l.out, "%s/\n", rel)
		if err := l.walk(rel, depth+1); err != nil && l.ctx.Err() == nil {
			fmt.Fprintf(&l.out, "%s/ (could not be listed)\n", rel)
		}
	}
}
