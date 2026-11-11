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
