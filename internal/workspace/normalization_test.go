package workspace_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/tools"
	"github.com/aryanpappu/patchbot/internal/workspace"
)

// Finding 5: absent protected paths need lexical protection independent of
// filesystem identity or the host's treatment of canonical equivalents.
func TestCanonicalNormalizationProtectsPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		protected string
		candidate string
	}{
		{"hangul", "\uac00", "\u1100\u1161"},
		{"hangul-reverse", "\u1100\u1161", "\uac00"},
		{"accent", "caf\u00e9", "cafe\u0301"},
		{"accent-reverse", "cafe\u0301", "caf\u00e9"},
		{"components", "\uac00/caf\u00e9", "\u1100\u1161/cafe\u0301"},
		{"kelvin", "", ".github/wor\u212aflows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var extra []string
			if tc.protected != "" {
				extra = []string{tc.protected}
			}
			dir, ws := openDir(t, workspace.Options{Protected: extra})
			parent := path.Dir(tc.candidate)
			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(parent)), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.candidate, filepath.Join(dir, "alias")); err != nil {
				t.Fatal(err)
			}
			search := func(name string) tools.Result {
				t.Helper()
				raw, err := json.Marshal(map[string]string{"path": name, "pattern": "SECRET|public"})
				if err != nil {
					t.Fatal(err)
				}
				result, err := (tools.Search{}).Execute(t.Context(), &tools.Env{WS: ws}, raw)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			checkDenied := func(t *testing.T) {
				t.Helper()
				for _, name := range []string{tc.candidate, tc.candidate + "/secret.txt", "alias", "alias/secret.txt"} {
					for opName, op := range ops {
						out, err := op(ws, name)
						if !errors.Is(err, workspace.ErrPathProtected) || out != "" {
							t.Errorf("%s(%q) = %q, %v; want no data and ErrPathProtected", opName, name, out, err)
						}
					}
					if got := search(name); !got.IsError || !strings.Contains(got.Content, workspace.ErrPathProtected.Error()) || strings.Contains(got.Content, "SECRET") {
						t.Errorf("search(%q) = %+v; want protected-path error without data", name, got)
					}
				}
			}
			t.Run("absent", checkDenied)
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(tc.candidate))); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("denied operations created the protected path: %v", err)
			}

			secret := filepath.Join(dir, filepath.FromSlash(tc.candidate), "secret.txt")
			mustWrite(t, secret, "SECRET\n", 0o600)
			public := path.Join(parent, "public.txt")
			mustWrite(t, filepath.Join(dir, filepath.FromSlash(public)), "public\n", 0o600)
			t.Run("existing", checkDenied)
			entries, err := ws.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			want := "public.txt"
			if parent == "." {
				want = "alias public.txt"
			}
			if got := strings.Join(names, " "); got != want {
				t.Errorf("ReadDir(%q) = %q, want %q", parent, got, want)
			}
			if got := search("."); got.IsError || got.Content != public+":1: public\n[1 match in 1 file searched]" {
				t.Errorf("search(root) = %+v; want only the public file", got)
			}
			if got := string(hostFile(t, secret)); got != "SECRET\n" {
				t.Errorf("protected content changed: %q", got)
			}
		})
	}
}

func TestNormalizationSafety(t *testing.T) {
	t.Parallel()
	p, err := workspace.NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cafe\u0301", "\u1100\u1161", "x\u034f", "x\u1ab0", "x\u20dd", "x" + strings.Repeat("\u0301", 30)} {
		if clean, err := p.Clean(name); err != nil || clean != name {
			t.Errorf("Clean(%q) = %q, %v; want original spelling", name, clean, err)
		}
		if _, err := workspace.NewPolicy([]string{name}); err != nil {
			t.Errorf("NewPolicy(%q): %v", name, err)
		}
	}
	// More than 30 non-starters makes norm insert CGJ. An explicit CGJ
	// elsewhere must not hide that insertion; malformed UTF-8 is also unsafe.
	for _, name := range []string{"x" + strings.Repeat("\u0301", 31), "\u034fx" + strings.Repeat("\u0301", 31), "bad\xff"} {
		if _, err := p.Clean(name); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("Clean(%q) = %v, want ErrInvalidPath", name, err)
		}
		if _, err := workspace.NewPolicy([]string{name}); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("NewPolicy(%q) = %v, want ErrInvalidPath", name, err)
		}
		dir, ws := openDir(t, workspace.Options{})
		if err := ws.WriteFile(name, nil); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("WriteFile(%q) = %v, want ErrInvalidPath", name, err)
		}
		if err := os.Symlink(name, filepath.Join(dir, "alias")); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.ReadFile("alias"); !errors.Is(err, workspace.ErrInvalidPath) {
			t.Errorf("ReadFile(alias to %q) = %v, want ErrInvalidPath", name, err)
		}
	}
}
