//go:build unix

package execx

import (
	"context"
	"errors"
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
		WorkspaceMaxBytes: DefaultWorkspaceMaxBytes,
	}
}

func TestContainerArgv(t *testing.T) {
	t.Parallel()
	common := func(runtime string) []string {
		return []string{runtime, "create", "--name", "patchbot-x", "--label", "io.patchbot.managed=true",
			"--pull", "never", "--network", "none", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256",
			"--memory", "2g", "--cpus", "2", "--user", "65532:65532"}
	}
	docker := func(c *ContainerConfig) {
		c.Runtime, c.Flavor = "/usr/local/bin/docker", RuntimeDocker
	}
	for _, test := range []struct {
		name    string
		mutate  func(*ContainerConfig)
		gitDir  string
		workdir string
		env     []string
		argv    []string
		want    []string
	}{
		{
			name: "podman", workdir: "/work", env: []string{"PATH=/bin", "CI=1"}, argv: []string{"go", "test", "./..."},
			want: append(common("/usr/bin/podman"), "--http-proxy=false", "--workdir", "/work", "-v", "/ws/repo:/work:rw",
				"-e", "PATH=/bin", "-e", "CI=1", testImage, "go", "test", "./..."),
		},
		{
			name: "podman keep-id with read-only git dir", mutate: func(c *ContainerConfig) { c.UserNS = "keep-id" },
			gitDir: "/ws/repo/.git", workdir: "/work/sub", argv: []string{"make"},
			want: append(common("/usr/bin/podman"), "--userns", "keep-id:uid=65532,gid=65532", "--http-proxy=false",
				"--workdir", "/work/sub", "-v", "/ws/repo:/work:rw", "-v", "/ws/repo/.git:/work/.git:ro", testImage, "make"),
		},
		{
			name: "docker literal args after image", mutate: docker, gitDir: "/ws/repo/.git",
			workdir: "/work", argv: []string{"go", "--privileged", "$(id)"},
			want: append(common("/usr/local/bin/docker"), "--workdir", "/work", "-v", "/ws/repo:/work:rw",
				"-v", "/ws/repo/.git:/work/.git:ro", testImage, "go", "--privileged", "$(id)"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := baseContainerConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			got := containerArgv(cfg, "patchbot-x", "/ws/repo", test.gitDir, test.workdir, test.env, test.argv)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, test.want)
			}
		})
	}
}

func TestContainerEnvRemapsPrivateDirsAndBlanksProxies(t *testing.T) {
	t.Parallel()
	env := []string{"CI=1", "GOCACHE=/host/run/.command-1/home/go-cache", "HOME=/host/run/.command-1/home",
		"PATH=/usr/bin:/bin", "TMPDIR=/host/run/.command-1/tmp", "OTHER=/host/run/.command-1/homeless", "HTTPS_PROXY=http://proxy:3128"}
	got := containerEnv(env, []string{"/usr/local/go/bin", "/bin"})
	want := []string{"PATH=/usr/local/go/bin:/bin", "CI=1", "GOCACHE=/tmp/go-cache", "HOME=/tmp", "TMPDIR=/tmp", "OTHER=/host/run/.command-1/homeless"}
	for _, name := range proxyNames {
		want = append(want, name+"=")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("containerEnv = %q, want %q", got, want)
	}
	for _, name := range []string{"HTTP_PROXY", "https_proxy", "NO_PROXY", "all_proxy", "FTP_PROXY"} {
		if !slices.Contains(got, name+"=") {
			t.Errorf("missing explicit empty %s", name)
		}
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
		{"workspace max", func(c *ContainerConfig) { c.WorkspaceMaxBytes = 0 }, false},
		{"relative runtime", func(c *ContainerConfig) { c.Runtime = "podman" }, false},
		{"flavor", func(c *ContainerConfig) { c.Flavor = "lxc" }, false},
		{"keep-id docker", func(c *ContainerConfig) {
			c.Flavor, c.UserNS, c.HostEnv = RuntimeDocker, "keep-id", []string{"DOCKER_CONFIG=/data/docker"}
		}, false},
		{"docker without private config", func(c *ContainerConfig) { c.Flavor = RuntimeDocker }, false},
		{"docker with private config", func(c *ContainerConfig) { c.Flavor, c.HostEnv = RuntimeDocker, []string{"DOCKER_CONFIG=/data/docker"} }, true},
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
	vars := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1001", "GITHUB_TOKEN": "ghp_secret", "DOCKER_HOST": "unix:///run/user/1001/docker.sock",
		"ANTHROPIC_API_KEY": "k", "DOCKER_CONFIG": "/home/u/.docker", "DOCKER_CONTEXT": "desktop", "HTTPS_PROXY": "http://p"}
	got := RuntimeHostEnv([]string{"/usr/bin", "/bin"}, func(name string) (string, bool) { v, ok := vars[name]; return v, ok })
	want := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/1001", "DOCKER_HOST=unix:///run/user/1001/docker.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeHostEnv = %q, want %q", got, want)
	}
}

