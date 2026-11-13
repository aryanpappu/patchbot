//go:build unix

package execx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Adversarial containment tests against a real rootless runtime. They run only
// with PATCHBOT_CONTAINER_TEST=1 and need:
//
//	PATCHBOT_CONTAINER_IMAGE    digest-pinned busybox/alpine image, already pulled
//	PATCHBOT_CONTAINER_RUNTIME  optional absolute podman/docker path (default: podman, then docker)
//	PATCHBOT_CONTAINER_USERNS   optional; defaults to keep-id for podman
//	XDG_RUNTIME_DIR             required by rootless podman
//
// Example (Mumbai, rootless podman 4.9):
//
//	PATCHBOT_CONTAINER_TEST=1 PATCHBOT_CONTAINER_IMAGE=docker.io/library/busybox@sha256:... \
//	  go test -race -count=1 -run Integration -v ./internal/execx/
func integrationExecutor(t *testing.T, mutate func(*ContainerConfig)) (*ContainerExecutor, string) {
	t.Helper()
	if os.Getenv("PATCHBOT_CONTAINER_TEST") != "1" {
		t.Skip("set PATCHBOT_CONTAINER_TEST=1 to run container integration tests")
	}
	image := os.Getenv("PATCHBOT_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set PATCHBOT_CONTAINER_IMAGE to a digest-pinned busybox or alpine image")
	}
	runtime := os.Getenv("PATCHBOT_CONTAINER_RUNTIME")
	if runtime == "" {
		for _, candidate := range []string{"/usr/bin/podman", "/usr/local/bin/podman", "/usr/bin/docker", "/usr/local/bin/docker"} {
			if _, err := os.Stat(candidate); err == nil {
				runtime = candidate
				break
			}
		}
	}
	if runtime == "" {
		t.Skip("no podman or docker binary found")
	}
	flavor := RuntimeDocker
	if strings.Contains(filepath.Base(runtime), "podman") {
		flavor = RuntimePodman
	}
	userns, ok := os.LookupEnv("PATCHBOT_CONTAINER_USERNS")
	if !ok && flavor == RuntimePodman {
		userns = "keep-id"
	}
	hostEnv := RuntimeHostEnv([]string{"/usr/local/bin", "/usr/bin", "/bin"}, os.LookupEnv)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, runtime, "image", "inspect", image) // #nosec G204 G702 -- operator-supplied test runtime.
	probe.Env = hostEnv
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("runtime unavailable or image not pulled (%v): %s", err, strings.TrimSpace(string(output)))
	}
	cfg := ContainerConfig{
		Runtime: runtime, Flavor: flavor, Image: image, Memory: "256m", CPUs: "1", PidsLimit: 64,
		TmpfsSize: "16m", Network: "none", User: "65532:65532", UserNS: userns, HostEnv: hostEnv,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	root := t.TempDir()
	e, err := NewContainerExecutor(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, e.root
}

func runIn(t *testing.T, e *ContainerExecutor, root string, timeout time.Duration, argv ...string) Result {
	t.Helper()
	result, err := e.Run(t.Context(), Spec{Argv: argv, Dir: root, Timeout: timeout, Env: []string{"HOME=/tmp", "TMPDIR=/tmp"}})
	if err != nil {
		t.Fatalf("Run(%q) error: %v (output %q)", argv, err, result.Output)
	}
	t.Logf("%q -> exit %d timed_out=%t\n%s", argv, result.ExitCode, result.TimedOut, result.Output)
	return result
}

func containerExists(t *testing.T, e *ContainerExecutor, name string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), e.cfg.Runtime, "ps", "-a", "-q", "--filter", "name=^"+name+"$") // #nosec G204 -- test runtime.
	cmd.Env = e.cfg.HostEnv
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	return strings.TrimSpace(string(output)) != ""
}

func TestIntegrationContainerIdentityAndWorkspace(t *testing.T) {
	e, root := integrationExecutor(t, nil)
	if r := runIn(t, e, root, time.Minute, "id", "-u"); r.ExitCode != 0 || strings.TrimSpace(r.Output) != "65532" {
		t.Fatalf("container user is not 65532: %+v", r)
	}
	if r := runIn(t, e, root, time.Minute, "sh", "-c", "echo ok > /work/out.txt"); r.ExitCode != 0 {
		t.Fatalf("workspace is not writable: %+v", r)
	}
	if data, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(data) != "ok\n" { // #nosec G304 -- test workspace.
		t.Fatalf("host did not see workspace write: %q, %v", data, err)
	}
	if r := runIn(t, e, root, time.Minute, "touch", "/etc/patchbot-escape"); r.ExitCode == 0 {
		t.Fatal("root filesystem is writable")
	}
	if r := runIn(t, e, root, time.Minute, "sh", "-c", "exit 7"); r.ExitCode != 7 {
		t.Fatalf("exit code not passed through: %+v", r)
	}
}

