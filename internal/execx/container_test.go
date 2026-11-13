//go:build unix

package execx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testImage = "registry.example/golang:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func baseContainerConfig() ContainerConfig {
	return ContainerConfig{
		Runtime: "/usr/bin/podman", Flavor: RuntimePodman, Image: testImage, Memory: "2g", CPUs: "2",
		PidsLimit: 256, TmpfsSize: "512m", Network: "none", User: "65532:65532", Path: DefaultContainerPath,
	}
}

func TestContainerArgv(t *testing.T) {
	t.Parallel()
	common := func(runtime string) []string {
		return []string{runtime, "run", "--rm", "--name", "patchbot-x", "--label", "io.patchbot.managed=true",
			"--pull", "never", "--network", "none", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256",
			"--memory", "2g", "--cpus", "2", "--user", "65532:65532"}
	}
	for _, test := range []struct {
		name    string
		mutate  func(*ContainerConfig)
		workdir string
		env     []string
		argv    []string
		want    []string
	}{
		{
			name: "podman", workdir: "/work", env: []string{"PATH=/bin", "CI=1"}, argv: []string{"go", "test", "./..."},
			want: append(common("/usr/bin/podman"), "--workdir", "/work", "-v", "/ws/repo:/work:rw",
				"-e", "PATH=/bin", "-e", "CI=1", testImage, "go", "test", "./..."),
		},
		{
			name: "podman keep-id", mutate: func(c *ContainerConfig) { c.UserNS = "keep-id" }, workdir: "/work/sub",
			argv: []string{"make"},
			want: append(common("/usr/bin/podman"), "--userns", "keep-id:uid=65532,gid=65532", "--workdir", "/work/sub",
				"-v", "/ws/repo:/work:rw", testImage, "make"),
		},
		{
			name: "docker, literal args after image", mutate: func(c *ContainerConfig) { c.Runtime, c.Flavor = "/usr/local/bin/docker", RuntimeDocker },
			workdir: "/work", argv: []string{"go", "--privileged", "$(id)"},
			want: append(common("/usr/local/bin/docker"), "--workdir", "/work", "-v", "/ws/repo:/work:rw",
				testImage, "go", "--privileged", "$(id)"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := baseContainerConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			got := containerArgv(cfg, "patchbot-x", "/ws/repo", test.workdir, test.env, test.argv)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, test.want)
			}
		})
	}
}

func TestContainerEnvRemapsPrivateDirs(t *testing.T) {
	t.Parallel()
	env := []string{"CI=1", "GOCACHE=/host/run/.command-1/home/go-cache", "HOME=/host/run/.command-1/home",
		"PATH=/usr/bin:/bin", "TMPDIR=/host/run/.command-1/tmp", "OTHER=/host/run/.command-1/homeless"}
	got := containerEnv(env, []string{"/usr/local/go/bin", "/bin"})
	want := []string{"PATH=/usr/local/go/bin:/bin", "CI=1", "GOCACHE=/tmp/go-cache", "HOME=/tmp", "TMPDIR=/tmp", "OTHER=/host/run/.command-1/homeless"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containerEnv = %q, want %q", got, want)
	}
}

func TestContainerConfigValidate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*ContainerConfig)
		ok     bool
	}{
		{"valid", func(*ContainerConfig) {}, true},
		{"docker hub short name", func(c *ContainerConfig) { c.Image = "golang@sha256:" + strings.Repeat("a", 64) }, true},
		{"registry port", func(c *ContainerConfig) { c.Image = "localhost:5000/team/img:v1@sha256:" + strings.Repeat("b", 64) }, true},
		{"tag only", func(c *ContainerConfig) { c.Image = "golang:1.27" }, false},
		{"short digest", func(c *ContainerConfig) { c.Image = "golang@sha256:abc" }, false},
		{"leading dash image", func(c *ContainerConfig) { c.Image = "-v@sha256:" + strings.Repeat("a", 64) }, false},
		{"root user", func(c *ContainerConfig) { c.User = "0:0" }, false},
		{"root gid", func(c *ContainerConfig) { c.User = "1000:0" }, false},
		{"named user", func(c *ContainerConfig) { c.User = "nobody" }, false},
		{"network host", func(c *ContainerConfig) { c.Network = "host" }, false},
		{"bridge", func(c *ContainerConfig) { c.Network = "bridge" }, false},
		{"memory", func(c *ContainerConfig) { c.Memory = "lots" }, false},
		{"tmpfs flag smuggling", func(c *ContainerConfig) { c.TmpfsSize = "1m,exec" }, false},
		{"cpus zero", func(c *ContainerConfig) { c.CPUs = "0" }, false},
		{"cpus fraction", func(c *ContainerConfig) { c.CPUs = "1.5" }, true},
		{"pids", func(c *ContainerConfig) { c.PidsLimit = 0 }, false},
		{"relative runtime", func(c *ContainerConfig) { c.Runtime = "podman" }, false},
		{"flavor", func(c *ContainerConfig) { c.Flavor = "lxc" }, false},
		{"keep-id docker", func(c *ContainerConfig) { c.Flavor, c.UserNS = RuntimeDocker, "keep-id" }, false},
		{"keep-id podman", func(c *ContainerConfig) { c.UserNS = "keep-id" }, true},
		{"userns host", func(c *ContainerConfig) { c.UserNS = "host" }, false},
		{"relative path", func(c *ContainerConfig) { c.Path = []string{"bin"} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := baseContainerConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); (err == nil) != test.ok {
				t.Fatalf("Validate() = %v, want ok=%t", err, test.ok)
			}
		})
	}
}

