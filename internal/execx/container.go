package execx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Container runtime flavors.
const (
	RuntimePodman = "podman"
	RuntimeDocker = "docker"
)

const (
	// ContainerWorkdir is where the workspace is mounted inside the container.
	ContainerWorkdir = "/work"
	// containerTmp is the only writable path besides the workspace.
	containerTmp = "/tmp"
	// containerLabel marks every container patchbot starts, for cleanup audits.
	containerLabel = "io.patchbot.managed=true"
	// runtimeErrorExit is the exit status docker and podman use for their own
	// failures (daemon unreachable, image missing, invalid flag).
	runtimeErrorExit = 125
	cleanupTimeout   = 15 * time.Second
	// DefaultReconcileTimeout bounds how long removal of a container is
	// retried before the command fails loudly.
	DefaultReconcileTimeout = 60 * time.Second
	// DefaultWorkspaceMaxBytes is the default executor.workspace_max_bytes.
	DefaultWorkspaceMaxBytes int64 = 2 << 30
	// maxWorkspaceEntries bounds the post-command workspace walk.
	maxWorkspaceEntries = 1_000_000
	// cliDir is the runtime CLI's working directory. It is never the workspace,
	// so nothing the container does to /work can break cleanup.
	cliDir = "/"
)

// ErrWorkspaceLimit reports that a container command grew the workspace past
// executor.workspace_max_bytes, or left it unmeasurable.
var ErrWorkspaceLimit = errors.New("workspace exceeds executor.workspace_max_bytes")

// ErrContainerNotRemoved reports that a container could not be confirmed gone;
// the run must stop before host git touches the workspace.
var ErrContainerNotRemoved = errors.New("container could not be confirmed removed")

// proxyNames are blanked inside every container. Docker injects proxy values
// from the CLI's config.json and podman from the host environment; neither
// passes through scrubEnv, and with --network none they have no use.
var proxyNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "ftp_proxy", "no_proxy", "all_proxy"}

// DefaultContainerPath is the PATH inside the container. It covers common
// official toolchain images (Go installs to /usr/local/go/bin).
var DefaultContainerPath = []string{"/usr/local/go/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

var (
	// An image must be pinned by content digest; a tag alone is mutable.
	imageRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]{1,5})?/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
	sizeRE = regexp.MustCompile(`^[1-9][0-9]{0,8}[kKmMgG]$`)
	cpusRE = regexp.MustCompile(`^(?:[0-9]{1,3})(?:\.[0-9]{1,3})?$`)
	userRE = regexp.MustCompile(`^([0-9]{1,10}):([0-9]{1,10})$`)
)

// runtimeEnvNames are passed from patchbot's own environment to the runtime
// CLI process only (never into the container). XDG_RUNTIME_DIR is required by
// rootless podman; DOCKER_HOST selects a rootless docker daemon. DOCKER_CONFIG
// and DOCKER_CONTEXT are deliberately absent: docker gets a private, empty
// DOCKER_CONFIG so a user's config.json (proxies, credential helpers) is ignored.
var runtimeEnvNames = []string{
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME",
	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF",
	"DOCKER_HOST", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
}

// ContainerConfig is operator-owned container execution configuration.
type ContainerConfig struct {
	// Runtime is the absolute path of the podman or docker binary.
	Runtime string
	// Flavor is RuntimePodman or RuntimeDocker.
	Flavor string
	// Image must be digest-pinned: name[:tag]@sha256:<64 hex>.
	Image string
	// Memory and TmpfsSize are sizes such as "2g" or "512m".
	Memory    string
	CPUs      string
	PidsLimit int
	TmpfsSize string
	// Network must be "none".
	Network string
	// User is a non-root numeric "uid:gid".
	User string
	// UserNS is "" or "keep-id" (podman only): the host user is mapped to
	// User inside the container so the bind-mounted workspace is writable.
	UserNS string
	// Path is the PATH inside the container; nil selects DefaultContainerPath.
	Path []string
	// WorkspaceMaxBytes bounds the workspace size after each command.
	WorkspaceMaxBytes int64
	// HostEnv is the environment of the runtime CLI process. Build it with
	// RuntimeHostEnv. It is never forwarded into the container.
	HostEnv []string
}

// ValidateImage requires a digest-pinned image reference.
func ValidateImage(image string) error {
	if !imageRE.MatchString(image) {
		return errors.New("image must be digest-pinned, such as golang:1.27@sha256:<64 hex digits>")
	}
	return nil
}

