package gitx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const trustedPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

// command builds the entire environment explicitly. Credentials are deliberately
// absent from argv, including -c arguments, and from all non-publication calls.
func (r *Repo) command(ctx context.Context, authenticated bool, args ...string) (*exec.Cmd, error) {
	if authenticated && (len(args) == 0 || (args[0] != "clone" && args[0] != "push")) {
		return nil, errors.New("credentials are restricted to clone and push")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) || (r.token != "" && strings.Contains(arg, r.token)) {
			return nil, errors.New("invalid git argument")
		}
	}
	protocol := "never"
	if r.localRemote != "" {
		protocol = "always"
	}
	argv := []string{
		"--literal-pathspecs",
		"-c", "core.hooksPath=" + r.hooks,
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.file.allow=" + protocol,
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "credential.helper=",
		"-c", "credential.interactive=false",
		"-c", "http.followRedirects=false",
		"-c", "core.attributesFile=/dev/null",
		"-c", "core.quotePath=false",
		"-c", "core.pager=cat",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.git, argv...) // #nosec G204 -- executable is resolved from a fixed trusted PATH; literal git subcommands, no shell.
	cmd.Dir = r.dir
	cmd.Env = []string{
		"PATH=" + trustedPath, "HOME=" + r.home, "TMPDIR=" + r.temp,
		"LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_AUTHOR_NAME=" + r.author.Name, "GIT_AUTHOR_EMAIL=" + r.author.Email,
		"GIT_COMMITTER_NAME=" + r.author.Name, "GIT_COMMITTER_EMAIL=" + r.author.Email,
	}
	if authenticated && r.token != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://"+r.host+"/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+encoded)
	}
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	return cmd, nil
}

// run never includes argv, environment, stderr, or OS error strings in errors.
// Even a remote that echoes an encoded/fragmented secret cannot leak it there.
func (r *Repo) run(ctx context.Context, authenticated bool, args ...string) (string, bool, error) {
	return r.runIndex(ctx, authenticated, "", args...)
}

func (r *Repo) runIndex(ctx context.Context, authenticated bool, index string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd, err := r.command(ctx, authenticated, args...)
	if err != nil {
		return "", false, err
	}
	if index != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	}
	out, diagnostic := &cappedBuffer{limit: r.outputCap}, &cappedBuffer{limit: r.outputCap}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	err = cmd.Run()
	cleanupProcess(cmd)
	if ctx.Err() != nil {
		return "", false, fmt.Errorf("git command: %w", ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", false, fmt.Errorf("git command failed (exit %d)", exitErr.ExitCode())
		}
		return "", false, errors.New("git command could not complete")
	}
	return string(out.data), out.truncated, nil
}

func (r *Repo) complete(ctx context.Context, authenticated bool, args ...string) (string, error) {
	out, truncated, err := r.run(ctx, authenticated, args...)
	if err == nil && truncated {
		err = ErrOutputLimit
	}
	return out, err
}

// os/exec serializes writes to each buffer. Continue draining after the cap.
type cappedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, b.limit-len(b.data))
	b.data = append(b.data, p[:keep]...)
	b.truncated = b.truncated || keep != n
	return n, nil
}
