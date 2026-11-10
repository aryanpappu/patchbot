package gitx

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fakeToken = "ghp_m10_fake_credential_never_expose_123456789" // #nosec G101 -- deliberately fake regression-test credential.

type fixture struct {
	bare, seed, home, first, latest string
}

func gitFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	f := fixture{bare: filepath.Join(dir, "remote.git"), seed: filepath.Join(dir, "seed"), home: filepath.Join(dir, "home")}
	mustMkdir(t, f.home)
	f.git(t, dir, "init", "--bare", "--initial-branch=main", f.bare)
	f.git(t, dir, "init", "--initial-branch=main", f.seed)
	write(t, filepath.Join(f.seed, "original file.txt"), "one\ntwo\nthree\n")
	f.git(t, f.seed, "add", "--", "original file.txt")
	f.git(t, f.seed, "commit", "--no-verify", "-m", "first")
	f.first = strings.TrimSpace(f.git(t, f.seed, "rev-parse", "HEAD"))
	write(t, filepath.Join(f.seed, "second.txt"), "second\n")
	f.git(t, f.seed, "add", "--", "second.txt")
	f.git(t, f.seed, "commit", "--no-verify", "-m", "second")
	f.latest = strings.TrimSpace(f.git(t, f.seed, "rev-parse", "HEAD"))
	f.git(t, f.seed, "push", f.bare, "HEAD:refs/heads/main")
	return f
}

func (f fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper="}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...) // #nosec G204 -- test fixture only, literal git invocations and local temp paths.
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + trustedPath, "HOME=" + f.home, "TMPDIR=" + f.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("fixture git %v: %v\n%s", args, err, exitErr.Stderr)
		}
		t.Fatalf("fixture git %v: %v", args, err)
	}
	return string(out)
}

