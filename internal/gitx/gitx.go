// Package gitx owns deterministic git operations for one disposable checkout.
// Calls are serialized. Close removes private git HOME/hooks/temp directories;
// the caller owns removal of the checkout. This is not a sandbox against hostile
// processes running as the same user; callers must stop repository commands
// before inspecting or publishing the checkout.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds every git invocation, including version checks.
	DefaultTimeout = 2 * time.Minute
	// DefaultOutputCap bounds stdout and stderr independently.
	DefaultOutputCap = 256 << 10
	// MinimumGitVersion includes fixes for CVE-2022-23521 and the 2025 clone
	// vulnerabilities, including CVE-2025-48384. Older Git must not clone repos.
	MinimumGitVersion = "2.50.1"
)

var (
	// ErrTampered indicates changed git configuration or unexpected hooks.
	ErrTampered = errors.New("git metadata was tampered with")
	// ErrBranchGuard indicates an unsafe publication branch.
	ErrBranchGuard = errors.New("git branch guard refused publication")
	// ErrSubmodules indicates an unsupported submodule declaration.
	ErrSubmodules = errors.New("repositories with .gitmodules are refused")
	// ErrOutputLimit means structured output could not be read in full.
	ErrOutputLimit = errors.New("git output exceeded its limit")
)

// CloneOptions describes validated repository coordinates, never an arbitrary
// production URL. Branch is the branch to clone; DefaultBranch defaults to it
// and should come from trusted repository metadata when cloning another branch.
type CloneOptions struct {
	Dir           string
	Host          string
	Owner         string
	Name          string
	Branch        string
	DefaultBranch string
	Depth         int
	Token         string
	Timeout       time.Duration
	OutputCap     int
	// TestOnlyLocalRemote explicitly enables file transport. Only tests should
	// set it, to an absolute local bare repository path. No local hard links.
	TestOnlyLocalRemote string
}

// Author is operator-controlled identity, used for both author and committer.
// The zero value selects patchbot <patchbot@users.noreply.github.com>.
type Author struct {
	Name  string
	Email string
}

// Repo is one checkout. Its credentials and remote URL stay private.
type Repo struct {
	mu                                                    sync.Mutex
	dir, git, home, hooks, temp, private                  string
	host, remote, localRemote, token, base, defaultBranch string
	configHash                                            string
	author                                                Author
	timeout                                               time.Duration
	outputCap                                             int
	closed                                                bool
	// observeCommand is a per-repository test seam, called under mu.
	observeCommand func([]string)
}

var (
	coordinate = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	hostname   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	objectID   = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

// Clone checks MinimumGitVersion once and clones into a nonexistent Dir.
// On failure it removes only directories it created, including a partial clone.
func Clone(ctx context.Context, opts CloneOptions) (_ *Repo, cloneErr error) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		return nil, errors.New("git process groups require a Unix host")
	}
	if !filepath.IsAbs(opts.Dir) || strings.ContainsRune(opts.Dir, 0) || opts.Depth < 0 ||
		opts.Timeout < 0 || opts.OutputCap < 0 || !validBranch(opts.Branch) {
		return nil, errors.New("invalid clone options")
	}
	if opts.DefaultBranch == "" {
		opts.DefaultBranch = opts.Branch
	}
	if !validBranch(opts.DefaultBranch) || strings.ContainsRune(opts.Token, 0) {
		return nil, errors.New("invalid clone options")
	}
	if _, err := os.Lstat(opts.Dir); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("clone destination must not exist")
	}
	r := &Repo{dir: opts.Dir, host: opts.Host, token: opts.Token, defaultBranch: opts.DefaultBranch,
		timeout: opts.Timeout, outputCap: opts.OutputCap, localRemote: opts.TestOnlyLocalRemote,
		author: Author{"patchbot", "patchbot@users.noreply.github.com"}}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	if r.outputCap == 0 {
		r.outputCap = DefaultOutputCap
	}
	if r.outputCap < 64 {
		return nil, errors.New("output cap must be at least 64 bytes")
	}
	if r.localRemote != "" {
		if !filepath.IsAbs(r.localRemote) || strings.ContainsRune(r.localRemote, 0) {
			return nil, errors.New("test remote must be an absolute local path")
		}
		r.remote = (&url.URL{Scheme: "file", Path: r.localRemote}).String()
	} else {
		if !hostname.MatchString(opts.Host) || strings.Contains(opts.Host, "..") ||
			!coordinate.MatchString(opts.Owner) || !coordinate.MatchString(opts.Name) ||
			opts.Owner == "." || opts.Owner == ".." || opts.Name == "." || opts.Name == ".." {
			return nil, errors.New("invalid repository coordinates")
		}
		r.remote = "https://" + opts.Host + "/" + opts.Owner + "/" + opts.Name + ".git"
	}
	// Resolve from a fixed PATH, never the checkout or inherited PATH.
	for _, dir := range filepath.SplitList(trustedPath) {
		path := filepath.Join(dir, "git")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			r.git = path
			break
		}
	}
	if r.git == "" {
		return nil, errors.New("git executable not found in trusted PATH")
	}
	private, err := os.MkdirTemp("", "patchbot-git-")
	if err != nil {
		return nil, errors.New("create private git directories failed")
	}
	r.private = private
	defer func() {
		if cloneErr != nil {
			_ = os.RemoveAll(private)
		}
	}()
	r.home, r.hooks, r.temp = filepath.Join(private, "home"), filepath.Join(private, "hooks"), filepath.Join(private, "tmp")
	for _, dir := range []string{r.home, r.hooks, r.temp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return nil, errors.New("create private git directory failed")
		}
	}
	// Reserve the destination ourselves so a failed clone never removes a
	// directory another actor created between validation and git clone.
	if err := os.Mkdir(opts.Dir, 0700); err != nil {
		return nil, errors.New("reserve clone destination failed")
	}
	defer func() {
		if cloneErr != nil {
			_ = os.RemoveAll(opts.Dir)
		}
	}()
	r.dir = private
	version, err := r.complete(ctx, false, "--version")
	if err != nil {
		return nil, err
	}
	if err := checkGitVersion(version); err != nil {
		return nil, err
	}
	args := []string{"clone", "--no-local", "--single-branch", "--no-tags", "--no-recurse-submodules", "--template=" + r.hooks, "--branch", opts.Branch}
	if opts.Depth > 0 {
		args = append(args, "--depth", fmt.Sprint(opts.Depth))
	}
	args = append(args, "--", r.remote, opts.Dir)
	if _, err := r.complete(ctx, true, args...); err != nil {
		return nil, err
	}
	r.dir = opts.Dir
	if err := r.noSubmodules(ctx); err != nil {
		return nil, err
	}
	r.base, err = r.head(ctx)
	if err != nil {
		return nil, err
	}
	r.configHash, err = r.hashConfig()
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Dir is the absolute checkout path; it remains valid after Close.
func (r *Repo) Dir() string { return r.dir }