// ValidateSize checks a memory or tmpfs size such as "512m".
func ValidateSize(size string) error {
	if !sizeRE.MatchString(size) {
		return errors.New(`want a size such as "512m" or "2g"`)
	}
	return nil
}

// ValidateCPUs checks a positive CPU count such as "2" or "1.5".
func ValidateCPUs(cpus string) error {
	value, err := strconv.ParseFloat(cpus, 64)
	if !cpusRE.MatchString(cpus) || err != nil || value <= 0 {
		return errors.New(`want a positive CPU count such as "2" or "1.5"`)
	}
	return nil
}

// ValidateUser requires a numeric, non-root uid:gid.
func ValidateUser(user string) error {
	m := userRE.FindStringSubmatch(user)
	if m == nil {
		return errors.New(`want a numeric "uid:gid" such as "65532:65532"`)
	}
	for _, id := range m[1:] {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil || n == 0 {
			return errors.New("uid and gid must be non-root (not 0)")
		}
	}
	return nil
}

// ValidateContainerPath checks the in-container PATH entries.
func ValidateContainerPath(dirs []string) error {
	if len(dirs) == 0 {
		return errors.New("at least one directory is required")
	}
	for _, dir := range dirs {
		if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.ContainsAny(dir, ":\x00") {
			return errors.New("entries must be clean absolute paths without ':'")
		}
	}
	return nil
}

// Validate checks every field. It does not contact the runtime.
func (c *ContainerConfig) Validate() error {
	var errs []error
	if !filepath.IsAbs(c.Runtime) || strings.ContainsRune(c.Runtime, 0) {
		errs = append(errs, errors.New("runtime must be an absolute binary path"))
	}
	if c.Flavor != RuntimePodman && c.Flavor != RuntimeDocker {
		errs = append(errs, errors.New("runtime flavor must be podman or docker"))
	}
	if err := ValidateImage(c.Image); err != nil {
		errs = append(errs, err)
	}
	for _, size := range []string{c.Memory, c.TmpfsSize} {
		if err := ValidateSize(size); err != nil {
			errs = append(errs, err)
		}
	}
	if err := ValidateCPUs(c.CPUs); err != nil {
		errs = append(errs, err)
	}
	if c.WorkspaceMaxBytes <= 0 {
		errs = append(errs, errors.New("workspace max bytes must be greater than 0"))
	}
	if c.PidsLimit <= 0 {
		errs = append(errs, errors.New("pids limit must be greater than 0"))
	}
	if c.Network != "none" {
		errs = append(errs, errors.New(`network must be "none"`))
	}
	if err := ValidateUser(c.User); err != nil {
		errs = append(errs, err)
	}
	switch {
	case c.UserNS == "":
	case c.UserNS == "keep-id" && c.Flavor == RuntimePodman:
	default:
		errs = append(errs, errors.New(`userns must be empty or "keep-id" (podman only)`))
	}
	if c.Path != nil {
		if err := ValidateContainerPath(c.Path); err != nil {
			errs = append(errs, fmt.Errorf("container path: %w", err))
		}
	}
	dockerConfig := ""
	for _, entry := range c.HostEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			errs = append(errs, errInvalidEnv)
			break
		}
		if name == "DOCKER_CONFIG" {
			dockerConfig = value
		}
	}
	if c.Flavor == RuntimeDocker && !filepath.IsAbs(dockerConfig) {
		errs = append(errs, errors.New("docker requires a private absolute DOCKER_CONFIG in the runtime environment"))
	}
	return errors.Join(errs...)
}