func (f fixture) clone(t *testing.T, depth int) *Repo {
	t.Helper()
	r, err := Clone(context.Background(), CloneOptions{Dir: filepath.Join(t.TempDir(), "checkout"), Host: "github.com",
		Branch: "main", Depth: depth, Token: fakeToken, TestOnlyLocalRemote: f.bare})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test-owned fixture paths only.
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCloneCommitPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	r := f.clone(t, 1)
	sha, err := r.HeadSHA(ctx)
	if err != nil || sha != f.latest {
		t.Fatalf("head = %q, %v", sha, err)
	}
	hash, err := r.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := r.HasChanges(ctx); err != nil || changed {
		t.Fatalf("fresh changes = %v, %v", changed, err)
	}
	if err := r.CreateBranch(ctx, "patchbot/issue-10"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Dir(), "original file.txt"), "one\ntwo\nthree\nfour\n")
	write(t, filepath.Join(r.Dir(), ":(exclude)literal file.txt"), "new\n")
	before, err := r.Status(ctx)
	if err != nil || len(before) != 2 {
		t.Fatalf("status = %+v, %v", before, err)
	}
	diff, err := r.Diff(ctx)
	if err != nil || !strings.Contains(diff, "+four") || !strings.Contains(diff, "+new") {
		t.Fatalf("diff = %q, %v", diff, err)
	}
	stat, err := r.DiffStat(ctx)
	if err != nil || stat != (DiffStat{Files: 2, Insertions: 2}) {
		t.Fatalf("stat = %+v, %v", stat, err)
	}
	after, err := r.Status(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("diff changed staging: before %+v after %+v, %v", before, after, err)
	}
	sha, err = r.CommitAll(ctx, "fix: example (#10)", Author{"Controlled Bot", "controlled@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	identity := f.git(t, r.Dir(), "show", "-s", "--format=%an|%ae|%cn|%ce", "HEAD")
	if strings.TrimSpace(identity) != "Controlled Bot|controlled@example.invalid|Controlled Bot|controlled@example.invalid" {
		t.Fatalf("identity = %s", identity)
	}
	if changed, err := r.HasChanges(ctx); err != nil || changed {
		t.Fatalf("after commit changes = %v, %v", changed, err)
	}
	if exists, err := r.RemoteBranchExists(ctx, "patchbot/issue-10"); err != nil || exists {
		t.Fatalf("branch exists before push = %v, %v", exists, err)
	}
	if err := r.Push(ctx, "patchbot/issue-10"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(f.git(t, f.bare, "rev-parse", "refs/heads/patchbot/issue-10")); got != sha {
		t.Fatalf("remote head = %q, want %q", got, sha)
	}
	if exists, err := r.RemoteBranchExists(ctx, "patchbot/issue-10"); err != nil || !exists {
		t.Fatalf("branch exists after push = %v, %v", exists, err)
	}
	if err := r.VerifyConfig(hash); err != nil {
		t.Fatal(err)
	}
	config := read(t, filepath.Join(r.Dir(), ".git", "config"))
	if strings.Contains(config, fakeToken) || strings.Contains(strings.ToLower(config), "extraheader") || strings.Contains(config, "AUTHORIZATION") {
		t.Fatalf("credential persisted: %q", config)
	}
	// The base remains pinned after commit, so the diff is still publishable.
	if diff, err := r.Diff(ctx); err != nil || !strings.Contains(diff, "+four") {
		t.Fatalf("committed diff = %q, %v", diff, err)
	}
}

func TestCheckoutShallowSHA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	r := f.clone(t, 1)
	if got := strings.TrimSpace(f.git(t, r.Dir(), "rev-parse", "--is-shallow-repository")); got != "true" {
		t.Fatalf("not shallow: %s", got)
	}
	if _, _, err := r.run(ctx, false, "cat-file", "-e", f.first+"^{commit}"); err == nil {
		t.Fatal("old commit unexpectedly available")
	}
	if err := r.CheckoutSHA(ctx, f.first); err != nil {
		t.Fatal(err)
	}
	if sha, err := r.HeadSHA(ctx); err != nil || sha != f.first {
		t.Fatalf("head = %q, %v", sha, err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "second.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second commit file still exists: %v", err)
	}
	if diff, err := r.Diff(ctx); err != nil || diff != "" {
		t.Fatalf("base was not reset: %q, %v", diff, err)
	}
	if err := r.CheckoutSHA(ctx, "main;bad"); err == nil {
		t.Fatal("accepted non-SHA revision")
	}
}

func TestStatusRenamesAndSpaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	r := f.clone(t, 0)
	f.git(t, r.Dir(), "mv", "--", "original file.txt", "renamed file.txt")
	write(t, filepath.Join(r.Dir(), "new file\nwith newline.txt"), "new\n")
	changes, err := r.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{{Code: "R ", Path: "renamed file.txt", OriginalPath: "original file.txt"},
		{Code: "??", Path: "new file\nwith newline.txt"}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("status = %+v, want %+v", changes, want)
	}
	if stat, err := r.DiffStat(ctx); err != nil || stat != (DiffStat{Files: 2, Insertions: 1}) {
		t.Fatalf("rename stat = %+v, %v", stat, err)
	}
	if _, err := r.CommitAll(ctx, "rename", Author{}); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.HasChanges(ctx); err != nil || changed {
		t.Fatalf("rename not committed: %v, %v", changed, err)
	}
}

func TestBranchGuards(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	r := f.clone(t, 0)
	ctx := context.Background()
	for _, branch := range []string{"main", "master", "other/issue-10", "patchbot/", "patchbot/../main", "patchbot/a:main", "+patchbot/x", "patchbot/a\n", "patchbot/a.lock"} {
		if err := r.Push(ctx, branch); !errors.Is(err, ErrBranchGuard) {
			t.Errorf("push %q = %v", branch, err)
		}
	}
	r.defaultBranch = "patchbot/default"
	if err := r.Push(ctx, "patchbot/default"); !errors.Is(err, ErrBranchGuard) {
		t.Fatalf("captured default guard: %v", err)
	}
	// A default-branch change after clone is guarded too.
	f.git(t, f.bare, "update-ref", "refs/heads/patchbot/new-default", f.latest)
	f.git(t, f.bare, "symbolic-ref", "HEAD", "refs/heads/patchbot/new-default")
	if err := r.Push(ctx, "patchbot/new-default"); !errors.Is(err, ErrBranchGuard) {
		t.Fatalf("live default guard: %v", err)
	}
}

func TestPushNeverOverwritesDivergentBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	r := f.clone(t, 0)
	if err := r.CreateBranch(ctx, "patchbot/divergent"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.Dir(), "local.txt"), "local\n")
	if _, err := r.CommitAll(ctx, "local change", Author{}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.seed, "remote.txt"), "someone else's work\n")
	f.git(t, f.seed, "add", "--", "remote.txt")
	f.git(t, f.seed, "commit", "--no-verify", "-m", "remote change")
	remoteSHA := strings.TrimSpace(f.git(t, f.seed, "rev-parse", "HEAD"))
	f.git(t, f.seed, "push", f.bare, "HEAD:refs/heads/patchbot/divergent")
	if err := r.Push(ctx, "patchbot/divergent"); err == nil {
		t.Fatal("overwrote a divergent remote branch")
	}
	if got := strings.TrimSpace(f.git(t, f.bare, "rev-parse", "refs/heads/patchbot/divergent")); got != remoteSHA {
		t.Fatalf("remote branch changed: %s", got)
	}
}

