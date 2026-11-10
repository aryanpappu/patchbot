package execx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyResolve(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commands := map[string][]string{"test": {os.Args[0], "fixed", "--trusted-flag"}}
	opts := PolicyOptions{AllowArgs: map[string]bool{"test": true}, MaxExtraArgs: 2, MaxArgLength: 16, MaxExtraBytes: 20}
	p, err := NewPolicy(root, commands, opts)
	if err != nil {
		t.Fatal(err)
	}
	commands["test"][1] = "mutated"
	opts.AllowArgs["test"] = false
	argv, err := p.Resolve("test", []string{"literal;|$(text)", "x"})
	if err != nil || len(argv) != 5 || argv[1] != "fixed" || !filepath.IsAbs(argv[0]) {
		t.Fatalf("Resolve = %q, %v", argv, err)
	}
	argv[1] = "mutated returned slice"
	if again, resolveErr := p.Resolve("test", nil); resolveErr != nil || again[1] != "fixed" {
		t.Fatalf("Resolve is mutable: %q, %v", again, resolveErr)
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{"unknown", nil}, {"test", []string{"bad\x00arg"}}, {"test", []string{"-flag"}},
		{"test", []string{"a", "b", "c"}}, {"test", []string{strings.Repeat("x", 17)}},
		{"test", []string{strings.Repeat("x", 11), strings.Repeat("y", 10)}},
	} {
		if _, err := p.Resolve(test.name, test.args); err == nil {
			t.Errorf("accepted %q %q", test.name, test.args)
		}
	}
	noArgs, err := NewPolicy(root, map[string][]string{"test": {os.Args[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noArgs.Resolve("test", []string{"file"}); err == nil {
		t.Fatal("extra args allowed by default")
	}
	flags, err := NewPolicy(root, map[string][]string{"test": {os.Args[0]}}, PolicyOptions{
		AllowArgs: map[string]bool{"test": true}, AllowLeadingDash: map[string]bool{"test": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flags.Resolve("test", []string{"--flag"}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRejectsExecutables(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"sh", "bash", "zsh", "fish", "dash", "env", "xargs", "sudo",
		"/bin/sh", "./test", "tools/test", "../test", "bad\x00name", "", filepath.Join(root, "command")} {
		if _, err := NewPolicy(root, map[string][]string{"test": {name}}); err == nil {
			t.Errorf("accepted executable %q", name)
		}
	}
	for _, argv := range [][]string{nil, {os.Args[0], "bad\x00arg"}} {
		if _, err := NewPolicy(root, map[string][]string{"test": argv}); err == nil {
			t.Errorf("accepted argv %q", argv)
		}
	}
	if _, err := NewPolicy(root, map[string][]string{"test": {os.Args[0]}}, PolicyOptions{MaxExtraArgs: -1}); err == nil {
		t.Fatal("accepted negative limit")
	}
}

func TestPolicyIgnoresAmbientPath(t *testing.T) {
	root := t.TempDir()
	// "true" lives in /usr/bin on every supported OS. "go" does not: CI runners install it outside TrustedPath.
	fake := filepath.Join(root, "true")
	if err := os.WriteFile(fake, []byte("not an executable format"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fake, 0o700); err != nil { // #nosec G302 -- owner execute permission is needed for LookPath, and this fixture is never executed.
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	p, err := NewPolicy(root, map[string][]string{"test": {"true"}})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := p.Resolve("test", nil)
	if err != nil || !filepath.IsAbs(argv[0]) || inside(root, argv[0]) {
		t.Fatalf("untrusted resolution: %q, %v", argv, err)
	}
	t.Setenv("PATH", ".")
	if _, err := p.Resolve("test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPolicy(root, map[string][]string{"test": {fake}}); err == nil {
		t.Fatal("accepted absolute workspace executable")
	}
}
