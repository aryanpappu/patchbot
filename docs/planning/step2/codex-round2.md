# Step 2 debate: Codex round 2

2026-11-13. Checked round 1 and Claude round 2 against the current implementation.
This is a planning review; no code or server configuration was changed.
Mumbai hardware, installed packages, website load and Cloudflare policy are reported premises,
not facts established by this source review.

## Merged order: amend, then accept

Accept M16 baseline → M17 isolation → M18 deployment rehearsal → M19 live pilot →
M20 memory → M21 optional reviewer → M22 optional webhooks.
Memory and review should earn their place through measured pilot evidence.
Amend M16: a trusted throwaway repository on the Mac is acceptable only as an explicit,
supervised local-execution risk decision; trusted content is not an OS boundary and dependencies
also execute. Prefer a disposable VM. This does not authorize unattended Mac execution.
M18 must pass before admitting real work on the shared server; M19 validates the full workflow.
Keep exact revision/config/model IDs, approval/spec hashes, test evidence, tokens, priced cost
or unknown, duration and publication identifiers in the M16 evidence. Failures remain evidence.

## Claims C–G

- **C — amend.** Accept rootless execution under a dedicated account, non-root container UID,
  network denial, read-only rootfs, dropped capabilities, no-new-privileges and resource limits.
  No rootful Docker group/socket access. Distinguish the host controller's narrowly scoped access
  to its rootless runtime from repository processes: the latter must never receive any runtime
  socket or API access. A controller must launch containers somehow; “never see the socket” cannot
  prohibit its chosen control channel too. Rootless alone does not prove effective cgroup limits
  or disk quotas. Current `execx.LocalExecutor` explicitly documents host privileges and escaped
  process-group descendants; its timeout/output limits are not containment.
- **D — amend substantially.** Host file tools and host Git are a viable design, not a completed
  boundary. `runner/phases.go:clone` constructs `workspace.Open`, `execx.NewPolicy`, then the
  injected executor; the default in `runner.go` is local. `execx/policy.go` resolves absolute
  executables against the HOST trusted PATH. `tools/run_cmd.go:ExecuteCommand` creates host
  HOME/TMP outside the checkout. Container executable resolution, working-directory mapping,
  HOME/TMP and environment therefore need an explicit contract; replacing the factory is insufficient.
  `gitx/command.go` already restricts token injection to clone/push and disables helpers/hooks;
  `gitx/gitx.go` enforces the 2.50.1 floor. Keep those controls and private Git directories host-only.
  However, a writable checkout bind mount exposes `.git` to commands despite file-tool protection.
  `gitx/integrity.go` checks config/hash and hook entries, not every metadata file or concurrent
  mutation. Hide/protect metadata or use a separate command tree with validated change import.
  Stop all container writers before host Git consumes results. Audit symlinks, hardlinks, special
  files, protected-path changes and host file-tool races; `workspace.Local`'s jail is not a syscall
  policy for tests. Keeping the token out of the environment alone is insufficient.
- **E — accept with qualification.** Operator-selected, digest-pinned images with native arm64
  support are required on the stated target. Image configuration and container execution do not
  exist in the inspected execution path. Verify the exact selected digest/platform and toolchain;
  image family names do not prove compatibility. Record image identity with command evidence.
- **F — accept deferral; amend certainty.** Polling already serves the approval path
  (`runner/sync.go`); webhooks need not block deployment. Whether Access blocks the route depends
  on the actual policy, which this code cannot establish. Any future public exception still needs
  raw-body HMAC, actor/repo authorization and crash-safe receipt/reconciliation. Access is not
  webhook authentication, and an exception must not expose the UI/API.
- **G — amend.** Accept one active run globally and at least the proposed site headroom as an
  initial operational target, subject to measurement. `server/manager.go` has a bounded in-memory
  queue and defaults to TWO workers; set one explicitly. This is process-local, not a cross-process
  lock: prevent overlapping service/CLI instances. CPU quotas limit consumption, not guaranteed
  site reservation. Account for controller, host Git, runtime, page cache, disk and I/O too.

## Remaining disagreements

1. An unrestricted checkout mount plus the existing config hash is not sufficient protection for
   host Git. M17 needs a concrete metadata and change-transfer design, with adversarial evidence.
2. M16 on a normal Mac remains an explicit risk exception, not evidence of isolation.
3. Numerical server headroom and rootless resource enforcement must be demonstrated on Mumbai;
   capacity figures and flags cannot substitute for a deployment drill.
The memory trust and opt-in reviewer concessions otherwise resolve round 1's central disputes.

## Final M17 acceptance criteria — container executor

1. All baseline, model command and harness verification execution uses the configured container
   path, including retries/re-clones. Missing runtime/image/limits fails closed; no local fallback.
2. Operator-owned image/digest/platform and command-ID policy are frozen and recorded. Resolve
   executables inside that image without weakening argument restrictions; map workspace and private
   HOME/TMP correctly. Preserve cancellation/timeout distinctions, output caps and stream redaction.
3. Non-root/rootless isolation, no privileged mode/host namespaces/devices/runtime sockets,
   no host home/service data/Git private dirs/provider or GitHub credentials. Only deliberate per-run
   mounts; protected metadata and safe import/host file access satisfy D. No background writer
   survives command completion or races host Git/publication.
4. Default-deny network is exercised, including host services and metadata endpoints. If dependency
   fetching is needed, define a separate bounded phase and network policy; never silently grant
   network to tests or execute repository install hooks with service credentials.
5. Effective CPU, memory/swap, PID, writable disk/inode and tmpfs limits are verified on the target.
   Include runtime storage/logs and workspace volumes: read-only rootfs alone does not bound disk.
6. Adversarial integration evidence covers host/secret reads, `.git` tampering, filesystem races,
   fork/setsid descendants, disk fill, egress, timeout/cancel, runtime failure and service death.
   Prove cleanup after restart and bounded damage while the controller remains responsive.

## Final M18 acceptance criteria — deployment rehearsal

1. M17 passes on the actual arm64 host. Dedicated account, protected config/data, patched supported
   Git >=2.50.1 from the configured trusted path, one service process and one active run globally.
   Verify bounded queue saturation and prevent independent CLI execution from bypassing admission.
2. systemd and runtime startup/shutdown work without rootful daemon authority; restart reconciles
   orphan containers and fails/reconciles stale work honestly. No promise of in-memory queue resume.
3. Exercise loopback binding, tunnel/Access plus application authentication, browser approval and
   SSE reconnect. Demonstrate unauthorized external access is refused; protect inference transport.
4. SQLite-consistent online backup plus restricted retention; restore into fresh data, validate
   migration ledger and stale-run handling. Rehearse compatible upgrade/rollback and credential rotation.
5. Load and fault drills measure website latency, peak CPU/RAM, disk/inodes and I/O with aggregate
   Patchbot limits. Demonstrate the agreed site headroom, disk-full behavior and operator stop path.
6. Document ambiguous push/PR reconciliation before retries, inspect stored/public evidence for
   secrets, and record reproducible commands/configuration and results. M19 then completes live
   approval, stale approval, repair, cancellation, restart and publication fault trials.
