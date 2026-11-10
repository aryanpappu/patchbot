# Command execution

Construct `NewPolicy(root, commands)` from operator-owned per-repository config.
Call `Resolve(commandID, extraArgs)` and use the returned argv in a `Spec` passed
to `NewLocalExecutor(root).Run`. Configure `Dir`, private `HOME` and `TMPDIR`
directories, and `Env: MinimalEnv(home, tmp, extras)` in the harness. Directory
creation and cleanup belong to the caller. Neither config parsing nor the
model-facing tool registry belongs to this package.

The policy copies configuration and returns fresh argv slices. Bare executable
names are resolved once with `exec.LookPath` over absolute candidates from
`TrustedPath`, without changing or reading the parent's PATH. Explicit absolute
executables are accepted outside the workspace. Existing symlinks are resolved
before checking workspace containment and shell restrictions. The operator must
control installed executables and the directories in `TrustedPath`.

`PolicyOptions.AllowArgs` opts individual command IDs into additional arguments.
`AllowLeadingDash` separately permits flags for an opted-in ID. Defaults allow
up to 64 extras, 8192 bytes per argument, and 8192 aggregate extra bytes. NUL is
always rejected, including in configured argv. `AllowShells` explicitly permits
named executable basenames; allowing `sh` also permits its installed symlink
target. An innocently named alias to a shell is rejected unless that target
shell is explicitly allowed. Model input must never control these options.

Zero limits select two minutes, 256 KiB of combined output, three seconds of
TERM grace, and one second of pipe-drain delay. Negative limits are invalid.
The minimum output cap is 64 bytes so a complete omission marker always fits.
The buffer retains a quarter-cap head and a rolling tail. The marker consumes
the output budget; its omitted count includes the bytes displaced by the marker.
`TotalBytes` counts original bytes drained, excluding the generated marker.
Writes do not allocate according to the incoming chunk or accumulated output.

Stdout and stderr share one pipe, preserving kernel-observed write order. Stdin
is the null device. Nonzero exits and signal deaths return a `Result`; exit code
`-1` denotes signal termination or no normal exit. Timeout and cancellation have
separate flags and normally return no error. Validation, start, signal, wait,
and pipe-drain infrastructure failures return errors. In particular, inherited
pipes held beyond `WaitDelay` on ordinary completion produce `exec.ErrWaitDelay`,
wrapped with the partial result. After cancellation, that drain error is subsumed
by the cancellation result. The direct child is always waited on after start.

On timeout/cancel the executor sends TERM to the process group, waits the grace
period, then sends KILL even if its leader has already exited. Ordinary exits
also clean up remaining group members. All cleanup and drain time contributes
to `Duration`, so return can occur after the configured command timeout.

`MinimalEnv` never reads the parent environment. It fixes PATH, HOME, TMPDIR,
LANG, CI, GOTOOLCHAIN, GOENV, GOFLAGS (empty), GOWORK, GOCACHE, and GOPATH; extras
cannot override these settings. It drops secret-like names case-insensitively.
`Run` also filters the explicit env against that deny list. Normal Go dependency
downloads remain enabled; automatic toolchain downloads are disabled.

Local execution remains trusted-code execution as the host user. It does not
prevent host file reads/writes, network access, resource exhaustion, or children
that deliberately escape their process group. The direct child is reaped here;
orphaned descendants rely on their OS parent to reap them. Existing working-dir
symlink escapes are rejected using canonical containment and `os.Root`, but a
host process can still race directory replacement before the final path-based
exec chdir. Installed executable replacement and hard-link aliases are also
outside this policy's guarantees. Minimal environment filtering cannot detect a
secret deliberately placed in an otherwise innocuous variable or file. Use an
isolated OS/container executor for hostile code. Non-Unix Run returns
`ErrUnsupported`.

The tests execute only the Go test binary via the `GO_WANT_HELPER` TestMain
pattern. They cover real process groups and PIDs, TERM-to-KILL escalation after
leader exit, cancellation, bounded inherited-pipe draining, ordered output,
10 MiB floods, deterministic buffer storage/allocation checks, child environment
contents, and command/directory rejection. No external command is executed by
the tests.
