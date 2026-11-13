package execx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
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
)

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
// rootless podman; the DOCKER_* names select a rootless docker daemon.
var runtimeEnvNames = []string{
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME",
	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF",
	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
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
	for _, entry := range c.HostEnv {
		if name, _, ok := strings.Cut(entry, "="); !ok || name == "" || strings.ContainsRune(entry, 0) {
			errs = append(errs, errInvalidEnv)
			break
		}
	}
	return errors.Join(errs...)
}

// RuntimeHostEnv builds the runtime CLI environment: the trusted PATH plus the
// runtime-selection variables above, read through lookup (os.LookupEnv).
func RuntimeHostEnv(trustedPath []string, lookup func(string) (string, bool)) []string {
	env := []string{"PATH=" + strings.Join(trustedPath, string(filepath.ListSeparator)), "LANG=C.UTF-8"}
	for _, name := range runtimeEnvNames {
		if value, ok := lookup(name); ok && value != "" && !strings.ContainsRune(value, 0) {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// ContainerExecutor runs each command in a fresh rootless container that sees
// only the workspace (read-write at /work) and a size-limited /tmp. The command
// policy is unchanged; only the place where the command runs differs. The
// runtime CLI is itself run through LocalExecutor, which supplies the output
// cap, redaction, timeout and process-group cleanup of the CLI. On timeout or
// cancellation the container is killed by name, which kills every descendant
// in its PID namespace, and it is always force-removed afterwards.
type ContainerExecutor struct {
	root  string
	cfg   ContainerConfig
	local *LocalExecutor
	// newName is replaceable in tests.
	newName func() (string, error)
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
	local, err := NewLocalExecutor(root)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(local.root, ":,") {
		return nil, errors.New("workspace path must not contain ':' or ','")
	}
	cfg.Path = slices.Clone(cfg.Path)
	cfg.HostEnv = slices.Clone(cfg.HostEnv)
	return &ContainerExecutor{root: local.root, cfg: cfg, local: local, newName: containerName}, nil
}

func containerName() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "patchbot-" + hex.EncodeToString(b[:]), nil
}

// Run executes spec.Argv inside a new container. Exit codes pass through;
// timeout and cancellation report ExitCode -1 as the local executor does. A
// runtime failure (exit 125) is returned as an error.
func (e *ContainerExecutor) Run(ctx context.Context, spec Spec) (Result, error) {
	result := Result{ExitCode: -1}
	if e == nil || e.local == nil {
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
	name, err := e.newName()
	if err != nil {
		return result, fmt.Errorf("container name: %w", err)
	}
	argv := containerArgv(e.cfg, name, e.root, workdir, containerEnv(env, e.cfg.Path), spec.Argv)
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	// Kill by name as soon as the run is interrupted. Signals to the CLI alone
	// would not reach the container's processes.
	stop := context.AfterFunc(runCtx, func() { e.cleanup(name, "kill") })
	result, runErr := e.local.Run(runCtx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: spec.Timeout, OutputCap: spec.OutputCap, Redactor: spec.Redactor})
	interrupted := !stop() || result.TimedOut || result.Cancelled
	// --rm removes a container that exited normally. rm -f covers interrupted
	// runs (including a container created after an early kill attempt) and
	// runtime failures.
	if interrupted || result.ExitCode == runtimeErrorExit || runErr != nil {
		e.cleanup(name, "rm", "-f")
	}
	if interrupted {
		result.ExitCode = -1
		return result, runErr
	}
	if runErr == nil && result.ExitCode == runtimeErrorExit {
		runErr = errors.New("container runtime failed to start the command (exit 125)")
		result.ExitCode = -1
	}
	return result, runErr
}

// cleanup runs a short runtime command against the named container. It is
// best-effort: "no such container" is the expected outcome after --rm.
func (e *ContainerExecutor) cleanup(name string, verb ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	argv := append(append([]string{e.cfg.Runtime}, verb...), name)
	result, err := e.local.Run(ctx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: cleanupTimeout, OutputCap: 4096})
	if err != nil || result.TimedOut {
		slog.Warn("container cleanup failed", "container", name, "action", verb[0], "timed_out", result.TimedOut)
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
	out := make([]string, 0, len(env)+1)
	out = append(out, "PATH="+strings.Join(containerPath, ":"))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if name == "PATH" {
			continue
		}
		out = append(out, name+"="+remap(value))
	}
	return out
}

// containerArgv builds the complete runtime invocation. Arguments after the
// image are passed to the container literally, never through a shell.
func containerArgv(cfg ContainerConfig, name, hostRoot, workdir string, env, argv []string) []string {
	out := []string{
		cfg.Runtime, "run", "--rm",
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
	out = append(out, "--workdir", workdir, "-v", hostRoot+":"+ContainerWorkdir+":rw")
	for _, entry := range env {
		out = append(out, "-e", entry)
	}
	out = append(out, cfg.Image)
	return append(out, argv...)
}