func TestTamperDetection(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	for _, kind := range []string{"config", "hook", "hook-directory", "hook-symlink", "config-symlink"} {
		t.Run(kind, func(t *testing.T) {
			r := f.clone(t, 0)
			hash, err := r.ConfigHash()
			if err != nil {
				t.Fatal(err)
			}
			hooks := filepath.Join(r.Dir(), ".git", "hooks")
			mustMkdir(t, hooks)
			switch kind {
			case "config":
				path := filepath.Join(r.Dir(), ".git", "config")
				write(t, path, read(t, path)+"\n[alias]\n surprise = !false\n")
			case "hook":
				write(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 0\n")
			case "hook-directory":
				mustMkdir(t, filepath.Join(hooks, "evil.sample"))
			case "hook-symlink":
				if err := os.Symlink("/dev/null", filepath.Join(hooks, "evil.sample")); err != nil {
					t.Fatal(err)
				}
			case "config-symlink":
				path := filepath.Join(r.Dir(), ".git", "config")
				outside := filepath.Join(t.TempDir(), "config")
				write(t, outside, read(t, path))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.VerifyConfig(hash); !errors.Is(err, ErrTampered) {
				t.Fatalf("verify = %v", err)
			}
			if _, err := r.CommitAll(context.Background(), "no", Author{}); !errors.Is(err, ErrTampered) {
				t.Fatalf("commit = %v", err)
			}
			if err := r.Push(context.Background(), "patchbot/tampered"); !errors.Is(err, ErrTampered) {
				t.Fatalf("push = %v", err)
			}
		})
	}
}

func TestSubmodulesRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := gitFixture(t)
	write(t, filepath.Join(f.seed, ".gitmodules"), "[submodule \"unsafe\"]\npath = unsafe\nurl = https://invalid.example/no.git\n")
	f.git(t, f.seed, "add", "--", ".gitmodules")
	f.git(t, f.seed, "commit", "--no-verify", "-m", "submodules")
	sha := strings.TrimSpace(f.git(t, f.seed, "rev-parse", "HEAD"))
	f.git(t, f.seed, "push", f.bare, "HEAD:refs/heads/main")
	dest := filepath.Join(t.TempDir(), "refused")
	_, err := Clone(ctx, CloneOptions{Dir: dest, Branch: "main", TestOnlyLocalRemote: f.bare})
	if !errors.Is(err, ErrSubmodules) {
		t.Fatalf("clone = %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed clone left checkout behind: %v", err)
	}
	// Checkout of an older/newer spec cannot bypass the submodule rule.
	f.git(t, f.bare, "update-ref", "refs/heads/main", f.latest)
	r := f.clone(t, 0)
	if err := r.CheckoutSHA(ctx, sha); !errors.Is(err, ErrSubmodules) {
		t.Fatalf("checkout submodules = %v", err)
	}
	// A newly written declaration must also be refused before commit.
	clean := f.clone(t, 0)
	write(t, filepath.Join(clean.Dir(), ".gitmodules"), "unsafe\n")
	if _, err := clean.CommitAll(ctx, "unsafe", Author{}); !errors.Is(err, ErrSubmodules) {
		t.Fatalf("commit submodules = %v", err)
	}
}

func TestFiltersAndHooksNeverRun(t *testing.T) {
	ctx := context.Background()
	f := gitFixture(t)
	marker := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(t.TempDir(), "malicious")
	write(t, script, "#!/bin/sh\n: > '"+marker+"'\ncat\n")
	if err := os.Chmod(script, 0700); err != nil { // #nosec G302 -- owner execute permission is necessary for this malicious-hook fixture.
		t.Fatal(err)
	}
	write(t, filepath.Join(f.seed, ".gitattributes"), "*.txt filter=evil diff=evil\n")
	mustMkdir(t, filepath.Join(f.seed, ".hooks"))
	write(t, filepath.Join(f.seed, ".hooks", "pre-commit"), read(t, script))
	if err := os.Chmod(filepath.Join(f.seed, ".hooks", "pre-commit"), 0700); err != nil { // #nosec G302 -- executable hook fixture; only the owner can access it.
		t.Fatal(err)
	}
	f.git(t, f.seed, "add", "--", ".gitattributes", ".hooks/pre-commit")
	f.git(t, f.seed, "commit", "--no-verify", "-m", "untrusted attributes and hook")
	f.git(t, f.seed, "push", f.bare, "HEAD:refs/heads/main")
	// Both ambient config injection and HOME configuration must be ignored.
	write(t, filepath.Join(f.home, ".gitconfig"), "[filter \"evil\"]\n clean = "+script+"\n smudge = "+script+"\n[core]\n hooksPath = .hooks\n")
	t.Setenv("HOME", f.home)
	t.Setenv("GIT_CONFIG_COUNT", "3")
	for i, pair := range [][2]string{{"filter.evil.clean", script}, {"filter.evil.smudge", script}, {"core.hooksPath", ".hooks"}} {
		suffix := string(rune('0' + i))
		t.Setenv("GIT_CONFIG_KEY_"+suffix, pair[0])
		t.Setenv("GIT_CONFIG_VALUE_"+suffix, pair[1])
	}
	r := f.clone(t, 0)
	write(t, filepath.Join(r.Dir(), "second.txt"), "edit\n")
	if _, err := r.Diff(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitAll(ctx, "safe", Author{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(ctx, "patchbot/safe"); err != nil {
		t.Fatal(err)
	}
	// Regular sample files are permitted; an active planted hook is rejected.
	hooks := filepath.Join(r.Dir(), ".git", "hooks")
	mustMkdir(t, hooks)
	write(t, filepath.Join(hooks, "pre-commit.sample"), read(t, script))
	if _, err := r.ConfigHash(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(hooks, "pre-commit"), read(t, script))
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0700); err != nil { // #nosec G302 -- executable hook fixture proves hooks cannot run.
		t.Fatal(err)
	}
	if _, err := r.CommitAll(ctx, "refused", Author{}); !errors.Is(err, ErrTampered) {
		t.Fatalf("planted hook: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malicious code ran: %v", err)
	}
}

func TestCredentialEnvironmentAndArgv(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	r := f.clone(t, 0)
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeToken))
	for _, operation := range []struct {
		auth bool
		args []string
	}{
		{true, []string{"clone", "--", "https://github.com/owner/repo.git", "/private/tmp/destination"}},
		{true, []string{"push", "--", "https://github.com/owner/repo.git", "HEAD:refs/heads/patchbot/example"}},
		{false, []string{"status", "--porcelain=v1", "-z"}},
		{false, []string{"fetch", "--", "https://github.com/owner/repo.git", f.first}},
	} {
		cmd, err := r.command(context.Background(), operation.auth, operation.args...)
		if err != nil {
			t.Fatal(err)
		}
		argv := strings.Join(cmd.Args, "\x00")
		if strings.Contains(argv, fakeToken) || strings.Contains(argv, basic) {
			t.Fatal("credential leaked into ps-visible argv")
		}
		env := strings.Join(cmd.Env, "\n")
		if strings.Contains(env, basic) != operation.auth {
			t.Fatalf("credential scope incorrect (authenticated %v)", operation.auth)
		}
		for _, forbidden := range []string{"GIT_ASKPASS=", "SSH_ASKPASS=", "GITHUB_TOKEN=", "GIT_SSH_COMMAND="} {
			if strings.Contains(env, forbidden) {
				t.Fatalf("inherited forbidden variable %s", forbidden)
			}
		}
		for _, required := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME=" + r.home} {
			if !strings.Contains(env, required) {
				t.Fatalf("missing environment control %s", required)
			}
		}
	}
	entries, err := os.ReadDir(r.home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("git HOME not empty: %v, %v", entries, err)
	}
	if _, err := r.command(context.Background(), true, "fetch", f.first); err == nil {
		t.Fatal("credentials permitted for fetch")
	}
	// Production uses HTTPS only, and credentials use exactly the host-scoped
	// ephemeral configuration key, rather than URL or persistent config.
	r.localRemote = ""
	cmd, err := r.command(context.Background(), true, "push", "--", "https://github.com/owner/repo.git", "HEAD:refs/heads/patchbot/test")
	if err != nil {
		t.Fatal(err)
	}
	argv, env := strings.Join(cmd.Args, "\x00"), strings.Join(cmd.Env, "\n")
	for _, required := range []string{"protocol.file.allow=never", "core.fsmonitor=false", "submodule.recurse=false", "credential.helper=", "core.hooksPath=" + r.hooks} {
		if !strings.Contains(argv, required) {
			t.Fatalf("missing git control %s", required)
		}
	}
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader") || !strings.Contains(env, "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic) {
		t.Fatal("missing host-scoped credential configuration")
	}
	// A remote/helper that echoes raw, basic-encoded and fragmented secrets
	// cannot put any of them in the surfaced error.
	script := filepath.Join(t.TempDir(), "fake-git")
	write(t, script, "#!/bin/sh\nprintf '%s' '"+fakeToken+" "+basic+"' >&2\nexit 23\n")
	if err := os.Chmod(script, 0700); err != nil { // #nosec G302 -- owner execute permission for the failing-command test fixture.
		t.Fatal(err)
	}
	r.git = script
	_, _, err = r.run(context.Background(), true, "push")
	if err == nil || strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), basic) {
		t.Fatalf("unsafe error: %v", err)
	}
	_, err = r.command(context.Background(), false, fakeToken)
	if err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("unsafe argument error: %v", err)
	}
}

