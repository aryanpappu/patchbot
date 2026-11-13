Reading additional input from stdin...
OpenAI Codex v0.166.0
--------
workdir: /Users/pappumishra/Documents/selfproject/patchbot-m17-container
model: gpt-6.1-sol
provider: openai
approval: never
sandbox: read-only
reasoning effort: none
reasoning summaries: none
session id: 01a1d577-d6cd-7bf0-9861-da14f9f75499
--------
user
Security review of branch m17-container vs development (git diff development...HEAD). Focus: internal/execx/container.go and internal/config/executor.go. Look for container escape via flag injection, env leakage of tokens into the container or runtime, workspace path/symlink escapes in -v and --workdir, cancel/timeout races leaving containers running, and validation gaps. Report only real findings with file:line, severity and a concrete fix. Be concise.
codex
I’ll inspect the diff and both files, then trace the relevant execution and cancellation paths to verify potential findings.
exec
/bin/zsh -lc 'git diff development...HEAD -- internal/execx/container.go internal/config/executor.go' in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
exec
/bin/zsh -lc "rg --files -g AGENTS.md -g '*container*' -g '*executor*' -g '*exec*' internal" in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
 succeeded in 0ms:
internal/cli/doctor_container_test.go
internal/config/executor.go
internal/config/executor_test.go
internal/execx/container.go
internal/execx/execx.go
internal/execx/container_integration_test.go
internal/execx/container_test.go
internal/runner/executor_test.go

exec
/bin/zsh -lc "pwd; rg --files -g AGENTS.md -g '"'!vendor'"'; nl -ba internal/execx/container.go; nl -ba internal/config/executor.go" in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
 succeeded in 0ms:
