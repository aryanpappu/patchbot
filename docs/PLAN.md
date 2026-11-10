# patchbot: Step 1 plan (merged)

Status: **awaiting owner approval**, 2026-11-10.
Sources: `planning/BRIEF.md` (requirements and owner decisions), `planning/PLAN-fable.md` (main design),
`planning/PLAN-codex.md` (independent review). Where they disagree, this file decides. For detail not
repeated here (exact type signatures, adapter mapping tables, failure table), read the section of
PLAN-fable.md given in brackets.

patchbot is written from scratch. No code, prompts or text from any employer project.

---

## 1. What Step 1 delivers

```
patchbot run    --repo owner/name --issue 12 [--mode fix|refine|investigate]
                [--provider anthropic --model claude-sonnet-5-5] [--dry-run]
patchbot approve <run-id> | revise <run-id> --notes "..." | reject <run-id> | cancel <run-id>
patchbot runs [--status awaiting_approval]      # list runs
patchbot models [--provider gemini]             # live model list, config fallback
patchbot config check | show                    # effective config with sources, secrets masked
patchbot serve [--addr 127.0.0.1:8080]          # the web page
```

Not in Step 1: webhooks, background polling of new issues, a durable worker queue, Docker, login, deploy.
The code leaves seams for each one (section 10).

## 2. The run flow, with the refine phase

```
issue ──► preflight ──► clone (read-only) ──► REFINE: explore + write spec
                                                   │
                         unclear ◄─────────────────┼─────────────────► clear
                            │                                             │
               post questions on issue                      post spec on issue
               status: needs_info                     status: awaiting_approval
                            │                                             │
                owner answers + /revise          ┌──────── owner decides ────────┐
                            │                    │ approve      revise      reject│
                            └──► re-refine ◄─────┘    │    (notes→new spec)   │   │
                                                      ▼                  closed ◄─┘
                         re-clone at the spec's base SHA, new branch patchbot/issue-<n>
                                                      ▼
                       baseline tests ─► CODE (agent loop) ─► submit tool
                                                      ▼
                     harness runs tests ──fail──► feed output back (max 3 attempts)
                                                      ▼ pass (or attempts used up)
                         commit ─► push ─► PR (draft if tests not green) ─► issue comment
```

**Modes (labels):**

| Label / `--mode` | Phases |
|---|---|
| `ai-refine` | refine only, ends at `awaiting_approval` or `needs_info`, never codes |
| `ai-fix` | refine → **your approval** → code → test → PR |
| `ai-investigate` | read-only analysis, one comment, no spec and no code |

**The spec** (stored in `specs`, posted as a comment, editable in the UI): summary, acceptance criteria
(checklist), files likely to change, risks, test plan, open questions. Each spec has a version number and
a SHA-256 hash.

**The approval gate (hard rule, tested):**
- Coding starts only from `approved`, and an approval is bound to one spec version and hash. Editing or
  revising the spec creates a new version, which clears the approval.
- Ways to approve: the UI **Approve** button, `patchbot approve <id>`, or an `/approve` comment on the
  issue from a GitHub login listed in `approvers` in the config. Comments from anyone else are ignored.
- Step 1 has no webhooks, so `serve` checks issue comments **only for runs that are waiting**, every 60 s.
  `patchbot runs --sync` does the same check once from the CLI.
- `/revise <notes>` in a comment, the UI or the CLI reruns refine with the notes, then waits again.
- `reject` closes the run. Nothing is pushed.
- The PR body links the approved spec and ticks each acceptance criterion the agent claims, next to the
  harness test result. The model's own "tests pass" never counts.

