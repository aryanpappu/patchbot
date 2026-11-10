// Package wstest builds the checkout that the jail tests of the workspace
// and tools packages attack. It is imported only by tests.
package wstest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Marker is part of the content of every file the agent must never read.
// A test fails when it shows up in a result.
const Marker = "-SECRET"

// Extra is the extra protected path the fixture is meant to be opened with.
var Extra = []string{"migrations/"}

// Fixture is a checkout with protected files, symlinks of every kind, and
// secrets next to it on the outside.
type Fixture struct {
	Base string // holds the checkout and its neighbours
	Repo string // the checkout
}

// Case is one path that must be refused by every operation.
type Case struct {
	Name string
	Path string
	// Kind names the expected workspace error: invalid, escape, protected
	// or symlink.
	Kind string
}

// New builds the fixture in a temporary directory.
func New(t testing.TB) Fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := Fixture{Base: base, Repo: filepath.Join(base, "repo")}
	files := map[string]string{
		"outside/secret.txt":            "OUTSIDE" + Marker + "\n",
		"repo-evil/secret.txt":          "SIBLING" + Marker + "\n",
		"repo/README.md":                "# demo\nhello world\n",
		"repo/src/main.go":              "package main\n\nfunc main() {}\n",
		"repo/src/sub/deep.txt":         "deep hello\n",
		"repo/.git/config":              "GIT" + Marker + "\n",
		"repo/.git/HEAD":                "ref: HEAD" + Marker + "\n",
		"repo/.env":                     "ENV" + Marker + "\n",
		"repo/.env.local":               "ENVLOCAL" + Marker + "\n",
		"repo/config/.env.production":   "ENVPROD" + Marker + "\n",
		"repo/config/app.toml":          "name = \"demo\"\n",
		"repo/.github/workflows/ci.yml": "WORKFLOW" + Marker + "\n",
		"repo/.github/CODEOWNERS":       "* @owner\n",
		"repo/migrations/001.sql":       "MIGRATION" + Marker + "\n",
	}
	for name, content := range files {
		p := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"out-file":  "../../outside/secret.txt",
		"out-abs":   filepath.Join(base, "outside", "secret.txt"),
		"out-dir":   "../../outside",
		"sibling":   "../../repo-evil",
		"git":       "../.git",
		"gitconfig": "../.git/config",
		"env":       "../.env",
		"workflows": "../.github/workflows",
		"migr":      "../migrations",
		"updown":    "../src/../.git",
		"hop":       "git",
		"dangling":  "nowhere",
		"loop-a":    "loop-b",
		"loop-b":    "loop-a",
		"self":      "self",
		"ok-file":   "../README.md",
		"ok-dir":    "../src",
		"chain":     "ok-dir",
	}
	linkDir := filepath.Join(f.Repo, "links")
	if err := os.Mkdir(linkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(linkDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// Cases returns every path that must be refused.
func (f Fixture) Cases() []Case {
	outside := filepath.ToSlash(filepath.Join(f.Base, "outside", "secret.txt"))
	return []Case{
		{"empty", "", "invalid"},
		{"NUL only", "\x00", "invalid"},
		{"NUL inside", "README.md\x00.txt", "invalid"},
		{"NUL after traversal", "src/\x00/../../outside/secret.txt", "invalid"},
		{"control character", "src/ma\nin.go", "invalid"},
		{"backslash traversal", `..\outside\secret.txt`, "invalid"},
		{"UNC backslash", `\\server\share\secret.txt`, "invalid"},
		{"volume backslash", `C:\Windows\win.ini`, "invalid"},
		{"invalid UTF-8", "src/\xff\xfe", "invalid"},
		{"over-long path", strings.Repeat("a/", 600) + "f", "invalid"},
		{"over-long name", strings.Repeat("n", 300), "invalid"},
		{"too deep", strings.Repeat("d/", 80) + "f", "invalid"},

		{"absolute", "/etc/passwd", "escape"},
		{"absolute outside file", outside, "escape"},
		{"absolute checkout file", filepath.ToSlash(filepath.Join(f.Repo, "README.md")), "escape"},
		{"double slash UNC", "//server/share/secret.txt", "escape"},
		{"volume slash", "C:/Windows/win.ini", "escape"},
		{"volume relative", "c:secret.txt", "escape"},
		{"parent", "..", "escape"},
		{"parent slash", "../", "escape"},
		{"dotdot file", "../outside/secret.txt", "escape"},
		{"dotdot inside", "src/../../outside/secret.txt", "escape"},
		{"dotdot staying inside", "src/../README.md", "escape"},
		{"dotdot at the end", "src/..", "escape"},
		{"dotdot trailing slash", "src/../", "escape"},
		{"dot then dotdot", "./../outside/secret.txt", "escape"},
		{"sibling prefix", "../repo-evil/secret.txt", "escape"},
		{"sibling prefix via own name", "../repo/../repo-evil/secret.txt", "escape"},
		{"dotdot to git", "src/../.git/config", "escape"},

		{"git dir", ".git", "protected"},
		{"git dir trailing slash", ".git/", "protected"},
		{"git dir trailing slashes", ".git///", "protected"},
		{"git file", ".git/config", "protected"},
		{"git file dot prefix", "./.git/config", "protected"},
		{"git file double slash", ".git//config", "protected"},
		{"git upper", ".GIT/config", "protected"},
		{"git mixed", ".Git/HEAD", "protected"},
		{"git mixed dir", ".gIt/", "protected"},
		{"git trailing dot", ".git./config", "protected"},
		{"git trailing space", ".git /config", "protected"},
		{"git stream", ".git::$INDEX_ALLOCATION/config", "protected"},
		{"git short name", "GIT~1/config", "protected"},
		{"git zero width", ".g\u200cit/config", "protected"},
		{"git nested", "src/.git/config", "protected"},
		{"git new file", ".git/hooks/pre-commit", "protected"},
		{"env", ".env", "protected"},
		{"env upper", ".ENV", "protected"},
		{"env mixed", ".Env", "protected"},
		{"env trailing slash", ".env/", "protected"},
		{"env local", ".env.local", "protected"},
		{"env local upper", ".ENV.LOCAL", "protected"},
		{"env nested", "config/.env.production", "protected"},
		{"env nested upper", "config/.ENV.Production", "protected"},
		{"env new", "src/.env.test", "protected"},
		{"env as directory", ".env/keys.txt", "protected"},
		{"workflow dir", ".github/workflows", "protected"},
		{"workflow dir trailing slash", ".github/workflows/", "protected"},
		{"workflow file", ".github/workflows/ci.yml", "protected"},
		{"workflow upper", ".GITHUB/WORKFLOWS/ci.yml", "protected"},
		{"workflow new", ".github/workflows/evil.yml", "protected"},
		{"configured", "migrations", "protected"},
		{"configured file", "migrations/001.sql", "protected"},
		{"configured upper", "MIGRATIONS/001.sql", "protected"},
		{"configured long s", "migration\u017f/001.sql", "protected"},
		{"configured new", "migrations/002.sql", "protected"},

		{"symlink to outside file", "links/out-file", "escape"},
		{"absolute symlink", "links/out-abs", "escape"},
		{"symlinked parent to outside", "links/out-dir/secret.txt", "escape"},
		{"symlinked dir to outside", "links/out-dir", "escape"},
		{"symlinked parent, new file outside", "links/out-dir/new.txt", "escape"},
		{"symlink to sibling", "links/sibling/secret.txt", "escape"},
		{"symlink to git dir", "links/git", "protected"},
		{"symlinked parent to git", "links/git/config", "protected"},
		{"symlinked parent, new file in git", "links/git/hooks/pre-commit", "protected"},
		{"symlink to git file", "links/gitconfig", "protected"},
		{"symlink to env", "links/env", "protected"},
		{"symlink to workflows", "links/workflows/ci.yml", "protected"},
		{"symlink to configured", "links/migr/001.sql", "protected"},
		{"symlink with dotdot to git", "links/updown/config", "protected"},
		{"symlink chain to git", "links/hop/config", "protected"},
		{"dangling symlink", "links/dangling", "symlink"},
		{"dangling symlink as parent", "links/dangling/new.txt", "symlink"},
		{"symlink loop", "links/loop-a", "symlink"},
		{"symlink loop as parent", "links/loop-b/file.txt", "symlink"},
		{"symlink to itself", "links/self", "symlink"},
	}
}

// Snapshot records every file, directory and symlink under Base with its
// mode and content, so a test can show that nothing changed.
func (f Fixture) Snapshot(t testing.TB) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(f.Base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.Base, p)
		entry := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p) //nolint:gosec // a path inside the test fixture
			if err != nil {
				return err
			}
			entry += " " + string(data)
		}
		snap[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// RequireUnchanged fails the test if the tree differs from the snapshot.
func (f Fixture) RequireUnchanged(t testing.TB, before map[string]string) {
	t.Helper()
	after := f.Snapshot(t)
	for name, want := range before {
		got, ok := after[name]
		switch {
		case !ok:
			t.Errorf("%s was removed", name)
		case got != want:
			t.Errorf("%s changed:\n was %q\n now %q", name, want, got)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		}
	}
}

// Entry returns the snapshot entry of one path under Base, in the same form
// as Snapshot, and whether the path exists.
func (f Fixture) Entry(name string) (string, bool) {
	p := filepath.Join(f.Base, filepath.FromSlash(name))
	info, err := os.Lstat(p)
	if err != nil {
		return "", false
	}
	entry := info.Mode().String()
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return "", false
		}
		entry += " -> " + target
	case info.Mode().IsRegular():
		data, err := os.ReadFile(p) //nolint:gosec // a path inside the test fixture
		if err != nil {
			return "", false
		}
		entry += " " + string(data)
	}
	return entry, true
}
