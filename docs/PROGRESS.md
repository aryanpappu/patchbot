# Progress

Updated 2026-11-13. Step 2 plan: docs/planning/step2/PLAN.md. M0–M15 implementation is done in this release worktree.
No release commit, tag or push was created by M15. See [M15 report](M15-REPORT.md)
for local checks, audit fixes and external validation limits.

| Milestone | State |
| --- | --- |
| M0 Scaffold, build, CI and license | Done |
| M1 Config, secrets, redaction and logging | Done |
| M2 SQLite store and migrations | Done |
| M3 LLM types, retry, pricing, schema and fake | Done |
| M4 Workspace jail and file tools | Done |
| M5 Command executor and tools | Done |
| M6 Agent loop, refinement and approval gate | Done |
| M7 Anthropic and model discovery | Done |
| M8 OpenAI-compatible adapter | Done |
| M9 Gemini adapter | Done |
| M10 Guarded git operations | Done |
| M11 GitHub client and comment commands | Done |
| M12 Runner and CLI controls | Done |
| M13 JSON API, events, SSE and lifecycle fixes | Done |
| M14 Embedded web UI and verified Vue asset | Done |
| M15 Hardening, doctor, offline smoke and release docs | Done |
| M16 Live baseline | Folded into M19 (owner: no local testing) |
| M17 Rootless container executor | Done; 9 isolation tests pass on Mumbai |
| M18 Deploy rehearsal on Oracle Mumbai | Done; see M18 report |
| M19 Live pilot and fault drills | Waiting for GitHub token on the server |
| M20 Repository memory | Done |
| M21 Opt-in reviewer | Done (off by default) |
| M22 Webhook and label ingress | Done (off by default), review fixes merged |
| M23 Login-based dashboard | Done (Cloudflare Access JWT; token mode default) |
| M26 Create issue from the dashboard | Done (`POST /api/issues`, migration 0011; approval gate unchanged) |
| M24 Repository management | Done (dashboard add/edit/remove, audited, frozen per run; migration 0010) |
| M29 Dashboard redesign | Done in code (ES modules, design system, inbox, run tabs, nav, dialogs, shortcuts); Chrome check on the Mumbai deploy pending |

## Known limitations

- Not a sandbox. Repository commands run as your user with host/network access.
  Process-group cleanup does not contain deliberate group escapes.
- One local operator. Local approvals use a configured name. No login, TLS,
  webhook receiver, durable worker queue, container executor or deploy system.
- GitHub waiting commands use polling. Runtime model calls are non-streaming.
- No prices are shipped. Unknown costs use token limits. Provider/model IDs in
  examples need checking against the operator's endpoint.
- Push/PR failures need manual reconciliation. Retry is a new run. Multi-process
  admission across data directories and worker leases are future work.
- Specs render as plain text. Browser runtime behavior and live provider/GitHub
  credentials need operator validation; the automated smoke uses local fakes.
- An offline govulncheck needs both the scanner and a vulnerability database.
  See the M15 report for the database availability in this environment.

[PLAN.md](PLAN.md) records the original design. [DECISIONS.md](DECISIONS.md)
records implementation choices. Older milestone reports describe their state at
that time; the current source and M15 report describe this release worktree.
