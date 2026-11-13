# Step 2 debate: Claude's opening position (2026-11-13)

Current state: v0.1.0, M0–M15 done. Single agent loop per phase (refine → approval → code → verify → publish),
context compaction only. No cross-run memory, no multi-agent orchestration, no webhooks, no durable queue,
no Docker executor, not deployed. Owner wants: memory, orchestration, then deploy and test on a real server
(candidate: Oracle "oracle-staging" box, Ubuntu, 1 OCPU / 6 GB, free tier; Ollama qwen2.5 7b available on another box).

## Proposal (ordered by value / risk)

1. **Repo memory (M16)** — per-repo, SQLite-backed, small and reviewable.
   - What is stored: facts the harness *verified* (test command that worked, build command, lint command,
     files touched per issue, reasons previous runs failed), plus short "lessons" the model proposes at
     the end of a run.
   - Model-proposed lessons are untrusted (prompt-injection from issue text could plant them). They are
     stored as `pending` and only become active after operator approval in UI/CLI. Verified harness facts
     are active immediately.
   - Injected into the refine/code system prompt with a hard token cap (e.g. 2 KB), newest/most-used first.
   - `patchbot memory list|approve|forget` + a UI panel. Redactor runs before store.
   - No vector DB, no embeddings. SQLite FTS5 keyword match on the issue text is enough.

2. **Orchestration, minimal (M17)** — not a swarm.
   - Role split inside one run: planner (refine, strong model), coder (cheaper model allowed), reviewer
     (read-only pass over the final diff vs. the approved spec before publish; can send back once).
   - Per-role provider/model in config. Shares the one run budget. Reviewer verdict goes in the PR body.
   - No parallel sub-agents editing the same tree.

3. **Webhooks + label trigger (M18)** — HMAC, delivery dedupe, `/approve` instantly. Required before
   deploying it as a service; otherwise polling is fine.

4. **Deploy (M19)** — systemd unit on oracle-staging, bind loopback, Cloudflare Tunnel + Access in front
   (no public port), token auth on, SQLite backup via cron. Docker executor deferred: patchbot runs only on
   the owner's own allowlisted repos.

5. **Live E2E test** — a throwaway demo repo, Ollama (free) and one Anthropic run with a $1 cap.

Deferred: durable queue/worker leases, Docker executor, GitHub App auth, multi-user login.

## Owner update (2026-11-13): deploy target changed
Deploy on the Oracle Mumbai CyberPanel box (aarch64, 4 OCPU / 24 GB / 193 GB, Ubuntu 24.04), not oracle-staging.
That box also hosts a live site (stock.mishra.eu.org). Docker is NOT installed yet, but CyberPanel ships a
Docker Manager module that can install it. Host git is 2.43.0, below patchbot's required 2.50.1.
Consequence: the Docker executor (old Step 4) moves into Step 2 — repo commands must run in a container
there (no network during tests, CPU/mem/pid limits, non-root), so they cannot touch the live site.