// Close discards credentials and removes the private git execution directories.
// The workspace owner is responsible for removing the checkout itself.
func (r *Repo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed, r.token = true, ""
	if err := os.RemoveAll(r.private); err != nil {
		return errors.New("remove private git directories failed")
	}
	return nil
}

func validBranch(name string) bool {
	if name == "" || name == "@" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, ".") ||
		strings.ContainsAny(name, " ~^:?*[\\\x00\t\r\n") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
		for _, ch := range part {
			if ch < 32 || ch == 127 {
				return false
			}
		}
	}
	return true
}

func (r *Repo) head(ctx context.Context) (string, error) {
	out, err := r.complete(ctx, false, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !objectID.MatchString(sha) {
		return "", errors.New("git returned an invalid commit ID")
	}
	return sha, nil
}

// HeadSHA returns the checked-out commit ID.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verify(); err != nil {
		return "", err
	}
	return r.head(ctx)
}

// CheckoutSHA detaches at a full commit ID and updates the base for future diffs.
// Fetches by ID when shallow history does not contain the approved spec's base.
// As required, fetch gets no token; a private host must permit this fetch without
// credentials, or the caller must initially use a full clone.
func (r *Repo) CheckoutSHA(ctx context.Context, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !objectID.MatchString(sha) {
		return errors.New("checkout requires a full commit ID")
	}
	if err := r.verify(); err != nil {
		return err
	}
	if _, err := r.complete(ctx, false, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := r.complete(ctx, false, "fetch", "--no-tags", "--recurse-submodules=no", "--depth=1", "--", r.remote, sha); err != nil {
			return err
		}
	}
	if _, err := r.complete(ctx, false, "checkout", "--detach", sha, "--"); err != nil {
		return err
	}
	if err := r.noSubmodules(ctx); err != nil {
		return err
	}
	var err error
	r.base, err = r.head(ctx)
	return err
}

// CreateBranch creates and switches to a new local branch.
func (r *Repo) CreateBranch(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validBranch(name) {
		return ErrBranchGuard
	}
	if err := r.verify(); err != nil {
		return err
	}
	_, err := r.complete(ctx, false, "switch", "--no-guess", "-c", name, "--")
	return err
}

func (r *Repo) noSubmodules(ctx context.Context) error {
	// ls-tree also finds .gitmodules hidden by a sparse checkout or deletion.
	out, err := r.complete(ctx, false, "ls-tree", "-r", "-z", "--name-only", "HEAD")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(out, "\x00") {
		for _, part := range strings.Split(path, "/") {
			if strings.EqualFold(part, ".gitmodules") {
				return ErrSubmodules
			}
		}
	}
	return nil
}