/Users/pappumishra/Documents/selfproject/patchbot-m17-container
     1	package execx
     2	
     3	import (
     4		"context"
     5		"crypto/rand"
     6		"encoding/hex"
     7		"errors"
     8		"fmt"
     9		"log/slog"
    10		"path"
    11		"path/filepath"
    12		"regexp"
    13		"slices"
    14		"strconv"
    15		"strings"
    16		"time"
    17	)
    18	
    19	// Container runtime flavors.
    20	const (
    21		RuntimePodman = "podman"
    22		RuntimeDocker = "docker"
    23	)
    24	
    25	const (
    26		// ContainerWorkdir is where the workspace is mounted inside the container.
    27		ContainerWorkdir = "/work"
    28		// containerTmp is the only writable path besides the workspace.
    29		containerTmp = "/tmp"
    30		// containerLabel marks every container patchbot starts, for cleanup audits.
    31		containerLabel = "io.patchbot.managed=true"
    32		// runtimeErrorExit is the exit status docker and podman use for their own
    33		// failures (daemon unreachable, image missing, invalid flag).
    34		runtimeErrorExit = 125
    35		cleanupTimeout   = 15 * time.Second
    36	)
    37	
    38	// DefaultContainerPath is the PATH inside the container. It covers common
    39	// official toolchain images (Go installs to /usr/local/go/bin).
    40	var DefaultContainerPath = []string{"/usr/local/go/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
    41	
    42	var (
    43		// An image must be pinned by content digest; a tag alone is mutable.
    44		imageRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]{1,5})?/)?` +
    45			`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
    46			`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
    47		sizeRE = regexp.MustCompile(`^[1-9][0-9]{0,8}[kKmMgG]$`)
    48		cpusRE = regexp.MustCompile(`^(?:[0-9]{1,3})(?:\.[0-9]{1,3})?$`)
    49		userRE = regexp.MustCompile(`^([0-9]{1,10}):([0-9]{1,10})$`)
    50	)
    51	
    52	// runtimeEnvNames are passed from patchbot's own environment to the runtime
    53	// CLI process only (never into the container). XDG_RUNTIME_DIR is required by
    54	// rootless podman; the DOCKER_* names select a rootless docker daemon.
    55	var runtimeEnvNames = []string{
    56		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME",
    57		"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF",
    58		"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
    59	}
    60	
    61	// ContainerConfig is operator-owned container execution configuration.
    62	type ContainerConfig struct {
    63		// Runtime is the absolute path of the podman or docker binary.
    64		Runtime string
    65		// Flavor is RuntimePodman or RuntimeDocker.
    66		Flavor string
    67		// Image must be digest-pinned: name[:tag]@sha256:<64 hex>.
    68		Image string
    69		// Memory and TmpfsSize are sizes such as "2g" or "512m".
    70		Memory    string
    71		CPUs      string
    72		PidsLimit int
    73		TmpfsSize string
    74		// Network must be "none".
    75		Network string
    76		// User is a non-root numeric "uid:gid".
    77		User string
    78		// UserNS is "" or "keep-id" (podman only): the host user is mapped to
    79		// User inside the container so the bind-mounted workspace is writable.
    80		UserNS string
    81		// Path is the PATH inside the container; nil selects DefaultContainerPath.
    82		Path []string
    83		// HostEnv is the environment of the runtime CLI process. Build it with
    84		// RuntimeHostEnv. It is never forwarded into the container.
    85		HostEnv []string
    86	}
    87	
    88	// ValidateImage requires a digest-pinned image reference.
    89	func ValidateImage(image string) error {
    90		if !imageRE.MatchString(image) {
    91			return errors.New("image must be digest-pinned, such as golang:1.27@sha256:<64 hex digits>")
    92		}
    93		return nil
    94	}
    95	
    96	// ValidateSize checks a memory or tmpfs size such as "512m".
    97	func ValidateSize(size string) error {
    98		if !sizeRE.MatchString(size) {
    99			return errors.New(`want a size such as "512m" or "2g"`)
   100		}
   101		return nil
   102	}
   103	
   104	// ValidateCPUs checks a positive CPU count such as "2" or "1.5".
   105	func ValidateCPUs(cpus string) error {
   106		value, err := strconv.ParseFloat(cpus, 64)
   107		if !cpusRE.MatchString(cpus) || err != nil || value <= 0 {
   108			return errors.New(`want a positive CPU count such as "2" or "1.5"`)
   109		}
   110		return nil
   111	}
   112	
   113	// ValidateUser requires a numeric, non-root uid:gid.
   114	func ValidateUser(user string) error {
   115		m := userRE.FindStringSubmatch(user)
   116		if m == nil {
   117			return errors.New(`want a numeric "uid:gid" such as "65532:65532"`)
   118		}
   119		for _, id := range m[1:] {
   120			n, err := strconv.ParseUint(id, 10, 32)
   121			if err != nil || n == 0 {
   122				return errors.New("uid and gid must be non-root (not 0)")
   123			}
   124		}
   125		return nil
   126	}
   127	
   128	// ValidateContainerPath checks the in-container PATH entries.
   129	func ValidateContainerPath(dirs []string) error {
   130		if len(dirs) == 0 {
   131			return errors.New("at least one directory is required")
   132		}
   133		for _, dir := range dirs {
   134			if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.ContainsAny(dir, ":\x00") {
   135				return errors.New("entries must be clean absolute paths without ':'")
   136			}
   137		}
   138		return nil
   139	}
   140	
   141	// Validate checks every field. It does not contact the runtime.
   142	func (c *ContainerConfig) Validate() error {
   143		var errs []error
   144		if !filepath.IsAbs(c.Runtime) || strings.ContainsRune(c.Runtime, 0) {
   145			errs = append(errs, errors.New("runtime must be an absolute binary path"))
   146		}
   147		if c.Flavor != RuntimePodman && c.Flavor != RuntimeDocker {
   148			errs = append(errs, errors.New("runtime flavor must be podman or docker"))
   149		}
   150		if err := ValidateImage(c.Image); err != nil {
   151			errs = append(errs, err)
   152		}
   153		for _, size := range []string{c.Memory, c.TmpfsSize} {
   154			if err := ValidateSize(size); err != nil {
   155				errs = append(errs, err)
   156			}
   157		}
   158		if err := ValidateCPUs(c.CPUs); err != nil {
   159			errs = append(errs, err)
   160		}
   161		if c.PidsLimit <= 0 {
   162			errs = append(errs, errors.New("pids limit must be greater than 0"))
   163		}
   164		if c.Network != "none" {
   165			errs = append(errs, errors.New(`network must be "none"`))
   166		}
   167		if err := ValidateUser(c.User); err != nil {
   168			errs = append(errs, err)
   169		}
   170		switch {
   171		case c.UserNS == "":
   172		case c.UserNS == "keep-id" && c.Flavor == RuntimePodman:
   173		default:
   174			errs = append(errs, errors.New(`userns must be empty or "keep-id" (podman only)`))
   175		}
   176		if c.Path != nil {
   177			if err := ValidateContainerPath(c.Path); err != nil {
   178				errs = append(errs, fmt.Errorf("container path: %w", err))
   179			}
   180		}
   181		for _, entry := range c.HostEnv {
   182			if name, _, ok := strings.Cut(entry, "="); !ok || name == "" || strings.ContainsRune(entry, 0) {
   183				errs = append(errs, errInvalidEnv)
   184				break
   185			}
   186		}
   187		return errors.Join(errs...)
   188	}
   189	
   190	// RuntimeHostEnv builds the runtime CLI environment: the trusted PATH plus the
   191	// runtime-selection variables above, read through lookup (os.LookupEnv).
   192	func RuntimeHostEnv(trustedPath []string, lookup func(string) (string, bool)) []string {
   193		env := []string{"PATH=" + strings.Join(trustedPath, string(filepath.ListSeparator)), "LANG=C.UTF-8"}
   194		for _, name := range runtimeEnvNames {
   195			if value, ok := lookup(name); ok && value != "" && !strings.ContainsRune(value, 0) {
   196				env = append(env, name+"="+value)
   197			}
   198		}
   199		return env
   200	}
   201	
   202	// ContainerExecutor runs each command in a fresh rootless container that sees
   203	// only the workspace (read-write at /work) and a size-limited /tmp. The command
   204	// policy is unchanged; only the place where the command runs differs. The
   205	// runtime CLI is itself run through LocalExecutor, which supplies the output
   206	// cap, redaction, timeout and process-group cleanup of the CLI. On timeout or
   207	// cancellation the container is killed by name, which kills every descendant
   208	// in its PID namespace, and it is always force-removed afterwards.
   209	type ContainerExecutor struct {
   210		root  string
   211		cfg   ContainerConfig
   212		local *LocalExecutor
   213		// newName is replaceable in tests.
   214		newName func() (string, error)
   215	}
   216	
   217	var _ Executor = (*ContainerExecutor)(nil)
   218	
   219	// NewContainerExecutor validates cfg and fixes the absolute workspace root.
   220	func NewContainerExecutor(root string, cfg ContainerConfig) (*ContainerExecutor, error) {
   221		if cfg.Path == nil {
   222			cfg.Path = DefaultContainerPath
   223		}
   224		if err := cfg.Validate(); err != nil {
   225			return nil, err
   226		}
   227		local, err := NewLocalExecutor(root)
   228		if err != nil {
   229			return nil, err
   230		}
   231		if strings.ContainsAny(local.root, ":,") {
   232			return nil, errors.New("workspace path must not contain ':' or ','")
   233		}
   234		cfg.Path = slices.Clone(cfg.Path)
   235		cfg.HostEnv = slices.Clone(cfg.HostEnv)
   236		return &ContainerExecutor{root: local.root, cfg: cfg, local: local, newName: containerName}, nil
   237	}
   238	
   239	func containerName() (string, error) {
   240		var b [12]byte
   241		if _, err := rand.Read(b[:]); err != nil {
   242			return "", err
   243		}
   244		return "patchbot-" + hex.EncodeToString(b[:]), nil
   245	}
   246	
   247	// Run executes spec.Argv inside a new container. Exit codes pass through;
   248	// timeout and cancellation report ExitCode -1 as the local executor does. A
   249	// runtime failure (exit 125) is returned as an error.
   250	func (e *ContainerExecutor) Run(ctx context.Context, spec Spec) (Result, error) {
   251		result := Result{ExitCode: -1}
   252		if e == nil || e.local == nil {
   253			return result, errors.New("use NewContainerExecutor to configure container execution")
   254		}
   255		if spec.Timeout < 0 || spec.OutputCap < 0 {
   256			return result, errors.New("execution limits must not be negative")
   257		}
   258		if spec.Timeout == 0 {
   259			spec.Timeout = DefaultTimeout
   260		}
   261		workdir, err := e.workdir(spec.Dir)
   262		if err != nil {
   263			return result, err
   264		}
   265		if len(spec.Argv) == 0 || spec.Argv[0] == "" || (strings.Contains(spec.Argv[0], "/") && !path.IsAbs(spec.Argv[0])) {
   266			return result, errors.New("a bare or absolute container executable is required")
   267		}
   268		for _, arg := range spec.Argv {
   269			if strings.ContainsRune(arg, 0) {
   270				return result, errors.New("argv must not contain NUL")
   271			}
   272		}
   273		env, err := scrubEnv(spec.Env)
   274		if err != nil {
   275			return result, err
   276		}
   277		name, err := e.newName()
   278		if err != nil {
   279			return result, fmt.Errorf("container name: %w", err)
   280		}
   281		argv := containerArgv(e.cfg, name, e.root, workdir, containerEnv(env, e.cfg.Path), spec.Argv)
   282		runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
   283		defer cancel()
   284		// Kill by name as soon as the run is interrupted. Signals to the CLI alone
   285		// would not reach the container's processes.
   286		stop := context.AfterFunc(runCtx, func() { e.cleanup(name, "kill") })
   287		result, runErr := e.local.Run(runCtx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: spec.Timeout, OutputCap: spec.OutputCap, Redactor: spec.Redactor})
   288		interrupted := !stop() || result.TimedOut || result.Cancelled
   289		// --rm removes a container that exited normally. rm -f covers interrupted
   290		// runs (including a container created after an early kill attempt) and
   291		// runtime failures.
   292		if interrupted || result.ExitCode == runtimeErrorExit || runErr != nil {
   293			e.cleanup(name, "rm", "-f")
   294		}
   295		if interrupted {
   296			result.ExitCode = -1
   297			return result, runErr
   298		}
   299		if runErr == nil && result.ExitCode == runtimeErrorExit {
   300			runErr = errors.New("container runtime failed to start the command (exit 125)")
   301			result.ExitCode = -1
   302		}
   303		return result, runErr
   304	}
   305	
   306	// cleanup runs a short runtime command against the named container. It is
   307	// best-effort: "no such container" is the expected outcome after --rm.
   308	func (e *ContainerExecutor) cleanup(name string, verb ...string) {
   309		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
   310		defer cancel()
   311		argv := append(append([]string{e.cfg.Runtime}, verb...), name)
   312		result, err := e.local.Run(ctx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: cleanupTimeout, OutputCap: 4096})
   313		if err != nil || result.TimedOut {
   314			slog.Warn("container cleanup failed", "container", name, "action", verb[0], "timed_out", result.TimedOut)
   315		}
   316	}
   317	
   318	func (e *ContainerExecutor) workdir(dir string) (string, error) {
   319		if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
   320			return "", errors.New("working directory must be absolute and inside the workspace")
   321		}
   322		resolved, err := canonicalRoot(dir)
   323		if err != nil {
   324			return "", err
   325		}
   326		if !inside(e.root, resolved) {
   327			return "", errors.New("working directory symlink escapes workspace")
   328		}
   329		rel, err := filepath.Rel(e.root, resolved)
   330		if err != nil {
   331			return "", err
   332		}
   333		return path.Join(ContainerWorkdir, filepath.ToSlash(rel)), nil
   334	}
   335	
   336	// containerEnv rewrites host HOME/TMPDIR (private per-command directories that
   337	// do not exist in the container) to the container's tmpfs and fixes PATH.
   338	func containerEnv(env, containerPath []string) []string {
   339		var home, tmp string
   340		for _, entry := range env {
   341			name, value, _ := strings.Cut(entry, "=")
   342			switch name {
   343			case "HOME":
   344				home = value
   345			case "TMPDIR":
   346				tmp = value
   347			}
   348		}
   349		remap := func(value string) string {
   350			for _, prefix := range []string{home, tmp} {
   351				if prefix == "" {
   352					continue
   353				}
   354				if value == prefix {
   355					return containerTmp
   356				}
   357				if rest, ok := strings.CutPrefix(value, prefix+"/"); ok {
   358					return containerTmp + "/" + rest
   359				}
   360			}
   361			return value
   362		}
   363		out := make([]string, 0, len(env)+1)
   364		out = append(out, "PATH="+strings.Join(containerPath, ":"))
   365		for _, entry := range env {
   366			name, value, _ := strings.Cut(entry, "=")
   367			if name == "PATH" {
   368				continue
   369			}
   370			out = append(out, name+"="+remap(value))
   371		}
   372		return out
   373	}
   374	
   375	// containerArgv builds the complete runtime invocation. Arguments after the
   376	// image are passed to the container literally, never through a shell.
   377	func containerArgv(cfg ContainerConfig, name, hostRoot, workdir string, env, argv []string) []string {
   378		out := []string{
   379			cfg.Runtime, "run", "--rm",
   380			"--name", name,
   381			"--label", containerLabel,
   382			"--pull", "never",
   383			"--network", cfg.Network,
   384			"--read-only",
   385			"--tmpfs", containerTmp + ":rw,nosuid,nodev,size=" + cfg.TmpfsSize,
   386			"--cap-drop", "ALL",
   387			"--security-opt", "no-new-privileges",
   388			"--pids-limit", strconv.Itoa(cfg.PidsLimit),
   389			"--memory", cfg.Memory,
   390			"--cpus", cfg.CPUs,
   391			"--user", cfg.User,
   392		}
   393		if cfg.UserNS == "keep-id" {
   394			uid, gid, _ := strings.Cut(cfg.User, ":")
   395			out = append(out, "--userns", "keep-id:uid="+uid+",gid="+gid)
   396		}
   397		out = append(out, "--workdir", workdir, "-v", hostRoot+":"+ContainerWorkdir+":rw")
   398		for _, entry := range env {
   399			out = append(out, "-e", entry)
   400		}
   401		out = append(out, cfg.Image)
   402		return append(out, argv...)
   403	}
     1	package config
     2	
     3	import (
     4		"fmt"
     5		"slices"
     6	
     7		"github.com/aryanpappu/patchbot/internal/execx"
     8	)
     9	
    10	// CommandPolicyOptions returns the policy options for repository commands.
    11	// Local execution resolves executables on the host trusted path; container
    12	// execution keeps names for the image's PATH. Command IDs and the allowlist
    13	// are the same either way.
    14	func (c *Config) CommandPolicyOptions(excluded ...string) execx.PolicyOptions {
    15		if c.Executor.Container() {
    16			return execx.PolicyOptions{InContainer: true, TrustedPath: slices.Clone(c.Executor.Path)}
    17		}
    18		return execx.PolicyOptions{TrustedPath: c.Limits.TrustedPath, ExcludedRoots: append([]string{c.DataDir()}, excluded...)}
    19	}
    20	
    21	// ContainerConfig resolves the runtime binary through limits.trusted_path
    22	// (never the ambient PATH) and returns the execx container configuration.
    23	// lookup reads patchbot's own environment for the runtime CLI variables, such
    24	// as XDG_RUNTIME_DIR; nothing from it is forwarded into the container.
    25	func (c *Config) ContainerConfig(lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
    26		e := c.Executor
    27		if !e.Container() {
    28			return execx.ContainerConfig{}, fmt.Errorf("executor.kind is %q, not container", e.Kind)
    29		}
    30		runtime, err := execx.ResolveExecutable(e.Runtime, c.Limits.TrustedPath, append([]string{c.DataDir()}, excluded...)...)
    31		if err != nil {
    32			return execx.ContainerConfig{}, fmt.Errorf("executor.runtime %s: %w", e.Runtime, err)
    33		}
    34		cfg := execx.ContainerConfig{
    35			Runtime: runtime, Flavor: e.Runtime, Image: e.Image, Memory: e.Memory, CPUs: e.CPUs,
    36			PidsLimit: e.PidsLimit, TmpfsSize: e.TmpfsSize, Network: e.Network, User: e.User,
    37			UserNS: e.UserNS, Path: slices.Clone(e.Path), HostEnv: execx.RuntimeHostEnv(c.Limits.TrustedPath, lookup),
    38		}
    39		return cfg, cfg.Validate()
    40	}

 succeeded in 18795ms:
git: warning: confstr() failed with code 5: couldn't get path of DARWIN_USER_TEMP_DIR; using /tmp instead
git: error: couldn't create cache file '/tmp/xcrun_db-mcino1Nc' (errno=Operation not permitted)
2026-11-13 20:57:56.160 xcodebuild[19431:1905434]  DVTFilePathFSEvents: Failed to start fs event stream.
2026-11-13 20:57:56.613 xcodebuild[19431:1905419] [MT] DVTDeveloperPaths: Failed to get length of DARWIN_USER_CACHE_DIR from confstr(3), error = Error Domain=NSPOSIXErrorDomain Code=5 "Input/output error". Using NSCachesDirectory instead.
git: warning: confstr() failed with code 5: couldn't get path of DARWIN_USER_TEMP_DIR; using /tmp instead
git: error: couldn't create cache file '/tmp/xcrun_db-785sgC4P' (errno=Operation not permitted)
2026-11-13 20:58:01.417 xcodebuild[19501:1906018]  DVTFilePathFSEvents: Failed to start fs event stream.
2026-11-13 20:58:02.234 xcodebuild[19501:1905991] [MT] DVTDeveloperPaths: Failed to get length of DARWIN_USER_CACHE_DIR from confstr(3), error = Error Domain=NSPOSIXErrorDomain Code=5 "Input/output error". Using NSCachesDirectory instead.
diff --git a/internal/config/executor.go b/internal/config/executor.go
new file mode 100644
index 0000000..b4fe4c5
--- /dev/null
+++ b/internal/config/executor.go
@@ -0,0 +1,40 @@
+package config
+
+import (
+	"fmt"
+	"slices"
+
+	"github.com/aryanpappu/patchbot/internal/execx"
+)
+
+// CommandPolicyOptions returns the policy options for repository commands.
+// Local execution resolves executables on the host trusted path; container
+// execution keeps names for the image's PATH. Command IDs and the allowlist
+// are the same either way.
+func (c *Config) CommandPolicyOptions(excluded ...string) execx.PolicyOptions {
+	if c.Executor.Container() {
+		return execx.PolicyOptions{InContainer: true, TrustedPath: slices.Clone(c.Executor.Path)}
+	}
+	return execx.PolicyOptions{TrustedPath: c.Limits.TrustedPath, ExcludedRoots: append([]string{c.DataDir()}, excluded...)}
+}
+
+// ContainerConfig resolves the runtime binary through limits.trusted_path
+// (never the ambient PATH) and returns the execx container configuration.
+// lookup reads patchbot's own environment for the runtime CLI variables, such
+// as XDG_RUNTIME_DIR; nothing from it is forwarded into the container.
+func (c *Config) ContainerConfig(lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
+	e := c.Executor
+	if !e.Container() {
+		return execx.ContainerConfig{}, fmt.Errorf("executor.kind is %q, not container", e.Kind)
+	}
+	runtime, err := execx.ResolveExecutable(e.Runtime, c.Limits.TrustedPath, append([]string{c.DataDir()}, excluded...)...)
+	if err != nil {
+		return execx.ContainerConfig{}, fmt.Errorf("executor.runtime %s: %w", e.Runtime, err)
+	}
+	cfg := execx.ContainerConfig{
+		Runtime: runtime, Flavor: e.Runtime, Image: e.Image, Memory: e.Memory, CPUs: e.CPUs,
+		PidsLimit: e.PidsLimit, TmpfsSize: e.TmpfsSize, Network: e.Network, User: e.User,
+		UserNS: e.UserNS, Path: slices.Clone(e.Path), HostEnv: execx.RuntimeHostEnv(c.Limits.TrustedPath, lookup),
+	}
+	return cfg, cfg.Validate()
+}
diff --git a/internal/execx/container.go b/internal/execx/container.go
new file mode 100644
index 0000000..d4251cf
--- /dev/null
+++ b/internal/execx/container.go
@@ -0,0 +1,403 @@
+package execx
+
+import (
+	"context"
+	"crypto/rand"
+	"encoding/hex"
+	"errors"
+	"fmt"
+	"log/slog"
+	"path"
+	"path/filepath"
+	"regexp"
+	"slices"
+	"strconv"
+	"strings"
+	"time"
+)
+
+// Container runtime flavors.
+const (
+	RuntimePodman = "podman"
+	RuntimeDocker = "docker"
+)
+
+const (
+	// ContainerWorkdir is where the workspace is mounted inside the container.
+	ContainerWorkdir = "/work"
+	// containerTmp is the only writable path besides the workspace.
+	containerTmp = "/tmp"
+	// containerLabel marks every container patchbot starts, for cleanup audits.
+	containerLabel = "io.patchbot.managed=true"
+	// runtimeErrorExit is the exit status docker and podman use for their own
+	// failures (daemon unreachable, image missing, invalid flag).
+	runtimeErrorExit = 125
+	cleanupTimeout   = 15 * time.Second
+)
+
+// DefaultContainerPath is the PATH inside the container. It covers common
+// official toolchain images (Go installs to /usr/local/go/bin).
+var DefaultContainerPath = []string{"/usr/local/go/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
+
+var (
+	// An image must be pinned by content digest; a tag alone is mutable.
+	imageRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]{1,5})?/)?` +
+		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
+		`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
+	sizeRE = regexp.MustCompile(`^[1-9][0-9]{0,8}[kKmMgG]$`)
+	cpusRE = regexp.MustCompile(`^(?:[0-9]{1,3})(?:\.[0-9]{1,3})?$`)
+	userRE = regexp.MustCompile(`^([0-9]{1,10}):([0-9]{1,10})$`)
+)
+
+// runtimeEnvNames are passed from patchbot's own environment to the runtime
+// CLI process only (never into the container). XDG_RUNTIME_DIR is required by
+// rootless podman; the DOCKER_* names select a rootless docker daemon.
+var runtimeEnvNames = []string{
+	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME",
+	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_STORAGE_CONF",
+	"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY",
+}
+
+// ContainerConfig is operator-owned container execution configuration.
+type ContainerConfig struct {
+	// Runtime is the absolute path of the podman or docker binary.
+	Runtime string
+	// Flavor is RuntimePodman or RuntimeDocker.
+	Flavor string
+	// Image must be digest-pinned: name[:tag]@sha256:<64 hex>.
+	Image string
+	// Memory and TmpfsSize are sizes such as "2g" or "512m".
+	Memory    string
+	CPUs      string
+	PidsLimit int
+	TmpfsSize string
+	// Network must be "none".
+	Network string
+	// User is a non-root numeric "uid:gid".
+	User string
+	// UserNS is "" or "keep-id" (podman only): the host user is mapped to
+	// User inside the container so the bind-mounted workspace is writable.
+	UserNS string
+	// Path is the PATH inside the container; nil selects DefaultContainerPath.
+	Path []string
+	// HostEnv is the environment of the runtime CLI process. Build it with
+	// RuntimeHostEnv. It is never forwarded into the container.
+	HostEnv []string
+}
+
+// ValidateImage requires a digest-pinned image reference.
+func ValidateImage(image string) error {
+	if !imageRE.MatchString(image) {
+		return errors.New("image must be digest-pinned, such as golang:1.27@sha256:<64 hex digits>")
+	}
+	return nil
+}
+
+// ValidateSize checks a memory or tmpfs size such as "512m".
+func ValidateSize(size string) error {
+	if !sizeRE.MatchString(size) {
+		return errors.New(`want a size such as "512m" or "2g"`)
+	}
+	return nil
+}
+
+// ValidateCPUs checks a positive CPU count such as "2" or "1.5".
+func ValidateCPUs(cpus string) error {
+	value, err := strconv.ParseFloat(cpus, 64)
+	if !cpusRE.MatchString(cpus) || err != nil || value <= 0 {
+		return errors.New(`want a positive CPU count such as "2" or "1.5"`)
+	}
+	return nil
+}
+
+// ValidateUser requires a numeric, non-root uid:gid.
+func ValidateUser(user string) error {
+	m := userRE.FindStringSubmatch(user)
+	if m == nil {
+		return errors.New(`want a numeric "uid:gid" such as "65532:65532"`)
+	}
+	for _, id := range m[1:] {
+		n, err := strconv.ParseUint(id, 10, 32)
+		if err != nil || n == 0 {
+			return errors.New("uid and gid must be non-root (not 0)")
+		}
+	}
+	return nil
+}
+
+// ValidateContainerPath checks the in-container PATH entries.
+func ValidateContainerPath(dirs []string) error {
+	if len(dirs) == 0 {
+		return errors.New("at least one directory is required")
+	}
+	for _, dir := range dirs {
+		if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.ContainsAny(dir, ":\x00") {
+			return errors.New("entries must be clean absolute paths without ':'")
+		}
+	}
+	return nil
+}
+
+// Validate checks every field. It does not contact the runtime.
+func (c *ContainerConfig) Validate() error {
+	var errs []error
+	if !filepath.IsAbs(c.Runtime) || strings.ContainsRune(c.Runtime, 0) {
+		errs = append(errs, errors.New("runtime must be an absolute binary path"))
+	}
+	if c.Flavor != RuntimePodman && c.Flavor != RuntimeDocker {
+		errs = append(errs, errors.New("runtime flavor must be podman or docker"))
+	}
+	if err := ValidateImage(c.Image); err != nil {
+		errs = append(errs, err)
+	}
+	for _, size := range []string{c.Memory, c.TmpfsSize} {
+		if err := ValidateSize(size); err != nil {
+			errs = append(errs, err)
+		}
+	}
+	if err := ValidateCPUs(c.CPUs); err != nil {
+		errs = append(errs, err)
+	}
+	if c.PidsLimit <= 0 {
+		errs = append(errs, errors.New("pids limit must be greater than 0"))
+	}
+	if c.Network != "none" {
+		errs = append(errs, errors.New(`network must be "none"`))
+	}
+	if err := ValidateUser(c.User); err != nil {
+		errs = append(errs, err)
+	}
+	switch {
+	case c.UserNS == "":
+	case c.UserNS == "keep-id" && c.Flavor == RuntimePodman:
+	default:
+		errs = append(errs, errors.New(`userns must be empty or "keep-id" (podman only)`))
+	}
+	if c.Path != nil {
+		if err := ValidateContainerPath(c.Path); err != nil {
+			errs = append(errs, fmt.Errorf("container path: %w", err))
+		}
+	}
+	for _, entry := range c.HostEnv {
+		if name, _, ok := strings.Cut(entry, "="); !ok || name == "" || strings.ContainsRune(entry, 0) {
+			errs = append(errs, errInvalidEnv)
+			break
+		}
+	}
+	return errors.Join(errs...)
+}
+
+// RuntimeHostEnv builds the runtime CLI environment: the trusted PATH plus the
+// runtime-selection variables above, read through lookup (os.LookupEnv).
+func RuntimeHostEnv(trustedPath []string, lookup func(string) (string, bool)) []string {
+	env := []string{"PATH=" + strings.Join(trustedPath, string(filepath.ListSeparator)), "LANG=C.UTF-8"}
+	for _, name := range runtimeEnvNames {
+		if value, ok := lookup(name); ok && value != "" && !strings.ContainsRune(value, 0) {
+			env = append(env, name+"="+value)
+		}
+	}
+	return env
+}
+
+// ContainerExecutor runs each command in a fresh rootless container that sees
+// only the workspace (read-write at /work) and a size-limited /tmp. The command
+// policy is unchanged; only the place where the command runs differs. The
+// runtime CLI is itself run through LocalExecutor, which supplies the output
+// cap, redaction, timeout and process-group cleanup of the CLI. On timeout or
+// cancellation the container is killed by name, which kills every descendant
+// in its PID namespace, and it is always force-removed afterwards.
+type ContainerExecutor struct {
+	root  string
+	cfg   ContainerConfig
+	local *LocalExecutor
+	// newName is replaceable in tests.
+	newName func() (string, error)
+}
+
+var _ Executor = (*ContainerExecutor)(nil)
+
+// NewContainerExecutor validates cfg and fixes the absolute workspace root.
+func NewContainerExecutor(root string, cfg ContainerConfig) (*ContainerExecutor, error) {
+	if cfg.Path == nil {
+		cfg.Path = DefaultContainerPath
+	}
+	if err := cfg.Validate(); err != nil {
+		return nil, err
+	}
+	local, err := NewLocalExecutor(root)
+	if err != nil {
+		return nil, err
+	}
+	if strings.ContainsAny(local.root, ":,") {
+		return nil, errors.New("workspace path must not contain ':' or ','")
+	}
+	cfg.Path = slices.Clone(cfg.Path)
+	cfg.HostEnv = slices.Clone(cfg.HostEnv)
+	return &ContainerExecutor{root: local.root, cfg: cfg, local: local, newName: containerName}, nil
+}
+
+func containerName() (string, error) {
+	var b [12]byte
+	if _, err := rand.Read(b[:]); err != nil {
+		return "", err
+	}
+	return "patchbot-" + hex.EncodeToString(b[:]), nil
+}
+
+// Run executes spec.Argv inside a new container. Exit codes pass through;
+// timeout and cancellation report ExitCode -1 as the local executor does. A
+// runtime failure (exit 125) is returned as an error.
+func (e *ContainerExecutor) Run(ctx context.Context, spec Spec) (Result, error) {
+	result := Result{ExitCode: -1}
+	if e == nil || e.local == nil {
+		return result, errors.New("use NewContainerExecutor to configure container execution")
+	}
+	if spec.Timeout < 0 || spec.OutputCap < 0 {
+		return result, errors.New("execution limits must not be negative")
+	}
+	if spec.Timeout == 0 {
+		spec.Timeout = DefaultTimeout
+	}
+	workdir, err := e.workdir(spec.Dir)
+	if err != nil {
+		return result, err
+	}
+	if len(spec.Argv) == 0 || spec.Argv[0] == "" || (strings.Contains(spec.Argv[0], "/") && !path.IsAbs(spec.Argv[0])) {
+		return result, errors.New("a bare or absolute container executable is required")
+	}
+	for _, arg := range spec.Argv {
+		if strings.ContainsRune(arg, 0) {
+			return result, errors.New("argv must not contain NUL")
+		}
+	}
+	env, err := scrubEnv(spec.Env)
+	if err != nil {
+		return result, err
+	}
+	name, err := e.newName()
+	if err != nil {
+		return result, fmt.Errorf("container name: %w", err)
+	}
+	argv := containerArgv(e.cfg, name, e.root, workdir, containerEnv(env, e.cfg.Path), spec.Argv)
+	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
+	defer cancel()
+	// Kill by name as soon as the run is interrupted. Signals to the CLI alone
+	// would not reach the container's processes.
+	stop := context.AfterFunc(runCtx, func() { e.cleanup(name, "kill") })
+	result, runErr := e.local.Run(runCtx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: spec.Timeout, OutputCap: spec.OutputCap, Redactor: spec.Redactor})
+	interrupted := !stop() || result.TimedOut || result.Cancelled
+	// --rm removes a container that exited normally. rm -f covers interrupted
+	// runs (including a container created after an early kill attempt) and
+	// runtime failures.
+	if interrupted || result.ExitCode == runtimeErrorExit || runErr != nil {
+		e.cleanup(name, "rm", "-f")
+	}
+	if interrupted {
+		result.ExitCode = -1
+		return result, runErr
+	}
+	if runErr == nil && result.ExitCode == runtimeErrorExit {
+		runErr = errors.New("container runtime failed to start the command (exit 125)")
+		result.ExitCode = -1
+	}
+	return result, runErr
+}
+
+// cleanup runs a short runtime command against the named container. It is
+// best-effort: "no such container" is the expected outcome after --rm.
+func (e *ContainerExecutor) cleanup(name string, verb ...string) {
+	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
+	defer cancel()
+	argv := append(append([]string{e.cfg.Runtime}, verb...), name)
+	result, err := e.local.Run(ctx, Spec{Argv: argv, Dir: e.root, Env: e.cfg.HostEnv, Timeout: cleanupTimeout, OutputCap: 4096})
+	if err != nil || result.TimedOut {
+		slog.Warn("container cleanup failed", "container", name, "action", verb[0], "timed_out", result.TimedOut)
+	}
+}
+
+func (e *ContainerExecutor) workdir(dir string) (string, error) {
+	if !filepath.IsAbs(dir) || strings.ContainsRune(dir, 0) {
+		return "", errors.New("working directory must be absolute and inside the workspace")
+	}
+	resolved, err := canonicalRoot(dir)
+	if err != nil {
+		return "", err
+	}
+	if !inside(e.root, resolved) {
+		return "", errors.New("working directory symlink escapes workspace")
+	}
+	rel, err := filepath.Rel(e.root, resolved)
+	if err != nil {
+		return "", err
+	}
+	return path.Join(ContainerWorkdir, filepath.ToSlash(rel)), nil
+}
+
+// containerEnv rewrites host HOME/TMPDIR (private per-command directories that
+// do not exist in the container) to the container's tmpfs and fixes PATH.
+func containerEnv(env, containerPath []string) []string {
+	var home, tmp string
+	for _, entry := range env {
+		name, value, _ := strings.Cut(entry, "=")
+		switch name {
+		case "HOME":
+			home = value
+		case "TMPDIR":
+			tmp = value
+		}
+	}
+	remap := func(value string) string {
+		for _, prefix := range []string{home, tmp} {
+			if prefix == "" {
+				continue
+			}
+			if value == prefix {
+				return containerTmp
+			}
+			if rest, ok := strings.CutPrefix(value, prefix+"/"); ok {
+				return containerTmp + "/" + rest
+			}
+		}
+		return value
+	}
+	out := make([]string, 0, len(env)+1)
+	out = append(out, "PATH="+strings.Join(containerPath, ":"))
+	for _, entry := range env {
+		name, value, _ := strings.Cut(entry, "=")
+		if name == "PATH" {
+			continue
+		}
+		out = append(out, name+"="+remap(value))
+	}
+	return out
+}
+
+// containerArgv builds the complete runtime invocation. Arguments after the
+// image are passed to the container literally, never through a shell.
+func containerArgv(cfg ContainerConfig, name, hostRoot, workdir string, env, argv []string) []string {
+	out := []string{
+		cfg.Runtime, "run", "--rm",
+		"--name", name,
+		"--label", containerLabel,
+		"--pull", "never",
+		"--network", cfg.Network,
+		"--read-only",
+		"--tmpfs", containerTmp + ":rw,nosuid,nodev,size=" + cfg.TmpfsSize,
+		"--cap-drop", "ALL",
+		"--security-opt", "no-new-privileges",
+		"--pids-limit", strconv.Itoa(cfg.PidsLimit),
+		"--memory", cfg.Memory,
+		"--cpus", cfg.CPUs,
+		"--user", cfg.User,
+	}
+	if cfg.UserNS == "keep-id" {
+		uid, gid, _ := strings.Cut(cfg.User, ":")
+		out = append(out, "--userns", "keep-id:uid="+uid+",gid="+gid)
+	}
+	out = append(out, "--workdir", workdir, "-v", hostRoot+":"+ContainerWorkdir+":rw")
+	for _, entry := range env {
+		out = append(out, "-e", entry)
+	}
+	out = append(out, cfg.Image)
+	return append(out, argv...)
+}

exec
/bin/zsh -lc "cat internal/execx/env.go internal/execx/local_unix.go; rg -n 'ContainerConfig\\(|NewContainerExecutor|proxy|race|cleanup|symlink|token' internal/execx/container_test.go internal/runner internal/config docs" in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
 succeeded in 0ms:
package execx

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// TrustedPath is independent of the caller's PATH and excludes the checkout.
// These directories must be controlled by the operator, as must their binaries.
const TrustedPath = "/usr/bin:/bin"

var errInvalidEnv = errors.New("invalid explicit environment entry")

// MinimalEnv constructs a child environment without reading os.Environ. home and
// tmp should be private absolute per-run directories, created by the caller.
// Extras can override ordinary variables, but cannot replace the fixed base
// settings or introduce secret-like names. Dependency downloads remain allowed;
// automatic toolchain downloads and ambient Go configuration are disabled.
func MinimalEnv(home, tmp string, extra map[string]string) []string {
	return minimalEnv(home, tmp, extra, TrustedPath)
}

func minimalEnv(home, tmp string, extra map[string]string, path string) []string {
	values := map[string]string{
		"PATH": path, "HOME": home, "TMPDIR": tmp, "LANG": "C.UTF-8",
		"GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "", "GOWORK": "off",
		"GOCACHE": filepath.Join(home, "go-cache"), "GOPATH": filepath.Join(home, "go"), "CI": "1",
	}
	for name, value := range extra {
		if _, fixed := values[name]; !fixed && validEnv(name, value) {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		if validEnv(name, values[name]) {
			env = append(env, name+"="+values[name])
		}
	}
	return env
}

func validEnv(name, value string) bool {
	if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
		return false
	}
	upper := strings.ToUpper(name)
	for _, word := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD"} {
		if strings.Contains(upper, word) {
			return false
		}
	}
	for _, prefix := range []string{"AWS_", "GH_", "GITHUB_", "ANTHROPIC_", "OPENAI_", "GEMINI_", "GOOGLE_"} {
		if strings.HasPrefix(upper, prefix) {
			return false
		}
	}
	return true
}

func scrubEnv(env []string) ([]string, error) {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			return nil, errInvalidEnv
		}
		if validEnv(name, value) {
			clean = append(clean, entry)
		}
	}
	return clean, nil
}
//go:build unix

package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aryanpappu/patchbot/internal/secret"
)

// Run starts one process group. Timeout/cancel signals TERM, then KILL after the
// grace period, even if the leader has already exited. The leader is not reaped
// until all group signaling finishes: its zombie pins the PGID against reuse.
// Descendant parents (or the OS reaper) reap their own children.
func (e *LocalExecutor) Run(ctx context.Context, spec Spec) (result Result, runErr error) {
	started := time.Now()
	result.ExitCode = -1
	defer func() { result.Duration = time.Since(started) }()
	if err := e.validate(&spec); err != nil {
		return result, err
	}
	env, err := scrubEnv(spec.Env)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		setCancellation(ctx, &result)
		return result, nil
	}
	output := newCapWriter(spec.OutputCap)
	var sink io.Writer = output
	var filtered *redactedOutput
	if spec.Redactor != nil {
		filtered = &redactedOutput{writer: secret.NewWriter(output, spec.Redactor)}
		sink = filtered
	}
	// Equal Writer values make os/exec use a single shared pipe. This preserves
	// write order across stdout/stderr rather than merging two reader goroutines.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) // #nosec G204 -- argv is operator-owned, never a shell string.
	cmd.Dir, cmd.Env = spec.Dir, env
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.WaitDelay = e.WaitDelay
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = DefaultWaitDelay
	}
	interrupted, waitErr, processErr := runProcessGroup(ctx, cmd, e.GracePeriod)
	runErr = processErr
	if interrupted {
		setCancellation(ctx, &result)
	}
	if cmd.ProcessState == nil {
		return result, runErr
	}
	result.ExitCode = cmd.ProcessState.ExitCode()
	if filtered != nil {
		runErr = errors.Join(runErr, filtered.writer.Flush())
	}
	result.Output, result.TotalBytes, result.Truncated = output.snapshot()
	if filtered != nil {
		result.TotalBytes = filtered.total
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		if !errors.Is(waitErr, exec.ErrWaitDelay) || (!result.Cancelled && !result.TimedOut) {
			runErr = errors.Join(runErr, fmt.Errorf("wait for command: %w", waitErr))
		}
	}
	return result, runErr
}

// os/exec uses one copy goroutine for the shared stdout/stderr writer.
type redactedOutput struct {
	writer *secret.Writer
	total  int64
}

func (w *redactedOutput) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	return w.writer.Write(p)
}

func (e *LocalExecutor) validate(spec *Spec) error {
	if e.root == "" {
		return errors.New("use NewLocalExecutor to set a workspace root")
	}
	if e.GracePeriod < 0 || e.WaitDelay < 0 || spec.Timeout < 0 || spec.OutputCap < 0 {
		return errors.New("execution limits must not be negative")
	}
	if spec.Timeout == 0 {
		spec.Timeout = DefaultTimeout
	}
	if spec.OutputCap == 0 {
		spec.OutputCap = DefaultOutputCap
	}
	if spec.OutputCap < MinOutputCap {
		return fmt.Errorf("output cap must be at least %d bytes", MinOutputCap)
	}
	if len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) {
		return errors.New("an absolute executable is required")
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("argv must not contain NUL")
		}
	}
	if !filepath.IsAbs(spec.Dir) || strings.ContainsRune(spec.Dir, 0) {
		return errors.New("working directory must be absolute and inside the workspace")
	}
	resolved, err := canonicalRoot(spec.Dir)
	if err != nil {
		return err
	}
	if !inside(e.root, resolved) {
		return errors.New("working directory symlink escapes workspace")
	}
	// OpenRoot checks containment using the same primitive as workspace tools.
	root, err := os.OpenRoot(e.root)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	rel, err := filepath.Rel(e.root, resolved)
	if err != nil {
		return err
	}
	dir, err := root.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("open working directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close working directory: %w", err)
	}
	spec.Dir = resolved
	return nil
}

func setCancellation(ctx context.Context, result *Result) {
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	result.Cancelled = errors.Is(ctx.Err(), context.Canceled)
}
internal/execx/container_test.go:19:func baseContainerConfig() ContainerConfig {
internal/execx/container_test.go:62:			cfg := baseContainerConfig()
internal/execx/container_test.go:117:			cfg := baseContainerConfig()
internal/execx/container_test.go:204:	cfg := baseContainerConfig()
internal/execx/container_test.go:207:	e, err := NewContainerExecutor(root, cfg)
internal/execx/container_test.go:212:	e.local.GracePeriod = 100 * time.Millisecond
internal/execx/container_test.go:251:			t.Fatalf("token reached the container: %s", joined)
internal/execx/container_test.go:323:func TestNewContainerExecutorRejectsInvalid(t *testing.T) {
internal/execx/container_test.go:325:	cfg := baseContainerConfig()
internal/execx/container_test.go:327:	if _, err := NewContainerExecutor(t.TempDir(), cfg); err == nil {
internal/execx/container_test.go:330:	cfg = baseContainerConfig()
internal/execx/container_test.go:335:	if _, err := NewContainerExecutor(dir, cfg); err == nil {
internal/runner/publish.go:237:	tokens := "unknown"
internal/runner/publish.go:239:		tokens = fmt.Sprintf("%d input / %d output", *run.InputTokens, *run.OutputTokens)
internal/runner/publish.go:241:	fmt.Fprintf(&b, "\nUsage: %s tokens; cost: %s.\n", tokens, cost)
docs/tasks/M9.md:21:results; mixed text+tool call; empty tool result; malformed arguments; max tokens with partial call; refusal/blocked;
docs/tasks/M9.md:24:Must pass: gofmt -l . empty, go vet ./..., golangci-lint run 0 issues, go test -race ./... (cache overrides under
internal/config/load.go:369:// so github_token, apiKey and client-secret are all caught.
internal/config/load.go:377:// "token" counts unless it is the plural counter, as in max_total_tokens.
internal/config/load.go:389:		i := strings.Index(rest, "token")
internal/config/load.go:393:		rest = rest[i+len("token"):]
docs/DECISIONS.md:8:| 2026-11-10 | Go 1.27 (local 1.27.3) | Includes the GO-2026-4970 `os.Root` symlink fix the path jail depends on |
docs/DECISIONS.md:13:| 2026-11-10 | Follow-up: `execx.stopGroup` should poll for group exit instead of always waiting the full grace period | Minor latency on cancel; not a safety issue |
docs/DECISIONS.md:28:  world writes. Validation resolves existing prefixes and symlinks, including
docs/DECISIONS.md:51:- `[executor] kind = "container"` runs every repository command in a fresh rootless podman/docker container (digest-pinned image, `--network none`, read-only rootfs + tmpfs, `--cap-drop ALL`, no-new-privileges, pids/memory/cpu limits, non-root uid, only the workspace mounted); default stays `local`, git and file tools stay host-side and the GitHub token never enters the container.
docs/M12-REPORT.md:30:- `internal/runner/heartbeat_test.go`: deterministic heartbeat/parking race.
docs/M12-REPORT.md:81:6. Usage events plus an accounting artifact restore conservative cost/token
docs/M12-REPORT.md:85:   dollar ceiling still applies on resume. Unpriced models use the token cap,
docs/M12-REPORT.md:141: go test -race ./...
docs/M12-REPORT.md:173:`git diff --check` also passes. The first complete race run exposed a
docs/M12-REPORT.md:174:heartbeat/parking CAS race; it was fixed, covered by a deterministic regression,
docs/M12-REPORT.md:175:and the final full race run above passes.
docs/tasks/M13.md:11:  When the token is set, require it as a bearer token (constant-time compare) on every request.
docs/tasks/M13.md:26:  - waits for in-flight runs on graceful shutdown (SIGINT/SIGTERM), with a timeout
docs/tasks/M13.md:43:  - Use a consistent error JSON shape `{"error":{"kind","message"}}`, with no stack traces and no raw
docs/tasks/M13.md:58:- non-loopback bind refused without a token, and bearer required with one
docs/tasks/M13.md:61:- graceful shutdown
docs/tasks/M13.md:62:- secrets never appearing in any response (seed a fake token and grep responses)
docs/tasks/M13.md:65:`make ci` (gofmt, vet, golangci-lint 0 issues, `go test -race ./...`, build). Use `-race`-safe code throughout.
internal/config/validate_test.go:217:	// No token shape: only the loaded-secret list can catch it.
internal/config/validate_test.go:259:// known price of 0 for the other, so those tokens were free in the budget.
internal/config/validate_test.go:293:			// cannot get a price that counts some tokens as free.
docs/tasks/M15.md:15:   group is gone instead of waiting the full grace period. Keep the SIGKILL escalation. Test it with a fast-
docs/tasks/M15.md:21:   - the GitHub token env var, data dir permissions (0700) and database migration state
docs/tasks/M15.md:47:  git token handling, redaction, web protections), and the extension seams from PLAN section 10.
docs/tasks/M8.md:10:Target the Chat Completions API (not the Responses API): assistant tool_calls with JSON-string arguments, tool messages with tool_call_id, system message role. A per-instance quirks table (config driven or by base_url detection) handles: servers that reject or ignore parallel_tool_calls; max_tokens vs max_completion_tokens naming; missing usage; null content alongside tool_calls; arguments that are an object instead of a string (some local servers); finish_reason values stop, tool_calls, length, content_filter, function_call (legacy). ListModels via GET /models (OpenAI, OpenRouter, Ollama /v1/models, LM Studio, Groq). Base URL required, API key optional (Ollama/LM Studio need none). Fixtures: synthetic, one set per server family (openai, openrouter, ollama, groq) incl. the quirks.
docs/tasks/M8.md:21:results; mixed text+tool call; empty tool result; malformed arguments; max tokens with partial call; refusal/blocked;
docs/tasks/M8.md:24:Must pass: gofmt -l . empty, go vet ./..., golangci-lint run 0 issues, go test -race ./... (cache overrides under
docs/tasks/M14.md:24:     - phase timeline, live log over SSE with automatic resume via `Last-Event-ID`, and token and cost
docs/tasks/M14.md:49:- the token is never rendered into HTML
docs/tasks/M14.md:53:`make ci` (gofmt, vet, golangci-lint 0 issues, `go test -race ./...`, build).
docs/tasks/M7.md:15:   Stop reasons end_turn/tool_use/max_tokens/refusal/unknown->other. Usage incl. cache creation/read tokens. Errors to
docs/tasks/M7.md:23:   both results; mixed text+tool_use; thinking round trip; max_tokens with a partial tool call; refusal; 429 retry-after;
docs/tasks/M7.md:30:gofmt -l . empty, go vet ./..., golangci-lint run 0 issues, go test -race ./..., make build (use /private/tmp cache
docs/tasks/M6.md:27:  tokens: drop truncated calls, nudge. Refusal -> EndRefusal. Stuck detector (same name+args hash 3x -> warn, again -> EndStuck).
docs/tasks/M6.md:42:  <= max. Code() refuses without a token, and the code registry (write tools) is only built after the token check. This
docs/tasks/M6.md:43:  is hard rule 1 of the product: make it impossible to call Code without passing the gate (unexported token type).
docs/tasks/M6.md:48:## Tests (fake provider + temp workspace + fake executor + in-memory fakes of Recorder; -race)
docs/tasks/M6.md:49:Happy refine -> spec; refine with open questions -> needs_info; revise with notes produces v2; Code without token impossible
docs/tasks/M6.md:58:gofmt -l . empty, go vet ./..., golangci-lint run 0 issues, go test -race ./... (cache overrides under /private/tmp if
internal/config/llm.go:13:// micro-USD per million tokens here, once, so that no float reaches the cost
docs/tasks/M12.md:20:- Code phase: start only from `approved`. Obtain the approval token from the agent gate (never construct
docs/tasks/M12.md:57:`make ci`: gofmt, vet, `golangci-lint` with 0 issues, `go test -race ./...`, build.
docs/M15-REPORT.md:88:   refuses public data directories, symlink data directories, nonregular or
docs/M15-REPORT.md:113:   including rejected config-layer keys and injected server GitHub tokens.
docs/M15-REPORT.md:136:    resolves existing prefixes and dangling symlinks. Policy copies the list;
docs/M15-REPORT.md:139:12. Cancellation always consumed the full grace period. The executor now checks
docs/M15-REPORT.md:141:    It retains SIGKILL at the grace deadline. Fast/cooperative and slow/TERM-
docs/M15-REPORT.md:142:    resistant children are tested, alongside existing descendant cleanup tests.
docs/M15-REPORT.md:199:go test -race ./...
docs/M15-REPORT.md:354:Usage: 300 input / 150 output tokens; cost: 0 micro-USD.
docs/M15-REPORT.md:368:workspace cleanup, zero GitHub posts and zero remote patchbot branches. Its
docs/planning/BRIEF.md:23:   - SQLite store for runs, steps, tokens and cost (modernc.org/sqlite, no CGO).
docs/planning/BRIEF.md:27:5. Safety: file tools jailed to the workspace (no path escape, no symlink escape), run_cmd allowlist + timeout + output cap,
docs/planning/BRIEF.md:28:   per-run token/dollar budget cap, never push to the default branch, secrets only from env (never stored in DB/logs, redact in logs),
docs/planning/BRIEF.md:46:   diff, test output, tokens and cost. Edits made in the UI also need approval, and every action is recorded in the run history.
docs/M13-FINDINGS-REPORT.md:9:| 2. Healthy queued jobs are recovered as interrupted | Starts persist an initial admission heartbeat. Revision and approval/execution decisions establish ownership when committed. One manager ticker refreshes every admitted queued/running job, continuing through graceful shutdown draining. Store heartbeats now include queued runs. | `TestQueuedJobsKeepOwnershipBeforeWork` holds both workers, queues start/revise/approve, observes a heartbeat after admission, then constructs another runner against a separate SQLite connection with a controlled recovery cutoff. The queued job must remain active. |
docs/M13-FINDINGS-REPORT.md:10:| 3. Reapproval commits without a worker | Approval denial resets the phase to `refine`. `ApproveAndExecute` reserves queue capacity before committing approval and its clone execution claim together. The HTTP API and GitHub sync use this combined operation; the reservation guarantees dispatch even if a request disconnects or graceful shutdown starts. | `TestReapprovalDispatchesAfterQueuedExpiry` advances the clock while approved work queues, then verifies phase reset and a second accepted dispatch. `TestReapprovalAfterCloneOrBaselineDenial` covers expiry and issue-freshness denial at clone and baseline, followed by successful reapproval/execution. `TestApprovalAdmissionFailureLeavesDecisionUnchanged` and queue-full API assertions verify no undispatchable decision is recorded. |
docs/M13-FINDINGS-REPORT.md:29:  `go test -race ./...`, and the production build all passed.
docs/M13-FINDINGS-REPORT.md:30:- The final listener-synchronization assertion also passed under `-race`;
internal/runner/runner_test.go:115:	tokens := runner.Token{Value: secret.New("fixture-credential")}
internal/runner/runner_test.go:116:	client, err := github.New(server.Client(), server.URL, tokens)
internal/runner/runner_test.go:121:	f.opts = runner.Options{GitHub: client, Tokens: tokens, BotLogin: "patchbot", Clock: f.clock, HeartbeatInterval: 5 * time.Millisecond, Constructors: llm.Constructors{"anthropic": func(llm.ProviderConfig) (llm.Provider, error) { return f.p, nil }}, Clone: func(ctx context.Context, opts gitx.CloneOptions) (*gitx.Repo, error) {
internal/config/executor.go:25:func (c *Config) ContainerConfig(lookup func(string) (string, bool), excluded ...string) (execx.ContainerConfig, error) {
internal/config/config.go:78:	MaxOutputTokens int   `toml:"max_output_tokens"`
internal/config/config.go:79:	MaxTotalTokens  int64 `toml:"max_total_tokens"`
internal/config/config.go:173:// million tokens.
internal/config/config.go:178:// would then stop counting output tokens. Write 0 for a rate that is free.
internal/config/config.go:284:// well as token-shaped text. Diagnostics about the config pass through it,
internal/config/config.go:306:// or symlink. Doctor reports the same policy without changing the filesystem.
internal/config/show_test.go:129:		{"user and password", "https://deploy:Zq7pLx2MvB9c@proxy.example.com/v1", `base_url = "https://proxy.example.com/v1 (credentials removed)"`, []string{"deploy", "Zq7pLx2MvB9c"}},
internal/config/show_test.go:130:		{"token as the user", "https://Zq7pLx2MvB9c@proxy.example.com/v1", `"https://proxy.example.com/v1 (credentials removed)"`, []string{"Zq7pLx2MvB9c"}},
internal/config/show_test.go:138:		{"encoded slash is unchanged", "https://proxy.example/tenant%2Fname/v1", `base_url = "https://proxy.example/tenant%2Fname/v1"`, nil},
internal/config/show_test.go:139:		{"encoded path with credentials", "https://deploy:Zq7pLx2MvB9c@proxy.example/tenant%2fname/v1?key=Rt5WnYh3KdF8#secret", `base_url = "https://proxy.example/tenant%2fname/v1 (credentials removed) (query removed)"`, []string{"deploy", "Zq7pLx2MvB9c", "Rt5WnYh3KdF8", "#secret"}},
docs/M17-REPORT.md:25:  and CLI process-group cleanup are shared code. On timeout/cancel the container is killed by name
docs/M17-REPORT.md:33:  into the container. The GitHub token is never in either environment.
docs/M17-REPORT.md:39:  through `tools.ExecuteCommand` and so through the selected executor. Git (clone/push with token) and file
docs/M17-REPORT.md:51:`make ci` green (fmt-check, vet, golangci-lint 0 issues, `go test -race ./...`, build).
docs/M17-REPORT.md:53:New tests (all pass, `-race`):
docs/M17-REPORT.md:62:| `execx.TestContainerExecutorRunWithFakeRuntime` | test binary as fake runtime: exit pass-through, workdir mapping, token filtered from `-e`, 125 -> error + `rm -f`, timeout and cancel -> `kill <name>` + `rm -f <name>`, symlink/dir escapes rejected |
docs/M17-REPORT.md:78:  go test -race -count=1 -run Integration -v ./internal/execx/
internal/config/executor_test.go:25:	if _, err := c.ContainerConfig(func(string) (string, bool) { return "", false }); err == nil {
docs/M14-REPORT.md:20:  status-bound actions, artifacts, token entry and fetch-based resumable SSE.
docs/M14-REPORT.md:39:  diagnostic, protected shells without token/private data and Host/API guards.
docs/M14-REPORT.md:47:- `internal/cli/serve.go`: update serve help for the page/token flow.
docs/M14-REPORT.md:69:   asset routes are therefore public. When token protection is enabled, shells
docs/M14-REPORT.md:71:   Every API, SSE and artifact request still requires the bearer token. Host
docs/M14-REPORT.md:87:   remain authoritative when a control races another action.
docs/M14-REPORT.md:88:7. New runs default to dry-run checked. Unknown cost/token totals stay unknown.
docs/M14-REPORT.md:108:go test -race ./...
docs/M14-REPORT.md:150:all three render paths, dry-run JSON submission and filter submission, token
docs/M14-REPORT.md:158:detail (token entry): render and API smoke passed; bearer SSE chunking and Last-Event-ID terminal replay passed
docs/M14-REPORT.md:216:4. Open `http://127.0.0.1:8080/runs/new`. If prompted, enter the UI token once;
docs/planning/PLAN-fable.md:10:- GitHub auth in Step 1 is a fine-grained personal access token in `GITHUB_TOKEN`. A GitHub App comes in Step 5.
docs/planning/PLAN-fable.md:34:     view the log, diff, test output, tokens and cost. Every action is written to the run history.
docs/planning/PLAN-fable.md:41:6. SQLite (modernc, no CGO) stores runs, steps, tokens and cost.
docs/planning/PLAN-fable.md:43:   timeouts, output caps, token and dollar budgets, never push to the default branch, env-only secrets with
docs/planning/PLAN-fable.md:57:- No token-level streaming from providers. The UI streams per-step events, which is enough for a live log.
docs/planning/PLAN-fable.md:58:- No login. The server binds to loopback and refuses a non-loopback address unless a UI token is set.
docs/planning/PLAN-fable.md:84:│   │   ├── serve.go                # `patchbot serve`: wires store, runner, bus, HTTP server, graceful shutdown
docs/planning/PLAN-fable.md:96:│   │   ├── redactor.go             # Redactor: known values plus token-shaped regexes
docs/planning/PLAN-fable.md:143:│   │   ├── workspace.go            # Workspace interface, Manager (create, cleanup)
docs/planning/PLAN-fable.md:149:│   │   ├── jail_test.go            # escape attempts incl. symlinks
docs/planning/PLAN-fable.md:164:│   │   ├── limits.go               # Limits and Budget (turns, tokens, micro-USD, wall clock)
docs/planning/PLAN-fable.md:303:    StopMaxTokens StopReason = "max_tokens"
docs/planning/PLAN-fable.md:310:    OutputTokens     int64 // includes reasoning tokens
docs/planning/PLAN-fable.md:410:| `MaxTokens` | `max_tokens` (required by the API; default 8192, see note below) |
docs/planning/PLAN-fable.md:411:| Usage | `input_tokens` -> Input, `output_tokens` -> Output, `cache_read_input_tokens` -> CacheRead, `cache_creation_input_tokens` -> CacheWrite |
docs/planning/PLAN-fable.md:413:Stop reasons: `end_turn`, `stop_sequence` -> `StopEndTurn`. `tool_use` -> `StopToolUse`. `max_tokens`,
docs/planning/PLAN-fable.md:426:- Requests are non-streaming. Keep `max_tokens` at 16384 or below, because the SDK refuses non-streaming
docs/planning/PLAN-fable.md:445:| `MaxTokens` | `max_tokens`, or `max_completion_tokens` when `max_tokens_field = "max_completion_tokens"` in config |
docs/planning/PLAN-fable.md:446:| Usage | Input = `prompt_tokens - prompt_tokens_details.cached_tokens`, CacheRead = `cached_tokens`, Output = `completion_tokens`, Reasoning = `completion_tokens_details.reasoning_tokens` |
docs/planning/PLAN-fable.md:466:`(?i)context (length|window)|maximum context|too many tokens`.
docs/planning/PLAN-fable.md:504:  `INVALID_ARGUMENT` with a token-limit message -> context_too_long.
docs/planning/PLAN-fable.md:601:    Push(ctx context.Context, token secret.Secret, force bool) error
docs/planning/PLAN-fable.md:702:finish race-free, and it is the hook the Step 3 queue uses to claim work.
docs/planning/PLAN-fable.md:814:// when it lost a race.
docs/planning/PLAN-fable.md:916:A waiting run uses no process, no workspace and no tokens. It is only a database row, so it survives
docs/planning/PLAN-fable.md:949:2. **Preflight.** `GetRepo` and `GetIssue`. Fail if the issue is closed, is a pull request, or the token has
docs/planning/PLAN-fable.md:1000:`estimate = (last call's input tokens + MaxOutputTokens)` priced at the model's rates, or just the token
docs/planning/PLAN-fable.md:1001:count for the token budget. The run stops if `spent + estimate > cap`. Both budgets are checked. When the
docs/planning/PLAN-fable.md:1002:model has no price entry, only the token budget applies, and the run logs one warning at the start.
docs/planning/PLAN-fable.md:1073:   model's summary, test status with attempt count, baseline status, provider and model, tokens and cost,
docs/planning/PLAN-fable.md:1085:plain words, how far the run got, and no stack traces or raw provider messages.
docs/planning/PLAN-fable.md:1232:max_output_tokens  = 8192
docs/planning/PLAN-fable.md:1233:max_total_tokens   = 2000000      # per run, across refine, revisions and code
docs/planning/PLAN-fable.md:1305:`headers` (table), `max_tokens_field`, `system_role`, `models`.
docs/planning/PLAN-fable.md:1325:- A config key named `api_key`, `token`, `secret` or `password` at any depth is an error that tells the user
docs/planning/PLAN-fable.md:1347:type Price struct { // micro-USD per million tokens, so integer math throughout
docs/planning/PLAN-fable.md:1366:that run, the token cap still is, and the run logs a warning at the start. Costs are always labelled as
docs/planning/PLAN-fable.md:1413:    input_tokens     INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1414:    output_tokens    INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1415:    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1416:    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1443:    input_tokens  INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1444:    output_tokens INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1445:    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1446:    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
docs/planning/PLAN-fable.md:1586:| `usage` | `turn, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros, total_cost_micros, budget_micros, total_tokens, budget_tokens` |
docs/planning/PLAN-fable.md:1596:1. **Header bar.** Name, version, a GitHub token indicator (present or missing, never the value), count of
docs/planning/PLAN-fable.md:1604:3. **Runs list.** Table: status badge, repo#issue, mode, provider/model, turns, tokens, cost, started,
docs/planning/PLAN-fable.md:1608:   button while active. A counters row: turns `12/40`, tokens, cost against budget as a bar, test attempts.
docs/planning/PLAN-fable.md:1615:   GitHub token each get a specific message with the fix.
docs/planning/PLAN-fable.md:1641:path, or a symlink, and there is no check-then-use race.
docs/planning/PLAN-fable.md:1652:4. **Symlink alias check.** A symlink inside the checkout could point at `.git`. Before a write, the target
docs/planning/PLAN-fable.md:1654:   existing symlink, the link itself is never replaced: `write_file` refuses when `Lstat` shows a symlink.
docs/planning/PLAN-fable.md:1657:No tool creates symlinks. `os.Root` does not stop a process started by `run_cmd`, which is the limit stated
docs/planning/PLAN-fable.md:1695:  `SIGTERM` to the group, 5 seconds grace, then `SIGKILL` to the group, with `cmd.WaitDelay` set so a
docs/planning/PLAN-fable.md:1710:  token is ever in the environment of a command the model starts or of the test command.
docs/planning/PLAN-fable.md:1711:- The git token is given only to the harness's own `clone` and `push` processes, through
docs/planning/PLAN-fable.md:1714:- `secret.Redactor` replaces (a) every known secret value and its base64 form, and (b) token-shaped strings:
docs/planning/PLAN-fable.md:1715:  `ghp_`, `gho_`, `ghs_`, `github_pat_`, `sk-ant-`, `sk-`, `AIza`, `Bearer <long token>`, and PEM private
docs/planning/PLAN-fable.md:1718:  GitHub. A test puts a fake token through each path and asserts it never appears.
docs/planning/PLAN-fable.md:1726:before the harness uses it with a token.
docs/planning/PLAN-fable.md:1747:The README also recommends a fine-grained token limited to the target repositories (contents, pull
docs/planning/PLAN-fable.md:1780:- With a UI token set, the API needs `Authorization: Bearer`, compared with `subtle.ConstantTimeCompare`.
docs/planning/PLAN-fable.md:1809:| `workspace` (jail) | `..`, absolute path, NUL, symlink to outside, symlink to `.git`, symlinked parent directory, `.GIT` case variant, FIFO, oversized write, workflow path |
docs/planning/PLAN-fable.md:1811:| `events` | Ordering, slow subscriber dropped without blocking the publisher, cancel removes the subscriber, run under `-race` |
docs/planning/PLAN-fable.md:1840:end of turn without submit (nudge, then accept); truncated output; refusal; max turns; token budget; dollar
docs/planning/PLAN-fable.md:1857:carries tool results, max-tokens stop, refusal or safety stop, each error status, list models. Extra:
docs/planning/PLAN-fable.md:1865:are reviewed by hand before commit. A CI step greps `testdata/` for token-shaped strings.
docs/planning/PLAN-fable.md:1883:behind the `e2e` build tag and needs real tokens.
docs/planning/PLAN-fable.md:1893:   `go test -race -shuffle=on -coverprofile=cover.out ./...`. Coverage is reported, with a floor of 75%
docs/planning/PLAN-fable.md:1900:Makefile targets mirror CI: `build`, `test`, `test-race`, `lint`, `fmt`, `cover`, `vuln`, `run`, `serve`,
docs/planning/PLAN-fable.md:1938:| M1 | Config, secrets, logging | `config`, `secret`, `logging`, `patchbot config check\|show`, `patchbot.example.toml` | Precedence tests pass. A file with `api_key = ...` is rejected. A log line with a known token prints `[redacted]`. `config show` lists each value's source. |
docs/planning/PLAN-fable.md:1943:| M6 | Agent loop | `agent` | All fake-provider cases in 9.2 pass under `-race`. Steps and events are written through interfaces (fakes in tests). |
docs/planning/PLAN-fable.md:1965:| No sandbox in Step 1 | Tests run arbitrary code as the local user | Stated plainly in the README. Scrubbed env, git integrity checks, least-privilege token. Step 4 fixes it properly. Consider making `serve` refuse non-loopback binds entirely until Step 4. |
docs/planning/PLAN-fable.md:1997:   accepts any repo the token can push to. An allowlist is safer once webhooks arrive, and may be worth
docs/M13-REPORT.md:16:  headers, HTTP timeouts, maintenance ticker, serving and graceful shutdown.
docs/M13-REPORT.md:39:  cancellation, disconnected browsers, graceful serving/shutdown and timeout,
docs/M13-REPORT.md:70:  credential registration, including short UI tokens, with a regression test.
docs/M13-REPORT.md:119:    time. Only loopback IP literals and localhost can bind without a token;
docs/M13-REPORT.md:122:    proxy host headers or additional trusted origins are enabled.
docs/M13-REPORT.md:152:go test -race ./...
docs/M13-REPORT.md:186:0 and documents the address/config flags and bearer-token requirement. No live
internal/runner/phases.go:58:	token, err := r.opts.Tokens.Token(ctx)
internal/runner/phases.go:60:		return nil, errors.New("GitHub token unavailable; set GITHUB_TOKEN")
internal/runner/phases.go:70:		repo, cloneErr = r.opts.Clone(ctx, gitx.CloneOptions{Dir: filepath.Join(dir, "repo"), Host: "github.com", Owner: ref(run.Repo).Owner, Name: ref(run.Repo).Name, Branch: meta.DefaultBranch, DefaultBranch: meta.DefaultBranch, Token: token, TrustedPath: r.cfg.Limits.TrustedPath, ExcludedRoots: []string{r.cfg.DataDir()}})
internal/runner/phases.go:411:	token, err := agent.Check(ctx, r.db, run.ID, agent.IssueHash(issue.Title, issue.Body), r.gateOptions())
internal/runner/phases.go:421:	result, codeErr := loop.Code(ctx, token, agent.CodeRequest{RunID: run.ID, Title: issue.Title, Body: issue.Body, Changes: env.repo, Baseline: baselineInfo, TestCommand: command, Session: session, Limits: limits.Agent, OnPhase: func(c context.Context, phase store.Phase) error {
docs/PROGRESS.md:29:  Process-group cleanup does not contain deliberate group escapes.
docs/PROGRESS.md:33:- No prices are shipped. Unknown costs use token limits. Provider/model IDs in
docs/planning/PLAN-codex.md:62:The brief's Go 1.25.4 environment is not a security pin. Check Go security advisories before implementation and use a patched toolchain. In particular, [GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970) documents an `os.Root` symlink escape fixed in Go 1.25.12 on that release line. If the 1.25 line is no longer supported, use a supported patched toolchain while preserving the requested language compatibility where possible.
docs/planning/PLAN-codex.md:70:Request includes model ID, separate system instructions, ordered conversation messages, tool definitions, and a maximum output-token allowance. Response includes ordered content blocks, normalized stop reason, usage with explicit known/unknown fields, provider request ID, and optional in-memory opaque continuation metadata. Provider errors expose category, retryability, HTTP status, Retry-After, and sanitized diagnostic details.
docs/planning/PLAN-codex.md:80:| Anthropic | Separate system instructions; ordered assistant text/`tool_use` blocks; user `tool_result` blocks matched by `tool_use_id`, with error flag. Return all results before further conversational text. | Multiple calls, mixed blocks, `tool_use` vs normal completion, token truncation, refusal, unknown blocks. Preserve required signed thinking/continuation blocks in memory if enabled; default advanced thinking features off until fixtures prove replay. |
docs/planning/PLAN-codex.md:81:| Gemini | Native content roles and `functionCall` / `functionResponse` parts; system instruction separate; result uses function name and structured response. | Parallel calls, repeated function names, optional/version-dependent call IDs, blocked/empty candidates, malformed arguments, token limits, and opaque thought signatures attached to the correct part. Preserve ordering and original parts needed for replay. Never synthesize or discard signatures. |
docs/planning/PLAN-codex.md:90:3. Execute tool batches sequentially in provider order. Do not introduce parallel writes or read/write races. Every valid call gets a result, including tool execution errors. Missing IDs may receive deterministic internal IDs only when the provider protocol permits name/order correlation; ambiguous or duplicate provider IDs are protocol errors.
docs/planning/PLAN-codex.md:106:Track input, output, cached, and reasoning tokens separately when available, plus raw usage categories needed to avoid double counting. Costs use configured, dated per-model rates and integer micro-USD with conservative rounding; no floating-point billing arithmetic. Display costs as estimates, not invoices.
docs/planning/PLAN-codex.md:108:Before each request, reserve an upper bound for its possible input and maximum output cost and tokens, including provider-specific billed categories. Reduce output allowance to remaining budget. If no reliable local token bound/counting method exists, use a documented conservative bound with model context ceiling, or refuse budget-enforced execution. Do not rely on `characters/4` as a hard limit. Each network attempt must fit its own reservation; ambiguous charges retain their reservation. Reconcile only with authoritative usage. Missing usage keeps the conservative reservation and marks accounting uncertain.
docs/planning/PLAN-codex.md:125:6. If tests fail and repair attempts remain, append trusted verification status plus untrusted/redacted output to the conversation, increment repair count, and re-enter the agent. Each repair session still shares the original total time/token/dollar/turn budgets. Any post-test edit invalidates that test result and requires a new verification.
docs/planning/PLAN-codex.md:139:Use a per-run directory mode 0700 under a configured data root and open the checkout with `os.OpenRoot`. Every model path is a relative slash-separated path. Reject empty/NUL/absolute/volume/UNC paths, `..` components, excessive length, and protected components before opening. Never trust a `filepath.Clean` plus string-prefix check or an `EvalSymlinks`-then-open sequence; those are race-prone. [Go's traversal-resistant file API explanation](https://go.dev/blog/osroot) describes the intended primitive and limitations.
docs/planning/PLAN-codex.md:141:Use Root-relative operations for reads, traversal, creation, rename, and removal. Accept symlinks only when the final operation remains confined by Root; reject external targets, loops, and unsupported file kinds. Writes use a same-directory temporary regular file and Root-relative rename, preserving appropriate permission bits. `apply_edit` requires an expected old-content hash or unique exact match; ambiguous/stale edits fail rather than silently changing the wrong section. Do not expose unrestricted deletion or file mode changes initially.
docs/planning/PLAN-codex.md:143:Deny `.git` and any case-equivalent protected path, credential/config directories, `.env` variants, and operator-configured sensitive paths for read, write, list, and search. For an allowed symlink alias to a protected path, containment alone is insufficient: inspect root-relative symlink resolution and reapply policy at each component; reject ambiguous aliases. Serialize file tools with command execution to reduce races; require race-resistant access for the outside-workspace boundary. Refuse a checkout that contains unsafe special files or suspicious hard links; clones should not use shared objects/hard-linked local sources. Document hard-link/mount limits rather than claiming Root controls them.
docs/planning/PLAN-codex.md:145:Implement bounded search in Go using Root-relative traversal, avoiding an external recursive search that follows links differently. Cap directory entries, file count, bytes scanned, returned matches, and individual file size. Skip binary files, denied paths, and symlink traversal by default. Tests must prove all file tools, not only `read_file`, enforce the same policy.
docs/planning/PLAN-codex.md:151:Resolve working directories through the jail. Launch with `exec.CommandContext`, a minimal explicit environment, isolated HOME/cache/temp directories, no inherited API/GitHub tokens, no interactive stdin, and fixed trusted PATH. For Go, constrain hidden behavior such as GOENV, GOFLAGS, workspace selection, and automatic toolchain download; document dependency-download policy. Approved tests may still execute edited code and scripts. Inspecting the command name does not prevent host reads, network egress, fork bombs, or generated files outside the checkout. Step 1 must say this plainly and use a dedicated OS account/host with minimal privileges for practical isolation.
docs/planning/PLAN-codex.md:155:Separate the deterministic Git operations from the model tool registry. Disable hooks using a controlled empty hooks directory, ignore inherited Git/global config, reject submodules by default, disable recursive checkout, and avoid Git filters/external helpers from untrusted configuration. No token in clone URLs or command arguments. A narrow temporary credential helper may serve only the configured GitHub host during trusted clone/push; remove it and its environment before any test/model command. Never store credentials in `.git/config`.
docs/planning/PLAN-codex.md:167:Centralize redaction before persistence, SSE, `slog`, tool results returned to the LLM, error formatting, and publication bodies. Match known secret values plus common token/private-key formats; suppress authorization headers and credential-bearing URL components. A streaming redactor must retain an overlap buffer so split output chunks cannot leak a token. Sanitize terminal control characters. Debug mode does not enable raw HTTP bodies. Do not log full issue/file content by default.
docs/planning/PLAN-codex.md:173:Bind `127.0.0.1` by default. Require a bootstrap session established using an env-provided UI secret, an HttpOnly SameSite cookie, CSRF tokens, and Origin/Host checks for mutations. Authenticate SSE and read endpoints too. Do not place tokens in SSE URLs. Remote binding requires explicit operator configuration and a trusted TLS/auth proxy; Step 1 does not claim multi-user authorization.
docs/planning/PLAN-codex.md:186:| `runs` | id TEXT PRIMARY KEY; host, owner, repo TEXT with canonical case normalization; issue_number INTEGER CHECK > 0; mode, status, phase TEXT; dry_run INTEGER CHECK IN (0,1); provider_instance, model TEXT; config_snapshot_json TEXT (safe fields only); prompt_version TEXT; base_branch, base_sha, branch, commit_sha TEXT nullable; workspace_id TEXT nullable; created_at, started_at, finished_at INTEGER UTC milliseconds; repair_count INTEGER; token_limit INTEGER; dollar_limit_microusd INTEGER; error_code, error_summary TEXT nullable; final_test_outcome TEXT; pr_number INTEGER nullable; pr_url TEXT nullable |
docs/planning/PLAN-codex.md:189:| `llm_calls` | id INTEGER PRIMARY KEY; run_id REFERENCES runs(id); step_id REFERENCES steps(id); request_seq, http_attempt INTEGER; provider_request_id TEXT nullable; input_tokens, output_tokens, cached_tokens, reasoning_tokens INTEGER nullable; usage_known INTEGER; reserved_tokens INTEGER; reserved_cost_microusd INTEGER; estimated_cost_microusd INTEGER nullable; accounting_status TEXT; rate_snapshot_json TEXT; latency_ms INTEGER; finish_reason TEXT; UNIQUE(run_id, request_seq, http_attempt) |
docs/planning/PLAN-codex.md:199:Events are bounded per record and per run (for example 8 KiB per chunk and 10 MiB retained live-log data). On quota exhaustion keep lifecycle/error/usage events and emit one dropped-output marker; do not let command chatter fill disk. Retain event metadata longer than workspace contents. A cleanup command removes expired artifacts and events while retaining run summaries/publication IDs. Never delete active work. Document WAL-aware backup and restore; copying only the DB file during writes is not a backup procedure.
docs/planning/PLAN-codex.md:217:SSE events use per-run monotonic sequence as `id`, named event types (`status`, `step`, `log`, `usage`, `publication`, `terminal`), and JSON payloads. Send `text/event-stream`, disable intermediary caching/buffering, flush complete records, and heartbeat around every 15 seconds. Parse `Last-Event-ID`, reject invalid or cross-run cursors, replay persisted events, then follow committed notifications. Avoid replay/subscribe races by treating notifications only as wakeups and always querying DB for `seq > cursor` until caught up. Slow clients get bounded buffers and disconnect/reconnect, never backpressure the agent.
docs/planning/PLAN-codex.md:219:Use request-scoped cancellation for SSE only. Run contexts derive from application lifecycle. Configure header/idle timeouts, maximum request bytes, SSE write deadlines/heartbeats appropriate for long responses, and graceful shutdown that cancels runs and closes streams within a deadline. On terminal event the browser closes EventSource; late subscribers still receive the persisted terminal event. If retention removed old events, send an explicit reset/gap record plus current run state.
docs/planning/PLAN-codex.md:230:- Run defaults: provider, mode rules, token/dollar limits, total time, max turns/tool calls, repair attempts, output/context limits, dry-run default.
docs/planning/PLAN-codex.md:236:Example defaults worth making explicit: 40 model turns, 120 tool calls, N=2 repair attempts, 20-minute run, 2-minute command, 256 KiB command output. Choose a conservative token cap and require an explicit dollar cap/rate configuration before billable execution. These are policy choices, not universal model limits.
docs/planning/PLAN-codex.md:248:- **Workspace security:** table tests for `../`, absolute/UNC/volume paths, sibling-prefix tricks, NUL, trailing separators, Unicode/case policy, symlink outside root, dangling/loop links, aliases into `.git`/secret paths, parent-component swaps, and atomic writes. Exercise list/search/edit too. Fuzz path validation and tool JSON parsing; run race-oriented traversal tests on macOS and Linux. Ensure the patched runtime regression is covered.
docs/planning/PLAN-codex.md:258:Makefile targets: `fmt`, `fmt-check`, `vet`, `test`, `test-race`, `lint`, `build`, `check`, and opt-in provider conformance. Pin a golangci-lint version compatible with the chosen patched Go toolchain. Start with actionable checks (`govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`), adding stricter checks only with clear benefit. Check HTTP body/file/process cleanup and error handling.
docs/planning/PLAN-codex.md:260:GitHub Actions on pushes/PRs: least-privilege `contents: read`; pinned action commit SHAs; job timeouts; concurrency cancellation; patched supported Go toolchain and module cache; format check, vet, lint, unit/integration tests, race tests on a suitable runner, and `govulncheck` with a documented handling process. Build with `CGO_ENABLED=0` for macOS arm64 and Linux amd64/arm64 to verify pure-Go SQLite and embedded assets. Run platform-sensitive workspace/runner tests on both macOS and Linux. No provider keys, write tokens, or network integration tests on PR workflows, and no `pull_request_target` execution of contributed code.
docs/planning/PLAN-codex.md:269:- [ ] **M3 — Workspace and runner boundary.** Implement private clone, jailed file tools, protected paths, exact command policy, minimal env, time/output caps, and Git controls. Acceptance: path/symlink/protected-alias tests pass on macOS/Linux; flood/timeout/cancel tests finish boundedly; no credential reaches helper processes; agent cannot invoke Git publication.
docs/planning/PLAN-codex.md:281:3. **Path checks that pass tests but race in production.** String-prefix/EvalSymlinks checks allow traversal races and protected-file aliases. Use patched Root-relative operations, enforce policy through aliases, bound all file tools, and test component swaps on both target platforms.
docs/planning/PLAN-codex.md:283:5. **Runaway costs despite a nominal cap.** Post-response accounting, retries, unknown rates/usage, and reasoning tokens overspend. Reserve conservatively before every attempt, require prices, retain uncertain reservations, and stop when usage cannot be bounded.
docs/planning/step2/codex-round1.md:19:The cap needs bytes, entry count, and a model-context allowance; 2 KB is not 2,000 tokens. Freeze the selected entry IDs/versions and content hashes per stage and record them so a later approval or deletion cannot silently change an in-flight task. Scope by canonical repository identity, include source run/SHA/config provenance, expire stale command observations, and support disabling memory. Forgetting must remove retrieval/index eligibility; explain what remains in historical audit evidence and backups.
docs/planning/step2/codex-round1.md:29:“Shares the budget” needs enforcement, not prose. `internal/agent/types.go` carries run totals in Session; `internal/runner/accounting.go` reconstructs conservative charges from usage events and accounting artifacts. Preserve those totals across reviewer, coder repair and second review, including provider retries and unknown usage. Reserve review capacity before coding consumes everything. A cheaper coder can cost more once it needs more turns, repairs and a strong reviewer. Compare complete run cost, tokens, latency and accepted fixes against the existing loop.
docs/planning/step2/codex-round1.md:45:Loopback, token auth, a protected tunnel and backups are reasonable ingredients. They protect access to the service; they do not contain a repository process. I have not inspected oracle-staging or verified any tunnel configuration. The host sizing and existing workloads are assumptions from Claude's proposal.
docs/planning/step2/codex-round1.md:49:Allowlisting an owner's repo does not make its future contents trustworthy. A compromised dependency, a malicious test already present at the pinned base, or a model-written test can execute through an allowed `go test`/build command. It can read accessible host files, alter same-user state, contact the network, exhaust disk/RAM/CPU, or deliberately escape process-group cleanup. Clean env does not revoke filesystem access. On a same-UID deployment, the database, config and service credentials may be reachable even though they were not passed to the child. Baseline execution alone is enough; blocking the model's shell tool does not remove the attack path.
docs/planning/step2/codex-round1.md:61:A $1 cap is conditional. No prices ship; `internal/agent/loop.go` handles unpriced calls with token limits. Configure and validate pricing for the exact paid model before claiming a monetary ceiling, and account for retry/reservation limits rather than promising invoice precision. Keep explicit token and wall-time caps on local inference too.
docs/planning/step2/codex-round1.md:80:| M16 | Baseline evidence and evaluation fixtures | Run the current workflow on a disposable isolated environment and demo repo; record exact revision, config/model IDs, approval/spec hashes, test evidence, tokens, priced cost or unknown, duration and resulting PR. Keep repeatable fake-provider cases for adversarial issue/file/test text and known patch defects. Document live failures rather than treating offline smoke as live success. |
docs/planning/step2/codex-round1.md:85:| M21 | Optional webhook/label ingress | Only after the pilot shows a latency need: bounded request body, constant-time HMAC verification over raw bytes, durable dedupe with a defined crash-safe handoff or hint/reconcile design, actor/repo/event checks, no approval via labels. Duplicate, reordered and missed deliveries plus polling/webhook races preserve exact-spec approval and create at most one intended run. |
internal/runner/findings_test.go:197:		t.Fatalf("cancel/edit race accepted: %v", err)
docs/planning/step2/claude-round2.md:33:   only `execx` moves into the container. Git (clone/push with token) stays on the host, never in the
docs/planning/step2/claude-round2.md:34:   container, so the GitHub token never enters the sandbox. Host git must be upgraded to >= 2.50.1.
internal/config/validate.go:117:		{"limits.max_output_tokens", int64(l.MaxOutputTokens)},
internal/config/validate.go:118:		{"limits.max_total_tokens", l.MaxTotalTokens},
internal/config/validate.go:310:				warnf("provider %q: model %q has no price, so its cost is unknown and only the token limit applies; "+
docs/ARCHITECTURE.md:104:`agent.Check` returns a private approval-token type whose fields callers cannot
docs/ARCHITECTURE.md:105:construct. The code stage and code-tool registry require that token. They repeat
docs/ARCHITECTURE.md:135:  including unresolved symlink locations, must be root/current-user owned and
docs/ARCHITECTURE.md:140:- The `os.Root` jail rejects escapes and protected paths, including symlink and
docs/ARCHITECTURE.md:146:  groups. Cancellation sends TERM, retains the leader through the full grace
docs/ARCHITECTURE.md:160:  UI token when set. Registered credentials require at least eight characters;
docs/ARCHITECTURE.md:169:- Loopback is the default web bind. Other binds require a token. API/SSE/artifact
docs/ARCHITECTURE.md:171:  bootstrap data in token mode. Host checks, cross-origin mutation protection,
docs/ARCHITECTURE.md:186:| GitHub App tokens | `github.TokenSource`, shared by git and API |
docs/planning/step2/claude-position.md:17:   - Injected into the refine/code system prompt with a hard token cap (e.g. 2 KB), newest/most-used first.
docs/planning/step2/claude-position.md:31:   (no public port), token auth on, SQLite backup via cron. Docker executor deferred: patchbot runs only on
docs/planning/step2/PLAN.md:22:   never sees a rootful socket. Git with the token stays host-side; file tools stay host-side (os.Root).
docs/planning/step2/PLAN.md:25:5. Deploy: dedicated `patchbot` Unix user (not a CyberPanel/site user), systemd, loopback bind + UI token,
docs/planning/step2/PLAN.md:32:| M16 | Live baseline on Mac with a demo repo | Claude (needs owner GitHub token + model key) | refine→approve→code→test→PR on demo repo; evidence recorded in docs/M16-REPORT.md |
internal/config/defaults.toml:19:max_output_tokens = 8192
internal/config/defaults.toml:20:max_total_tokens = 2000000
internal/runner/cli_redaction_m15_test.go:17:	const token = "ui-credential-regression-value" // #nosec G101 -- dummy disclosure fixture.
internal/runner/cli_redaction_m15_test.go:19:	f.issue.Title = "Issue containing " + token
internal/runner/cli_redaction_m15_test.go:20:	f.issue.Body = "Read README.md; " + token
internal/runner/cli_redaction_m15_test.go:21:	if err := os.WriteFile(filepath.Join(f.seed, "README.md"), []byte("private credential "+token), 0o600); err != nil {
internal/runner/cli_redaction_m15_test.go:31:				return token, true
internal/runner/cli_redaction_m15_test.go:38:		if strings.Contains(out.String()+errs.String(), token) {
internal/runner/cli_redaction_m15_test.go:46:	if strings.Contains(string(requests), token) || !strings.Contains(string(requests), "redacted") {
internal/runner/cli_redaction_m15_test.go:62:	if strings.Contains(string(raw), token) {
internal/runner/cli_redaction_m15_test.go:67:		if err == nil && strings.Contains(artifact.Content, token) {
internal/runner/README.md:37:Usage events retain conservative token/cost reservations, including unknown
internal/runner/README.md:40:unknown prices still use the token cap. Run limits are frozen at creation and
internal/runner/README.md:55:secret-backed token source, clock, clone and executor functions. Production uses the
internal/runner/README.md:73:Admission survives request disconnects and the start of graceful shutdown.
internal/runner/runner.go:134:	if token, err := opts.Tokens.Token(ctx); err == nil {
internal/runner/runner.go:135:		r.redactor.AddSecrets(secret.New(token))
internal/runner/runner.go:418:		containerCfg, err := cfg.ContainerConfig(os.LookupEnv, dir)
internal/runner/runner.go:422:		return execx.NewContainerExecutor(dir, containerCfg)
internal/config/load_test.go:300:		{Key: "limits.max_total_tokens", Value: "123456789012", Flag: "--max-tokens"},
internal/config/load_test.go:496:		{name: "token at top level", file: "github_token = \"abc\"", wantErr: []string{"github_token", "looks like a secret"}},
internal/config/load_test.go:504:		{name: "token inside a word", file: "[server]\nuitoken = \"v\"", wantErr: []string{"looks like a secret"}},
internal/config/load_test.go:506:		{name: "tokens then token", file: "[limits]\ntokens_token = \"v\"", wantErr: []string{"looks like a secret"}},
internal/config/load_test.go:508:		{name: "unknown key ending in _env is still unknown", file: "[providers.x]\ntoken_env = \"X\"", wantErr: []string{"unknown key providers.x.token_env"}},
internal/config/load_test.go:534:	// Free names (provider, model, command ID) and the token counters are not
internal/config/load_test.go:538:max_total_tokens = 1000
internal/config/load_test.go:539:max_output_tokens = 100
internal/config/load_test.go:545:[providers.secret.prices.token]
internal/config/load_test.go:550:commands = { token = ["true"] }
internal/config/load_test.go:553:	if c.Limits.MaxTotalTokens != 1000 || c.Providers["secret"].Prices["token"] == nil {
internal/config/load_test.go:616:	// No recognizable token shape, so only the loader's own care hides it.
internal/runner/accounting.go:57:				Tokens    int64 `json:"total_tokens"`
docs/PLAN.md:133:- Default limits per run, all configurable: 40 turns, 3 test attempts, **$2** and 2M tokens, 30 minutes,
docs/PLAN.md:136:  integer micro-USD. A model with no price set may run only under the token cap and logs a warning. A
docs/PLAN.md:150:   or a flag cannot add one. This comes from Codex and replaces Fable's "any repo the token can push to".
docs/PLAN.md:155:3. **Path jail:** `os.Root` for every file tool. Absolute paths, `..`, NUL, and symlinks leading out are
docs/PLAN.md:157:   write. A fuzz test covers this. Policy configuration, candidate paths and symlink targets reject
docs/PLAN.md:162:4. **Go 1.27** (installed 1.27.3; must stay ≥ 1.25.12 because GO-2026-4970 fixed an `os.Root` symlink escape). The `go.mod` `go` line
docs/PLAN.md:165:   Output is capped, and the child gets a minimal environment with no API keys or GitHub token, plus its
docs/PLAN.md:167:6. **Git:** the token is passed only through `GIT_CONFIG_*` env vars during clone and push, never in a
docs/PLAN.md:191:| `steps` | run_id, seq, phase, kind (llm/tool/test/git/publish), tool, status, duration, exit_code, capped redacted input/output, tokens, cost |
docs/PLAN.md:204:  3. **Run detail** (`/runs/{id}`): phase timeline, live log (SSE), token and cost counters,
docs/PLAN.md:258:- CI (GitHub Actions, Linux + macOS): `gofmt`, `go vet`, `golangci-lint`, `go test -race`, `govulncheck`,
docs/PLAN.md:259:  and a `CGO_ENABLED=0` build. No provider keys in CI. Make targets: `fmt lint test race build ci`.
docs/PLAN.md:276:| M1 | Config, secrets, redactor, slog | Claude | precedence table passes; secret-looking key rejected; token prints `[redacted]` |
docs/PLAN.md:281:| M6 | Agent loop + **refine phase + approval gate** | Claude | all fake-provider cases and gate tests pass under `-race` |
docs/PLAN.md:285:| M10 | Git workspace + guards | Claude | token never in argv or `.git/config`; branch guards tested |
docs/planning/step2/codex-round1.log:148:internal/agent/testdata/without_token/invalid.go
docs/planning/step2/codex-round1.log:256:   - Injected into the refine/code system prompt with a hard token cap (e.g. 2 KB), newest/most-used first.
docs/planning/step2/codex-round1.log:270:   (no public port), token auth on, SQLite backup via cron. Docker executor deferred: patchbot runs only on
docs/planning/step2/codex-round1.log:304:  Process-group cleanup does not contain deliberate group escapes.
docs/planning/step2/codex-round1.log:308:- No prices are shipped. Unknown costs use token limits. Provider/model IDs in
docs/planning/step2/codex-round1.log:423:`agent.Check` returns a private approval-token type whose fields callers cannot
docs/planning/step2/codex-round1.log:424:construct. The code stage and code-tool registry require that token. They repeat
docs/planning/step2/codex-round1.log:454:  including unresolved symlink locations, must be root/current-user owned and
docs/planning/step2/codex-round1.log:459:- The `os.Root` jail rejects escapes and protected paths, including symlink and
docs/planning/step2/codex-round1.log:465:  groups. Cancellation sends TERM, retains the leader through the full grace
docs/planning/step2/codex-round1.log:479:  UI token when set. Registered credentials require at least eight characters;
docs/planning/step2/codex-round1.log:488:- Loopback is the default web bind. Other binds require a token. API/SSE/artifact
docs/planning/step2/codex-round1.log:490:  bootstrap data in token mode. Host checks, cross-origin mutation protection,
docs/planning/step2/codex-round1.log:505:| GitHub App tokens | `github.TokenSource`, shared by git and API |
docs/planning/step2/codex-round1.log:528:| M1 | Config, secrets, redactor, slog | Claude | precedence table passes; secret-looking key rejected; token prints `[redacted]` |
docs/planning/step2/codex-round1.log:533:| M6 | Agent loop + **refine phase + approval gate** | Claude | all fake-provider cases and gate tests pass under `-race` |
docs/planning/step2/codex-round1.log:537:| M10 | Git workspace + guards | Claude | token never in argv or `.git/config`; branch guards tested |
docs/planning/step2/codex-round1.log:585:// Zero token/turn fields select defaults; zero MaxCostMicros disables the cost cap.
docs/planning/step2/codex-round1.log:793:// token. Revalidate it immediately before registering writes and commands.
docs/planning/step2/codex-round1.log:1147:			if err := l.event(ctx, "log", map[string]any{"level": "warn", "message": "Model price is unknown; only the token budget applies."}); err != nil {
docs/planning/step2/codex-round1.log:1236:	// One token per serialized byte, plus envelope overhead, is a conservative
docs/planning/step2/codex-round1.log:1237:	// first-call estimate without depending on any provider's tokenizer. Never
docs/planning/step2/codex-round1.log:1244:	tokens := add(input, int64(lim.MaxOutputTokens))
docs/planning/step2/codex-round1.log:1246:	if exceeds(s.Tokens, tokens, lim.MaxTotalTokens) || (priced && lim.MaxCostMicros > 0 && exceeds(s.Cost, reserve, lim.MaxCostMicros)) {
docs/planning/step2/codex-round1.log:1268:		s.Tokens = add(s.Tokens, tokens)
docs/planning/step2/codex-round1.log:1285:	inputJSON, _ := json.Marshal(map[string]any{"model": l.Model, "messages": recordedMessages(req.Messages, l.redactor()), "reserved_tokens": tokens, "reserved_cost_micros": reserve, "usage_missing": usage == nil})
docs/planning/step2/codex-round1.log:1309:	if err := l.event(ctx, "usage", map[string]any{"turn": s.Turns, "usage": usage, "cost_micros": step.CostMicros, "total_cost_micros": s.Cost, "total_tokens": s.Tokens, "accounting_uncertain": s.AccountingUncertain, "unpriced": s.Unpriced}); err != nil {
docs/planning/step2/codex-round1.log:1910:	token, err := r.opts.Tokens.Token(ctx)
docs/planning/step2/codex-round1.log:1912:		return nil, errors.New("GitHub token unavailable; set GITHUB_TOKEN")
docs/planning/step2/codex-round1.log:1922:		repo, cloneErr = r.opts.Clone(ctx, gitx.CloneOptions{Dir: filepath.Join(dir, "repo"), Host: "github.com", Owner: ref(run.Repo).Owner, Name: ref(run.Repo).Name, Branch: meta.DefaultBranch, DefaultBranch: meta.DefaultBranch, Token: token, TrustedPath: r.cfg.Limits.TrustedPath, ExcludedRoots: []string{r.cfg.DataDir()}})
docs/planning/step2/codex-round1.log:2263:	token, err := agent.Check(ctx, r.db, run.ID, agent.IssueHash(issue.Title, issue.Body), r.gateOptions())
docs/planning/step2/codex-round1.log:2273:	result, codeErr := loop.Code(ctx, token, agent.CodeRequest{RunID: run.ID, Title: issue.Title, Body: issue.Body, Changes: env.repo, Baseline: baselineInfo, TestCommand: command, Session: session, Limits: limits.Agent, OnPhase: func(c context.Context, phase store.Phase) error {
docs/planning/step2/codex-round1.log:2425:				Tokens    int64 `json:"total_tokens"`
docs/planning/step2/codex-round1.log:2812:	tokens := "unknown"
docs/planning/step2/codex-round1.log:2814:		tokens = fmt.Sprintf("%d input / %d output", *run.InputTokens, *run.OutputTokens)
docs/planning/step2/codex-round1.log:2816:	fmt.Fprintf(&b, "\nUsage: %s tokens; cost: %s.\n", tokens, cost)
docs/planning/step2/codex-round1.log:2913:	InputTokens      *int64          `json:"input_tokens"`
docs/planning/step2/codex-round1.log:2914:	OutputTokens     *int64          `json:"output_tokens"`
docs/planning/step2/codex-round1.log:2915:	CacheReadTokens  *int64          `json:"cache_read_tokens"`
docs/planning/step2/codex-round1.log:2916:	CacheWriteTokens *int64          `json:"cache_write_tokens"`
docs/planning/step2/codex-round1.log:3024:	InputTokens      *int64     `json:"input_tokens"`
docs/planning/step2/codex-round1.log:3025:	OutputTokens     *int64     `json:"output_tokens"`
docs/planning/step2/codex-round1.log:3026:	CacheReadTokens  *int64     `json:"cache_read_tokens"`
docs/planning/step2/codex-round1.log:3027:	CacheWriteTokens *int64     `json:"cache_write_tokens"`
docs/planning/step2/codex-round1.log:3438:const stepColumns = `run_id,seq,phase,kind,tool,status,turn,input_json,output_text,stop_reason,input_tokens,output_tokens,
docs/planning/step2/codex-round1.log:3439:cache_read_tokens,cache_write_tokens,cost_micros,exit_code,duration_ms,started_at,finished_at`
docs/planning/step2/codex-round1.log:3476:				input_tokens=?,output_tokens=?,cache_read_tokens=?,cache_write_tokens=?,cost_micros=?,exit_code=?,duration_ms=?,started_at=?,finished_at=?
docs/planning/step2/codex-round1.log:3485:		// NULL propagates if any completed llm call lacks a token count or price.
docs/planning/step2/codex-round1.log:3488:			(input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,turns)=(
docs/planning/step2/codex-round1.log:3489:			SELECT CASE WHEN COUNT(input_tokens)=COUNT(*) THEN SUM(input_tokens) END,
docs/planning/step2/codex-round1.log:3490:			CASE WHEN COUNT(output_tokens)=COUNT(*) THEN SUM(output_tokens) END,
docs/planning/step2/codex-round1.log:3491:			CASE WHEN COUNT(cache_read_tokens)=COUNT(*) THEN SUM(cache_read_tokens) END,
docs/planning/step2/codex-round1.log:3492:			CASE WHEN COUNT(cache_write_tokens)=COUNT(*) THEN SUM(cache_write_tokens) END,
docs/planning/step2/codex-round1.log:3648:// StartCode atomically closes the edit/approval race before any model writes.
docs/planning/step2/codex-round1.log:3835:				Tokens    int64 `json:"total_tokens"`
docs/planning/step2/codex-round1.log:3937:	token, err := r.opts.Tokens.Token(ctx)
docs/planning/step2/codex-round1.log:3939:		return nil, errors.New("GitHub token unavailable; set GITHUB_TOKEN")
docs/planning/step2/codex-round1.log:3949:		repo, cloneErr = r.opts.Clone(ctx, gitx.CloneOptions{Dir: filepath.Join(dir, "repo"), Host: "github.com", Owner: ref(run.Repo).Owner, Name: ref(run.Repo).Name, Branch: meta.DefaultBranch, DefaultBranch: meta.DefaultBranch, Token: token, TrustedPath: r.cfg.Limits.TrustedPath, ExcludedRoots: []string{r.cfg.DataDir()}})
docs/planning/step2/codex-round1.log:4530:		return "", fmt.Errorf("resolve executable symlink: %w", err)
docs/planning/step2/codex-round1.log:4666:internal/agent/loop.go:55:			if err := l.event(ctx, "log", map[string]any{"level": "warn", "message": "Model price is unknown; only the token budget applies."}); err != nil {
docs/planning/step2/codex-round1.log:4675:internal/agent/loop.go:154:	if exceeds(s.Tokens, tokens, lim.MaxTotalTokens) || (priced && lim.MaxCostMicros > 0 && exceeds(s.Cost, reserve, lim.MaxCostMicros)) {
docs/planning/step2/codex-round1.log:4680:internal/agent/loop.go:193:	inputJSON, _ := json.Marshal(map[string]any{"model": l.Model, "messages": recordedMessages(req.Messages, l.redactor()), "reserved_tokens": tokens, "reserved_cost_micros": reserve, "usage_missing": usage == nil})
docs/planning/step2/codex-round1.log:4681:internal/agent/loop.go:217:	if err := l.event(ctx, "usage", map[string]any{"turn": s.Turns, "usage": usage, "cost_micros": step.CostMicros, "total_cost_micros": s.Cost, "total_tokens": s.Tokens, "accounting_uncertain": s.AccountingUncertain, "unpriced": s.Unpriced}); err != nil {
docs/planning/step2/codex-round1.log:4979:The harness has verified a human approval for this specification. Only the harness can grant write access. Commands are configured names with literal arguments, never shell text or an executable chosen by you. The harness enforces all turn, token, cost, time, path, output, and command limits. It independently runs the configured tests after submit; your own claim that tests passed does not establish verification. If verification fails, use the provided diagnostic evidence to repair the patch and submit again. A limit may end the stage and leave a draft for human review. Do not attempt to publish, push, approve, or contact external services.
docs/planning/step2/codex-round1.log:5584:+The cap needs bytes, entry count, and a model-context allowance; 2 KB is not 2,000 tokens. Freeze the selected entry IDs/versions and content hashes per stage and record them so a later approval or deletion cannot silently change an in-flight task. Scope by canonical repository identity, include source run/SHA/config provenance, expire stale command observations, and support disabling memory. Forgetting must remove retrieval/index eligibility; explain what remains in historical audit evidence and backups.
docs/planning/step2/codex-round1.log:5594:+“Shares the budget” needs enforcement, not prose. `internal/agent/types.go` carries run totals in Session; `internal/runner/accounting.go` reconstructs conservative charges from usage events and accounting artifacts. Preserve those totals across reviewer, coder repair and second review, including provider retries and unknown usage. Reserve review capacity before coding consumes everything. A cheaper coder can cost more once it needs more turns, repairs and a strong reviewer. Compare complete run cost, tokens, latency and accepted fixes against the existing loop.
docs/planning/step2/codex-round1.log:5610:+Loopback, token auth, a protected tunnel and backups are reasonable ingredients. They protect access to the service; they do not contain a repository process. I have not inspected oracle-staging or verified any tunnel configuration. The host sizing and existing workloads are assumptions from Claude's proposal.
docs/planning/step2/codex-round1.log:5614:+Allowlisting an owner's repo does not make its future contents trustworthy. A compromised dependency, a malicious test already present at the pinned base, or a model-written test can execute through an allowed `go test`/build command. It can read accessible host files, alter same-user state, contact the network, exhaust disk/RAM/CPU, or deliberately escape process-group cleanup. Clean env does not revoke filesystem access. On a same-UID deployment, the database, config and service credentials may be reachable even though they were not passed to the child. Baseline execution alone is enough; blocking the model's shell tool does not remove the attack path.
docs/planning/step2/codex-round1.log:5626:+A $1 cap is conditional. No prices ship; `internal/agent/loop.go` handles unpriced calls with token limits. Configure and validate pricing for the exact paid model before claiming a monetary ceiling, and account for retry/reservation limits rather than promising invoice precision. Keep explicit token and wall-time caps on local inference too.
docs/planning/step2/codex-round1.log:5645:+| M16 | Baseline evidence and evaluation fixtures | Run the current workflow on a disposable isolated environment and demo repo; record exact revision, config/model IDs, approval/spec hashes, test evidence, tokens, priced cost or unknown, duration and resulting PR. Keep repeatable fake-provider cases for adversarial issue/file/test text and known patch defects. Document live failures rather than treating offline smoke as live success. |
docs/planning/step2/codex-round1.log:5650:+| M21 | Optional webhook/label ingress | Only after the pilot shows a latency need: bounded request body, constant-time HMAC verification over raw bytes, durable dedupe with a defined crash-safe handoff or hint/reconcile design, actor/repo/event checks, no approval via labels. Duplicate, reordered and missed deliveries plus polling/webhook races preserve exact-spec approval and create at most one intended run. |
docs/planning/step2/codex-round1.log:5692:tokens used
internal/runner/lifecycle.go:377:	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
internal/runner/lifecycle.go:379:	current, err := r.db.GetRun(cleanup, run.ID)
internal/runner/lifecycle.go:399:	err = r.transition(cleanup, &run, status, run.Phase, store.RunPatch{Outcome: &outcome, ErrorKind: &kind, ErrorMessage: &message})
internal/runner/lifecycle.go:401:		_, commentErr := r.gh.CreateIssueComment(cleanup, ref(run.Repo), int(run.IssueNumber), fmt.Sprintf("patchbot run %d stopped during %s (%s). Review the run locally before retrying.", run.ID, run.Phase, kind))
internal/runner/lifecycle.go:403:			err = r.artifact(cleanup, run.ID, "failure_comment_warning", "Could not post failure comment.")

codex
The argument construction and explicit environment scrubbing look sound so far. I’m checking two remaining issues: Docker’s automatic proxy environment injection and whether cleanup can finish before a daemon-side container creation completes.
web search: 
web search: site.docs.docker.com engine cli proxy configuration automatically populated containers config.json
exec
/bin/zsh -lc "sed -n '130,325p' internal/execx/container_test.go; cat docs/tasks/M17.md; rg -n 'rootless|proxy|cleanup|race' docs/M17* internal/execx/container_integration_test.go" in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
 succeeded in 0ms:
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
cat: docs/tasks/M17.md: No such file or directory
internal/execx/container_integration_test.go:15:// Adversarial containment tests against a real rootless runtime. They run only
internal/execx/container_integration_test.go:21://	XDG_RUNTIME_DIR             required by rootless podman
internal/execx/container_integration_test.go:23:// Example (Mumbai, rootless podman 4.9):
internal/execx/container_integration_test.go:26://	  go test -race -count=1 -run Integration -v ./internal/execx/
docs/M17-REPORT.md:1:# M17 report: rootless container executor
docs/M17-REPORT.md:25:  and CLI process-group cleanup are shared code. On timeout/cancel the container is killed by name
docs/M17-REPORT.md:31:  runtime CLI process receives `RuntimeHostEnv`: trusted PATH plus `XDG_RUNTIME_DIR` (needed by rootless
docs/M17-REPORT.md:43:  {{.SecurityOptions}}` (expects `rootless`) and WARNs if not rootless, reports the digest-pinned image, and
docs/M17-REPORT.md:51:`make ci` green (fmt-check, vet, golangci-lint 0 issues, `go test -race ./...`, build).
docs/M17-REPORT.md:53:New tests (all pass, `-race`):
docs/M17-REPORT.md:64:| `cli.TestDoctorContainerExecutor` | PASS rootless, WARN rootful, WARN image missing, FAIL missing runtime; runtime env has XDG_RUNTIME_DIR and no credentials |
docs/M17-REPORT.md:74:Mumbai box (rootless podman 4.9.3):
docs/M17-REPORT.md:78:  go test -race -count=1 -run Integration -v ./internal/execx/
docs/M17-REPORT.md:90:- Only flags verified on podman 4.9 rootless plus `--rm/--name/--label/--pull never/--workdir/-v/-e/--userns`
docs/M17-REPORT.md:105:Ran as user `patchbot` (rootless podman 4.9.3, arm64, cgroup v2), image

exec
/bin/zsh -lc "rg -n 'func openRoot|O_PATH|O_RDONLY' /usr/local/go/src/os/root* /opt/homebrew/opt/go/libexec/src/os/root*; sed -n '85,105p' docs/M17-REPORT.md; rg -n 'chmod|UserNS|keep-id' internal/execx/container_integration_test.go" in /Users/pappumishra/Documents/selfproject/patchbot-m17-container
 succeeded in 0ms:
zsh:1: no matches found: /usr/local/go/src/os/root*
- **Not yet proven on a real runtime.** Integration evidence is pending the Mumbai run (M18).
- **Rootless podman needs `userns = "keep-id"`** so the host service user maps to `user` inside the
  container; without it uid 65532 is a subordinate uid that cannot read the 0700 workspace. Rootless docker
  has no equivalent; there, use a world-readable workspace or run as container uid 0 (rejected by validation
  today), so podman is the supported production runtime.
- Only flags verified on podman 4.9 rootless plus `--rm/--name/--label/--pull never/--workdir/-v/-e/--userns`
  are used. `--memory-swap` is not set (swap behaviour follows the runtime default). Podman's default
  `--read-only-tmpfs` also mounts small tmpfs on `/run` and `/var/tmp`; they count against `--memory`.
- Image `ENV` values not overridden by patchbot's env remain visible to commands (they come from the
  operator-pinned image).
- If cancellation arrives while the runtime is still creating the container, `kill` may miss it; the
  follow-up `rm -f` covers the common case, but a container created after both calls would run until its
  command exits. The `io.patchbot.managed=true` label allows auditing for strays.
- An OOM kill surfaces as exit 137 (not distinguished from a program exiting 137).
- `--network none` means dependency downloads inside the container fail; images must contain dependencies
  or the repo must vendor them. A separate fetch phase is future work.
- The workspace bind mount is read-write: a malicious command can still modify the checkout (as before);
  the harness diff/check and protected paths still apply after the command.

## Server evidence (Mumbai, 2026-11-13)
Ran as user `patchbot` (rootless podman 4.9.3, arm64, cgroup v2), image
20://	PATCHBOT_CONTAINER_USERNS   optional; defaults to keep-id for podman
54:		userns = "keep-id"
66:		TmpfsSize: "16m", Network: "none", User: "65532:65532", UserNS: userns, HostEnv: hostEnv,

codex
- **High — [container.go:312](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:312): Cleanup depends on the untrusted workspace.** With Podman `keep-id`, repository code can execute `chmod 000 /work` and keep running. Both cleanup commands then fail workspace validation or `chdir`, leaving the container alive after timeout. **Fix:** execute cleanup from a trusted directory outside the workspace, using a separate executor.

- **Medium — [container.go:286](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:286): Cancellation can leave a late-created container running.** Killing the CLI does not synchronize daemon-side creation; both `kill` and the single `rm -f` can return “no such container” before creation completes. **Fix:** separate creation from startup, confirm the container ID before starting, and reconcile interrupted creations until removal is confirmed.

- **Medium — [container.go:55](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:55): Docker can inject host proxy credentials into containers.** Forwarding `HOME`/`DOCKER_CONFIG` loads Docker’s configuration, whose proxy settings automatically become container environment variables, bypassing `scrubEnv`. Authenticated proxy URLs expose credentials to repository code. [Docker documentation](https://docs.docker.com/engine/cli/proxy/). **Fix:** explicitly override every Docker proxy variable, in uppercase and lowercase, with empty values unless explicitly approved.
tokens used
60,831
- **High — [container.go:312](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:312): Cleanup depends on the untrusted workspace.** With Podman `keep-id`, repository code can execute `chmod 000 /work` and keep running. Both cleanup commands then fail workspace validation or `chdir`, leaving the container alive after timeout. **Fix:** execute cleanup from a trusted directory outside the workspace, using a separate executor.

- **Medium — [container.go:286](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:286): Cancellation can leave a late-created container running.** Killing the CLI does not synchronize daemon-side creation; both `kill` and the single `rm -f` can return “no such container” before creation completes. **Fix:** separate creation from startup, confirm the container ID before starting, and reconcile interrupted creations until removal is confirmed.

- **Medium — [container.go:55](/Users/pappumishra/Documents/selfproject/patchbot-m17-container/internal/execx/container.go:55): Docker can inject host proxy credentials into containers.** Forwarding `HOME`/`DOCKER_CONFIG` loads Docker’s configuration, whose proxy settings automatically become container environment variables, bypassing `scrubEnv`. Authenticated proxy URLs expose credentials to repository code. [Docker documentation](https://docs.docker.com/engine/cli/proxy/). **Fix:** explicitly override every Docker proxy variable, in uppercase and lowercase, with empty values unless explicitly approved.
