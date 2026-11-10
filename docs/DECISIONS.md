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