func TestIntegrationContainerCannotReadHostFiles(t *testing.T) {
	e, root := integrationExecutor(t, nil)
	secretFile := filepath.Join(t.TempDir(), "host-only.txt")
	if err := os.WriteFile(secretFile, []byte("host-only-content-4b1d"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runIn(t, e, root, time.Minute, "cat", secretFile)
	if r.ExitCode == 0 || strings.Contains(r.Output, "host-only-content-4b1d") {
		t.Fatalf("host file outside the mount was readable: %+v", r)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if r := runIn(t, e, root, time.Minute, "ls", home); r.ExitCode == 0 {
			t.Fatalf("host home directory visible: %+v", r)
		}
	}
}

func TestIntegrationContainerHasNoNetwork(t *testing.T) {
	e, root := integrationExecutor(t, nil)
	r := runIn(t, e, root, time.Minute, "wget", "-q", "-T", "5", "-O", "/dev/null", "http://1.1.1.1/")
	if r.ExitCode == 0 {
		t.Fatalf("network egress succeeded: %+v", r)
	}
	if r := runIn(t, e, root, time.Minute, "sh", "-c", "ls /sys/class/net"); strings.TrimSpace(r.Output) != "lo" {
		t.Errorf("unexpected interfaces: %q", r.Output)
	}
}

func TestIntegrationContainerForkBombIsCapped(t *testing.T) {
	e, root := integrationExecutor(t, func(c *ContainerConfig) { c.PidsLimit = 32 })
	r := runIn(t, e, root, time.Minute, "sh", "-c", "i=0; while [ $i -lt 100 ]; do sleep 20 & i=$((i+1)); done; wait")
	if !strings.Contains(strings.ToLower(r.Output), "fork") && !strings.Contains(r.Output, "Resource temporarily unavailable") {
		t.Fatalf("100 processes started under pids_limit 32: %+v", r)
	}
	name := "patchbot-forkbomb-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	e.newName = func() (string, error) { return name, nil }
	started := time.Now()
	result, err := e.Run(t.Context(), Spec{Argv: []string{"sh", "-c", "b(){ b | b & }; b; sleep 60"}, Dir: root, Timeout: 10 * time.Second})
	t.Logf("fork bomb: %+v err=%v after %s", result, err, time.Since(started))
	if time.Since(started) > 45*time.Second {
		t.Fatalf("fork bomb run did not end promptly: %s", time.Since(started))
	}
	if containerExists(t, e, name) {
		t.Fatal("fork bomb container survived")
	}
}

func TestIntegrationContainerTmpfsIsBounded(t *testing.T) {
	e, root := integrationExecutor(t, func(c *ContainerConfig) { c.TmpfsSize = "8m" })
	r := runIn(t, e, root, time.Minute, "dd", "if=/dev/zero", "of=/tmp/fill", "bs=1048576", "count=32")
	if r.ExitCode == 0 || !strings.Contains(r.Output, "No space left") {
		t.Fatalf("wrote 32 MiB into an 8 MiB tmpfs: %+v", r)
	}
}

func TestIntegrationContainerCancelKillsDescendants(t *testing.T) {
	e, root := integrationExecutor(t, nil)
	name := "patchbot-cancel-test"
	e.newName = func() (string, error) { return name, nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(3*time.Second, cancel)
	started := time.Now()
	result, err := e.Run(ctx, Spec{Argv: []string{"sh", "-c", "sleep 300 & sleep 300 & echo started; wait"}, Dir: root, Timeout: 5 * time.Minute})
	elapsed := time.Since(started)
	t.Logf("cancel: %+v err=%v after %s", result, err, elapsed)
	if !result.Cancelled || result.ExitCode != -1 {
		t.Fatalf("want cancelled result, got %+v", result)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
	if containerExists(t, e, name) {
		t.Fatal("container (and its sleeping descendants) survived cancellation")
	}
}
