//go:build unix

package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/testpath"
)

func TestDoctorContainerExecutor(t *testing.T) {
	bin := testpath.Dir(t)
	envLog := filepath.Join(t.TempDir(), "runtime-env")
	// #nosec G306 -- executable fixture needs its owner execute bit.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.50.1'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRuntime := func(rootless string, imagePresent bool) {
		t.Helper()
		image := "exit 1"
		if imagePresent {
			image = "echo sha256:abc"
		}
		script := fmt.Sprintf("#!/bin/sh\necho \"XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR GH=$GITHUB_TOKEN KEY=$ANTHROPIC_API_KEY\" > %q\ncase \"$1\" in\ninfo) echo '%s' ;;\nimage) %s ;;\nesac\n", envLog, rootless, image)
		// #nosec G306 -- executable fixture needs its owner execute bit.
		if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	image := "docker.io/library/golang:1.27@sha256:" + strings.Repeat("ab", 32)
	file := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n[executor]\nkind=\"container\"\nimage=%q\nuserns=\"keep-id\"\n[repos.\"owner/repo\"]\ncommands={test=[\"go\",\"test\",\"./...\"]}\n",
		bin, filepath.Join(t.TempDir(), "data"), image)
	// #nosec G101 -- dummy credentials for disclosure checks.
	vars := map[string]string{"ANTHROPIC_API_KEY": "fixture-provider-key", "GITHUB_TOKEN": "dummy-github-token", "XDG_RUNTIME_DIR": "/run/user/4242"}

	writeRuntime("true", true)
	code, out, errs := runApp(t, file, vars, "doctor")
	text := out + errs
	if code != 0 {
		t.Fatalf("doctor = %d\n%s", code, text)
	}
	for _, want := range []string{"container runtime", "podman runs rootless", "digest-pinned", "available locally", "not required on the host", "go (resolved in the container image)"} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, text)
		}
	}
	env, err := os.ReadFile(envLog) // #nosec G304 -- test-owned file.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "XDG_RUNTIME_DIR=/run/user/4242") || strings.Contains(string(env), "dummy-github-token") || strings.Contains(string(env), "fixture-provider-key") {
		t.Fatalf("runtime environment is wrong:\n%s", env)
	}

	writeRuntime("false", false)
	code, out, errs = runApp(t, file, vars, "doctor")
	text = out + errs
	if code != 0 || !strings.Contains(text, "is not rootless") || !strings.Contains(text, "pull it first") {
		t.Fatalf("rootful runtime should warn: %d\n%s", code, text)
	}

	if err := os.Remove(filepath.Join(bin, "podman")); err != nil {
		t.Fatal(err)
	}
	code, out, errs = runApp(t, file, vars, "doctor")
	if text = out + errs; code != 1 || !strings.Contains(text, "executor.runtime podman") {
		t.Fatalf("missing runtime should fail: %d\n%s", code, text)
	}
}

func TestDoctorContainerPerRepoImages(t *testing.T) {
	bin := testpath.Dir(t)
	// #nosec G306 -- executable fixture needs its owner execute bit.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'git version 2.50.1'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	php := "docker.io/library/php:8@sha256:" + strings.Repeat("cd", 32)
	golang := "docker.io/library/golang:1.27@sha256:" + strings.Repeat("ab", 32)
	// Only the php image is present locally.
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\ninfo) echo true ;;\nimage) case \"$*\" in *%s*) echo sha256:x ;; *) exit 1 ;; esac ;;\nesac\n", "php:8")
	// #nosec G306 -- executable fixture needs its owner execute bit.
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	file := fmt.Sprintf("[defaults]\napprovers=[\"owner\"]\n[limits]\ntrusted_path=[%q]\n[data]\ndir=%q\n[executor]\nkind=\"container\"\nimage=%q\n[repos.\"owner/go\"]\ncommands={test=[\"go\",\"test\"]}\n[repos.\"owner/php\"]\nimage=%q\ncommands={test=[\"phpunit\"]}\n",
		bin, filepath.Join(t.TempDir(), "data"), golang, php)
	// #nosec G101 -- dummy credentials for the fixture.
	_, out, errs := runApp(t, file, map[string]string{"GITHUB_TOKEN": "dummy-github-token", "XDG_RUNTIME_DIR": "/run/user/4242"}, "doctor")
	text := out + errs
	for _, want := range []string{
		`container image owner/go`, `digest-pinned ` + golang + ` (from executor.image)`,
		`container image owner/php`, `digest-pinned ` + php + ` (from repos."owner/php".image)`,
		"available locally", "pull it first: podman pull " + golang,
		"phpunit (resolved in the container image) from " + php,
		"go (resolved in the container image) from " + golang,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "pull it first: podman pull "+php) {
		t.Errorf("present php image reported missing:\n%s", text)
	}
}
