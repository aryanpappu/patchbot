package execx

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestTrustedPathValidation(t *testing.T) {
	root := testpath.Dir(t)
	data := filepath.Join(root, "data")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dirs []string
		bad  bool
	}{
		{"safe", []string{bin}, false},
		{"future directory", []string{filepath.Join(root, "future")}, true},
		{"empty list", []string{}, true},
		{"empty entry", []string{bin, ""}, true},
		{"relative", []string{"bin"}, true},
		{"parent", []string{bin + "/../bin"}, true},
		{"PATH injection", []string{bin + string(filepath.ListSeparator) + "/bin"}, true},
		{"data", []string{data}, true},
		{"data child", []string{filepath.Join(data, "workspaces", "new")}, true},
		{"symlink into future data", []string{alias}, true},
		{"file", []string{file}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTrustedPath(tc.dirs, data); (err != nil) != tc.bad {
				t.Fatalf("error = %v, bad = %v", err, tc.bad)
			}
		})
	}
	if runtime.GOOS != "windows" {
		// #nosec G302 -- deliberately unsafe permissions exercise rejection.
		if err := os.Chmod(bin, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTrustedPath([]string{bin}); err == nil {
			t.Fatal("accepted world-writable directory")
		}
	}
}

func TestPolicyConfiguredPathAndEnvironment(t *testing.T) {
	root, bin := t.TempDir(), testpath.Dir(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(bin, "custom-tool")); err != nil {
		t.Fatal(err)
	}
	dirs := []string{bin}
	p, err := NewPolicy(root, map[string][]string{"test": {"custom-tool", "env"}}, PolicyOptions{TrustedPath: dirs})
	if err != nil {
		t.Fatal(err)
	}
	dirs[0] = "/untrusted"
	argv, err := p.Resolve("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != want {
		t.Fatalf("resolved %q, want %q", argv[0], want)
	}
	env := envMap(p.MinimalEnv(t.TempDir(), t.TempDir(), map[string]string{"PATH": "/untrusted"}))
	if env["PATH"] != bin {
		t.Fatalf("child PATH = %q", env["PATH"])
	}
	if _, err := NewPolicy(root, map[string][]string{"test": {"true"}}, PolicyOptions{TrustedPath: []string{bin}}); err == nil {
		t.Fatal("used ambient/default PATH")
	}
	if _, err := NewPolicy(root, nil, PolicyOptions{TrustedPath: []string{root}}); err == nil {
		t.Fatal("trusted workspace")
	}
	if !reflect.DeepEqual(p.path, []string{bin}) || strings.Contains(env["PATH"], "untrusted") {
		t.Fatal("policy path changed")
	}
}
