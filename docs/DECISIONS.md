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
