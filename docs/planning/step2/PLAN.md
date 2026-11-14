# patchbot Step 2 plan (merged, owner-approved 2026-11-13)

Sources: claude-position.md, codex-round1.md, claude-round2.md, codex-round2.md (Codex final word; where it
adds stricter acceptance criteria for M17/M18 those criteria win).

Deploy target: Oracle Mumbai CyberPanel box (aarch64, 4 OCPU/24 GB, Ubuntu 24.04). It also hosts the live
site stock.mishra.eu.org, so isolation is mandatory and patchbot is fully separated from CyberPanel users.

## Agreed principles
1. Memory: auto-active memory = typed observations only (command ID, exit status, base/head SHA, policy
   digest, timeout flag, evidence ref). Prose lessons are `pending` until operator approval. All memory is
   delivered as an untrusted data block, never in system instructions. Per-stage selected entry IDs +
   content hashes are frozen and recorded. Memory cannot change command policy, protected paths, repo
   scope or the approved spec. Off switch. No FTS/embeddings in v1; deterministic bounded selection.
2. Orchestration: planner/coder split already exists (RefineModel/CodeModel). Add only an opt-in
   sequential reviewer: read-only tools, structured verdict bound to spec hash + diff hash, one repair
   cycle with re-verification, shared budget/deadline. Timeout/refusal/malformed verdict never approves.
   Off by default.
3. Isolation: every repo command (baseline, agent run_cmd, harness tests) runs in a rootless container:
   `--network none`, read-only rootfs + tmpfs, `--cap-drop ALL`, no-new-privileges, pids/memory/cpu limits,
   non-root UID, only the per-run workspace mounted. The service user is never in the `docker` group and
   never sees a rootful socket. Git with the token stays host-side; file tools stay host-side (os.Root).
4. Webhooks are wake-up hints: HMAC over raw body, durable delivery dedupe, then reconcile against GitHub
   state; polling remains. Labels can start refinement, never approve. Off by default.
5. Deploy: dedicated `patchbot` Unix user (not a CyberPanel/site user), systemd, loopback bind + UI token,
   Cloudflare Tunnel + Access (owner only), host git >= 2.50.1, one active run, cgroup caps leaving the live
   site >= 2 OCPU / 12 GB, SQLite online backup + restore drill.

## Milestones
| # | Milestone | Owner | Done when |
|---|---|---|---|
| M16 | Live baseline on Mac with a demo repo | Claude (needs owner GitHub token + model key) | refine→approve→code→test→PR on demo repo; evidence recorded in docs/M16-REPORT.md |
| M17 | Container executor (podman/docker rootless, `executor = "local"\|"container"`) | Claude agent | adversarial tests: host-file read, net egress, fork bomb, disk fill, descendant kill on cancel all contained; local executor unchanged |
| M18 | Deploy rehearsal on Mumbai | Claude | user/systemd/tunnel/Access up; doctor green; backup+restore and restart/stale-run drills pass |
| M19 | Server pilot + fault drill | Claude | approve, obsolete approval, repair, cancel, restart, PR ambiguity on server; live-site impact measured |
| M20 | Memory v1 | Claude agent | tests: repo separation, pending/revoked never retrieved, zero-exit stdout poison never becomes trusted prose, memory can't alter policy/approval, frozen selection recorded |
| M21 | Opt-in reviewer | Claude agent | verdict bound to spec+diff hash; fail-closed; one repair + re-verify; budget shared; seeded-defect fake tests |
| M22 | Webhook/label ingress (off by default) | Codex | HMAC, bounded body, durable dedupe, reconcile; duplicate/reordered/missed delivery tests; no label approval |

Migration numbers reserved: 0003 memory, 0004 reviewer, 0005 webhooks.
Every milestone: `make ci` green, code-review pass, one conventional commit, merged into `development`.

## Owner requests after the first pilot round (2026-11-13), to build together later
| # | Milestone | Notes |
|---|---|---|
| M23 | Login-based dashboard | Verify the Cloudflare Access JWT (`Cf-Access-Jwt-Assertion`, team JWKS, audience tag) server-side and record the logged-in email as the approval actor; replaces the dropped UI token. Optional local user/password login for non-Cloudflare installs. |
| M24 | Repository management in the dashboard | Add/edit/remove allowlisted repos (commands, test command, protected paths, image) from the UI, stored in SQLite with audit; config file stays the bootstrap. Requires M23 login first. |
| M25 | Per-repo container image | `[repos."x"] image = ...` overriding `[executor] image`, so Go and PHP repos run side by side. |
| M26 | Create issue from the dashboard | Write a feature request in the UI → patchbot opens the GitHub issue and starts refine. |

Interim (2026-11-13): UI token removed on Mumbai at owner request. Port 8080 on loopback is limited by
iptables (owner match) to root (cloudflared) and the patchbot user, so other local users and CyberPanel
sites cannot reach the API; Cloudflare Access (owner only) is the only login.

## Build order for M23-M28 (owner: "complete all phases", 2026-11-14)
| Phase | Milestones | Migration numbers reserved |
|---|---|---|
| 1 (parallel) | M23 login via Cloudflare Access JWT; M25 per-repo image; M28 "Connect GitHub" App manifest flow | M23: 0008, M28: 0009, M25: none |
| 2 (parallel, after phase 1 merged) | M24 repo management in dashboard; M26 create issue from dashboard | M24: 0010, M26: 0011 |
| 3 | Codex security review per milestone, merge, make ci, deploy to Mumbai | |
| 4 | M19 live pilot on pappumishra #9 + fault drills (owner clicks Connect GitHub once) | |
| 5 | M29 dashboard redesign (see M29-dashboard-redesign.md) | none expected |
