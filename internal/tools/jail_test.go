package tools_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aryanpappu/patchbot/internal/llm"
	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
	"github.com/aryanpappu/patchbot/internal/workspace/wstest"
)

// fixtureRegistry returns the code registry on the attack fixture.
func fixtureRegistry(t testing.TB) (wstest.Fixture, *tools.Registry) {
	t.Helper()
	fx := wstest.New(t)
	ws, err := workspace.Open(fx.Repo, workspace.Options{Protected: wstest.Extra})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	reg, err := tools.NewCodeRegistry(&tools.Env{WS: ws, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return fx, reg
}

// dirRegistry returns the code registry on an empty directory.
func dirRegistry(t testing.TB, limits tools.Limits) (string, *tools.Registry) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(dir, workspace.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	reg, err := tools.NewCodeRegistry(&tools.Env{WS: ws, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return dir, reg
}

func put(t testing.TB, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func get(t testing.TB, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // a path inside the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type obj = map[string]any

// call dispatches one tool call with the given arguments.
func call(t testing.TB, reg *tools.Registry, name string, args any) llm.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Dispatch(context.Background(), llm.ToolCall{ID: "c1", Name: name, Args: raw})
	if err != nil {
		t.Fatalf("Dispatch(%s %s): %v", name, raw, err)
	}
	if res.CallID != "c1" || res.Name != name {
		t.Fatalf("result is for %q/%q, want c1/%s", res.CallID, res.Name, name)
	}
	return res
}

// ok dispatches a call that must succeed and returns its content.
func ok(t testing.TB, reg *tools.Registry, name string, args any) string {
	t.Helper()
	res := call(t, reg, name, args)
	if res.IsError {
		t.Fatalf("%s %v failed: %s", name, args, res.Content)
	}
	return res.Content
}

// bad dispatches a call that must fail and returns its error text.
func bad(t testing.TB, reg *tools.Registry, name string, args any) string {
	t.Helper()
	res := call(t, reg, name, args)
	if !res.IsError {
		t.Fatalf("%s %v succeeded: %s", name, args, res.Content)
	}
	return res.Content
}

// attacks are the calls that carry a path, one per tool. Every tool must
// refuse every escape case.
var attacks = map[string]func(path string) obj{
	"read_file":  func(p string) obj { return obj{"path": p} },
	"list_dir":   func(p string) obj { return obj{"path": p, "depth": 3} },
	"search":     func(p string) obj { return obj{"pattern": ".", "path": p} },
	"edit_file":  func(p string) obj { return obj{"path": p, "old_string": "SECRET", "new_string": "pwned"} },
	"write_file": func(p string) obj { return obj{"path": p, "content": "pwned\n"} },
}

var kindText = map[string]string{
	"invalid":   "invalid path",
	"escape":    "path escapes workspace",
	"protected": "path is protected",
	"symlink":   "unsafe symlink",
}

func TestEveryToolRefusesEveryEscape(t *testing.T) {
	t.Parallel()
	fx, reg := fixtureRegistry(t)
	before := fx.Snapshot(t)
	for _, tc := range fx.Cases() {
		for tool, args := range attacks {
			t.Run(tc.Name+"/"+tool, func(t *testing.T) {
				// list_dir and search treat an empty path as the root, so
				// the empty case does not apply to them.
				if tc.Path == "" && (tool == "list_dir" || tool == "search") {
					t.Skip("an empty path means the repository root for this tool")
				}
				// JSON cannot carry invalid UTF-8: the decoder turns it into
				// U+FFFD, which is an ordinary character in a file name.
				if !utf8.ValidString(tc.Path) {
					t.Skip("not expressible in JSON arguments")
				}
				res := call(t, reg, tool, args(tc.Path))
				if !res.IsError {
					t.Fatalf("%s on %q succeeded: %s", tool, tc.Path, res.Content)
				}
				if strings.Contains(res.Content, wstest.Marker) {
					t.Errorf("%s on %q leaked a secret: %s", tool, tc.Path, res.Content)
				}
				if !strings.Contains(res.Content, kindText[tc.Kind]) {
					t.Errorf("%s on %q: %q, want it to say %q", tool, tc.Path, res.Content, kindText[tc.Kind])
				}
				if strings.Contains(res.Content, fx.Base) && !strings.Contains(tc.Path, fx.Base) {
					t.Errorf("%s on %q names a host path: %s", tool, tc.Path, res.Content)
				}
			})
		}
	}
	fx.RequireUnchanged(t, before)
}

// The tools that walk the tree must not come across a protected file on
// their own, whatever the starting point.
func TestWalkingToolsSkipProtectedPaths(t *testing.T) {
	t.Parallel()
	fx, reg := fixtureRegistry(t)
	before := fx.Snapshot(t)

	for _, start := range []string{".", "", "links", "config", ".github", "links/ok-dir"} {
		out := ok(t, reg, "search", obj{"pattern": "SECRET|secret", "path": start, "max_results": 200})
		if !strings.HasPrefix(out, "[0 matches") {
			t.Errorf("search from %q found protected content:\n%s", start, out)
		}
	}
	out := ok(t, reg, "search", obj{"pattern": "hello"})
	if want := "README.md:2: hello world\nsrc/sub/deep.txt:1: deep hello\n"; !strings.HasPrefix(out, want) {
		t.Errorf("search = %q, want it to start with %q (symlinked copies must not be searched)", out, want)
	}

	listing := ok(t, reg, "list_dir", obj{"depth": 3})
	for line := range strings.SplitSeq(listing, "\n") {
		name, _, _ := strings.Cut(line, " ")
		lower := strings.ToLower(name)
		for _, banned := range []string{".git/", ".env", ".github/workflows", "migrations", "config/.env", "secret.txt", "ci.yml", "001.sql"} {
			if strings.HasPrefix(lower, banned) || strings.HasSuffix(lower, banned) || lower == ".git" {
				t.Errorf("list_dir shows %q", line)
			}
		}
	}
	for _, want := range []string{"README.md (", "src/", "src/sub/deep.txt (", ".github/CODEOWNERS (", "config/app.toml (", "links/git (symlink)", "links/ok-dir (symlink)"} {
		if !strings.Contains(listing, want) {
			t.Errorf("list_dir lacks %q:\n%s", want, listing)
		}
	}
	// A symlinked directory is shown but not walked into.
	if strings.Contains(listing, "links/ok-dir/") || strings.Contains(listing, "links/out-dir/") {
		t.Errorf("list_dir followed a symlink:\n%s", listing)
	}
	fx.RequireUnchanged(t, before)
}

// The read-only registry has no tool that can change the checkout.
func TestReadOnlyRegistryCannotWrite(t *testing.T) {
	t.Parallel()
	fx := wstest.New(t)
	ws, err := workspace.Open(fx.Repo, workspace.Options{Protected: wstest.Extra})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close(true) }()
	reg, err := tools.NewReadOnlyRegistry(&tools.Env{WS: ws})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(reg.Names(), ","), "read_file,list_dir,search"; got != want {
		t.Errorf("read-only tools = %s, want %s", got, want)
	}
	before := fx.Snapshot(t)
	for _, args := range []struct {
		tool string
		args obj
	}{
		{"write_file", obj{"path": "README.md", "content": "x"}},
		{"write_file", obj{"path": "new.txt", "content": "x"}},
		{"edit_file", obj{"path": "README.md", "old_string": "hello", "new_string": "bye"}},
		{"run_cmd", obj{"name": "test"}},
	} {
		if msg := bad(t, reg, args.tool, args.args); !strings.Contains(msg, "unknown tool") {
			t.Errorf("%s in the read-only registry: %s", args.tool, msg)
		}
	}
	ok(t, reg, "read_file", obj{"path": "README.md"})
	fx.RequireUnchanged(t, before)

	code, err := tools.NewCodeRegistry(&tools.Env{WS: ws})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(code.Names(), ","), "read_file,list_dir,search,edit_file,write_file"; got != want {
		t.Errorf("code tools = %s, want %s", got, want)
	}
}
