//go:build unix

package execx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER") == "1" {
		os.Exit(helperMain())
	}
	os.Exit(m.Run())
}

func helperMain() int {
	args := os.Args[1:]
	if len(args) == 0 {
		return 90
	}
	switch args[0] {
	case "exit":
		code, err := strconv.Atoi(args[1])
		if err != nil {
			return 91
		}
		return code
	case "order":
		for range 100 {
			if _, err := io.WriteString(os.Stdout, "stdout\n"); err != nil {
				return 92
			}
			if _, err := io.WriteString(os.Stderr, "stderr\n"); err != nil {
				return 92
			}
		}
	case "secret-output":
		if _, err := io.WriteString(os.Stdout, strings.Repeat("x", 246)+"private-command-secret"+strings.Repeat("x", 4000)+"TAIL"); err != nil {
			return 92
		}
	case "flood":
		chunk := bytes.Repeat([]byte("x"), 8192)
		for range 1280 {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 92
			}
		}
	case "env":
		if err := json.NewEncoder(os.Stdout).Encode(os.Environ()); err != nil {
			return 92
		}
	case "stdin":
		data, err := io.ReadAll(os.Stdin)
		if err != nil || len(data) != 0 {
			return 93
		}
	case "sleep":
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { // #nosec G703 -- helper argv is supplied only by this test, under t.TempDir.
			return 94
		}
		time.Sleep(time.Minute)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil { // #nosec G703 -- readiness path is created by the parent test.
			return 94
		}
		time.Sleep(time.Minute)
	case "leaf":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		ready := os.NewFile(3, "ready")
		if _, err := ready.Write([]byte{1}); err != nil {
			return 95
		}
		_ = ready.Close()
		<-term
	case "tree":
		return helperTree(args[1], false)
	case "pipe":
		return helperTree(args[1], true)
	default:
		return 96
	}
	return 0
}

func helperTree(pidFile string, exitEarly bool) int {
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	read, write, err := os.Pipe()
	if err != nil {
		return 97
	}
	child := exec.Command(os.Args[0], "leaf") // #nosec G204 G702 -- the test executable is the only child binary.
	child.Env = os.Environ()
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.ExtraFiles = []*os.File{write}
	if err := child.Start(); err != nil {
		return 98
	}
	_ = write.Close()
	var ready [1]byte
	if _, err := io.ReadFull(read, ready[:]); err != nil {
		return 99
	}
	_ = read.Close()
	if err := os.WriteFile(pidFile, fmt.Appendf(nil, "%d %d", os.Getpid(), child.Process.Pid), 0o600); err != nil { // #nosec G703 -- PID file is provided by the parent test in t.TempDir.
		return 100
	}
	if exitEarly {
		return 0
	}
	<-term
	// Reap the cooperative grandchild, then keep the leader alive to prove
	// the executor escalates to SIGKILL rather than only sending SIGTERM.
	if err := child.Wait(); err != nil {
		return 101
	}
	for {
		time.Sleep(time.Minute)
	}
}

func helperSpec(t *testing.T, mode string, args ...string) (*LocalExecutor, Spec) {
	t.Helper()
	root := t.TempDir()
	e, err := NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	e.GracePeriod, e.WaitDelay = 150*time.Millisecond, 100*time.Millisecond
	argv := append([]string{os.Args[0], mode}, args...)
	env := MinimalEnv(t.TempDir(), t.TempDir(), map[string]string{
		"GO_WANT_HELPER": "1", "GORACE": "atexit_sleep_ms=0",
	})
	return e, Spec{Argv: argv, Dir: root, Env: env, Timeout: 5 * time.Second}
}

func TestLocalExitAndOutput(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 1, 37} {
		e, spec := helperSpec(t, "exit", strconv.Itoa(code))
		result, err := e.Run(t.Context(), spec)
		if err != nil || result.ExitCode != code || result.TimedOut || result.Cancelled || result.Duration <= 0 {
			t.Fatalf("exit %d: %+v, %v", code, result, err)
		}
	}
	e, spec := helperSpec(t, "order")
	result, err := e.Run(t.Context(), spec)
	want := strings.Repeat("stdout\nstderr\n", 100)
	if err != nil || result.ExitCode != 0 || result.Output != want || result.TotalBytes != int64(len(want)) || result.Truncated {
		t.Fatalf("combined output: %+v, %v", result, err)
	}
	e, spec = helperSpec(t, "stdin")
	if result, err := e.Run(t.Context(), spec); err != nil || result.ExitCode != 0 {
		t.Fatalf("stdin must be EOF: %+v, %v", result, err)
	}
}

