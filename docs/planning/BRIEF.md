# patchbot: planning brief

## Product
patchbot is a self-hosted, single-binary Go tool. It turns a GitHub issue into a tested pull request using an LLM tool-calling loop.
It is a personal portfolio project and must be written from scratch. It is inspired by a work project, but no code or prompts from it may be copied.

## Core flow
GitHub issue (label `ai-fix` or `ai-investigate`) → clone repo into isolated workspace → new branch `patchbot/issue-<n>` →
LLM agent loop with tools (read_file, write_file/apply edit, list_dir, search, run_cmd) → run repo's test command →
on failure feed output back (max N attempts) → push → open PR (draft if tests still fail) → comment on issue.
`ai-investigate` = analysis comment only, no code changes.

## Hard requirements from the owner
1. **Multi-provider LLM, switchable**: Anthropic (Claude), Google Gemini, and a generic OpenAI-compatible provider
   (OpenAI, OpenRouter, Ollama, LM Studio, Groq...). One `Provider` interface with tool calling, normalized messages.
2. **Model choice**: the user picks the provider + model per run (CLI flag, config file default, and in the web UI).
   Models are listed from each provider's list-models API where available, with a fallback to the config.
   Known Anthropic IDs: claude-opus-5-5, claude-sonnet-5-5, claude-haiku-5-5, claude-fable-5-2. Do not hardcode Gemini/OpenAI model names as truth; keep them configurable.
3. **Step 1 scope (what we build first)**:
   - `patchbot run --repo owner/name --issue 12 [--provider anthropic --model claude-sonnet-5-5] [--dry-run]` does the whole loop locally, no server.
   - `patchbot serve`: a single server-rendered page (Go html/template), with Vue 3 loaded from a CDN (no npm build step) for interactivity:
     a provider/model picker, a "start run" form (repo + issue), a runs list and a live run log (SSE).
   - SQLite store for runs, steps, tokens and cost (modernc.org/sqlite, no CGO).
   - Webhooks, queue/worker pool and Docker sandbox come in later steps, but the design must leave room for them.
4. Professional quality: idiomatic Go 1.25, stdlib net/http (Go 1.22+ routing), slog, context everywhere, table-driven tests,
   a fake provider for deterministic agent-loop tests, golangci-lint config, Makefile, README, MIT license, GitHub Actions CI.
5. Safety: file tools jailed to the workspace (no path escape, no symlink escape), run_cmd allowlist + timeout + output cap,
   per-run token/dollar budget cap, never push to the default branch, secrets only from env (never stored in DB/logs, redact in logs),
   one active run per issue.

## Environment
macOS arm64, Go 1.25.4, Node 20 available but we avoid a JS build step. Owner is a senior PHP/Laravel + React dev learning Go.

## Decisions added by the owner (2026-11-10)
6. **No Managed Agents in the core.** patchbot runs its own provider-neutral agent loop. Managed Agents may come later as an optional Claude-only runner.
7. **Refine phase with spec approval (required).** Before any code is written:
   - The agent reads the issue and explores the repo read-only, then writes a spec: summary, acceptance criteria, files to touch, risks, test plan.
   - If the issue is unclear, it posts questions as an issue comment and the run stops at `needs_info`.
   - Otherwise it posts the spec as an issue comment and the run stops at `awaiting_approval`.
   - Coding starts only after the owner approves: an `/approve` comment from an allowed GitHub user on the issue, `patchbot approve <run-id>` on the CLI, or the Approve button in the web UI. `/revise <notes>` makes it rewrite the spec.
   - If the spec is edited or revised, it needs a new approval. The PR body links the approved spec and checks each acceptance criterion.
   - Labels: `ai-refine` (spec or questions only), `ai-fix` (refine → approval → code → test → PR), `ai-investigate` (analysis comment only).
8. **Full control from the web UI.** Everything the CLI or GitHub comments can do, the UI can do too:
   start a run (repo, issue, mode, provider, model, budget), view and edit the spec, Approve, Revise with notes,
   Reject, Cancel a running run, Retry a failed run (optionally with another provider/model), and view the live log,
   diff, test output, tokens and cost. Edits made in the UI also need approval, and every action is recorded in the run history.
