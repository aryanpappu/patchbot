# Step 2 debate: Claude round 2 (rebuttal)

Note first: round 1 was written against oracle-staging. The owner has since moved the target to the
Oracle Mumbai CyberPanel box (aarch64, 4 OCPU / 24 GB, Ubuntu 24.04), which ALSO hosts a live website
(stock.mishra.eu.org, OpenLiteSpeed, MySQL). Docker is not installed; host git is 2.43.0 (< required 2.50.1).
This strengthens Codex's point 2.

## Concessions
1. **Memory trust (point 1): conceded in full.** Auto-active memory = typed observations only
   (command ID, exit status, base/head SHA, policy digest, timeout flag, evidence ref). All prose lessons
   are pending until approved, and even approved memory is delivered as an untrusted data block in the
   user turn, never in the system prompt. Per-stage selected IDs + content hashes are frozen and recorded.
   Memory can never alter command policy, protected paths, repo scope or the approved spec. No FTS in v1.
2. **Isolation (point 2): conceded in full, and the new target makes it non-negotiable.** No repo command
   runs on that host outside a container.
3. **Reviewer (point 3): conceded.** Planner/coder split already exists (RefineModel/CodeModel). Step 2
   adds only an opt-in reviewer, patch-bound and spec-bound verdict, one repair cycle, off by default.
   Fail/timeout/malformed verdict never reads as approval.

## Rebuttals / additions
A. **Order.** The owner's goal is a deployed, tested service. Memory designed before any live run data
   is guesswork (we don't know which observations are useful). So: live baseline → isolation → server
   pilot → memory → reviewer → webhooks. Memory and reviewer become measured additions on a running pilot.
B. **Baseline location.** M16 live baseline can run on the owner's Mac against a throwaway demo repo the
   owner created (trusted content, no live services at risk). It does not need the isolated target yet.
C. **Docker is not automatically a boundary either.** The Docker daemon is root-equivalent: the patchbot
   service user must NOT be in the `docker` group and must never see the socket. Options: rootless Docker
   (or Podman rootless) under a dedicated `patchbot` user, `--network none`, `--read-only` rootfs + tmpfs,
   `--cap-drop ALL`, `--security-opt no-new-privileges`, `--pids-limit`, `--memory`, `--cpus`, per-run
   workspace volume only, non-root UID inside. CyberPanel's Docker Manager runs a rootful daemon — we should
   not use it for patchbot; it is for the panel's own apps.
D. **Workspace boundary.** File tools stay on host (os.Root jail) operating on the bind-mounted workspace;
   only `execx` moves into the container. Git (clone/push with token) stays on the host, never in the
   container, so the GitHub token never enters the sandbox. Host git must be upgraded to >= 2.50.1.
E. **arm64.** Test images must be multi-arch/arm64 (golang, node, php official images are). Per-repo image
   is an operator config field, digest-pinned.
F. **Webhooks behind Cloudflare Access.** Access blocks GitHub deliveries unless a bypass path is added for
   one route; that is another reason to defer webhooks and keep polling.
G. **Concurrency on a shared host.** One active run globally on that box, cgroup limits sized so the live
   site keeps >= 2 OCPU / 12 GB headroom.

## Proposed merged milestones
| # | Milestone |
|---|---|
| M16 | Live baseline on Mac + demo repo (Ollama via ai.mishra.eu.org and one priced Anthropic model), record evidence; fix what breaks |
| M17 | Container executor (rootless Docker/Podman), host git stays host-side, limits + network none, adversarial tests (read host files, fork bomb, disk fill, net egress) |
| M18 | Deploy rehearsal on Mumbai: dedicated user, systemd, loopback + Cloudflare Tunnel + Access, git >= 2.50.1, sqlite online backup + restore drill, restart/stale-run drill |
| M19 | Real-server pilot + fault drill (approval, obsolete approval, repair, cancel, restart, PR ambiguity), measure host impact on the live site |
| M20 | Memory v1 (typed observations + approved lessons, untrusted delivery, frozen selection, off switch) |
| M21 | Opt-in reviewer with seeded-defect comparison |
| M22 | Webhooks only if polling latency hurts |

Codex: accept, amend or reject this order and C–G. Final word, keep it short.
