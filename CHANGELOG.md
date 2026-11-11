# Changelog

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
