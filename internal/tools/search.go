package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// maxPatternBytes is the longest regular expression search accepts.
const maxPatternBytes = 1000

// Search is the search tool. It walks the checkout through the Workspace,
// so it sees exactly what read_file may read: no protected path, and no
// symlink is followed. The regular expressions are Go's RE2, which runs in
// time linear in the input, so a pattern cannot stall the run.
type Search struct{}

type searchArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxResults *int   `json:"max_results"`
}

// Spec describes search.
func (Search) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "search",
		Description: "Search the text files of the repository with a regular expression (Go RE2 syntax, matched " +
			"line by line; start the pattern with (?i) to ignore case). Returns matches as path:line: text. " +
			"Binary files, files over 1 MB, protected paths and symlinks are skipped. Returns 50 matches by default.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression in Go RE2 syntax. No backreferences or lookaround."},
    "path": {"type": "string", "description": "Directory or file to search, relative to the repository root. The default is the whole repository."},
    "glob": {"type": "string", "description": "Only search files whose name matches this glob, for example *.go. A glob with a slash, such as cmd/*/main.go, is matched against the whole path."},
    "max_results": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum number of matches to return. The default is 50."}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
	}
}

// Execute runs search.
func (Search) Execute(ctx context.Context, env *Env, raw json.RawMessage) (Result, error) {
	var args searchArgs
	if err := decodeArgs(raw, &args); err != nil {
		return failed("%v", err)
	}
	lim := env.Limits.withDefaults()
	switch {
	case args.Pattern == "":
		return failed("pattern is empty")
	case len(args.Pattern) > maxPatternBytes:
		return failed("pattern is longer than %d bytes", maxPatternBytes)
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return failed("pattern is not a valid RE2 regular expression: %v", err)
	}
	if args.Glob != "" {
		if _, err := path.Match(args.Glob, ""); err != nil {
			return failed("glob %q is not valid", args.Glob)
		}
	}
	maxResults := lim.SearchDefaultResults
	if args.MaxResults != nil {
		maxResults = *args.MaxResults
	}
	if maxResults < 1 {
		return failed("max_results must be 1 or more")
	}
	if args.Path == "" {
		args.Path = "."
	}
	s := searcher{ctx: ctx, env: env, lim: lim, re: re, glob: args.Glob, maxResults: min(maxResults, lim.SearchMaxResults)}

	// The path is checked by the workspace as the model gave it. Only then
	// is it made canonical: path.Clean would resolve a ".." before the jail
	// saw it.
	info, err := env.WS.Stat(args.Path)
	if err != nil {
		return failure(ctx, relabel(err, "search"))
	}
	start := path.Clean(args.Path)
	if info.IsDir() {
		err = s.walk(start)
	} else {
		// A file named directly is searched whatever the glob says.
		s.glob = ""
		err = s.file(start, info.Size())
	}
	if err != nil {
		return failure(ctx, err)
	}
	return Result{Content: s.report()}, nil
}

type searcher struct {
	ctx        context.Context
	env        *Env
	lim        Limits
	re         *regexp.Regexp
	glob       string
	maxResults int

	out     strings.Builder
	matches int
	files   int   // files read
	binary  int   // files read and then found to be binary
	dirs    int   // directories listed
	bytes   int64 // content read
	skipped int   // files not read: binary, too large or unreadable
	stopped string
}

// errStop ends the walk early once a cap is reached.
var errStop = errors.New("search stopped")

func (s *searcher) stop(why string) error {
	if s.stopped == "" {
		s.stopped = why
	}
	return errStop
}

// walk searches a directory tree. Only an error on the starting directory,
// or a cancelled context, is returned.
func (s *searcher) walk(dir string) error {
	err := s.walkDir(dir, true)
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

func (s *searcher) walkDir(dir string, top bool) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.dirs >= s.lim.SearchMaxDirs {
		return s.stop(fmt.Sprintf("stopped after %d directories", s.dirs))
	}
	s.dirs++
	entries, err := s.env.WS.ReadDir(dir)
	if err != nil && !errors.Is(err, workspace.ErrDirTruncated) {
		if top {
			return err
		}
		return nil // a subdirectory that cannot be listed is passed over
	}
	for _, e := range entries {
		rel := path.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if skippedDirs[e.Name()] {
				continue
			}
			if err := s.walkDir(rel, false); err != nil {
				return err
			}
		case e.Type().IsRegular():
			size := int64(-1)
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			if err := s.file(rel, size); err != nil {
				return err
			}
		}
		// Symlinks and special files are never read.
	}
	return nil
}

func (s *searcher) globMatches(rel string) bool {
	if s.glob == "" {
		return true
	}
	name := rel
	if !strings.Contains(s.glob, "/") {
		name = path.Base(rel)
	}
	ok, err := path.Match(s.glob, name)
	return err == nil && ok
}

// file searches one file. It returns errStop when a cap is reached and an
// error of the context when the run is cancelled.
func (s *searcher) file(rel string, size int64) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !s.globMatches(rel) {
		return nil
	}
	if size > s.lim.SearchMaxFileBytes {
		s.skipped++
		return nil
	}
	if s.files >= s.lim.SearchMaxFiles {
		return s.stop(fmt.Sprintf("stopped after %d files", s.files))
	}
	if s.bytes >= s.lim.SearchMaxBytes {
		return s.stop(fmt.Sprintf("stopped after reading %d bytes", s.bytes))
	}
	data, err := s.env.WS.ReadFile(rel)
	if err != nil {
		s.skipped++
		return nil
	}
	s.files++
	s.bytes += int64(len(data))
	if int64(len(data)) > s.lim.SearchMaxFileBytes || isBinary(data) {
		s.binary++
		s.skipped++
		return nil
	}
	for n := 1; len(data) > 0; n++ {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		if !s.re.Match(line) {
			continue
		}
		if s.matches >= s.maxResults {
			return s.stop(fmt.Sprintf("stopped at %d matches; there are more. Narrow the pattern, path or glob", s.matches))
		}
		text := printable(string(line))
		if len(text) > s.lim.SearchMaxLineBytes {
			text = cutUTF8(text, s.lim.SearchMaxLineBytes) + " [line cut]"
		}
		row := fmt.Sprintf("%s:%d: %s\n", rel, n, text)
		if s.out.Len()+len(row) > s.lim.MaxOutputBytes {
			return s.stop(fmt.Sprintf("stopped at %d matches because the result is full", s.matches))
		}
		s.out.WriteString(row)
		s.matches++
	}
	return nil
}

func (s *searcher) report() string {
	notes := []string{fmt.Sprintf("%s in %s searched", plural(s.matches, "match", "matches"), plural(s.files-s.binary, "file", "files"))}
	if s.skipped > 0 {
		notes = append(notes, plural(s.skipped, "file", "files")+" skipped (binary, too large or unreadable)")
	}
	if s.stopped != "" {
		notes = append(notes, s.stopped)
	}
	return s.out.String() + "[" + strings.Join(notes, "; ") + "]"
}