// RuntimeHostEnv builds the runtime CLI environment: the trusted PATH plus the
// runtime-selection variables above, read through lookup (os.LookupEnv). For
// docker, append DOCKER_CONFIG naming a private directory (see
// PrepareDockerConfig).
func RuntimeHostEnv(trustedPath []string, lookup func(string) (string, bool)) []string {
	env := []string{"PATH=" + strings.Join(trustedPath, string(filepath.ListSeparator)), "LANG=C.UTF-8"}
	for _, name := range runtimeEnvNames {
		if value, ok := lookup(name); ok && value != "" && !strings.ContainsRune(value, 0) {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// PrepareDockerConfig creates dir (mode 0700) as docker's private config
// directory and refuses one holding a config.json, whose proxy settings docker
// would inject into containers.
func PrepareDockerConfig(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("docker config directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create docker config directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("docker config directory must be a private 0700 directory")
	}
	if _, err := os.Lstat(filepath.Join(dir, "config.json")); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("docker config directory must not contain config.json")
	}
	return nil
}

// ContainerExecutor runs each command in a fresh rootless container that sees
// only the workspace (read-write at /work) and a size-limited /tmp. The command
// policy is unchanged; only the place where the command runs differs. The
// runtime CLI is itself run through LocalExecutor, which supplies the output
// cap, redaction, timeout and process-group cleanup of the CLI. On timeout or
// cancellation the container is killed by name, which kills every descendant
// in its PID namespace, and it is always force-removed afterwards.
type ContainerExecutor struct {
	root string
	cfg  ContainerConfig
	// cli runs the runtime binary with "/" as its working directory, so no
	// workspace state (permissions, symlinks) can affect create or cleanup.
	cli *LocalExecutor
	// newName is replaceable in tests.
	newName func() (string, error)
	// ReconcileTimeout and PollInterval bound removal retries; zero selects
	// DefaultReconcileTimeout and 500ms.
	ReconcileTimeout time.Duration
	PollInterval     time.Duration
}

var _ Executor = (*ContainerExecutor)(nil)

// NewContainerExecutor validates cfg and fixes the absolute workspace root.
func NewContainerExecutor(root string, cfg ContainerConfig) (*ContainerExecutor, error) {
	if cfg.Path == nil {
		cfg.Path = DefaultContainerPath
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	resolved, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(resolved, ":,") {
		return nil, errors.New("workspace path must not contain ':' or ','")
	}
	if cfg.Flavor == RuntimeDocker {
		for _, entry := range cfg.HostEnv {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
				if err := PrepareDockerConfig(value); err != nil {
					return nil, err
				}
			}
		}
	}
	cli, err := NewLocalExecutor(cliDir)
	if err != nil {
		return nil, err
	}
	cfg.Path = slices.Clone(cfg.Path)
	cfg.HostEnv = slices.Clone(cfg.HostEnv)
	e := &ContainerExecutor{root: resolved, cfg: cfg, cli: cli, newName: containerName}
	if _, err := e.gitDir(); err != nil {
		return nil, err
	}
	return e, nil
}

// gitDir returns the workspace's .git directory, which is mounted read-only so
// container commands cannot rewrite refs, objects, index or config that host
// git later commits and pushes. A .git file (gitlink) or symlink is refused.
// It returns "" when the workspace has no .git entry.
func (e *ContainerExecutor) gitDir() (string, error) {
	dir := filepath.Join(e.root, ".git")
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("inspect workspace .git: %w", err)
	case !info.IsDir():
		return "", errors.New("workspace .git must be a real directory, not a gitlink file or symlink")
	}
	if strings.ContainsAny(dir, ":,") {
		return "", errors.New("workspace path must not contain ':' or ','")
	}
	return dir, nil
}

func containerName() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "patchbot-" + hex.EncodeToString(b[:]), nil
}

// Run executes spec.Argv inside a new container: create (named, labelled,
// not yet running), then start --attach. Afterwards the container is removed
// by name and its absence is confirmed before Run returns, so no container
// process outlives the call (host git runs only after Run). Exit codes pass
// through; timeout and cancellation report ExitCode -1 as the local executor
// does. A runtime failure (exit 125), unconfirmed removal or a workspace over
// WorkspaceMaxBytes is returned as an error.
func (e *ContainerExecutor) Run(ctx context.Context, spec Spec) (Result, error) {
	result := Result{ExitCode: -1}
	if e == nil || e.cli == nil {
		return result, errors.New("use NewContainerExecutor to configure container execution")
	}
	if spec.Timeout < 0 || spec.OutputCap < 0 {
		return result, errors.New("execution limits must not be negative")
	}
	if spec.Timeout == 0 {
		spec.Timeout = DefaultTimeout
	}
	workdir, err := e.workdir(spec.Dir)
	if err != nil {
		return result, err
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" || (strings.Contains(spec.Argv[0], "/") && !path.IsAbs(spec.Argv[0])) {
		return result, errors.New("a bare or absolute container executable is required")
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return result, errors.New("argv must not contain NUL")
		}
	}
	env, err := scrubEnv(spec.Env)
	if err != nil {
		return result, err
	}
	gitDir, err := e.gitDir()
	if err != nil {
		return result, err
	}
	name, err := e.newName()
	if err != nil {
		return result, fmt.Errorf("container name: %w", err)
	}
	argv := containerArgv(e.cfg, name, e.root, gitDir, workdir, containerEnv(env, e.cfg.Path), spec.Argv)
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	created, createErr := e.runtime(runCtx, spec.Timeout, spec.Redactor, argv[1:]...)
	if createErr != nil || created.TimedOut || created.Cancelled || created.ExitCode != 0 {
		// A create interrupted mid-flight may still complete afterwards:
		// require absence on consecutive checks before trusting it.
		reconcileErr := e.reconcile(name, true)
		result.TimedOut, result.Cancelled, result.Duration = created.TimedOut, created.Cancelled, created.Duration
		if created.TimedOut || created.Cancelled {
			return result, reconcileErr
		}
		result.Output, result.TotalBytes, result.Truncated = created.Output, created.TotalBytes, created.Truncated
		if createErr == nil {
			createErr = fmt.Errorf("container create failed (exit %d)", created.ExitCode)
		}
		return result, errors.Join(createErr, reconcileErr)
	}

	// Kill by name as soon as the run is interrupted. Signals to the CLI alone
	// would not reach the container's processes.
	stop := context.AfterFunc(runCtx, func() { e.cleanup(name, "kill") })
	result, runErr := e.cli.Run(runCtx, Spec{Argv: []string{e.cfg.Runtime, "start", "--attach", name}, Dir: cliDir, Env: e.cfg.HostEnv, Timeout: spec.Timeout, OutputCap: spec.OutputCap, Redactor: spec.Redactor})
	interrupted := !stop() || result.TimedOut || result.Cancelled
	runErr = errors.Join(runErr, e.reconcile(name, false))
	if interrupted {
		result.ExitCode = -1
		return result, runErr
	}
	if runErr == nil && result.ExitCode == runtimeErrorExit {
		runErr = errors.New("container runtime failed to start the command (exit 125)")
		result.ExitCode = -1
	}
	if err := CheckWorkspaceSize(e.root, e.cfg.WorkspaceMaxBytes); err != nil {
		runErr = errors.Join(runErr, err)
	}
	return result, runErr
}