func TestOutputCapTimeoutAndClose(t *testing.T) {
	t.Parallel()
	f := gitFixture(t)
	r := f.clone(t, 0)
	r.outputCap = 128
	write(t, filepath.Join(r.Dir(), "second.txt"), strings.Repeat("a line\n", 5000))
	diff, err := r.Diff(context.Background())
	if err != nil || len(diff) != r.outputCap || !strings.HasSuffix(diff, "[diff truncated]\n") {
		t.Fatalf("capped diff: length %d, %v", len(diff), err)
	}
	for i := range 20 {
		write(t, filepath.Join(r.Dir(), strings.Repeat("x", i+1)), "x")
	}
	if _, err := r.Status(context.Background()); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("truncated status = %v", err)
	}
	script := filepath.Join(t.TempDir(), "slow-git")
	write(t, script, "#!/bin/sh\nsleep 30 &\nwait\n")
	if err := os.Chmod(script, 0700); err != nil { // #nosec G302 -- owner execute permission for the process-timeout test fixture.
		t.Fatal(err)
	}
	r.git, r.timeout = script, 50*time.Millisecond
	start := time.Now()
	if _, _, err := r.run(context.Background(), false, "status"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not bound helpers and pipe draining")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.private); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private directories retained: %v", err)
	}
	if _, err := r.HeadSHA(context.Background()); err == nil {
		t.Fatal("used closed repo")
	}
}

