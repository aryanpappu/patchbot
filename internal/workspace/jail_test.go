package workspace_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/workspace"
	"github.com/aryanpappu/patchbot/internal/workspace/wstest"
)

var kinds = map[string]error{
	"invalid":   workspace.ErrInvalidPath,
	"escape":    workspace.ErrPathEscape,
	"protected": workspace.ErrPathProtected,
	"symlink":   workspace.ErrSymlink,
}

func openFixture(t testing.TB) (wstest.Fixture, *workspace.Local) {
	t.Helper()
	fx := wstest.New(t)
	ws, err := workspace.Open(fx.Repo, workspace.Options{Protected: wstest.Extra})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close(true) })
	return fx, ws
}

// ops are the four operations of a Workspace. Each must refuse every case.
var ops = map[string]func(ws workspace.Workspace, path string) (string, error){
	"read": func(ws workspace.Workspace, path string) (string, error) {
		data, err := ws.ReadFile(path)
		return string(data), err
	},
	"write": func(ws workspace.Workspace, path string) (string, error) {
		return "", ws.WriteFile(path, []byte("pwned\n"))
	},
	"stat": func(ws workspace.Workspace, path string) (string, error) {
		info, err := ws.Stat(path)
		if err != nil {
			return "", err
		}
		return info.Name(), nil
	},
	"list": func(ws workspace.Workspace, path string) (string, error) {
		entries, err := ws.ReadDir(path)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return strings.Join(names, " "), err
	},
}

func TestEscapesRefusedByEveryOperation(t *testing.T) {
	t.Parallel()
	fx, ws := openFixture(t)
	before := fx.Snapshot(t)
	for _, tc := range fx.Cases() {
		for opName, op := range ops {
			t.Run(tc.Name+"/"+opName, func(t *testing.T) {
				out, err := op(ws, tc.Path)
				if err == nil {
					t.Fatalf("%s %q succeeded with %q, want %s error", opName, tc.Path, out, tc.Kind)
				}
				if !errors.Is(err, kinds[tc.Kind]) {
					t.Errorf("%s %q: error %v, want %v", opName, tc.Path, err, kinds[tc.Kind])
				}
				var pe *workspace.PathError
				if !errors.As(err, &pe) {
					t.Fatalf("error is %T, want *workspace.PathError", err)
				}
				if strings.Contains(err.Error(), fx.Base) && !strings.Contains(tc.Path, fx.Base) {
					t.Errorf("error names a host path: %v", err)
				}
				if out != "" {
					t.Errorf("%s %q returned data %q with its error", opName, tc.Path, out)
				}
			})
		}
	}
	fx.RequireUnchanged(t, before)
}

func TestAllowedPaths(t *testing.T) {
	t.Parallel()
	_, ws := openFixture(t)
	reads := map[string]string{
		"README.md":                "# demo\nhello world\n",
		"./README.md":              "# demo\nhello world\n",
		"src//main.go":             "package main\n\nfunc main() {}\n",
		"src/./sub/deep.txt":       "deep hello\n",
		"links/ok-file":            "# demo\nhello world\n",
		"links/ok-dir/main.go":     "package main\n\nfunc main() {}\n",
		"links/chain/sub/deep.txt": "deep hello\n",
		".github/CODEOWNERS":       "* @owner\n",
		"config/app.toml":          "name = \"demo\"\n",
	}
	for path, want := range reads {
		got, err := ws.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, dir := range []string{".", "./", "src", "src/", "src///", "links/ok-dir", "links/chain/", ".github"} {
		if _, err := ws.ReadDir(dir); err != nil {
			t.Errorf("ReadDir(%q): %v", dir, err)
		}
	}
	// Names that only look like protected ones stay usable.
	for _, path := range []string{"gitignore.txt", ".gitignore", ".github/dependabot.yml", "env.go", "src/environment.go", "migrations.md", "docs/.github/workflows/x.yml"} {
		if err := ws.WriteFile(path, []byte("ok\n")); err != nil {
			t.Errorf("WriteFile(%q): %v", path, err)
		}
	}
	if _, err := ws.ReadFile("missing.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := ws.ReadFile("README.md/x"); err == nil {
		t.Error("ReadFile below a regular file succeeded")
	}
	if _, err := ws.ReadFile("src"); !errors.Is(err, workspace.ErrNotRegular) {
		t.Errorf("ReadFile(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := ws.ReadDir("README.md"); !errors.Is(err, workspace.ErrNotDir) {
		t.Errorf("ReadDir(file) = %v, want ErrNotDir", err)
	}
	if err := ws.WriteFile(".", []byte("x")); !errors.Is(err, workspace.ErrNotRegular) {
		t.Errorf("WriteFile(.) = %v, want ErrNotRegular", err)
	}
	if err := ws.WriteFile("src", []byte("x")); !errors.Is(err, workspace.ErrNotRegular) {
		t.Errorf("WriteFile(dir) = %v, want ErrNotRegular", err)
	}
}

func TestReadDirHidesProtectedEntries(t *testing.T) {
	t.Parallel()
	_, ws := openFixture(t)
	names := func(dir string) string {
		entries, err := ws.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return strings.Join(out, " ")
	}
	if got, want := names("."), ".github README.md config links src"; got != want {
		t.Errorf("root = %q, want %q", got, want)
	}
	if got, want := names(".github"), "CODEOWNERS"; got != want {
		t.Errorf(".github = %q, want %q", got, want)
	}
	if got, want := names("config"), "app.toml"; got != want {
		t.Errorf("config = %q, want %q", got, want)
	}
	entries, err := ws.ReadDir("links")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			t.Errorf("links/%s is reported as %v, want a symlink", e.Name(), e.Type())
		}
	}
}