func TestPrepareDockerConfig(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "runtime", "docker-config")
	if err := PrepareDockerConfig(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("docker config dir = %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"proxies":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDockerConfig(dir); err == nil {
		t.Fatal("accepted a docker config dir with config.json")
	}
	if err := PrepareDockerConfig("relative"); err == nil {
		t.Fatal("accepted a relative docker config dir")
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

func TestCheckWorkspaceSize(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "f"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(outside, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkspaceSize(root, 1<<16, 100); err != nil {
		t.Fatalf("under limit (symlink target must not count): %v", err)
	}
	if err := checkWorkspaceSize(root, 999, 100); !errors.Is(err, ErrWorkspaceLimit) {
		t.Fatalf("over byte limit: %v", err)
	}
	if err := checkWorkspaceSize(root, 1<<16, 3); !errors.Is(err, ErrWorkspaceLimit) {
		t.Fatalf("over entry limit: %v", err)
	}
	hidden := filepath.Join(root, "a")
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(hidden, 0o700) }() // #nosec G302 -- restore owner access for cleanup.
	if err := checkWorkspaceSize(root, 1<<16, 100); !errors.Is(err, ErrWorkspaceLimit) {
		t.Fatalf("unreadable directory must fail closed: %v", err)
	}
}

// fakeRuntimeMain stands in for podman/docker: the test binary re-executes
// itself. Containers are files in FAKE_RUNTIME_STATE named after the
// container; their content is the command after the image.
func fakeRuntimeMain() int {
	args := os.Args[1:]
	state := os.Getenv("FAKE_RUNTIME_STATE")
	f, err := os.OpenFile(filepath.Join(state, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 G703 -- test-owned log file.
	if err != nil {
		return 98
	}
	cwd, _ := os.Getwd()
	_, _ = f.WriteString(cwd + "\x1f" + strings.Join(args, "\x1f") + "\n")
	_ = f.Close()
	if len(args) == 0 {
		return 97
	}
	container := func(name string) string { return filepath.Join(state, "c-"+name) }
	last := args[len(args)-1]
	switch args[0] {
	case "create":
		name := args[slices.Index(args, "--name")+1]
		i := slices.Index(args, testImage)
		command := args[i+1:]
		host, _, _ := strings.Cut(args[slices.Index(args, "-v")+1], ":")
		switch command[0] {
		case "create-fail":
			_, _ = os.Stdout.WriteString("Error: image not known\n")
			return runtimeErrorExit
		case "create-late":
			// Created, but the CLI hangs (as if cancelled mid-create).
			_ = os.WriteFile(container(name), []byte("sleep"), 0o600)
			time.Sleep(time.Minute)
		}
		_ = os.WriteFile(container(name), []byte(host+"\x1f"+strings.Join(command, "\x1f")), 0o600) // #nosec G703 -- fake runtime acting on its test workspace.
	case "start":
		data, err := os.ReadFile(container(last)) // #nosec G304 -- test state file.
		if err != nil {
			return runtimeErrorExit
		}
		fields := strings.Split(string(data), "\x1f")
		host, command := fields[0], fields[1:]
		switch command[0] {
		case "exit":
			code, _ := strconv.Atoi(command[1])
			return code
		case "echo":
			_, _ = os.Stdout.WriteString(strings.Join(command[1:], " ") + "\n")
		case "fill":
			n, _ := strconv.Atoi(command[1])
			_ = os.WriteFile(filepath.Join(host, "fill.bin"), make([]byte, n), 0o600) // #nosec G703 -- fake runtime acting on its test workspace.
		case "lockout":
			_ = os.Chmod(host, 0) // #nosec G703 -- fake runtime acting on its test workspace.
		case "sleep", "immortal":
			for range 600 {
				if _, err := os.Stat(container(last) + ".killed"); err == nil {
					return 137
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	case "kill":
		_ = os.WriteFile(container(last)+".killed", nil, 0o600)
	case "rm":
		if data, err := os.ReadFile(container(last)); err == nil && strings.HasSuffix(string(data), "immortal") { // #nosec G304 -- test state file.
			return 1
		}
		_ = os.Remove(container(last))
	case "ps":
		entries, _ := os.ReadDir(state)
		filter := args[slices.Index(args, "--filter")+1]
		for _, entry := range entries {
			name, ok := strings.CutPrefix(entry.Name(), "c-")
			if !ok || strings.HasSuffix(name, ".killed") {
				continue
			}
			if filter == "label="+containerLabel || filter == "name=^"+name+"$" {
				_, _ = os.Stdout.WriteString(name + "\n")
			}
		}
	}
	return 0
}

func newFakeContainer(t *testing.T) (*ContainerExecutor, string, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"sub", ".git"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	state := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := baseContainerConfig()
	cfg.Runtime = self
	cfg.WorkspaceMaxBytes = 4096
	cfg.HostEnv = []string{"GO_WANT_FAKE_RUNTIME=1", "FAKE_RUNTIME_STATE=" + state, "GORACE=atexit_sleep_ms=0"}
	e, err := NewContainerExecutor(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.newName = func() (string, error) { return "patchbot-fixed", nil }
	e.cli.GracePeriod = 100 * time.Millisecond
	e.ReconcileTimeout, e.PollInterval = 3*time.Second, 20*time.Millisecond
	return e, e.root, state
}

type runtimeCall struct {
	cwd  string
	args []string
}

func runtimeCalls(t *testing.T, state string) []runtimeCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "calls.log")) // #nosec G304 -- test-owned log file.
	if err != nil {
		t.Fatal(err)
	}
	var calls []runtimeCall
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "\x1f")
		calls = append(calls, runtimeCall{fields[0], fields[1:]})
	}
	return calls
}

func verbs(calls []runtimeCall) []string {
	var out []string
	for _, call := range calls {
		out = append(out, call.args[0])
	}
	return out
}

// assertContainerGone checks that the container does not exist and that the last
// runtime call confirmed it (host git may only run after that).
func assertContainerGone(t *testing.T, state string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(state, "c-patchbot-fixed")); err == nil {
		t.Fatal("container still exists after Run returned")
	}
	calls := runtimeCalls(t, state)
	if last := calls[len(calls)-1].args; last[0] != "ps" {
		t.Fatalf("last runtime call = %q; want a ps confirmation", last)
	}
	for _, call := range calls {
		if call.cwd != cliDir {
			t.Fatalf("runtime ran in %q, want %q: %q", call.cwd, cliDir, call.args)
		}
	}
}

func TestContainerExecutorRunWithFakeRuntime(t *testing.T) {
	t.Parallel()
	t.Run("exit code workdir and env", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		env := []string{"HOME=/private/x/home", "TMPDIR=/private/x/tmp", "GOCACHE=/private/x/home/go-cache", "PATH=/usr/bin", "GITHUB_TOKEN=leak"}
		result, err := e.Run(t.Context(), Spec{Argv: []string{"exit", "7"}, Dir: filepath.Join(root, "sub"), Env: env})
		if err != nil || result.ExitCode != 7 || result.TimedOut || result.Cancelled {
			t.Fatalf("Run = %+v, %v", result, err)
		}
		calls := runtimeCalls(t, state)
		if got := verbs(calls); !reflect.DeepEqual(got, []string{"create", "start", "rm", "ps"}) {
			t.Fatalf("runtime calls = %q", got)
		}
		joined := strings.Join(calls[0].args, " ")
		for _, want := range []string{"--workdir /work/sub", "-v " + root + ":/work:rw", "-v " + root + "/.git:/work/.git:ro",
			"-e HOME=/tmp", "-e TMPDIR=/tmp", "-e GOCACHE=/tmp/go-cache", "-e PATH=/usr/local/go/bin:", "-e HTTPS_PROXY= "} {
			if !strings.Contains(joined, want) {
				t.Errorf("create argv lacks %q: %s", want, joined)
			}
		}
		if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "leak") {
			t.Fatalf("token reached the container: %s", joined)
		}
		if got := calls[1].args; !reflect.DeepEqual(got, []string{"start", "--attach", "patchbot-fixed"}) {
			t.Fatalf("start argv = %q", got)
		}
		assertContainerGone(t, state)
	})
	t.Run("output", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		result, err := e.Run(t.Context(), Spec{Argv: []string{"echo", "hello"}, Dir: root})
		if err != nil || result.ExitCode != 0 || result.Output != "hello\n" {
			t.Fatalf("Run = %+v, %v", result, err)
		}
		assertContainerGone(t, state)
	})
	t.Run("create failure is an error and is reconciled", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		result, err := e.Run(t.Context(), Spec{Argv: []string{"create-fail"}, Dir: root})
		if err == nil || result.ExitCode != -1 || !strings.Contains(result.Output, "image not known") {
			t.Fatalf("Run = %+v, %v; want create error", result, err)
		}
		if slices.Contains(verbs(runtimeCalls(t, state)), "start") {
			t.Fatal("started a container whose create failed")
		}
		assertContainerGone(t, state)
	})
	t.Run("start runtime error", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		result, err := e.Run(t.Context(), Spec{Argv: []string{"exit", "125"}, Dir: root})
		if err == nil || result.ExitCode != -1 {
			t.Fatalf("Run = %+v, %v; want runtime error", result, err)
		}
		assertContainerGone(t, state)
	})
	for _, test := range []struct {
		name    string
		argv    string
		timeout time.Duration
		cancel  bool
	}{
		{"timeout kills and removes", "sleep", 300 * time.Millisecond, false},
		{"cancel kills and removes", "sleep", time.Minute, true},
		{"cancel during create removes a late container", "create-late", time.Minute, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			e, root, state := newFakeContainer(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			result, err := e.Run(ctx, Spec{Argv: []string{test.argv}, Dir: root, Timeout: test.timeout})
			if err != nil || result.ExitCode != -1 || result.TimedOut == test.cancel || result.Cancelled != test.cancel {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			calls := verbs(runtimeCalls(t, state))
			if test.argv == "sleep" && !slices.Contains(calls, "kill") {
				t.Fatalf("runtime calls = %q; want kill by name", calls)
			}
			if !slices.Contains(calls, "rm") {
				t.Fatalf("runtime calls = %q; want rm -f by name", calls)
			}
			assertContainerGone(t, state)
		})
	}
	t.Run("container that cannot be removed fails loudly", func(t *testing.T) {
		t.Parallel()
		e, root, _ := newFakeContainer(t)
		e.ReconcileTimeout = 300 * time.Millisecond
		_, err := e.Run(t.Context(), Spec{Argv: []string{"immortal"}, Dir: root, Timeout: 200 * time.Millisecond})
		if !errors.Is(err, ErrContainerNotRemoved) {
			t.Fatalf("Run error = %v; want ErrContainerNotRemoved", err)
		}
	})
	t.Run("workspace lockout cannot block cleanup", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		defer func() { _ = os.Chmod(root, 0o700) }() // #nosec G302 -- restore owner access for cleanup.
		_, err := e.Run(t.Context(), Spec{Argv: []string{"lockout"}, Dir: root})
		if !errors.Is(err, ErrWorkspaceLimit) {
			t.Fatalf("Run error = %v; want unmeasurable workspace", err)
		}
		assertContainerGone(t, state)
	})
	t.Run("workspace growth beyond limit fails", func(t *testing.T) {
		t.Parallel()
		e, root, state := newFakeContainer(t)
		if _, err := e.Run(t.Context(), Spec{Argv: []string{"fill", "100"}, Dir: root}); err != nil {
			t.Fatalf("small write: %v", err)
		}
		result, err := e.Run(t.Context(), Spec{Argv: []string{"fill", "8192"}, Dir: root})
		if !errors.Is(err, ErrWorkspaceLimit) || result.ExitCode != 0 {
			t.Fatalf("Run = %+v, %v; want workspace limit error", result, err)
		}
		assertContainerGone(t, state)
	})
	t.Run("rejects escapes bad argv and gitlinks", func(t *testing.T) {
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
		if err := os.Remove(filepath.Join(root, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Run(t.Context(), Spec{Argv: []string{"echo"}, Dir: root}); err == nil {
			t.Error("accepted a gitlink .git file")
		}
	})
}

func TestSweepContainers(t *testing.T) {
	t.Parallel()
	e, _, state := newFakeContainer(t)
	for _, name := range []string{"patchbot-old1", "patchbot-old2"} {
		if err := os.WriteFile(filepath.Join(state, "c-"+name), []byte("x\x1fsleep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The fake rm removes only its final argument; real runtimes remove all.
	n, err := SweepContainers(t.Context(), e.cfg)
	if err != nil || n != 2 {
		t.Fatalf("SweepContainers = %d, %v", n, err)
	}
	calls := runtimeCalls(t, state)
	if got := calls[len(calls)-1].args; !reflect.DeepEqual(got, []string{"rm", "-f", "patchbot-old1", "patchbot-old2"}) {
		t.Fatalf("sweep rm argv = %q", got)
	}
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
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewContainerExecutor(root, cfg); err == nil {
		t.Fatal("accepted a symlinked .git")
	}
	cfg.Flavor, cfg.HostEnv = RuntimeDocker, []string{"DOCKER_CONFIG=" + filepath.Join(t.TempDir(), "docker")}
	if _, err := NewContainerExecutor(t.TempDir(), cfg); err != nil {
		t.Fatalf("docker with private config: %v", err)
	}
}