func TestRuntimeHostEnvPassesOnlyRuntimeVariables(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1001", "GITHUB_TOKEN": "ghp_secret", "DOCKER_HOST": "unix:///run/user/1001/docker.sock", "ANTHROPIC_API_KEY": "k"}
	got := RuntimeHostEnv([]string{"/usr/bin", "/bin"}, func(name string) (string, bool) { v, ok := vars[name]; return v, ok })
	want := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/1001", "DOCKER_HOST=unix:///run/user/1001/docker.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeHostEnv = %q, want %q", got, want)
	}
}

func TestPolicyInContainer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p, err := NewPolicy(root, map[string][]string{"test": {"go", "test", "./..."}, "abs": {"/usr/local/go/bin/go", "vet"}},
		PolicyOptions{InContainer: true, TrustedPath: DefaultContainerPath})
	if err != nil {
		t.Fatal(err)
	}
	if argv, err := p.Resolve("test", nil); err != nil || argv[0] != "go" {
		t.Fatalf("Resolve(test) = %q, %v; want unresolved bare name", argv, err)
	}
	if argv, err := p.Resolve("abs", nil); err != nil || argv[0] != "/usr/local/go/bin/go" {
		t.Fatalf("Resolve(abs) = %q, %v", argv, err)
	}
	if env := p.MinimalEnv("/h", "/t", nil); !slices.Contains(env, "PATH="+strings.Join(DefaultContainerPath, ":")) {
		t.Fatalf("MinimalEnv PATH = %q", env)
	}
	for _, argv := range [][]string{{"sh", "-c", "id"}, {"bash"}, {"/bin/sh"}, {"bin/go"}, {"/usr/../bin/sh"}} {
		if _, err := NewPolicy(root, map[string][]string{"x": argv}, PolicyOptions{InContainer: true, TrustedPath: DefaultContainerPath}); err == nil {
			t.Errorf("accepted container command %q", argv)
		}
	}
	if _, err := NewPolicy(root, map[string][]string{"x": {"go"}}, PolicyOptions{InContainer: true, TrustedPath: []string{"relative"}}); err == nil {
		t.Error("accepted relative container PATH")
	}
}

// fakeRuntimeMain stands in for podman/docker: the test binary re-executes
// itself. It logs every invocation and interprets the command after the image.
func fakeRuntimeMain() int {
	args := os.Args[1:]
	logPath := os.Getenv("FAKE_RUNTIME_LOG")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 G703 -- test-owned log file.
	if err != nil {
		return 98
	}
	_, _ = f.WriteString(strings.Join(args, "\x1f") + "\n")
	_ = f.Close()
	if len(args) == 0 || args[0] != "run" {
		return 0
	}
	i := slices.Index(args, testImage)
	if i < 0 || i+1 >= len(args) {
		return 97
	}
	switch command := args[i+1:]; command[0] {
	case "exit":
		code, _ := strconv.Atoi(command[1])
		return code
	case "echo":
		_, _ = os.Stdout.WriteString(strings.Join(command[1:], " ") + "\n")
	case "sleep":
		time.Sleep(time.Minute)
	}
	return 0
}

func newFakeContainer(t *testing.T) (*ContainerExecutor, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseContainerConfig()
	cfg.Runtime = self
	cfg.HostEnv = []string{"GO_WANT_FAKE_RUNTIME=1", "FAKE_RUNTIME_LOG=" + logPath, "GORACE=atexit_sleep_ms=0"}
	e, err := NewContainerExecutor(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.newName = func() (string, error) { return "patchbot-fixed", nil }
	e.local.GracePeriod = 100 * time.Millisecond
	return e, e.root, logPath
}

func runtimeCalls(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath) // #nosec G304 -- test-owned log file.
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		calls = append(calls, strings.Split(line, "\x1f"))
	}
	return calls
}