// runtime runs the runtime binary from cliDir with the runtime environment.
func (e *ContainerExecutor) runtime(ctx context.Context, timeout time.Duration, redactor *secret.Redactor, args ...string) (Result, error) {
	argv := append([]string{e.cfg.Runtime}, args...)
	return e.cli.Run(ctx, Spec{Argv: argv, Dir: cliDir, Env: e.cfg.HostEnv, Timeout: timeout, OutputCap: 64 << 10, Redactor: redactor})
}

// cleanup runs a short runtime command against the named container. It is
// best-effort; reconcile confirms the outcome.
func (e *ContainerExecutor) cleanup(name string, verb ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	result, err := e.runtime(ctx, cleanupTimeout, nil, append(slices.Clone(verb), name)...)
	if err != nil || result.TimedOut {
		slog.Warn("container cleanup command failed", "container", name, "action", verb[0], "timed_out", result.TimedOut)
	}
}

// reconcile force-removes the named container and retries until the runtime
// confirms it is gone (settle: on two consecutive checks) or the reconcile
// deadline passes. It never depends on workspace state.
func (e *ContainerExecutor) reconcile(name string, settle bool) error {
	timeout, interval := e.ReconcileTimeout, e.PollInterval
	if timeout <= 0 {
		timeout = DefaultReconcileTimeout
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	need, absent := 1, 0
	if settle {
		need = 2
	}
	for {
		e.cleanup(name, "rm", "-f")
		present, err := e.present(ctx, name)
		if err == nil && !present {
			absent++
			if absent >= need {
				return nil
			}
		} else {
			absent = 0
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.Error("container could not be confirmed removed; repository commands are no longer contained", "container", name, "runtime", e.cfg.Flavor)
			return fmt.Errorf("%w: %s within %s", ErrContainerNotRemoved, name, timeout)
		case <-timer.C:
		}
	}
}

// present reports whether a container with exactly this name exists.
func (e *ContainerExecutor) present(ctx context.Context, name string) (bool, error) {
	result, err := e.runtime(ctx, cleanupTimeout, nil, "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil || result.TimedOut || result.Cancelled || result.ExitCode != 0 {
		return false, errors.New("list containers failed")
	}
	for line := range strings.SplitSeq(result.Output, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// SweepContainers force-removes every container carrying patchbot's label,
// such as ones left by a crashed process. Call it only when no run is active.
func SweepContainers(ctx context.Context, cfg ContainerConfig) (int, error) {
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	e := &ContainerExecutor{cfg: cfg}
	cli, err := NewLocalExecutor(cliDir)
	if err != nil {
		return 0, err
	}
	e.cli = cli
	listed, err := e.runtime(ctx, cleanupTimeout, nil, "ps", "-a", "--filter", "label="+containerLabel, "--format", "{{.Names}}")
	if err != nil || listed.ExitCode != 0 || listed.TimedOut || listed.Truncated {
		return 0, errors.New("list patchbot containers failed")
	}
	var names []string
	for line := range strings.SplitSeq(listed.Output, "\n") {
		if name := strings.TrimSpace(line); name != "" && !strings.HasPrefix(name, "-") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return 0, nil
	}
	removed, err := e.runtime(ctx, cleanupTimeout, nil, append([]string{"rm", "-f"}, names...)...)
	if err != nil || removed.ExitCode != 0 || removed.TimedOut {
		return 0, fmt.Errorf("remove %d stale patchbot containers failed", len(names))
	}
	return len(names), nil
}

// CheckWorkspaceSize walks root without following symlinks and fails when the
// apparent size of its entries exceeds limit, when it has more than
// maxWorkspaceEntries entries, or when any part cannot be read (a container
// could otherwise hide data behind chmod 000).
func CheckWorkspaceSize(root string, limit int64) error {
	return checkWorkspaceSize(root, limit, maxWorkspaceEntries)
}

func checkWorkspaceSize(root string, limit int64, maxEntries int) error {
	var total int64
	entries := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("%w: more than %d entries", ErrWorkspaceLimit, maxEntries)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		if total > limit {
			return fmt.Errorf("%w: more than %d bytes", ErrWorkspaceLimit, limit)
		}
		return nil
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrWorkspaceLimit):
		return err
	default:
		return fmt.Errorf("%w: workspace could not be measured", ErrWorkspaceLimit)
	}
}

func (e *ContainerExecutor) workdir(dir string) (string, error) {
	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
		return "", errors.New("working directory must be absolute and inside the workspace")
	}
	resolved, err := canonicalRoot(dir)
	if err != nil {
		return "", err
	}
	if !inside(e.root, resolved) {
		return "", errors.New("working directory symlink escapes workspace")
	}
	rel, err := filepath.Rel(e.root, resolved)
	if err != nil {
		return "", err
	}
	return path.Join(ContainerWorkdir, filepath.ToSlash(rel)), nil
}