func TestPolicyClean(t *testing.T) {
	t.Parallel()
	p, err := workspace.NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		".":                      ".",
		"./":                     ".",
		"././/.":                 ".",
		"a":                      "a",
		"a/":                     "a",
		"a//b/./c///":            "a/b/c",
		"./a/b":                  "a/b",
		"...":                    "...",
		"a/.../b":                "a/.../b",
		"..a/b..":                "..a/b..",
		"name with sp":           "name with sp",
		"üñí/日本":                 "üñí/日本",
		"ab:c":                   "ab:c",
		strings.Repeat("d/", 64): strings.TrimSuffix(strings.Repeat("d/", 64), "/"),
	} {
		got, err := p.Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNewPolicyExtraPaths(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", ".", "./", "/abs", "../up", "a/../b", "a\x00b", `a\b`, ". /x"} {
		if _, err := workspace.NewPolicy([]string{bad}); err == nil {
			t.Errorf("NewPolicy(%q) succeeded", bad)
		}
	}
	p, err := workspace.NewPolicy([]string{"migrations/", "Secrets/prod.yaml", "./vendor//private"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"migrations":             true,
		"migrations/001.sql":     true,
		"Migrations/x/y":         true,
		"migrations2/001.sql":    false,
		"db/migrations/001.sql":  false, // extra paths are anchored at the root
		"secrets/prod.yaml":      true,
		"SECRETS/PROD.YAML/x":    true,
		"secrets/dev.yaml":       false,
		"secrets":                false,
		"vendor/private/key":     true,
		"vendor/public":          false,
		".github/workflows":      true,
		".github/workflowsx":     false,
		"docs/.github/workflows": false,
		".gitignore":             false,
		".gitattributes":         false,
		".github":                false,
		".git":                   true,
		"a/b/.GIT":               true,
		".env":                   true,
		".envrc":                 true,
		".environment":           true,
		"env":                    false,
		"a.env":                  false,
		".":                      false,
	} {
		if _, got := p.Protected(path); got != want {
			t.Errorf("Protected(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPathErrorHidesHostPath(t *testing.T) {
	t.Parallel()
	fx, ws := openFixture(t)
	for _, path := range []string{"missing/file.txt", "README.md/x", "src"} {
		_, err := ws.ReadFile(path)
		if err == nil {
			t.Fatalf("ReadFile(%q) succeeded", path)
		}
		if strings.Contains(err.Error(), fx.Base) || strings.Contains(err.Error(), filepath.Base(fx.Base)) {
			t.Errorf("error names a host path: %v", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error does not name the path %q: %v", path, err)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	if _, err := workspace.Open(filepath.Join(t.TempDir(), "missing"), workspace.Options{}); err == nil {
		t.Error("Open of a missing directory succeeded")
	}
	if _, err := workspace.Open(t.TempDir(), workspace.Options{Protected: []string{"../x"}}); err == nil {
		t.Error("Open with a bad protected path succeeded")
	}
}

// FuzzJailPath checks that no path, however malformed, lets any operation
// read a protected or outside file or change anything it must not.
func FuzzJailPath(f *testing.F) {
	fx, ws := openFixture(f)
	for _, tc := range fx.Cases() {
		f.Add(tc.Path)
	}
	for _, seed := range []string{
		"README.md", "src/main.go", "links/ok-file", "links/chain/sub/deep.txt", "new/dir/file.txt", ".",
		"links/ok-dir/../../.git/config", "links/git/../.env", ".GIT/CONFIG", "links/../.git/config",
		"src/.ENV.x", ".github/Workflows/a", "a/b/../../../outside/secret.txt", "links/out-dir/../repo/.git/config",
		"\u202e.git/config", ".git\u200d/config", "..\u200c/outside/secret.txt", "%2e%2e/outside/secret.txt",
	} {
		f.Add(seed)
	}
	policy := ws.Policy()
	secrets := fx.Snapshot(f)
	// The fuzzer may write wherever writing is allowed. These must never
	// change: everything outside the checkout and every protected file.
	guarded := map[string]string{}
	for name, entry := range secrets {
		if !strings.HasPrefix(name, "repo/") || strings.Contains(entry, wstest.Marker) || strings.HasPrefix(name, "repo/links/") {
			guarded[name] = entry
		}
	}

	f.Fuzz(func(t *testing.T, path string) {
		clean, err := policy.Check(path)
		if err == nil {
			if !filepath.IsLocal(filepath.FromSlash(clean)) {
				t.Fatalf("Check(%q) accepted %q, which is not a local path", path, clean)
			}
			if clean != "." {
				for part := range strings.SplitSeq(clean, "/") {
					if part == "" || part == "." || part == ".." {
						t.Fatalf("Check(%q) = %q, which holds the component %q", path, clean, part)
					}
					lower := strings.ToLower(part)
					if lower == ".git" || strings.HasPrefix(lower, ".env") {
						t.Fatalf("Check(%q) = %q, which holds the protected name %q", path, clean, part)
					}
				}
			}
			if again, err := policy.Check(clean); err != nil || again != clean {
				t.Fatalf("Check is not stable: Check(%q) = %q, %v", clean, again, err)
			}
			// A different ASCII case must get the same verdict, or a
			// case-insensitive file system would open the same file.
			for _, variant := range []string{strings.ToUpper(clean), strings.ToLower(clean)} {
				if _, isProtected := policy.Protected(variant); isProtected {
					t.Fatalf("%q is accepted but its case variant %q is protected", clean, variant)
				}
			}
		}

		data, readErr := ws.ReadFile(path)
		if strings.Contains(string(data), wstest.Marker) {
			t.Fatalf("ReadFile(%q) returned a secret: %q", path, data)
		}
		if err != nil && readErr == nil {
			t.Fatalf("ReadFile(%q) succeeded on a path that Check refuses: %v", path, err)
		}
		// The fuzzer's own writes may create any allowed name, so only the
		// names that are protected wherever they are can be checked here.
		entries, _ := ws.ReadDir(path)
		for _, e := range entries {
			lower := strings.ToLower(e.Name())
			if lower == ".git" || strings.HasPrefix(lower, ".env") {
				t.Fatalf("ReadDir(%q) listed %q", path, e.Name())
			}
		}
		if info, statErr := ws.Stat(path); statErr == nil && err != nil {
			t.Fatalf("Stat(%q) = %v on a path that Check refuses: %v", path, info.Name(), err)
		}
		writeErr := ws.WriteFile(path, []byte("fuzz\n"))
		if err != nil && writeErr == nil {
			t.Fatalf("WriteFile(%q) succeeded on a path that Check refuses: %v", path, err)
		}
		for name, want := range guarded {
			got, ok := fx.Entry(name)
			if !ok || got != want {
				t.Fatalf("after operations on %q, %s changed:\n was %q\n now %q", path, name, want, got)
			}
		}
	})
}

// Finding 5: protect absent configured paths without relying on filesystem
// identity. The conservative normalization rule rejects decomposed spellings on
// every platform, for both policy inputs and candidates (not just macOS).
func TestCombiningMarksCannotBypassPolicy(t *testing.T) {
	t.Parallel()
	p, err := workspace.NewPolicy([]string{"caf\u00e9/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cafe\u0301/x", "x\u034f/y", "x\u1ab0/y", "x\u20dd/y"} {
		if _, err := p.Check(name); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("Check(%q) = %v, want ErrInvalidPath", name, err)
		}
		if _, err := workspace.NewPolicy([]string{name}); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("NewPolicy(%q) = %v, want ErrInvalidPath", name, err)
		}
	}
	if _, err := p.Check("caf\u00e9/x"); !errors.Is(err, workspace.ErrPathProtected) {
		t.Fatalf("composed configured name is not protected: %v", err)
	}
	dir, ws := openDir(t, workspace.Options{Protected: []string{"caf\u00e9/"}})
	if err := ws.WriteFile("cafe\u0301/x", []byte("pwned")); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("decomposed creation = %v", err)
	}
	if err := os.Symlink("cafe\u0301/x", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ReadFile("alias"); !errors.Is(err, workspace.ErrInvalidPath) {
		t.Fatalf("decomposed symlink target = %v", err)
	}
}
