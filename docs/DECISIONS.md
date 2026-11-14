# Decisions log

Decisions made while building, newest last. The owner set the goal; Claude (Fable) and Codex decide routine
engineering questions together and record them here.

| Date | Decision | Why |
|---|---|---|
| 2026-11-10 | Go 1.27 (local 1.27.3) | Includes the GO-2026-4970 `os.Root` symlink fix the path jail depends on |
| 2026-11-10 | Remote over SSH, work on `development`, `main` only by PR | HTTPS had no credentials; keeps `main` releasable |
| 2026-11-10 | Each milestone is built in its own worktree under `../patchbot-wt/`, reviewed by the other agent plus `make ci`, merged as one commit | Lets Fable and Codex work in parallel without clashing |
| 2026-11-10 | Codex model `gpt-6.1-sol` at high reasoning; Claude side `claude-fable-5-2` | Best models available to each tool |
| 2026-11-10 | Open questions 1 to 10 from PLAN.md section 12 take the listed defaults | Owner delegated routine decisions |
| 2026-11-10 | Follow-up: `execx.stopGroup` should poll for group exit instead of always waiting the full grace period | Minor latency on cancel; not a safety issue |
| 2026-11-10 | Add `golang.org/x/text` for Unicode normalization in the path jail | Canonical-equivalent names (e.g. Hangul Jamo) bypassed protected paths on APFS; a hand-rolled table would be worse |
| 2026-11-10 | Require git >= 2.50.1 and refuse diffs over 5 MiB per file / 50 MiB total | Known clone-time CVEs in older git; git diff memory is unbounded on huge files |
| 2026-11-13 | M20 memory: only typed harness observations are auto-active; prose lessons need operator approval; all memory is untrusted user-turn data with a frozen, recorded per-stage selection, no FTS or use-count ranking | Codex round 1 section 1: a zero exit status verifies an execution, not a lesson, and remembered prose must never become policy |
| 2026-11-13 | Every git remote operation (clone, fetch, ls-remote, push) uses the header-only `GIT_CONFIG_*` extraheader; `gitx.command` refuses remote subcommands without it and credentials for local ones | Private repos failed on unauthenticated `ls-remote`/re-fetch; tests use an auth-requiring `git http-backend` HTTPS remote |
| 2026-11-13 | Shared identity: a token-owner comment counts only if the owner is an approver and collaborator, the comment is unedited, carries no `<!-- patchbot` marker and its ID is not in `posted_comments` (migration 0007) | Single-user PAT made GitHub `/approve` impossible; dropping the bot check would let patchbot's own text approve |

## 2026-11-10: CI fixes and trusted path follow-up
- The revive `unexported-return` exception for `agent.Check` lives in `.golangci.yml`, scoped to `gate.go`,
  instead of an inline `nolint`. revive flagged it on macOS but not on Linux, so nolintlint failed CI.
- `execx` tests use `true`, not `go`, because CI runners install Go outside `TrustedPath`.
- Follow-up for M15: make `TrustedPath` configurable (an absolute-dirs-only `[limits] trusted_path`) so toolchains
  installed through asdf, mise or a toolcache can run repo tests.

## 2026-11-11: M15 release hardening

- `limits.trusted_path` defaults to the old execx path. Missing directories are
  allowed for portable configs. Existing entries must be directories without
  world writes. Validation resolves existing prefixes and symlinks, including
  dangling aliases, and checks data and actual workspace roots. The immutable
  policy supplies both executable lookup and child PATH.
- Doctor is offline unless `--online` is explicit. It reports absent fresh
  storage as a warning. It never creates a database or migrates an existing one.
  Existing pending, modified, gapped or newer migration ledgers fail inspection.
  Online probes only call provider ListModels and GitHub /user, with deadlines.
- Startup sets Unix umask 077 before work so git and child commands inherit
  private creation defaults. App-owned files and directories use 0600/0700.
  File rewrites retain only owner permission bits, including existing execute
  permission. Existing public data directories/databases are refused.
- The smoke script builds the production binary and tests its doctor command.
  The Go integration uses the same CLI dispatcher with injected fake provider,
  httptest GitHub and local bare remote. No production fake-provider flag or
  alternative approval path is added. The fixture includes its actual Go bin
  directory for setup-go/toolcache portability.
- CLI usage errors omit unparsed values. Loaded provider credentials plus
  GitHub/UI credentials are masked at the final output stream too. Registered
  credentials are masked even below the generic redactor's eight-byte threshold.
- This task does not create a release commit, tag or push. An offline cached
  scanner without a vulnerability database is not evidence of a clean scan.

## 2026-11-13: M21 opt-in reviewer
- Reviewer is opt-in and sequential: verdict bound to spec hash + diff hash (base, full tree ID, bounded diff), one repair inside the approved code stage, fail closed to failure or a marked draft, selection frozen on the run at approval, one shared budget with a 15% default reserve.

## 2026-11-13: M17 container executor
- `[executor] kind = "container"` runs every repository command in a fresh rootless podman/docker container (digest-pinned image, `--network none`, read-only rootfs + tmpfs, `--cap-drop ALL`, no-new-privileges, pids/memory/cpu limits, non-root uid, only the workspace mounted); default stays `local`, git and file tools stay host-side and the GitHub token never enters the container.
- Review hardening: create + start --attach with confirmed removal by name, runtime CLI cwd `/`, `.git` mounted read-only, `workspace_max_bytes` (2 GiB), blanked proxy env plus a private DOCKER_CONFIG, and a startup sweep of labelled containers when no run is active.

## 2026-11-13: M22 webhook hints and label ingress

- Webhooks are off by default. HMAC-SHA256 over at most 1 MiB of raw body replaces
  UI bearer authentication only at the configured webhook path. Authentication
  precedes JSON parsing. Missing secrets prevent enabled-server startup.
- Persist normalized wake-up hints, deduplicate delivery IDs permanently, and
  acknowledge before reconciliation. Startup and periodic maintenance replay
  pending receipts; no raw webhook content authorizes approval.
- Reuse the runner's comment sync and Start gate. Keep comment polling; add
  paginated open-issue label polling only when webhook ingress is enabled.
- Persist the selected trigger label and generation. An atomic unique run key
  prevents duplicate automatic starts even if a competing run already finished.
  Use lexical label priority. An observed label removal/change rearms the trigger;
  unobserved remove/reapply cycles need explicit operator retry.
- Reserve migrations 0003/0004 with single comments in this branch as requested;
  webhook state belongs to 0005. Reconcile those placeholders before using M20/M21
  migrations against an existing database, since migration checksums are immutable.
- M22 review fixes: coalesce pending receipts per issue (cap 1000 pending, 10 attempts, 404/410 done, 14-day prune), poll labels on `webhook.label_poll_interval` (5m) from listing data only, and treat a repository's first label sweep as a no-start baseline; schema in 0006 because 0005 is deployed.
- M27: GitHub App auth (`[github] auth = "app"`) mints per-run installation tokens narrowed to one repository ID with contents/issues/pull_requests write + metadata read, routed by explicit `RepoRef` (REST) and a per-operation `TokenFunc` (git); PAT mode unchanged and explicit; no fallback from App to PAT; key file only, never env/DB.
