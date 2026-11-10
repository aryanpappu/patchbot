package tools_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/tools"
)

func numbered(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestReadFile(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	put(t, dir, "small.txt", "alpha\nbeta\ngamma\n")
	put(t, dir, "nonl.txt", "alpha\nbeta")
	put(t, dir, "empty.txt", "")
	put(t, dir, "crlf.txt", "one\r\ntwo\r\n")
	put(t, dir, "long.txt", numbered(1, 2500))
	put(t, dir, "bin.dat", "ab\x00cd")
	put(t, dir, "latin1.txt", "caf\xe9\n")
	put(t, dir, "wide.txt", strings.Repeat("é", 3000)+"\nnext\n")

	if got, want := ok(t, reg, "read_file", obj{"path": "small.txt"}),
		"     1\talpha\n     2\tbeta\n     3\tgamma\n[lines 1-3 of 3; end of file]"; got != want {
		t.Errorf("small = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "read_file", obj{"path": "nonl.txt"}),
		"     1\talpha\n     2\tbeta\n[lines 1-2 of 2; end of file]"; got != want {
		t.Errorf("no final newline = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "read_file", obj{"path": "small.txt", "offset": 2, "limit": 1}),
		"     2\tbeta\n[lines 2-2 of 3; 1 more line; continue with offset=3]"; got != want {
		t.Errorf("offset/limit = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "read_file", obj{"path": "small.txt", "offset": 3, "limit": 50}),
		"     3\tgamma\n[lines 3-3 of 3; end of file]"; got != want {
		t.Errorf("last line = %q, want %q", got, want)
	}
	if got := ok(t, reg, "read_file", obj{"path": "empty.txt"}); got != "[the file is empty]" {
		t.Errorf("empty = %q", got)
	}
	if got := ok(t, reg, "read_file", obj{"path": "crlf.txt"}); strings.Contains(got, "\r") || !strings.Contains(got, "     2\ttwo\n") {
		t.Errorf("crlf = %q", got)
	}
	if got := ok(t, reg, "read_file", obj{"path": "latin1.txt"}); !strings.Contains(got, "caf\uFFFD") {
		t.Errorf("invalid UTF-8 = %q", got)
	}

	// The default is 400 lines and the result says how to go on.
	got := ok(t, reg, "read_file", obj{"path": "long.txt"})
	if !strings.Contains(got, "   400\tline 400\n") || strings.Contains(got, "line 401") ||
		!strings.HasSuffix(got, "[lines 1-400 of 2500; 2100 more lines; continue with offset=401]") {
		t.Errorf("default window ends with %q", got[len(got)-120:])
	}
	got = ok(t, reg, "read_file", obj{"path": "long.txt", "offset": 2400, "limit": 2000})
	if !strings.HasPrefix(got, "  2400\tline 2400\n") || !strings.HasSuffix(got, "[lines 2400-2500 of 2500; end of file]") {
		t.Errorf("tail window = %q ... %q", got[:30], got[len(got)-60:])
	}
	// A long line is cut on a character boundary.
	got = ok(t, reg, "read_file", obj{"path": "wide.txt", "limit": 1})
	if !strings.Contains(got, "[line cut: it has 6000 bytes]") || strings.Contains(got, "\uFFFD") || len(got) > 2200 {
		t.Errorf("wide line: %d bytes, tail %q", len(got), got[len(got)-90:])
	}

	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"missing":      {obj{"path": "nope.txt"}, `read "nope.txt": file does not exist`},
		"directory":    {obj{"path": "."}, "not a regular file (it is a directory)"},
		"binary":       {obj{"path": "bin.dat"}, `"bin.dat" is a binary file (5 bytes)`},
		"offset past":  {obj{"path": "small.txt", "offset": 4}, "offset 4 is past the end of the file, which has 3 lines"},
		"offset zero":  {obj{"path": "small.txt", "offset": 0}, "offset: 0 is below the minimum 1"},
		"limit zero":   {obj{"path": "small.txt", "limit": 0}, "limit: 0 is below the minimum 1"},
		"limit huge":   {obj{"path": "small.txt", "limit": 2001}, "limit: 2001 is above the maximum 2000"},
		"limit float":  {obj{"path": "small.txt", "limit": 1.5}, "limit: want a whole number"},
		"path number":  {obj{"path": 7}, "path: want string, got number"},
		"path missing": {obj{}, "path: missing required property"},
	} {
		if got := bad(t, reg, "read_file", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want it to hold %q", name, got, tc.want)
		}
	}
}

func TestReadFileByteCap(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{MaxOutputBytes: 200, MaxLineBytes: 1000})
	put(t, dir, "long.txt", numbered(1, 100))
	put(t, dir, "fat.txt", strings.Repeat("x", 500)+"\nsecond\n")

	got := ok(t, reg, "read_file", obj{"path": "long.txt"})
	body, footer, _ := strings.Cut(got, "[")
	if len(body) > 200 || len(body) < 150 {
		t.Errorf("body is %d bytes, want just under the 200-byte cap", len(body))
	}
	last := strings.Count(body, "\n")
	if want := fmt.Sprintf("lines 1-%d of 100; %d more lines; continue with offset=%d]", last, 100-last, last+1); footer != want {
		t.Errorf("footer = %q, want %q", footer, want)
	}
	// One line over the cap is still returned, so a read always advances.
	got = ok(t, reg, "read_file", obj{"path": "fat.txt"})
	if !strings.HasSuffix(got, "[lines 1-1 of 2; 1 more line; continue with offset=2]") {
		t.Errorf("fat line footer = %q", got[len(got)-70:])
	}
}

func TestReadFileRefusesHugeFile(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	put(t, dir, "huge.txt", strings.Repeat("0123456789abcdef\n", 300_000)) // 5.1 MB
	if got := bad(t, reg, "read_file", obj{"path": "huge.txt"}); !strings.Contains(got, "file too large") {
		t.Errorf("huge file: %q", got)
	}
}

func TestListDir(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	put(t, dir, "b.txt", "bb")
	put(t, dir, "a.txt", "a")
	put(t, dir, "pkg/x.go", "package pkg\n")
	put(t, dir, "pkg/inner/y.go", "")
	put(t, dir, "pkg/inner/deeper/z.go", "")
	put(t, dir, "pkg/inner/deeper/deepest/w.go", "")
	put(t, dir, "node_modules/lib/index.js", "")
	if err := os.Mkdir(filepath.Join(dir, "hollow"), 0o750); err != nil {
		t.Fatal(err)
	}

	if got, want := ok(t, reg, "list_dir", obj{}),
		"a.txt (1 byte)\nb.txt (2 bytes)\nhollow/\nnode_modules/\npkg/"; got != want {
		t.Errorf("root = %q, want %q", got, want)
	}
	for _, p := range []string{".", "./", ""} {
		if got := ok(t, reg, "list_dir", obj{"path": p}); !strings.HasPrefix(got, "a.txt (1 byte)\n") {
			t.Errorf("path %q = %q", p, got)
		}
	}
	if got, want := ok(t, reg, "list_dir", obj{"path": "pkg/", "depth": 2}),
		"pkg/inner/\npkg/inner/deeper/\npkg/inner/y.go (0 bytes)\npkg/x.go (12 bytes)"; got != want {
		t.Errorf("depth 2 = %q, want %q", got, want)
	}
	got := ok(t, reg, "list_dir", obj{"depth": 3})
	for _, want := range []string{"pkg/inner/deeper/\n", "pkg/inner/y.go (0 bytes)", "node_modules/ (not expanded; list it directly)"} {
		if !strings.Contains(got, want) {
			t.Errorf("depth 3 lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "z.go") || strings.Contains(got, "index.js") {
		t.Errorf("depth 3 went too deep:\n%s", got)
	}
	if got := ok(t, reg, "list_dir", obj{"path": "node_modules", "depth": 2}); !strings.Contains(got, "node_modules/lib/index.js") {
		t.Errorf("node_modules listed directly = %q", got)
	}
	if got := ok(t, reg, "list_dir", obj{"path": "hollow"}); got != "[the directory is empty]" {
		t.Errorf("empty dir = %q", got)
	}
	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"file":      {obj{"path": "a.txt"}, `list "a.txt": not a directory`},
		"missing":   {obj{"path": "nope"}, `list "nope": file does not exist`},
		"depth 0":   {obj{"depth": 0}, "depth: 0 is below the minimum 1"},
		"depth 4":   {obj{"depth": 4}, "depth: 4 is above the maximum 3"},
		"extra arg": {obj{"recursive": true}, "recursive: unknown property; known properties: depth, path"},
	} {
		if got := bad(t, reg, "list_dir", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want it to hold %q", name, got, tc.want)
		}
	}
}

func TestListDirEntryCap(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{ListMaxEntries: 7})
	for i := range 5 {
		put(t, dir, fmt.Sprintf("d%d/a.txt", i), "")
		put(t, dir, fmt.Sprintf("d%d/b.txt", i), "")
	}
	got := ok(t, reg, "list_dir", obj{"depth": 2})
	if lines := strings.Split(got, "\n"); len(lines) != 8 || lines[7] != "[stopped at 7 entries; list a subdirectory to see more]" {
		t.Errorf("capped listing:\n%s", got)
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	put(t, dir, "a.go", "package a\n\nfunc Hello() {}\nfunc hello2() {}\n")
	put(t, dir, "b.txt", "say Hello\r\nbye\n")
	put(t, dir, "cmd/app/main.go", "package main\n// Hello there\n")
	put(t, dir, "cmd/tool/main.go", "package main\n")
	put(t, dir, "bin.dat", "Hello\x00binary")
	put(t, dir, "big.txt", strings.Repeat("Hello padding line\n", 60_000)) // 1.1 MB
	put(t, dir, "node_modules/lib/index.js", "Hello from a dependency\n")
	put(t, dir, "wide.txt", strings.Repeat("é", 400)+" Hello\n")
	if err := os.Symlink("a.go", filepath.Join(dir, "alias.go")); err != nil {
		t.Fatal(err)
	}

	got := ok(t, reg, "search", obj{"pattern": "Hello"})
	want := "a.go:3: func Hello() {}\nb.txt:1: say Hello\ncmd/app/main.go:2: // Hello there\nwide.txt:1: "
	if !strings.HasPrefix(got, want) {
		t.Errorf("search = %q, want prefix %q", got, want)
	}
	if !strings.HasSuffix(got, "[line cut]\n[4 matches in 5 files searched; 2 files skipped (binary, too large or unreadable)]") {
		t.Errorf("search footer = %q", got[len(got)-110:])
	}
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("a long line was cut inside a character: %q", got)
	}

	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"case-insensitive": {obj{"pattern": "(?i)hello\\d", "glob": "*.go"}, "a.go:4: func hello2() {}\n[1 match in 3 files searched]"},
		"glob by name":     {obj{"pattern": "package", "glob": "main.go"}, "cmd/app/main.go:1: package main\ncmd/tool/main.go:1: package main\n[2 matches in 2 files searched]"},
		"glob by path":     {obj{"pattern": "package", "glob": "cmd/*/main.go", "max_results": 1}, "cmd/app/main.go:1: package main\n[1 match in 2 files searched; stopped at 1 matches; there are more. Narrow the pattern, path or glob]"},
		"sub path":         {obj{"pattern": "Hello", "path": "cmd/"}, "cmd/app/main.go:2: // Hello there\n[1 match in 2 files searched]"},
		"single file":      {obj{"pattern": "^bye$", "path": "b.txt", "glob": "*.go"}, "b.txt:2: bye\n[1 match in 1 file searched]"},
		"anchors per line": {obj{"pattern": "^func .*\\{\\}$", "path": "a.go"}, "a.go:3: func Hello() {}\na.go:4: func hello2() {}\n[2 matches in 1 file searched]"},
		"no match":         {obj{"pattern": "zzz-not-here", "glob": "*.go"}, "[0 matches in 3 files searched]"},
		"dependency dir":   {obj{"pattern": "Hello", "path": "node_modules"}, "node_modules/lib/index.js:1: Hello from a dependency\n[1 match in 1 file searched]"},
		"binary file":      {obj{"pattern": "Hello", "path": "bin.dat"}, "[0 matches in 0 files searched; 1 file skipped (binary, too large or unreadable)]"},
		"big file":         {obj{"pattern": "Hello", "path": "big.txt"}, "[0 matches in 0 files searched; 1 file skipped (binary, too large or unreadable)]"},
		"symlink":          {obj{"pattern": "Hello", "glob": "alias.go"}, "[0 matches in 0 files searched]"},
	} {
		if got := ok(t, reg, "search", tc.args); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"bad regex":     {obj{"pattern": "(unclosed"}, "pattern is not a valid RE2 regular expression"},
		"backreference": {obj{"pattern": `(a)\1`}, "pattern is not a valid RE2 regular expression"},
		"lookahead":     {obj{"pattern": "a(?=b)"}, "pattern is not a valid RE2 regular expression"},
		"empty":         {obj{"pattern": ""}, "pattern is empty"},
		"too long":      {obj{"pattern": strings.Repeat("a", 1001)}, "pattern is longer than 1000 bytes"},
		"bad glob":      {obj{"pattern": "x", "glob": "[a"}, `glob "[a" is not valid`},
		"missing path":  {obj{"pattern": "x", "path": "nope"}, `search "nope": file does not exist`},
		"max zero":      {obj{"pattern": "x", "max_results": 0}, "max_results: 0 is below the minimum 1"},
		"max huge":      {obj{"pattern": "x", "max_results": 201}, "max_results: 201 is above the maximum 200"},
		"no pattern":    {obj{"path": "."}, "pattern: missing required property"},
	} {
		if got := bad(t, reg, "search", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want it to hold %q", name, got, tc.want)
		}
	}
}

func TestSearchCaps(t *testing.T) {
	t.Parallel()
	fill := func(dir string) {
		for i := range 20 {
			put(t, dir, fmt.Sprintf("d%02d/f.txt", i), strings.Repeat("needle\n", 10))
		}
	}
	for name, tc := range map[string]struct {
		limits tools.Limits
		want   string
	}{
		"default matches": {tools.Limits{}, "[50 matches in 6 files searched; stopped at 50 matches; there are more. Narrow the pattern, path or glob]"},
		"files":           {tools.Limits{SearchMaxFiles: 3}, "[30 matches in 3 files searched; stopped after 3 files]"},
		"bytes":           {tools.Limits{SearchMaxBytes: 100}, "[20 matches in 2 files searched; stopped after reading 140 bytes]"},
		"dirs":            {tools.Limits{SearchMaxDirs: 3}, "[20 matches in 2 files searched; stopped after 3 directories]"},
		"output":          {tools.Limits{MaxOutputBytes: 100}, "[5 matches in 1 file searched; stopped at 5 matches because the result is full]"},
		"file size":       {tools.Limits{SearchMaxFileBytes: 69}, "[0 matches in 0 files searched; 20 files skipped (binary, too large or unreadable)]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, reg := dirRegistry(t, tc.limits)
			fill(dir)
			got := ok(t, reg, "search", obj{"pattern": "needle"})
			if !strings.HasSuffix(got, tc.want) {
				t.Errorf("got %q\nwant suffix %q", got[max(0, len(got)-160):], tc.want)
			}
		})
	}
}

func TestEditFile(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	const src = "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 1\n}\n"
	put(t, dir, "f.go", src)
	edit := func(old, repl string, extra ...any) obj {
		args := obj{"path": "f.go", "old_string": old, "new_string": repl}
		for i := 0; i+1 < len(extra); i += 2 {
			args[extra[i].(string)] = extra[i+1]
		}
		return args
	}

	// Refused edits leave the file untouched.
	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"two matches":   {edit("\treturn 1\n", "\treturn 2\n"), `old_string matches 2 times in "f.go"; nothing was changed`},
		"three matches": {edit("func", "fn"), "old_string matches 2 times"},
		"no match":      {edit("return 3", "return 4"), `old_string was not found in "f.go" (0 matches); nothing was changed`},
		"whitespace":    {edit("    return 1", "x"), "(0 matches)"},
		"empty old":     {edit("", "x"), "old_string is empty"},
		"same":          {edit("func a", "func a"), "old_string and new_string are the same"},
		"missing file":  {obj{"path": "nope.go", "old_string": "a", "new_string": "b"}, `read "nope.go": file does not exist`},
		"no new_string": {obj{"path": "f.go", "old_string": "a"}, "new_string: missing required property"},
		"null old":      {obj{"path": "f.go", "old_string": nil, "new_string": "b"}, "old_string: want string, got null"},
		"bad flag":      {edit("func a", "func z", "replace_all", "yes"), "replace_all: want boolean, got string"},
	} {
		if got := bad(t, reg, "edit_file", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want it to hold %q", name, got, tc.want)
		}
		if now := get(t, dir, "f.go"); now != src {
			t.Fatalf("%s changed the file to %q", name, now)
		}
	}

	if got, want := ok(t, reg, "edit_file", edit("func b() {\n\treturn 1", "func b() {\n\treturn 2")),
		"edited f.go: replaced 1 occurrence, at line 5"; got != want {
		t.Errorf("edit = %q, want %q", got, want)
	}
	if got, want := get(t, dir, "f.go"), "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "edit_file", edit("return", "yield", "replace_all", true)),
		"edited f.go: replaced 2 occurrences, the first at line 2"; got != want {
		t.Errorf("replace_all = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "edit_file", edit("\tyield 2\n", "")), "edited f.go: replaced 1 occurrence, at line 6"; got != want {
		t.Errorf("deletion = %q, want %q", got, want)
	}
	if got, want := get(t, dir, "f.go"), "func a() {\n\tyield 1\n}\n\nfunc b() {\n}\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}

	// Text copied from read_file has plain line feeds; a CRLF file keeps its
	// line endings.
	put(t, dir, "win.txt", "one\r\ntwo\r\nthree\r\n")
	ok(t, reg, "edit_file", obj{"path": "win.txt", "old_string": "one\ntwo", "new_string": "1\n2"})
	if got, want := get(t, dir, "win.txt"), "1\r\n2\r\nthree\r\n"; got != want {
		t.Errorf("CRLF file = %q, want %q", got, want)
	}

	put(t, dir, "bin.dat", "ab\x00cd")
	if got := bad(t, reg, "edit_file", obj{"path": "bin.dat", "old_string": "ab", "new_string": "x"}); !strings.Contains(got, "binary file") {
		t.Errorf("binary edit: %q", got)
	}

	// The mode survives an edit.
	script := filepath.Join(dir, "run.sh")
	//nolint:gosec // the test is about keeping the executable bit
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok(t, reg, "edit_file", obj{"path": "run.sh", "old_string": "old", "new_string": "new"})
	if info, _ := os.Stat(script); info.Mode().Perm() != 0o755 {
		t.Errorf("mode after edit = %v, want 0755", info.Mode().Perm())
	}
}

func TestWriteFile(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	if got, want := ok(t, reg, "write_file", obj{"path": "new/dir/f.txt", "content": "hello\n"}), "created new/dir/f.txt (6 bytes)"; got != want {
		t.Errorf("create = %q, want %q", got, want)
	}
	if got, want := ok(t, reg, "write_file", obj{"path": "new/dir/f.txt", "content": "x"}), "replaced new/dir/f.txt (1 byte)"; got != want {
		t.Errorf("replace = %q, want %q", got, want)
	}
	if got := get(t, dir, "new/dir/f.txt"); got != "x" {
		t.Errorf("content = %q", got)
	}
	ok(t, reg, "write_file", obj{"path": "empty.txt", "content": ""})
	if got := get(t, dir, "empty.txt"); got != "" {
		t.Errorf("empty content = %q", got)
	}
	ok(t, reg, "write_file", obj{"path": "limit.txt", "content": strings.Repeat("x", 1<<20)})

	for name, tc := range map[string]struct {
		args obj
		want string
	}{
		"too large":      {obj{"path": "big.txt", "content": strings.Repeat("x", 1<<20+1)}, "file too large (1048577 bytes, the limit is 1048576)"},
		"directory":      {obj{"path": "new/dir", "content": "x"}, "not a regular file (it is a directory)"},
		"below a file":   {obj{"path": "empty.txt/child", "content": "x"}, `write "empty.txt/child"`},
		"no content":     {obj{"path": "a.txt"}, "content: missing required property"},
		"null content":   {obj{"path": "a.txt", "content": nil}, "content: want string, got null"},
		"content number": {obj{"path": "a.txt", "content": 5}, "content: want string, got number"},
		"extra":          {obj{"path": "a.txt", "content": "x", "mode": "0777"}, "mode: unknown property"},
	} {
		if got := bad(t, reg, "write_file", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want it to hold %q", name, got, tc.want)
		}
	}
	for _, name := range []string{"big.txt", "a.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused write created %s", name)
		}
	}
}

// Finding 6: a short source can expand into a huge NFA. This rejects the
// original adversarial expression before any matching and skips oversized
// lines whole so anchors never see an invented end of line.
func TestSearchBoundsExpandedProgramAndInputLine(t *testing.T) {
	t.Parallel()
	dir, reg := dirRegistry(t, tools.Limits{})
	put(t, dir, "file", "a")
	pattern := strings.Repeat("[ab]{1000}", 80) + "c"
	if got := bad(t, reg, "search", obj{"pattern": pattern, "path": "file"}); !strings.Contains(got, "instructions") {
		t.Fatalf("expanded program not rejected: %q", got)
	}
	put(t, dir, "file", strings.Repeat("a", 1<<20))
	got := ok(t, reg, "search", obj{"pattern": "a$", "path": "file"})
	if !strings.Contains(got, "0 matches") || !strings.Contains(got, "1 lines skipped (over 8192 bytes)") {
		t.Fatalf("long line was scanned: %q", got)
	}
	put(t, dir, "file", strings.Repeat("a", 8192)+"\nneedle\n"+strings.Repeat("a", 8193)+"\nneedle\n")
	got = ok(t, reg, "search", obj{"pattern": "a$|needle", "path": "file"})
	for _, want := range []string{"file:1:", "file:2: needle", "file:4: needle", "3 matches", "1 lines skipped"} {
		if !strings.Contains(got, want) {
			t.Errorf("search lacks %q: %q", want, got)
		}
	}
}
