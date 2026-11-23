# Changelog

## Unreleased

- Add answers to refinement questions and support-level escalation (M30). A
  `needs_info` run shows one answer box per open question; `POST
  /api/runs/{id}/answer` records the answers (bound to the spec version and
  hash) and revises the spec with them. Opt-in `[escalation]` retries a run one
  level up a provider/model ladder when it ends with a configured trigger
  (provider errors, `max_turns`, exhausted budget, incomplete refinement, or a
  dry run whose tests still fail), reusing a still-valid approved spec; manual
  `POST /api/runs/{id}/escalate`. Migration 0012 rebuilds `actions` for the new
  audit kinds.
- Fix GitHub App mode reporting no push permission (every fix run failed
  preflight) and Gemini replay rejecting tool calls whose arguments the agent
  loop had re-encoded with sorted keys.

- Redesign the dashboard (M29): `app.js` is split into native ES modules
  (`main.js`, `components/*`, `composables/use*`, `ui.js`, `format.js`) served from
  a fixed asset list (shared by token mode and Cloudflare Access mode, no
  wildcards); CSP and the pinned Vue SRI are unchanged. New "Waiting for you"
  inbox landing page, run phase stepper with plain-language failure reason, run
  tabs (Spec, Live log, Changes), budget meters, sidebar navigation with badge
  counts and `(n) patchbot` tab title, light/dark themes, confirm dialogs for
  Reject/Cancel/Forget/Remove, toasts, empty/loading/error states and keyboard
  shortcuts. New pages `/memory`; `/` now opens the inbox. No API changes.
- Add "Create issue from the dashboard" (M26): `POST /api/issues` opens a GitHub
  issue (with a visible "created from the patchbot dashboard" footer, no
  requester email, and a hidden `patchbot:issue-created` marker) and can start the refine run
  through the same path as `POST /api/runs`. A client idempotency key makes
  retries safe, including after a crash mid-request. `/runs/new` gets a "Write a
  new request" mode. Migration 0011 (`dashboard_issues`); the approval gate is
  unchanged.
- Add repository management in the dashboard (M24): a Repositories page and
  `/api/repos` to add, edit and remove allowlisted repositories (commands as argv,
  test command, protected paths, digest-pinned image and PATH) with an append-only
  audit of who changed what. Config-file repositories stay read-only; changes apply to
  new runs without a restart, runs in flight keep the settings they started with,
  and removal is refused while a repository has active runs. Migration 0010.

- Add "Connect GitHub" (M28): create the GitHub App from the dashboard with the
  App manifest flow (`[github] setup_public_url`), the key is written straight
  to a 0600 file and only the App ID and slug are stored. Without an App the
  server starts in setup mode and refuses runs; `doctor` reports it.
- Add Cloudflare Access login (`[server.auth] mode = "cloudflare_access"`, M23):
  verifies the `Cf-Access-Jwt-Assertion` JWT (RS256, team JWKS, issuer, audience,
  allowed emails), records the verified email as the actor of UI/API decisions,
  adds `GET /api/session` and a "Signed in as" header, and extends `doctor`.
  Token mode is unchanged and remains the default.

- Add GitHub App authentication (`[github] auth = "app"`, M27): per-run
  installation tokens narrowed to one repository ID and four permissions,
  refreshed per git network operation, bot identity `<slug>[bot]` for comments
  and commits, and app-mode `doctor` checks. PAT mode is unchanged and remains
  the default.

- Add repository memory: typed harness observations, operator-approved lessons,
  bounded deterministic retrieval as untrusted user-turn data, frozen per-stage
  selection records, `patchbot memory` commands, API endpoints and a UI panel.
- Memory is **enabled by default** (`[memory] enabled = true`). This adds the
  `propose_lesson` tool to the code stage and writes typed observations after
  baseline and verification tests. Set `enabled = false` to turn both off.
- Memory approval is bound to the reviewed content: `memory show` prints the
  full text and content hash, `memory approve ID --hash PREFIX` (and the UI/API
  `content_hash`) must match, and entries outside the repo allowlist are hidden
  and cannot be decided. Lesson text must be printable (no format, bidi, tag,
  private-use or separator characters). Memory write failures only warn.

## v0.1.0 — 2026-11-11

- Refine GitHub issues into versioned specs with explicit approval before code.
- Run code and harness tests, then publish a guarded branch and PR or draft.
- Support Anthropic, Gemini and OpenAI-compatible providers with model discovery.
- Add a local CLI and web UI with persisted events, SSE replay and artifact views.
- Enforce repo/command allowlists, file jail, clean env, budgets and redaction.
- Configure trusted tool directories and return promptly after group cancellation.
- Add offline doctor checks and an offline CLI smoke test.
- Document setup, architecture, security boundaries and release limits.

Repository commands run as the operator. This release has no sandbox, login,
webhooks, durable worker queue or deployment system. A dry run still needs
approval and can execute host code. See SECURITY.md before use.