// containerEnv rewrites host HOME/TMPDIR (private per-command directories that
// do not exist in the container) to the container's tmpfs and fixes PATH.
func containerEnv(env, containerPath []string) []string {
	var home, tmp string
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case "HOME":
			home = value
		case "TMPDIR":
			tmp = value
		}
	}
	remap := func(value string) string {
		for _, prefix := range []string{home, tmp} {
			if prefix == "" {
				continue
			}
			if value == prefix {
				return containerTmp
			}
			if rest, ok := strings.CutPrefix(value, prefix+"/"); ok {
				return containerTmp + "/" + rest
			}
		}
		return value
	}
	out := make([]string, 0, len(env)+1+len(proxyNames))
	out = append(out, "PATH="+strings.Join(containerPath, ":"))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if name == "PATH" || slices.Contains(proxyNames, name) {
			continue
		}
		out = append(out, name+"="+remap(value))
	}
	// Explicit empty values override runtime-injected proxy settings.
	for _, name := range proxyNames {
		out = append(out, name+"=")
	}
	return out
}

// containerArgv builds the complete create invocation. Arguments after the
// image are passed to the container literally, never through a shell. gitDir,
// when set, is mounted read-only over /work/.git.
func containerArgv(cfg ContainerConfig, name, hostRoot, gitDir, workdir string, env, argv []string) []string {
	out := []string{
		cfg.Runtime, "create",
		"--name", name,
		"--label", containerLabel,
		"--pull", "never",
		"--network", cfg.Network,
		"--read-only",
		"--tmpfs", containerTmp + ":rw,nosuid,nodev,size=" + cfg.TmpfsSize,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(cfg.PidsLimit),
		"--memory", cfg.Memory,
		"--cpus", cfg.CPUs,
		"--user", cfg.User,
	}
	if cfg.UserNS == "keep-id" {
		uid, gid, _ := strings.Cut(cfg.User, ":")
		out = append(out, "--userns", "keep-id:uid="+uid+",gid="+gid)
	}
	if cfg.Flavor == RuntimePodman {
		out = append(out, "--http-proxy=false")
	}
	out = append(out, "--workdir", workdir, "-v", hostRoot+":"+ContainerWorkdir+":rw")
	if gitDir != "" {
		out = append(out, "-v", gitDir+":"+ContainerWorkdir+"/.git:ro")
	}
	for _, entry := range env {
		out = append(out, "-e", entry)
	}
	out = append(out, cfg.Image)
	return append(out, argv...)
}