func TestLocalOutputFlood(t *testing.T) {
	t.Parallel()
	e, spec := helperSpec(t, "flood")
	result, err := e.Run(t.Context(), spec)
	if err != nil || result.ExitCode != 0 || result.TotalBytes != 10<<20 || !result.Truncated {
		t.Fatalf("flood: exit %d, bytes %d, truncated %v, error %v", result.ExitCode, result.TotalBytes, result.Truncated, err)
	}
	assertCut(t, result.Output, result.TotalBytes, DefaultOutputCap)
	if !strings.HasPrefix(result.Output, strings.Repeat("x", DefaultOutputCap/4)) ||
		!strings.HasSuffix(result.Output, strings.Repeat("x", 1024)) {
		t.Fatal("flood lost head/tail")
	}
}

func TestLocalEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "parent-anthropic-secret")
	t.Setenv("GITHUB_TOKEN", "parent-github-secret")
	t.Setenv("UNRELATED_PARENT", "never inherit this either")
	e, spec := helperSpec(t, "env")
	spec.Env = append(spec.Env, "OPENAI_API_KEY=explicit-secret", "AWS_PROFILE=explicit-secret", "GITHUB_ACTOR=explicit-secret")
	result, err := e.Run(t.Context(), spec)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("env: %+v, %v", result, err)
	}
	var env []string
	if err := json.Unmarshal([]byte(result.Output), &env); err != nil {
		t.Fatal(err)
	}
	want := envMap(spec.Env[:len(spec.Env)-3])
	if !reflect.DeepEqual(envMap(env), want) {
		t.Fatalf("child environment = %#v, want %#v", envMap(env), want)
	}
	for _, secret := range []string{"parent-anthropic-secret", "parent-github-secret", "explicit-secret", "UNRELATED_PARENT"} {
		if strings.Contains(result.Output, secret) {
			t.Fatalf("child inherited %q", secret)
		}
	}
}

func waitPIDFile(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) // #nosec G304 -- readiness files belong to this test's t.TempDir.
		if err == nil && len(data) > 0 {
			var pids []int
			for _, field := range strings.Fields(string(data)) {
				pid, err := strconv.Atoi(field)
				if err != nil || pid <= 0 {
					t.Fatalf("invalid PID file %q: %v", data, err)
				}
				pids = append(pids, pid)
			}
			return pids
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("helper failed to become ready")
	return nil
}

func assertGone(t *testing.T, pids []int) {
	t.Helper()
	for _, pid := range pids {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("PID %d still exists after group cleanup (kill 0: %v)", pid, err)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestLocalTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pids")
	e, spec := helperSpec(t, "tree", pidFile)
	spec.Timeout = time.Second
	result, err := e.Run(t.Context(), spec)
	if err != nil || !result.TimedOut || result.Cancelled || result.ExitCode != -1 || result.Duration > 3*time.Second {
		t.Fatalf("timeout: %+v, %v", result, err)
	}
	pids := waitPIDFile(t, pidFile)
	if len(pids) != 2 {
		t.Fatalf("expected child and grandchild, got %v", pids)
	}
	assertGone(t, pids)
}

func TestLocalCancellation(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	e, spec := helperSpec(t, "sleep", pidFile)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := e.Run(ctx, spec)
		done <- outcome{result, err}
	}()
	pids := waitPIDFile(t, pidFile)
	cancel()
	select {
	case got := <-done:
		if got.err != nil || !got.result.Cancelled || got.result.TimedOut {
			t.Fatalf("cancel: %+v, %v", got.result, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not return")
	}
	assertGone(t, pids)
	beforeStart, stop := context.WithCancel(t.Context())
	stop()
	result, err := e.Run(beforeStart, spec)
	if err != nil || !result.Cancelled || result.ExitCode != -1 || result.Output != "" {
		t.Fatalf("cancel before start: %+v, %v", result, err)
	}
}

func TestLocalDeadlineContext(t *testing.T) {
	t.Parallel()
	e, spec := helperSpec(t, "sleep", filepath.Join(t.TempDir(), "pid"))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	result, err := e.Run(ctx, spec)
	if err != nil || !result.TimedOut || result.Cancelled {
		t.Fatalf("context deadline: %+v, %v", result, err)
	}
}

func TestLocalKillsGroupAfterLeaderExits(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "leader")
	e, spec := helperSpec(t, "sleep", pidFile)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := e.Run(ctx, spec)
		if err == nil && !result.Cancelled {
			err = errors.New("expected cancellation")
		}
		done <- err
	}()
	leader := waitPIDFile(t, pidFile)[0]
	peerFile := filepath.Join(t.TempDir(), "peer")
	// This test owns the TERM-resistant group member, so it can reap it even
	// after the leader exits. A real grandchild would be reaped by the OS then;
	// some container PID 1 implementations do not reap orphans promptly.
	peer := exec.Command(os.Args[0], "stubborn", peerFile) // #nosec G204 G702 -- only the test binary and parent-created readiness file are passed.
	peer.Env = spec.Env
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader}
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = peer.Process.Kill()
		_ = peer.Wait()
	}()
	peerPID := waitPIDFile(t, peerFile)[0]
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("group cleanup did not return")
	}
	// Wait in a goroutine so an incorrect TERM-only implementation fails
	// promptly. Deferred cleanup still kills and reaps the helper on failure.
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Wait() }()
	select {
	case err := <-peerDone:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || peer.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("TERM-resistant peer was not killed with SIGKILL: %v", err)
		}
	case <-time.After(time.Second):
		_ = peer.Process.Kill()
		cleanupErr := <-peerDone
		t.Fatalf("group member survived leader exit (cleanup: %v)", cleanupErr)
	}
	assertGone(t, []int{leader, peerPID})
}

