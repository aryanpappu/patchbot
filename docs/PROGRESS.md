# Progress

Updated 2026-11-10 20:45. Step 1 plan: `docs/PLAN.md`. Decisions: `docs/DECISIONS.md`.

## Merged into `development` (reviewed, `make ci` green)
M0 scaffold · M1 config/secrets/redaction/logging · M2 SQLite store · M3 LLM core + fake provider · M4 workspace jail +
file tools · M5a command executor · M7 Anthropic adapter + `patchbot models` · M10 git operations · M11 GitHub client

## In progress (branches pushed)
| Branch | Milestone | State | Next step |
|---|---|---|---|
| `m8-openai` | M8 OpenAI-compatible adapter | built, green, committed | fresh Codex review (stopped at usage limit), then merge |
| `m9-gemini` | M9 Gemini adapter | built, green, committed | fresh Codex review (stopped at usage limit), then merge |
| `m6-agent` | M5b tools + M6 agent loop, refine, approval gate | WIP, about 300 lines, not compiling-checked | rerun the M6 brief on this branch, continuing the WIP |

## Remaining
M12 runner + CLI (run/approve/revise/reject/cancel/runs) · M13 events, JSON API, SSE · M14 web page (Vue via CDN) ·
M15 hardening, README, SECURITY.md, ARCHITECTURE.md, v0.1.0 · follow-up: execx grace-period polling

## Working method
Codex (gpt-6.1-sol, high/xhigh) builds, fixes and reviews in worktrees under `../patchbot-wt/`; Claude writes briefs,
runs `make ci`, spot-checks and merges. Briefs live in the session scratchpad; the M6 brief is summarised in PLAN.md
sections 2, 4 and 5.