func TestStatusParserAndPathValidation(t *testing.T) {
	t.Parallel()
	for _, malformed := range []string{" M missing terminator", "x\x00", "R  renamed\x00", "R  renamed\x00\x00"} {
		if _, err := parseStatus(malformed); err == nil {
			t.Errorf("accepted malformed status %q", malformed)
		}
	}
	for _, path := range []string{".git/config", ".GIT/hooks/pre-commit", "nested/.git/config", "../escape", "/absolute", "."} {
		if _, err := changedPaths([]Change{{Code: "??", Path: path}}); err == nil {
			t.Errorf("accepted unsafe stage path %q", path)
		}
	}
	changes, err := parseStatus(" C copied file\x00old file\x00")
	if err != nil || len(changes) != 1 || changes[0].OriginalPath != "old file" {
		t.Fatalf("copy parse = %+v, %v", changes, err)
	}
}

func TestCloneValidationAndFreshDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, opts := range []CloneOptions{
		{Dir: "relative", Branch: "main"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "--bad"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "main", Host: "https://github.com", Owner: "owner", Name: "repo"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "main", Host: "github.com", Owner: "../escape", Name: "repo"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "main", Host: "github.com", Owner: "owner", Name: "repo.git?query"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "main", TestOnlyLocalRemote: "relative.git"},
		{Dir: filepath.Join(t.TempDir(), "repo"), Branch: "main", Depth: -1},
	} {
		if _, err := Clone(ctx, opts); err == nil {
			t.Fatalf("accepted invalid options %+v", opts)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keep"), "existing data")
	if _, err := Clone(ctx, CloneOptions{Dir: dir, Branch: "main"}); err == nil {
		t.Fatal("accepted existing destination")
	}
	if got := read(t, filepath.Join(dir, "keep")); got != "existing data" {
		t.Fatal("modified an existing destination")
	}
}