func TestLocalWaitDelayAndEarlyExitCleanup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pids")
	e, spec := helperSpec(t, "pipe", pidFile)
	result, err := e.Run(t.Context(), spec)
	if !errors.Is(err, exec.ErrWaitDelay) || result.Duration > 3*time.Second || result.TimedOut || result.Cancelled {
		t.Fatalf("inherited pipe: %+v, %v", result, err)
	}
	// The direct child is reaped by Wait. The grandchild has been terminated;
	// its new OS parent is responsible for reaping it after the leader's exit.
	pids := waitPIDFile(t, pidFile)
	assertGone(t, pids[:1])
}

func TestLocalRejectsInvalidSpecsAndStartFailures(t *testing.T) {
	t.Parallel()
	e, base := helperSpec(t, "exit", "0")
	outside := t.TempDir()
	link := filepath.Join(base.Dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Spec){
		"outside":             func(s *Spec) { s.Dir = outside },
		"prefix sibling":      func(s *Spec) { s.Dir += "-sibling" },
		"relative dir":        func(s *Spec) { s.Dir = "." },
		"symlink escape":      func(s *Spec) { s.Dir = link },
		"NUL dir":             func(s *Spec) { s.Dir += "\x00" },
		"relative executable": func(s *Spec) { s.Argv = []string{"./helper"} },
		"no argv":             func(s *Spec) { s.Argv = nil },
		"NUL argv":            func(s *Spec) { s.Argv = []string{os.Args[0], "bad\x00arg"} },
		"negative timeout":    func(s *Spec) { s.Timeout = -1 },
		"negative cap":        func(s *Spec) { s.OutputCap = -1 },
		"tiny cap":            func(s *Spec) { s.OutputCap = MinOutputCap - 1 },
		"bad env":             func(s *Spec) { s.Env = []string{"NO_EQUALS"} },
		"NUL env":             func(s *Spec) { s.Env = []string{"A=\x00"} },
		"binary missing":      func(s *Spec) { s.Argv = []string{filepath.Join(outside, "missing")} },
		"cannot start":        func(s *Spec) { s.Argv = []string{outside} },
	} {
		t.Run(name, func(t *testing.T) {
			spec := base
			change(&spec)
			result, err := e.Run(t.Context(), spec)
			if err == nil || result.ExitCode != -1 {
				t.Fatalf("invalid spec accepted: %+v, %v", result, err)
			}
		})
	}
	if _, err := NewLocalExecutor("."); err == nil {
		t.Fatal("accepted relative root")
	}
	if _, err := NewLocalExecutor(os.Args[0]); err == nil {
		t.Fatal("accepted file root")
	}
}

func TestPolicyShellOptInAndSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := NewPolicy(root, map[string][]string{"shell": {"sh"}}, PolicyOptions{AllowShells: map[string]bool{"sh": true}}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "innocent-name")
	if err := os.Symlink("/bin/sh", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPolicy(root, map[string][]string{"shell": {alias}}); err == nil {
		t.Fatal("accepted symlink to disallowed shell")
	}
	workspaceBinary := filepath.Join(root, "binary")
	if err := os.WriteFile(workspaceBinary, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspaceBinary, 0o700); err != nil { // #nosec G302 -- owner execute permission is needed for LookPath, and this fixture is never executed.
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspaceBinary, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPolicy(root, map[string][]string{"test": {alias}}); err == nil {
		t.Fatal("accepted symlink to workspace executable")
	}
}

func TestLocalRedactsStreamBeforeCap(t *testing.T) {
	t.Parallel()
	e, spec := helperSpec(t, "secret-output")
	spec.OutputCap = 1024
	spec.Redactor = secret.NewRedactor("private-command-secret")
	result, err := e.Run(t.Context(), spec)
	if err != nil || result.ExitCode != 0 || !result.Truncated || len(result.Output) > 1024 {
		t.Fatalf("%+v %v", result, err)
	}
	if strings.Contains(result.Output, "private") || !strings.Contains(result.Output, secret.Mask) || !strings.HasSuffix(result.Output, "TAIL") {
		t.Fatalf("redaction followed truncation: %q", result.Output)
	}
	if result.TotalBytes != int64(246+len("private-command-secret")+4000+4) {
		t.Fatal("raw byte accounting changed")
	}
}
