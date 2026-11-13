# M17 report: rootless container executor

Date: 2026-11-13. Branch `m17-container`.

## What was done

- **Config** `[executor]` (`internal/config/config.go`, `defaults.toml`, `validate.go`, `executor.go`):
  `kind = "local" | "container"` (default `local`, unchanged behaviour), `runtime = "podman" | "docker"`
  (resolved to an absolute binary through `limits.trusted_path`, never the ambient PATH), `image` (required
  for container; must be `name[:tag]@sha256:<64 hex>`), `memory`, `cpus`, `pids_limit`, `tmpfs_size`,
  `network` (only `"none"`), `user` (numeric non-root `uid:gid`, default `65532:65532`), `userns`
  (`""` or `"keep-id"`, podman only) and `path` (PATH inside the container). `config show` prints the table
  with per-key sources like every other table.
- **Executor** `execx.ContainerExecutor` (`internal/execx/container.go`) behind the existing `execx.Executor`
  interface. Each command runs as:

  ```
  <runtime> run --rm --name patchbot-<24 hex> --label io.patchbot.managed=true --pull never
    --network none --read-only --tmpfs /tmp:rw,nosuid,nodev,size=S --cap-drop ALL
    --security-opt no-new-privileges --pids-limit N --memory M --cpus C --user U
    [--userns keep-id:uid=U,gid=G] --workdir /work/<rel> -v <workspace>:/work:rw -e ... IMAGE argv...
  ```

  The runtime CLI runs through `LocalExecutor`, so output capping (capwriter), streaming redaction, timeout
  and CLI process-group cleanup are shared code. On timeout/cancel the container is killed by name
  (`<runtime> kill`, which kills every process in its PID namespace) and then `rm -f`'d. Exit codes pass
  through; timeout/cancel report `ExitCode -1` and `TimedOut`/`Cancelled` exactly like the local executor;
  runtime failure (exit 125) is returned as an error.
- **Environment**: the container receives only the command's explicit env after the existing secret-name
  filter, with PATH replaced by `executor.path` and the host's private HOME/TMPDIR remapped to `/tmp`. The
  runtime CLI process receives `RuntimeHostEnv`: trusted PATH plus `XDG_RUNTIME_DIR` (needed by rootless
  podman), `HOME`, `CONTAINER_HOST`/`CONTAINERS_*` and `DOCKER_HOST`/`DOCKER_*`. None of these is forwarded
  into the container. The GitHub token is never in either environment.
- **Policy**: `PolicyOptions.InContainer` keeps command IDs, the allowlist, extra-argument rules and the
  shell/launcher denial, but leaves executable names unresolved on the host (the image's PATH resolves
  them). Relative paths with a slash and non-clean absolute paths are rejected.
- **Wiring**: `runner.New` chooses the executor from config (`defaultExecutor`); `runner/phases.go` builds the
  policy with `cfg.CommandPolicyOptions()`. Baseline tests, agent `run_cmd` and harness verification all go
  through `tools.ExecuteCommand` and so through the selected executor. Git (clone/push with token) and file
  tools (`os.Root` jail) remain host-side.
- **Doctor**: for `kind = "container"` it checks the runtime binary resolves through the trusted path, runs
  `podman info --format {{.Host.Security.Rootless}}` (expects `true`) or `docker info --format
  {{.SecurityOptions}}` (expects `rootless`) and WARNs if not rootless, reports the digest-pinned image, and
  WARNs if the image is not present locally (runs use `--pull never`). Host `go` is not required in
  container mode; repo commands are shown as "resolved in the container image".
- **Docs**: `patchbot.example.toml`, `docs/ARCHITECTURE.md` seams table, `docs/DECISIONS.md`,
  `internal/execx/README.md`.

## Test evidence

`make ci` green (fmt-check, vet, golangci-lint 0 issues, `go test -race ./...`, build).

New tests (all pass, `-race`):

| Test | Proves |
| --- | --- |
| `execx.TestContainerArgv` | exact argv for podman, podman keep-id, docker; literal args after image |
| `execx.TestContainerEnvRemapsPrivateDirs` | HOME/TMPDIR/GOCACHE remapped to `/tmp`, PATH replaced |
| `execx.TestContainerConfigValidate` | digest pinning, non-root user, network none, size/cpu syntax, flag smuggling (`1m,exec`), keep-id docker rejected |
| `execx.TestRuntimeHostEnvPassesOnlyRuntimeVariables` | XDG_RUNTIME_DIR/DOCKER_HOST pass; GITHUB_TOKEN/API keys do not |
| `execx.TestPolicyInContainer` | IDs unchanged, names unresolved, shells/relative paths denied |
| `execx.TestContainerExecutorRunWithFakeRuntime` | test binary as fake runtime: exit pass-through, workdir mapping, token filtered from `-e`, 125 -> error + `rm -f`, timeout and cancel -> `kill <name>` + `rm -f <name>`, symlink/dir escapes rejected |
| `config.TestExecutor*` | defaults local, validation messages, container policy options, `config show` with sources |
| `cli.TestDoctorContainerExecutor` | PASS rootless, WARN rootful, WARN image missing, FAIL missing runtime; runtime env has XDG_RUNTIME_DIR and no credentials |
| `runner.TestDefaultExecutorFollowsConfig` | local by default; container kind resolves the runtime via trusted path |