**Approval details (from Fable's revision):**
- The gate is a hash match between `specs` and `approvals`, not a boolean flag. It is checked in
  `Approve`, at the start of the code stage, and before write tools are registered.
- An approval is voided by a revise, a UI edit, or a change to the issue title or body after the spec
  was written.
- An `/approve` comment counts only if its author is in `approvers` **and** a repo collaborator.
  Edited comments never count. Hand edits to the spec comment on GitHub are not imported: the bot asks
  for `/revise` or a UI edit instead.
- Waiting runs expire after 14 days. A run allows at most 5 revisions.
- There is no pre-approve flag on `patchbot run`.
- Retry creates a **new run** linked by `retry_of`. An already-approved spec is reused only if the issue
  is unchanged.
- One budget covers the whole run, including refine and every revision. Per-run budgets are capped by
  `hard_max_cost_usd` in the config.
- CLI and UI approvals are recorded under a configured local name, because Step 1 has no login.

**Statuses:** `queued → running → awaiting_approval | needs_info → approved → running → succeeded | failed |
cancelled | rejected`. A separate `phase` column shows `preflight, refine, clone, baseline, code, verify,
publish`. All non-terminal statuses count as "active", and a partial unique index allows one active run
per issue.

**Workspace between phases:** refine clones read-only and records the base SHA. The workspace is deleted
while the run waits. On approval the code phase re-clones at that same SHA, so a run can wait for days
without holding a directory. If the default branch moved, the PR says so.

## 3. LLM providers and model choice  [Fable 3.1 to 3.5]

- One interface: `Name()`, `Chat(ctx, ChatRequest) (ChatResponse, error)`, `ListModels(ctx)`.
  Calls are non-streaming. Live updates in the UI come from events for each step.
- Normalized types: roles `user | assistant | tool`, ordered parts (text, tool call, tool result) and an
  opaque `ProviderMeta` part. That part carries Anthropic thinking blocks and Gemini thought signatures
  back unchanged. The adapter fails loudly on unknown required blocks and never drops them silently.
- Adapters:
  - **anthropic:** the official `anthropic-sdk-go`, with its own retries turned off.
  - **gemini:** raw `net/http` against the public REST API.
  - **openai:** raw `net/http` against Chat Completions. This covers OpenAI, OpenRouter, Ollama, LM Studio
    and Groq. Several named instances can be set up, each with its own base URL, key env var and a quirks
    table.
- Tool calls run one at a time, in the order the model gave. Arguments are checked locally against a small
  JSON Schema subset, whether or not the provider enforces schemas.
- Retries cover 429, 5xx and network errors only, up to 4 attempts with jitter, honouring `Retry-After`.
  Auth errors, bad-request errors and refusals fail at once.
- **Model choice:** per run, from the UI dropdown, `--model`, the config default, or the provider default.
  `ListModels` is cached for 5 minutes and merged with the config list; if it fails, the config list is
  shown with a warning. The UI also accepts a typed model ID. Provider and model are fixed once a run
  starts. Retry can pick another one.
- Seed Anthropic IDs: `claude-opus-5-5`, `claude-sonnet-5-5` (default), `claude-haiku-5-5`, `claude-fable-5-2`.
  No Gemini or OpenAI model names are hardcoded; they come from the live list or your config.
- Refine and code may use **different models** (`refine_model`, `code_model`). For example, a strong model
  writes the spec and a cheaper one codes.

## 4. Agent loop  [Fable 4.3 to 4.6]

- Tools: `read_file`, `list_dir`, `search`, `edit_file` (unique exact match or fail), `write_file`
  (atomic), `run_cmd`, `submit`. Refine and investigate get the **read-only** set plus `submit_spec` or
  `submit_report`.
- Each turn checks, in order: cancel, deadline, max turns, budget, then compaction at 70% of the context
  window (old tool outputs are replaced by a stub; call/result pairs stay intact).
- Default limits per run, all configurable: 40 turns, 3 test attempts, **$2** and 2M tokens, 30 minutes,
  2 minutes per command, 256 KB command output stored and 8 KB sent to the model.
- Budget: before each call the loop reserves a worst-case cost (last input + max output). Cost is kept in
  integer micro-USD. A model with no price set may run only under the token cap and logs a warning. A
  local model with price 0 must say so explicitly.
- Stuck detector: the same call with the same arguments 3 times gets a warning, and a second trip ends
  the run.
- **Salvage rule:** if the code phase hits a limit with changes in the tree, the harness runs the tests
  once and opens a **draft** PR that says why it stopped. A cancel, refusal or auth error pushes nothing.
- No diff means no PR. The run ends `no_changes` and comments with the explanation.

## 5. Security  [Fable 8, Codex 5]

Step 1 is **not a sandbox**. Tests run repository code as your user. The README states this up front.
Rules that follow from it:

1. **Repo allowlist:** only repos listed under `[repos.*]` in the config can run, and only if the browser
   or a flag cannot add one. This comes from Codex and replaces Fable's "any repo the token can push to".
2. **Command IDs, not command strings:** `run_cmd` takes a name defined per repo, such as `test`, `lint`
   or `build`, mapped to a literal argv like `["go","test","./..."]`. No shell, pipes or redirection, and
   no model-supplied executable. This comes from Codex and is stricter than Fable's binary/subcommand
   allowlist.
3. **Path jail:** `os.Root` for every file tool. Absolute paths, `..`, NUL, and symlinks leading out are
   rejected. `.git/`, `.env*`, `.github/workflows/` and paths you configure are protected for read and
   write. A fuzz test covers this. Policy configuration, candidate paths and symlink targets reject
   combining marks U+0300–U+036F and all Unicode Mn/Me characters. This conservative rule refuses
   decomposed (NFD) spellings rather than allowing them to alias a protected composed (NFC) name;
   full Unicode normalization is not available without an additional dependency. Such names cannot
   be addressed by file tools and are omitted from directory listings.
4. **Go 1.27** (installed 1.27.3; must stay ≥ 1.25.12 because GO-2026-4970 fixed an `os.Root` symlink escape). The `go.mod` `go` line
   line and CI enforce it.
5. **Process control:** each command runs in its own process group. A timeout kills the whole group.
   Output is capped, and the child gets a minimal environment with no API keys or GitHub token, plus its
   own HOME and temp directory.
6. **Git:** the token is passed only through `GIT_CONFIG_*` env vars during clone and push, never in a
   URL, argv or `.git/config`. Hooks are off, submodules are refused, and `.git/config` is hash-checked
   before push. Three branch guards: never the default branch, never force, only `patchbot/*`.
7. **Secrets:** only from env vars. The config holds env var names, and a config key that looks like a
   secret is rejected. One redactor runs before SQLite, SSE, logs, PR bodies and tool results sent to the
   model.
8. **Prompt injection:** issue text, repo files and test output are marked as untrusted data in the
   prompt. Limits are enforced in code, not by the prompt. The agent has no network, GitHub or git tools.
   `@mentions` are neutralised in PR and comment text.
9. **Web:** binds 127.0.0.1 by default and refuses other addresses unless `PATCHBOT_UI_TOKEN` is set.
   Every POST is protected with Go's `http.CrossOriginProtection` plus a Host check. CSP header, no
   `v-html`, all LLM output shown as text.

## 6. Storage  [Fable 6, plus refine tables]

SQLite through `modernc.org/sqlite` (no CGO), WAL, foreign keys on, one writer connection, embedded
forward-only migrations with a small migrator. All times are UTC milliseconds, all money is micro-USD,
and NULL means unknown, never zero.

| Table | Holds |
|---|---|
| `runs` | id, repo, issue, mode, status, phase, outcome, provider, refine_model, code_model, base_sha, branch, pr_url, limits, totals, error_kind/message, heartbeat_at, timestamps |
| `specs` | run_id, version, body_md, hash, source (`agent`/`ui_edit`/`revise`), notes, comment_id, created_at |
| `approvals` | run_id, spec_version, spec_hash, decision (`approve`/`revise`/`reject`), actor, channel (`ui`/`cli`/`github`), created_at |
| `steps` | run_id, seq, phase, kind (llm/tool/test/git/publish), tool, status, duration, exit_code, capped redacted input/output, tokens, cost |
| `events` | run_id, seq, type, payload_json. This is the SSE replay source. |
| `actions` | the audit log of every UI/CLI action (start, edit spec, approve, revise, reject, cancel, retry) |

## 7. Web page (Go html/template + Vue 3 from a CDN, no build step)  [Fable 7]

- Go renders the page and puts the initial state in a `<script type="application/json">` block. Vue 3
  (global build from jsDelivr, exact version, SRI hash) mounts on it, using `[[ ]]` delimiters so it
  doesn't clash with Go templates. A `vue_src = "local"` option serves a vendored copy for offline use.
- Screens:
  1. **New run:** repo (dropdown of allowed repos), issue number, mode, provider → model (live list),
     optional separate refine model, budget, dry-run.
  2. **Runs list:** status chips, an "awaiting approval" badge with a count, cost, PR link, filters.
  3. **Run detail** (`/runs/{id}`): phase timeline, live log (SSE), token and cost counters,
     **spec panel** (rendered markdown with an edit box and version history), buttons **Approve,
     Revise (notes), Reject, Cancel, Retry (choose provider and model)**, diff view, test output, link
     to the PR.
- JSON API: `GET /api/providers`, `GET /api/providers/{id}/models`, `GET/POST /api/runs`,
  `GET /api/runs/{id}`, `PUT /api/runs/{id}/spec`, `POST /api/runs/{id}/approve|revise|reject|cancel|retry`,
  `GET /api/runs/{id}/events` (SSE).
- SSE: the event id is the per-run `seq`. The stream replays from SQLite after `Last-Event-ID`, then
  follows live, with a heartbeat every 15 s. Closing the browser never cancels a run; only Cancel does.

## 8. Configuration  [Fable 5]

TOML (`BurntSushi/toml`). Unknown keys are errors. Precedence: **flag > env (`PATCHBOT_*`) > file >
embedded defaults**. File search order: `--config`, `./patchbot.toml`, `~/.config/patchbot/config.toml`.

```toml
[defaults]
provider = "anthropic"
refine_model = "claude-opus-5-5"
code_model = "claude-sonnet-5-5"
max_cost_usd = 2.0
approvers = ["aryanpappu"]

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.gemini]
kind = "gemini"
api_key_env = "GEMINI_API_KEY"

[providers.ollama]
kind = "openai"
base_url = "http://localhost:11434/v1"
models = ["qwen3-coder"]          # example only, your own local model
price_free = true

[repos."aryanpappu/demo-app"]
commands = { test = ["go", "test", "./..."], lint = ["go", "vet", "./..."] }
protected = ["migrations/"]
```

## 9. Testing and CI  [Fable 9]

- A **fake provider** with scripted responses proves the agent loop, refine/approval gate, repair loop,
  budgets, stuck detection, cancel and salvage before any real API is called.
- **Golden tests** for each adapter: `httptest` server, request checked against `*.req.golden.json`,
  replayed recorded responses (redacted, dummy keys). These cover multi-call turns, missing IDs, Gemini
  signatures, malformed arguments, 429 and refusals.
- **Gate tests** (required): coding is impossible without an approval matching the current spec hash;
  editing the spec clears approval; `/approve` from a non-approver is ignored; reject pushes nothing.
- Jail table tests plus a fuzz test. Executor tests for timeouts, killing grandchildren, output floods and
  scrubbed env. Git integration tests against local bare repos, which never push to a real remote.
- A CLI end-to-end test: fake provider + fake GitHub (`httptest`) + local bare remote + real SQLite.
- CI (GitHub Actions, Linux + macOS): `gofmt`, `go vet`, `golangci-lint`, `go test -race`, `govulncheck`,
  and a `CGO_ENABLED=0` build. No provider keys in CI. Make targets: `fmt lint test race build ci`.

## 10. Roadmap after Step 1

| Step | Adds | Seam already in Step 1 |
|---|---|---|
| 2 | GitHub webhooks (HMAC, delivery dedupe) + label poller; `/approve` replies arrive instantly | `Runner.Start`, `runs.trigger` |
| 3 | Durable queue + worker pool with leases | `Dispatcher` interface, `queued` status, `heartbeat_at` |
| 4 | Docker executor: no network during tests, CPU/mem/pid limits, non-root | `Executor` separate from `Workspace` |
| 5 | Deploy: distroless image, Caddy TLS, GitHub App auth, UI login, Litestream backup | `TokenSource`, auth middleware slot |
| later | Optional Claude Managed Agents runner; PR review-comment follow-ups | `Provider`/`Runner` boundaries |

## 11. Build milestones (each ends with `make ci` green and one commit)

| # | Milestone | Owner | Done when |
|---|---|---|---|
| M0 | Scaffold: go.mod (go 1.27, local toolchain 1.27.3), cli dispatcher, version, Makefile, lint, CI, LICENSE, README stub | Codex | `patchbot version` works, CI green |
| M1 | Config, secrets, redactor, slog | Claude | precedence table passes; secret-looking key rejected; token prints `[redacted]` |
| M2 | Store + migrations (incl. specs, approvals, actions) | Codex | second active run per issue refused; stale-run recovery works |
| M3 | LLM core types, errors, retry, pricing, schema, fake provider | Claude | invariants validated; micro-USD math exact |
| M4 | Workspace jail + file tools | Claude | every escape case refused; fuzz 30 s clean |
| M5 | Executor, command IDs, `run_cmd`, `submit*` tools | Codex, reviewed by Claude | grandchild killed on timeout; 10 MB output bounded; env clean |
| M6 | Agent loop + **refine phase + approval gate** | Claude | all fake-provider cases and gate tests pass under `-race` |
| M7 | Anthropic adapter + `patchbot models` | Claude | golden tests; live list with key, config fallback without |
| M8 | OpenAI-compatible adapter | Codex | golden tests incl. quirks; works with local Ollama |
| M9 | Gemini adapter | Codex | golden tests incl. signature round trip |
| M10 | Git workspace + guards | Claude | token never in argv or `.git/config`; branch guards tested |
| M11 | GitHub client (issues, comments, PRs, comment sync for `/approve`) | Codex | `httptest` suite passes |
| M12 | Runner + `patchbot run/approve/revise/reject/cancel/runs` | Claude | dry run prints spec, diff and PR text and pushes nothing; real run on a demo repo opens a PR after approval |
| M13 | Events, JSON API, SSE | Codex, reviewed by Claude | replay + `Last-Event-ID` reconnect works |
| M14 | Web page (Vue via CDN) | Claude | start → spec → approve → live log → PR, all from the browser; reload keeps state |
| M15 | Hardening, README, SECURITY.md, ARCHITECTURE.md, tag v0.1.0 | Claude | fresh clone to first dry run in under 5 minutes |

M7, M8 and M9 don't depend on each other and can run in parallel. Claude reviews and tests every Codex
milestone before it is committed.

## 12. Decisions on the open questions (owner may override)

1. Module path `github.com/aryanpappu/patchbot`.
2. No test command found: **draft PR** marked "unverified".
3. Salvage rule: **on** (draft PR when a limit is hit with changes).
4. Failure comments on the issue: **on**, with a `comment_on_failure` switch.
5. Baseline test run: **on**.
6. Dependency installs on the host (`npm ci`, `composer install`): **off by default**. Allowed only as an
   explicit per-repo command ID until Docker arrives.
7. Gemini: raw HTTP (lighter binary). Could switch to the SDK later in one package.
8. Prices: **none shipped**. `patchbot config check` warns when a model has no price. You add prices from
   the official pages.
9. Repo allowlist: **required** (rule 5.1).
10. Transcripts: capped, redacted steps only, no full conversation.

## Trimmed from the Codex plan (too heavy for Step 1)

OS-level lock files per run, publication-intent reconciliation records, multi-process admission across
data directories, and per-HTTP-attempt cost reservations. A simple heartbeat, a partial unique index and
"mark interrupted on restart" are enough for one person on one machine. These come back in Step 3.