func TestContainerExecutorRunWithFakeRuntime(t *testing.T) {
	t.Parallel()
	t.Run("exit code workdir and env", func(t *testing.T) {
		t.Parallel()
		e, root, logPath := newFakeContainer(t)
		env := []string{"HOME=/private/x/home", "TMPDIR=/private/x/tmp", "GOCACHE=/private/x/home/go-cache", "PATH=/usr/bin", "GITHUB_TOKEN=leak"}
		result, err := e.Run(t.Context(), Spec{Argv: []string{"exit", "7"}, Dir: filepath.Join(root, "sub"), Env: env})
		if err != nil || result.ExitCode != 7 || result.TimedOut || result.Cancelled {
			t.Fatalf("Run = %+v, %v", result, err)
		}
		calls := runtimeCalls(t, logPath)
		if len(calls) != 1 {
			t.Fatalf("want only run (--rm cleans up), got %q", calls)
		}
		run := calls[0]
		joined := strings.Join(run, " ")
		for _, want := range []string{"--workdir /work/sub", "-v " + root + ":/work:rw", "-e HOME=/tmp", "-e TMPDIR=/tmp", "-e GOCACHE=/tmp/go-cache", "-e PATH=/usr/local/go/bin:"} {
			if !strings.Contains(joined, want) {
				t.Errorf("run argv lacks %q: %s", want, joined)
			}
		}
		if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "leak") {
			t.Fatalf("token reached the container: %s", joined)
		}
	})
	t.Run("output", func(t *testing.T) {
		t.Parallel()
		e, root, _ := newFakeContainer(t)
		result, err := e.Run(t.Context(), Spec{Argv: []string{"echo", "hello"}, Dir: root})
		if err != nil || result.ExitCode != 0 || result.Output != "hello\n" {
			t.Fatalf("Run = %+v, %v", result, err)
		}
	})
	t.Run("runtime error is an error and is cleaned up", func(t *testing.T) {
		t.Parallel()
		e, root, logPath := newFakeContainer(t)
		result, err := e.Run(t.Context(), Spec{Argv: []string{"exit", "125"}, Dir: root})
		if err == nil || result.ExitCode != -1 {
			t.Fatalf("Run = %+v, %v; want runtime error", result, err)
		}
		if calls := runtimeCalls(t, logPath); len(calls) != 2 || calls[1][0] != "rm" || calls[1][2] != "patchbot-fixed" {
			t.Fatalf("calls = %q", calls)
		}
	})
	for _, test := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
	}{{"timeout kills and removes", 300 * time.Millisecond, false}, {"cancel kills and removes", time.Minute, true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			e, root, logPath := newFakeContainer(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			result, err := e.Run(ctx, Spec{Argv: []string{"sleep"}, Dir: root, Timeout: test.timeout})
			if err != nil || result.ExitCode != -1 || result.TimedOut == test.cancel || result.Cancelled != test.cancel {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			var verbs []string
			for _, call := range runtimeCalls(t, logPath) {
				verbs = append(verbs, strings.Join(call[:len(call)-1], " ")+" "+call[len(call)-1])
			}
			if !slices.Contains(verbs, "kill patchbot-fixed") || !slices.Contains(verbs, "rm -f patchbot-fixed") {
				t.Fatalf("runtime calls = %q; want kill and rm -f by name", verbs)
			}
		})
	}
	t.Run("rejects escapes and bad argv", func(t *testing.T) {
		t.Parallel()
		e, root, _ := newFakeContainer(t)
		outside := t.TempDir()
		link := filepath.Join(root, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		for _, spec := range []Spec{
			{Argv: []string{"echo"}, Dir: outside},
			{Argv: []string{"echo"}, Dir: link},
			{Argv: []string{"echo"}, Dir: "relative"},
			{Argv: []string{"bin/echo"}, Dir: root},
			{Argv: nil, Dir: root},
			{Argv: []string{"echo", "a\x00b"}, Dir: root},
			{Argv: []string{"echo"}, Dir: root, Timeout: -1},
		} {
			if _, err := e.Run(t.Context(), spec); err == nil {
				t.Errorf("accepted %+v", spec)
			}
		}
	})
}

func TestNewContainerExecutorRejectsInvalid(t *testing.T) {
	t.Parallel()
	cfg := baseContainerConfig()
	cfg.Image = "golang:latest"
	if _, err := NewContainerExecutor(t.TempDir(), cfg); err == nil {
		t.Fatal("accepted an unpinned image")
	}
	cfg = baseContainerConfig()
	dir := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewContainerExecutor(dir, cfg); err == nil {
		t.Fatal("accepted a workspace path that breaks -v syntax")
	}
}