Adversarial integration tests (`internal/execx/container_integration_test.go`) are written but **not run on
the Mac** by owner instruction. They skip unless `PATCHBOT_CONTAINER_TEST=1` and a pulled, digest-pinned
`PATCHBOT_CONTAINER_IMAGE` (busybox or alpine) are set. They cover: container uid is 65532 and workspace is
writable while rootfs is not; a host file outside the mount is unreadable; network egress fails and only
`lo` exists; 100 background processes fail under `pids_limit = 32` and a real fork bomb ends within its
timeout with the container removed; `dd` of 32 MiB into an 8 MiB tmpfs fails with "No space left";
cancellation of `sh` with two background `sleep 300` returns promptly and leaves no container. To run on the
Mumbai box (rootless podman 4.9.3):

```
PATCHBOT_CONTAINER_TEST=1 PATCHBOT_CONTAINER_IMAGE=docker.io/library/busybox@sha256:<digest> \
  go test -race -count=1 -run Integration -v ./internal/execx/
```

(`PATCHBOT_CONTAINER_USERNS` defaults to `keep-id` for podman; `XDG_RUNTIME_DIR` must be set.)

## Limits and open points

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
`docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662`:
all six `Integration` tests PASS (identity/workspace, host-file read denied, no network,
fork bomb capped, tmpfs bounded, cancel kills descendants in 6.1 s).

## Security review fixes (2026-11-13)

Codex reviewed the branch and found five issues. All are fixed in `fix: harden container cleanup and runtime env (M17)`.

1. **HIGH, cleanup depended on the workspace.** `kill`/`rm -f` used to run with the workspace as the CLI's working directory. With keep-id, a container could `chmod 000 /work`, the cleanup call would fail, and the container would survive. Every runtime call now runs from `/` through a separate `LocalExecutor`, so cleanup no longer touches the workspace. Unit test: the fake runtime runs `chmod 000` on the workspace, cleanup and the removal check still succeed, and every runtime call is recorded with cwd `/`.
2. **MEDIUM, a container created late could keep running after a cancel.** The executor now runs `create` (named and labelled, not started) and then `start --attach`. After every command it runs `rm -f` and confirms absence with `ps --filter name=^<name>$`. After an interrupted create it requires two consecutive absent checks. Retries stop after 60 s, and if removal is never confirmed it logs at ERROR and returns `ErrContainerNotRemoved`. At startup the runner also calls `SweepContainers`, which removes leftover containers labelled `io.patchbot.managed=true`, but only when the store has no queued or running run. Unit tests: cancel during a hanging create that has already made the container, timeout, cancel, a container that cannot be removed, a sweep, and the sweep skipped when a run is active or the store errors.
3. **MEDIUM, proxy settings from docker's config.json leaked into containers.** Every container now gets explicit empty values for `HTTP(S)_PROXY`, `FTP_PROXY`, `NO_PROXY` and `ALL_PROXY` in both upper and lower case, and podman also gets `--http-proxy=false`. `DOCKER_CONFIG` and `DOCKER_CONTEXT` are no longer passed through. Docker instead gets a private 0700 `DOCKER_CONFIG` at `<data>/runtime/docker-config`, and a directory that already contains `config.json` is refused.
4. **HIGH, container commands could rewrite `.git`.** The repo's real `.git` directory is now mounted read-only at `/work/.git`. A gitlink file or a symlink in its place is refused. `Run` returns only after removal is confirmed, so host git never runs while a container still exists. Unit tests check the argv and the gitlink/symlink refusal. Integration test: writes to HEAD and refs, `mkdir`, `mv` and `rm` of `.git` all fail, while the worktree stays writable.
5. **MEDIUM, nothing limited workspace disk use.** New setting `[executor] workspace_max_bytes`, default 2 GiB. After each command the workspace is walked without following symlinks, with a cap of 1,000,000 entries. Going over the limit, or hitting an unreadable entry, fails with `ErrWorkspaceLimit`. `tools.ExecuteCommand` reports this error and `ErrContainerNotRemoved` as named errors, without any runtime output. Unit and integration tests cover it.

New integration tests, still gated by `PATCHBOT_CONTAINER_TEST=1` and not yet run: `TestIntegrationContainerGitDirIsReadOnly`, `TestIntegrationContainerWorkspaceLimit` and `TestIntegrationSweepRemovesLabelledContainers`. They need a run on Mumbai, and so does the switch from `run --rm` to `create`/`start --attach`, which the earlier Mumbai evidence predates.

## Server evidence after security fixes (Mumbai, 2026-11-13)
create + start --attach flow, same busybox digest: all nine `Integration` tests PASS, including
GitDirIsReadOnly, WorkspaceLimit and SweepRemovesLabelledContainers. `podman start --attach`
propagates the exit code (IdentityAndWorkspace exit-7 case). Zero labelled containers remained.
