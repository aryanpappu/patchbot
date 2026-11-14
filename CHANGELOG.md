# Changelog

## Unreleased

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
