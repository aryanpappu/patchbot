# patchbot implementation plan

Independent engineering plan based on `docs/planning/BRIEF.md`. This repository currently contains planning documents, with no application or existing package conventions to preserve. This document proposes implementation decisions; it does not implement them. All implementation, prompts, and fixtures must be original or based on public provider specifications, never copied from the motivating work project.

## 1. Scope, assumptions, and decisions

Build the local CLI workflow first, then expose the same application service through a single-page server. Step 1 includes three LLM adapters, model discovery, SQLite persistence, bounded agent execution, GitHub publication, and live web logs. It excludes webhooks, a durable work queue, parallel workers, remote multi-user operation, automatic merging, and container isolation.

Important scope decisions:

- Step 1 is for repositories and executable test code trusted by the operator. Issue text, model responses, repository content, and command output remain untrusted instructions. A local subprocess is arbitrary host code execution: neither a path jail nor a command allowlist makes tests a secure sandbox. Do not accept arbitrary public-repository submissions through the server. An operator-owned repository allowlist and explicit local-execution configuration are prerequisites for executing tests.
- `run` obtains repo and issue explicitly. `--mode fix|investigate` can select behavior; otherwise exactly one of `ai-fix` or `ai-investigate` must be present. Reject conflicting or absent labels rather than guessing. Later webhook admission can reuse this rule.
- Fix runs may edit and run approved commands. Investigation runs expose only read/list/search tools, create no branch or commit, and post an analysis comment. No `run_cmd` in investigation mode: even a test command can write files or execute arbitrary code.
- `--dry-run` allows local edits, LLM calls, and explicitly trusted tests but performs no GitHub mutations, push, PR, or issue comment. It still needs network access to fetch the issue/repository and may incur LLM costs. Print this meaning in CLI help and the UI.
- Use immutable per-run provider/model/config selection. Switching a picker changes the next run, not an ongoing conversation.
- Define `max_repair_attempts = N` as N repair cycles after the initial final verification. A baseline run is separate; final-verification executions are at most N+1. Provider HTTP retries and agent tool turns have separate bounds.
- First release permits one active run per issue across CLI/server processes sharing one data directory, and one executing run per server process. A second web submission while busy returns a visible conflict; no hidden in-memory queue.
- Never publish an empty patch or call an infrastructure failure a failing test. Draft publication is for a real patch whose authoritative tests failed after allowed repairs. Security violations, unknown spend, cancellation, or incomplete verification stop publication.

## 2. Repository layout and boundaries

Proposed layout:

```text
cmd/patchbot/main.go               process entry, signals, exit status
internal/cli/                     flag parsing, run/serve wiring
internal/config/                  parsing, precedence, validation, safe snapshot
internal/domain/                  run state, IDs, usage, events, errors
internal/app/                     RunService, admission, orchestration
internal/agent/                   conversation loop, limits, repair sessions
internal/llm/                     Provider contract and normalized types
internal/llm/anthropic/            Messages adapter and discovery
internal/llm/gemini/               native Gemini adapter and discovery
internal/llm/openaicompat/         Chat Completions adapter and discovery
internal/tools/                   schemas, validation, mode-specific registry
internal/workspace/               private clone, jailed I/O, patch validation
internal/runner/                  approved command execution, process lifetime
internal/github/                  issue retrieval, PR/comment reconciliation
internal/store/                   SQLite repositories and embedded migrations
internal/events/                  sanitized event publishing/replay
internal/redact/                  central streaming and structured redaction
internal/web/                     stdlib routes, handlers, embedded assets
internal/web/templates/index.html Go html/template shell
internal/web/static/app.js        Vue interaction, no build step
internal/web/static/app.css
internal/prompts/                 original versioned embedded instructions
testdata/providers/               synthetic/sanitized provider wire fixtures
testdata/repos/                   tiny fixture repository sources
configs/patchbot.example.json     non-secret example and explicit policies
docs/                            operations, security, provider compatibility
.github/workflows/ci.yml
.golangci.yml
Makefile
README.md
LICENSE                          MIT
```

Keep packages responsibility-based; no generic `utils`, public SDK, dependency-injection framework, or interface for every struct. Define small interfaces at consuming boundaries: provider, GitHub gateway, workspace factory, command runner, store/event sink, clock where needed. Main constructs concrete dependencies. CLI and HTTP must never contain their own agent loops.

Use Go 1.25 language compatibility, `context.Context` as the first parameter for blocking work, error wrapping with operation context, `errors.Is/As` for typed failures, and structured `slog` fields. No panic for provider/tool failures. Embed templates, prompts, and migrations into the binary. Git and any configured test tools remain external prerequisites; “single binary” means application distribution, not bundling compilers or Git.

The brief's Go 1.25.4 environment is not a security pin. Check Go security advisories before implementation and use a patched toolchain. In particular, [GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970) documents an `os.Root` symlink escape fixed in Go 1.25.12 on that release line. If the 1.25 line is no longer supported, use a supported patched toolchain while preserving the requested language compatibility where possible.

## 3. LLM abstraction and provider contracts

### Normalized interface

Define a single `Provider` interface with `Generate(ctx, Request) (Response, error)` and `ListModels(ctx) ([]Model, error)`. Resolve a named provider instance from configuration: two OpenAI-compatible instances can have distinct names, endpoints, credentials, and defaults. Keep transport HTTP and endpoint construction inside the adapter.

Request includes model ID, separate system instructions, ordered conversation messages, tool definitions, and a maximum output-token allowance. Response includes ordered content blocks, normalized stop reason, usage with explicit known/unknown fields, provider request ID, and optional in-memory opaque continuation metadata. Provider errors expose category, retryability, HTTP status, Retry-After, and sanitized diagnostic details.

Message parts must represent ordered text and tool calls/results. Tool calls carry internal stable ID, original provider ID when present, name, raw JSON arguments, and the validated typed arguments only after tool validation. Tool results carry matching call ID, tool name, bounded content, and `is_error`. Preserve the assistant message containing all calls before appending any results. Do not flatten mixed text/tool output into one string.

Use a deliberately small JSON Schema subset: objects, explicit properties, required fields, arrays, enums, and primitive values. Avoid recursive schemas, unions, remote references, and provider-specific strict-mode keywords in the shared schema. Validate independently in Go, reject unknown fields and trailing JSON, enforce byte/depth limits, and validate numeric bounds. Providers claiming schema enforcement do not replace local validation.

### Provider-specific handling

| Adapter | Encoding and continuation | Required edge cases |
| --- | --- | --- |
| Anthropic | Separate system instructions; ordered assistant text/`tool_use` blocks; user `tool_result` blocks matched by `tool_use_id`, with error flag. Return all results before further conversational text. | Multiple calls, mixed blocks, `tool_use` vs normal completion, token truncation, refusal, unknown blocks. Preserve required signed thinking/continuation blocks in memory if enabled; default advanced thinking features off until fixtures prove replay. |
| Gemini | Native content roles and `functionCall` / `functionResponse` parts; system instruction separate; result uses function name and structured response. | Parallel calls, repeated function names, optional/version-dependent call IDs, blocked/empty candidates, malformed arguments, token limits, and opaque thought signatures attached to the correct part. Preserve ordering and original parts needed for replay. Never synthesize or discard signatures. |
| OpenAI-compatible | Target Chat Completions: assistant `tool_calls` with JSON-string arguments, followed by `tool` messages with `tool_call_id`. | Missing/null content, several calls, malformed JSON strings, absent usage, refusal/content filter, length finish, unsupported optional parameters, and incompatible endpoint behavior. Do not assume compatibility with the distinct Responses protocol. |

These mappings follow [Anthropic tool handling](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls), [Gemini function calling](https://ai.google.dev/gemini-api/docs/generate-content/function-calling), [Gemini continuation signatures](https://ai.google.dev/gemini-api/docs/generate-content/thinking), and the [Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create). Recheck the selected API versions when implementing; these are protocol boundaries, not promises that every model supports every feature.

Additional rules:

1. Start with non-streaming LLM responses. SSE streams application events and command output independently. Never execute a partially streamed or truncated tool call.
2. Normalize stop reasons to complete, tools, length, blocked, or unsupported. Text accompanying calls is not completion. Reject inconsistent calls/stop states unless a tested adapter-specific mapping handles them. Unknown required continuation blocks cause an explicit unsupported response, not silent loss.
3. Execute tool batches sequentially in provider order. Do not introduce parallel writes or read/write races. Every valid call gets a result, including tool execution errors. Missing IDs may receive deterministic internal IDs only when the provider protocol permits name/order correlation; ambiguous or duplicate provider IDs are protocol errors.
4. Cache completed call outcomes by run, turn, and call ID. Same ID plus different arguments is an error. This prevents immediate duplicate execution; it is not a durable crash-resume guarantee. No automatic execution resume in Step 1.
5. Retry only bounded transient transport failures, 429, and selected 5xx responses with jitter and cancellation-aware Retry-After. Three total HTTP attempts is a reasonable default. Authentication, invalid model, unsupported tools, malformed requests, and policy blocks fail clearly. An ambiguous timeout can have incurred charges; account conservatively and never retry a tool side effect as an HTTP retry.
6. Use explicit per-instance capability settings for optional request fields, maximum-output parameter naming, tool support, and usage semantics. Do not silently strip tools or silently switch models. A compatibility error should identify the instance/model and the rejected feature without echoing request secrets.
7. Keep the transcript in memory. Store only sanitized summaries and metadata by default; do not persist opaque thinking, raw prompts, or provider payloads. Context overflow stops the run with an actionable reason in Step 1. Later compaction must preserve complete call/result groups and provider continuation constraints.

### Discovery and selection

Call each adapter's models endpoint with timeout, response cap, and pagination handling. Gemini discovery should filter on documented generation capabilities where available; a listed model still may not support tools. OpenAI-compatible discovery often provides no tool-capability metadata. Keep configured capability overrides and do not present discovery as proof of compatibility.

Cache results for five minutes by provider instance and configuration revision. Merge configured IDs with discovered IDs, preserve exact requested IDs, and label source/staleness. If discovery fails or is unsupported, show configured models and a sanitized warning; a discovery outage must not prevent a configured model from running. Allow explicit model entry subject to length/control-character validation and server policy.

Include the brief's Anthropic IDs (`claude-opus-5-5`, `claude-sonnet-5-5`, `claude-haiku-5-5`, `claude-fable-5-2`) as owner-supplied example configuration, not verified availability or immutable defaults. Do not hardcode Gemini/OpenAI model names. Resolve provider before choosing its default model; never accidentally carry one provider's default into another adapter.

### Token and dollar budgets

Track input, output, cached, and reasoning tokens separately when available, plus raw usage categories needed to avoid double counting. Costs use configured, dated per-model rates and integer micro-USD with conservative rounding; no floating-point billing arithmetic. Display costs as estimates, not invoices.

Before each request, reserve an upper bound for its possible input and maximum output cost and tokens, including provider-specific billed categories. Reduce output allowance to remaining budget. If no reliable local token bound/counting method exists, use a documented conservative bound with model context ceiling, or refuse budget-enforced execution. Do not rely on `characters/4` as a hard limit. Each network attempt must fit its own reservation; ambiguous charges retain their reservation. Reconcile only with authoritative usage. Missing usage keeps the conservative reservation and marks accounting uncertain.

Require configured pricing for a positive dollar cap; local free models need an explicit zero-price entry. Unknown price is not zero. Stop before issuing any request that might exceed limits. A strict guarantee against a provider billing differently is impossible; explain that the enforceable cap is against the configured conservative accounting model. Bound total turns, tool calls, elapsed runtime, command time, and response sizes independently.

## 4. Run lifecycle, repair loop, and GitHub publication

### Application state machine

Persist explicit states: `admitted → preparing → baseline → agent → verifying → repairing → publishing → succeeded`. Investigation omits baseline, verification, and code publication, proceeding from agent to analysis-comment publication. Terminal alternatives are `draft_pr`, `no_change`, `failed`, `cancelled`, `budget_exceeded`, `interrupted`, and `publication_uncertain`. A dry run records its local outcome with `dry_run=true` and never enters external mutation operations. Keep terminal status distinct from phase and test outcome.

Detailed sequence:

1. Validate CLI/form input, configured repo allowlist, credentials presence, provider selection, test policy, budgets, and execution trust. Fetch issue from the configured GitHub host, bound issue length, and snapshot title/body/labels for this run. Ignore comments unless explicitly added to product scope. Admit by transactional issue lock before any clone/LLM work.
2. Resolve default branch and its commit SHA from GitHub; clone that exact base into a new random private directory. Avoid user-supplied Git URLs and local clone paths. Fix mode creates `patchbot/issue-<n>` when available. If a remote branch already exists, use `patchbot/issue-<n>-<run-short-id>`; never overwrite or force-push another run's work.
3. For fix runs, execute the trusted configured baseline test command, record exit classification and sanitized bounded output, then ensure baseline execution has not left tracked edits; restore only this disposable clone to the pinned base before editing. Keep build artifacts isolated where possible. Baseline failure is evidence, not permission to claim a later failure is unrelated. Missing executable, timeout, and dependency/network problems are infrastructure outcomes with distinct policy.
4. Build the original system prompt with tool descriptions, issue-as-data framing, intended task, and budgets. Run the loop: check cancellation/limits; request completion; account usage; validate response; append assistant blocks; validate/execute permitted calls; append bounded results; persist events. Invalid tool arguments produce structured errors within the same total-turn bound. Three consecutive identical errors or unchanged tool patterns stop as stalled.
5. Agent completion only signals readiness for verification. The orchestrator inspects the actual working-tree diff, changed-file limits, and protected paths; runs the trusted verification command itself; and records the current diff fingerprint and test result. A model's statement that tests passed is never sufficient.
6. If tests fail and repair attempts remain, append trusted verification status plus untrusted/redacted output to the conversation, increment repair count, and re-enter the agent. Each repair session still shares the original total time/token/dollar/turn budgets. Any post-test edit invalidates that test result and requires a new verification.
7. When tests pass, prepare a normal PR. When tests genuinely fail after N repairs and the patch passes policy validation, prepare a draft PR with exact failure summary and baseline comparison. If no diff exists, finish `no_change` and optionally comment with the explanation; never create an empty commit/PR. For unreadable tests, exhaustion before verification, safety failures, or cancellation, retain local diagnostics and publish no patch.
8. Revalidate diff against pinned base, reject protected paths and untracked secret-like files, stage only enumerated allowed changed paths, then create the commit using controlled Git identity. Verify committed tree equals the tested tracked tree; reject unexpected staged changes. Check target branch differs from the latest default branch before push. Push an explicit source/destination refspec; no implicit push defaults, deletion refspecs, or force.
9. Create PR against the captured base branch after checking it is still appropriate. If default branch changed, stop for operator review instead of guessing. The head remains pinned to the run's base SHA; record base drift in the PR. Post one issue comment linking the PR and tests. Investigation publishes a clearly labeled analysis comment only.
10. Release admission lock, mark terminal result, close processes/files, and retain/delete workspace according to configured retention. Cleanup failure is logged separately without erasing a successful PR outcome.

External mutations need reconciliation. Persist a publishing intent before push/PR/comment, include a stable run marker in PR/comment bodies, and record returned IDs. After ambiguous GitHub responses, query branch/PR/comments by marker before repeating. On restart, do not replay the agent or blindly republish. Mark interrupted work, preserve publication intent, and offer a reconciliation operation or documented manual procedure. Failed issue comments must leave the PR URL visible and record a partial publication error.

CLI uses the same store and RunService as the web server, handles SIGINT/SIGTERM by cancelling the run context, and returns documented exit codes: success/local success, failed verification/draft, configuration failure, operational failure, cancellation. Closing an SSE browser connection does not cancel the run. A dedicated authenticated cancel action does.

## 5. Workspace, commands, and security

### File tool jail

Use a per-run directory mode 0700 under a configured data root and open the checkout with `os.OpenRoot`. Every model path is a relative slash-separated path. Reject empty/NUL/absolute/volume/UNC paths, `..` components, excessive length, and protected components before opening. Never trust a `filepath.Clean` plus string-prefix check or an `EvalSymlinks`-then-open sequence; those are race-prone. [Go's traversal-resistant file API explanation](https://go.dev/blog/osroot) describes the intended primitive and limitations.

Use Root-relative operations for reads, traversal, creation, rename, and removal. Accept symlinks only when the final operation remains confined by Root; reject external targets, loops, and unsupported file kinds. Writes use a same-directory temporary regular file and Root-relative rename, preserving appropriate permission bits. `apply_edit` requires an expected old-content hash or unique exact match; ambiguous/stale edits fail rather than silently changing the wrong section. Do not expose unrestricted deletion or file mode changes initially.

Deny `.git` and any case-equivalent protected path, credential/config directories, `.env` variants, and operator-configured sensitive paths for read, write, list, and search. For an allowed symlink alias to a protected path, containment alone is insufficient: inspect root-relative symlink resolution and reapply policy at each component; reject ambiguous aliases. Serialize file tools with command execution to reduce races; require race-resistant access for the outside-workspace boundary. Refuse a checkout that contains unsafe special files or suspicious hard links; clones should not use shared objects/hard-linked local sources. Document hard-link/mount limits rather than claiming Root controls them.

Implement bounded search in Go using Root-relative traversal, avoiding an external recursive search that follows links differently. Cap directory entries, file count, bytes scanned, returned matches, and individual file size. Skip binary files, denied paths, and symlink traversal by default. Tests must prove all file tools, not only `read_file`, enforce the same policy.

### Command policy and local execution limit

Model-facing `run_cmd` accepts an approved command identifier and narrowly typed options, never a shell command string. Configuration maps identifiers to an absolute executable, literal argv, fixed allowed working directory, timeout, and output cap. Prefer exact argv arrays for Step 1, such as an operator-approved test command. Do not allow arbitrary `go`/`git`/`python`/`sh` binaries with arbitrary arguments. No `sh -c`, pipes, substitutions, redirection, executable chosen from the checkout, arbitrary environment, or model-provided test command. Test commands come from operator configuration outside the clone, never from issue instructions or an automatically trusted repo file.

Resolve working directories through the jail. Launch with `exec.CommandContext`, a minimal explicit environment, isolated HOME/cache/temp directories, no inherited API/GitHub tokens, no interactive stdin, and fixed trusted PATH. For Go, constrain hidden behavior such as GOENV, GOFLAGS, workspace selection, and automatic toolchain download; document dependency-download policy. Approved tests may still execute edited code and scripts. Inspecting the command name does not prevent host reads, network egress, fork bombs, or generated files outside the checkout. Step 1 must say this plainly and use a dedicated OS account/host with minimal privileges for practical isolation.

On macOS/Linux manage a process group, propagate cancellation, escalate TERM to KILL, reap children, and set a bounded pipe-drain wait. Escaped/daemonized children are another limitation of local execution. Bound combined stdout/stderr; continue draining and discarding after the cap so the child cannot deadlock. Emit truncation metadata. Default command timeout: two minutes, output cap: 256 KiB; run lifetime: twenty minutes, all configurable within server ceilings. Disk/CPU/process hard limits and network isolation require an OS/container runner later; do not pretend a context deadline supplies them.

Separate the deterministic Git operations from the model tool registry. Disable hooks using a controlled empty hooks directory, ignore inherited Git/global config, reject submodules by default, disable recursive checkout, and avoid Git filters/external helpers from untrusted configuration. No token in clone URLs or command arguments. A narrow temporary credential helper may serve only the configured GitHub host during trusted clone/push; remove it and its environment before any test/model command. Never store credentials in `.git/config`.

### Prompt injection and outbound data

Issue descriptions, repo files, test logs, and LLM-produced text are data, even if they contain “system” instructions. Mark their origin in prompts, quote bounded content, and tell the agent they cannot change tools, credentials, budgets, destination repo, or publication policy. Enforce those limits outside the model. No prompt can guarantee injection resistance.

Do not expose network, GitHub mutation, credential, or shell tools to the agent. Deterministic application code controls all publication. The repo allowlist and provider endpoint policy are operator configuration and cannot be changed through the UI or issue. Repository content sent to an external provider is a deliberate data transfer; document it and allow only authorized repositories/providers.

### Secrets and redaction

Credentials come only from named environment variables; config stores environment variable names, never values. Keep provider/GitHub credentials in narrowly scoped clients. Use least-privilege GitHub credentials with only required repository issue/content/PR permissions. No credentials in run snapshots, DB, logs, fixtures, browser responses, URLs, or command environments.

Centralize redaction before persistence, SSE, `slog`, tool results returned to the LLM, error formatting, and publication bodies. Match known secret values plus common token/private-key formats; suppress authorization headers and credential-bearing URL components. A streaming redactor must retain an overlap buffer so split output chunks cannot leak a token. Sanitize terminal control characters. Debug mode does not enable raw HTTP bodies. Do not log full issue/file content by default.

Secret scanning cannot recognize every encoded or unknown credential. Restrict file reads, inspect outbound patch and text, keep raw artifacts private with short retention, and fail closed on known secret detection. Redaction must not rewrite files silently or corrupt provider continuation metadata; opaque provider fields stay memory-only and out of observable events.

### HTTP security

Bind `127.0.0.1` by default. Require a bootstrap session established using an env-provided UI secret, an HttpOnly SameSite cookie, CSRF tokens, and Origin/Host checks for mutations. Authenticate SSE and read endpoints too. Do not place tokens in SSE URLs. Remote binding requires explicit operator configuration and a trusted TLS/auth proxy; Step 1 does not claim multi-user authorization.

Accept only configured provider IDs, exact repo coordinates, and constrained model text from forms. Never accept a base URL or API key from browser input. Allow configured localhost HTTP for local LLMs; require HTTPS otherwise, prohibit credential-forwarding redirects, and bound remote responses. Configured endpoints are trusted operator choices; browser-controlled endpoints would be SSRF.

## 6. SQLite data model and consistency

Use `database/sql` with `modernc.org/sqlite`; no CGO. Embed numbered SQL migrations with a migration-version table. Apply migrations before accepting work under a migration lock; fail startup on migration errors. Use private data-directory permissions, WAL, foreign keys on every connection, a bounded busy timeout, and short transactions. Start with one application write connection, small read pool, and retry only well-defined busy transactions. Never hold a DB transaction during LLM, GitHub, or command execution.

Proposed schema (types shown as SQLite storage types; application validates JSON and enums):

| Table | Important columns and constraints |
| --- | --- |
| `schema_migrations` | version INTEGER PRIMARY KEY, checksum TEXT, applied_at INTEGER |
| `runs` | id TEXT PRIMARY KEY; host, owner, repo TEXT with canonical case normalization; issue_number INTEGER CHECK > 0; mode, status, phase TEXT; dry_run INTEGER CHECK IN (0,1); provider_instance, model TEXT; config_snapshot_json TEXT (safe fields only); prompt_version TEXT; base_branch, base_sha, branch, commit_sha TEXT nullable; workspace_id TEXT nullable; created_at, started_at, finished_at INTEGER UTC milliseconds; repair_count INTEGER; token_limit INTEGER; dollar_limit_microusd INTEGER; error_code, error_summary TEXT nullable; final_test_outcome TEXT; pr_number INTEGER nullable; pr_url TEXT nullable |
| `issue_locks` | host, owner, repo, issue_number composite PRIMARY KEY; run_id TEXT UNIQUE REFERENCES runs(id); owner_instance TEXT; acquired_at, heartbeat_at INTEGER |
| `steps` | id INTEGER PRIMARY KEY; run_id REFERENCES runs(id); seq INTEGER; kind TEXT; turn_number, repair_attempt INTEGER nullable; tool_call_id, tool_name TEXT nullable; status TEXT; started_at, finished_at INTEGER; duration_ms INTEGER; exit_code INTEGER nullable; summary_json TEXT; UNIQUE(run_id, seq). No raw file bodies or command argv containing credentials. |
| `llm_calls` | id INTEGER PRIMARY KEY; run_id REFERENCES runs(id); step_id REFERENCES steps(id); request_seq, http_attempt INTEGER; provider_request_id TEXT nullable; input_tokens, output_tokens, cached_tokens, reasoning_tokens INTEGER nullable; usage_known INTEGER; reserved_tokens INTEGER; reserved_cost_microusd INTEGER; estimated_cost_microusd INTEGER nullable; accounting_status TEXT; rate_snapshot_json TEXT; latency_ms INTEGER; finish_reason TEXT; UNIQUE(run_id, request_seq, http_attempt) |
| `events` | run_id REFERENCES runs(id), seq INTEGER, created_at INTEGER, event_type TEXT, payload_json TEXT (sanitized and byte-capped); PRIMARY KEY(run_id, seq). This is the SSE replay source. |
| `publications` | id INTEGER PRIMARY KEY; run_id REFERENCES runs(id); kind TEXT (push/PR/comment); marker TEXT UNIQUE; status TEXT (intent/confirmed/uncertain/failed); remote_id, remote_url, target_ref TEXT nullable; attempt_count INTEGER; last_error TEXT nullable; UNIQUE(run_id, kind) |

Add indexes on `runs(created_at DESC)`, `runs(host,owner,repo,issue_number,created_at)`, `runs(status)`, and `steps(run_id,seq)`. Usage totals are sums of `llm_calls`; cached totals on runs, if introduced, must update in the same transaction. Nullable usage/cost means unknown, never zero. Nonnegative CHECK constraints apply to known counts, limits, and costs.

Admission transaction inserts the run and issue lock together; a uniqueness conflict returns HTTP 409 or a clear CLI error with the active run ID. Terminal update, final event, and lock release are transactional. Add a partial unique index on issue identity for active run statuses as defense against lock-management bugs. Updating run/step state and inserting its event share one transaction; notify live subscribers only after commit.

CLI and server use the same canonical data directory by default. To handle stale processes safely, hold an OS advisory lock file for each active run for its entire lifetime, alongside the DB lock and heartbeat. Recovery may reclaim a DB issue lock only after obtaining that OS lock; timeout alone cannot steal work from a paused live process. Mark recovered runs `interrupted`, preserve publication intents, and never resume arbitrary tool execution. Different data directories or different machines do not coordinate; document that scope explicitly. Later multi-host workers require leases/fencing with stronger semantics.

Events are bounded per record and per run (for example 8 KiB per chunk and 10 MiB retained live-log data). On quota exhaustion keep lifecycle/error/usage events and emit one dropped-output marker; do not let command chatter fill disk. Retain event metadata longer than workspace contents. A cleanup command removes expired artifacts and events while retaining run summaries/publication IDs. Never delete active work. Document WAL-aware backup and restore; copying only the DB file during writes is not a backup procedure.

## 7. Single web page and SSE behavior

Use Go 1.22+ `net/http` patterns and `html/template` for the initial document. Embed CSS and plain JS. Load an exact pinned Vue 3 production compiler-included CDN build with integrity and crossorigin metadata; compute/verify integrity at implementation time. Use alternate Vue delimiters such as `[[ ... ]]` to avoid Go template conflicts. Prefer an HTML template element in the document; never interpolate issue text into executable JS or use `v-html` for logs/LLM output. If CDN loading fails, show a useful server-rendered message and basic run list.

Page regions: provider and model picker; repository/issue/mode/dry-run form; visible configured budget and test policy; recent runs with statuses, estimated cost, and PR links; selected run details with live log, cancellation button, and truncation/reconnection indicators. Default to the provider's configured model; disable models while an older provider's discovery request is pending and discard stale discovery responses. Do not expose credential values or full config snapshots. Start returns the persisted run ID before execution proceeds.

Routes:

- `GET /`: shell, recent runs, CSRF state, safe provider metadata.
- `POST /session` and `POST /logout`: local UI session lifecycle, rate-limited authentication.
- `GET /api/providers` and `GET /api/providers/{id}/models`: enabled instances, discovery/fallback source and warnings.
- `POST /api/runs`: bounded JSON/form body, validated immutable RunSpec; return 202 with ID or 409 for busy/active issue.
- `GET /api/runs?before=...&limit=...` and `GET /api/runs/{id}`: bounded pagination and sanitized details.
- `POST /api/runs/{id}/cancel`: idempotent cancellation request.
- `GET /api/runs/{id}/events`: authorized SSE replay/live stream.

SSE events use per-run monotonic sequence as `id`, named event types (`status`, `step`, `log`, `usage`, `publication`, `terminal`), and JSON payloads. Send `text/event-stream`, disable intermediary caching/buffering, flush complete records, and heartbeat around every 15 seconds. Parse `Last-Event-ID`, reject invalid or cross-run cursors, replay persisted events, then follow committed notifications. Avoid replay/subscribe races by treating notifications only as wakeups and always querying DB for `seq > cursor` until caught up. Slow clients get bounded buffers and disconnect/reconnect, never backpressure the agent.

Use request-scoped cancellation for SSE only. Run contexts derive from application lifecycle. Configure header/idle timeouts, maximum request bytes, SSE write deadlines/heartbeats appropriate for long responses, and graceful shutdown that cancels runs and closes streams within a deadline. On terminal event the browser closes EventSource; late subscribers still receive the persisted terminal event. If retention removed old events, send an explicit reset/gap record plus current run state.

## 8. Configuration and precedence

Use JSON config decoded with unknown-field rejection to avoid adding a YAML dependency. Pick one documented platform-aware default path (`os.UserConfigDir()/patchbot/config.json`) and one data directory (`os.UserConfigDir()/patchbot/data` initially, overridable). `--config` selects exactly one file; do not merge arbitrary repo-local files. Explicitly requested missing files are errors; a missing default file is acceptable only if required settings resolve elsewhere.

Resolution order, lowest to highest: built-in safe defaults → selected config file → documented `PATCHBOT_*` non-secret environment settings → explicitly supplied CLI flags or web per-run fields. CLI and web are separate entry points at the last tier. Track whether a flag was explicitly set so `false` and `0` do not accidentally override configuration. Per-run input cannot loosen server ceilings, execution permissions, repo/endpoint allowlists, protected paths, or credentials.

Config sections:

- Provider instances: adapter kind, base URL, API version/profile, credential environment-variable name, fallback model IDs, per-instance default model, capabilities, context/output ceilings, dated prices.
- Run defaults: provider, mode rules, token/dollar limits, total time, max turns/tool calls, repair attempts, output/context limits, dry-run default.
- Repository policies keyed by exact host/owner/repo: allowed modes, trusted-local-execution flag, named test command with literal argv, protected paths, patch/file-size ceilings, dependency-network policy, publication permission.
- Storage/workspace: data root, retention, event/disk quotas.
- Server: listen address, session-secret environment-variable name, allowed hosts/origins, maximum executing runs fixed to one for Step 1.
- GitHub: fixed host/API URL, credential environment-variable name, controlled commit identity.

Example defaults worth making explicit: 40 model turns, 120 tool calls, N=2 repair attempts, 20-minute run, 2-minute command, 256 KiB command output. Choose a conservative token cap and require an explicit dollar cap/rate configuration before billable execution. These are policy choices, not universal model limits.

Validate all resolved settings before creating work. Report missing env variable names without values. `config check` and `config show --effective` should print only safe fields and provenance. Persist that safe effective configuration and rate snapshot per run so later edits do not change its behavior. CLI `--provider` without `--model` selects the chosen provider's default; incompatible model/provider selection fails before clone. Credentials are read from env independently of general precedence and can never be supplied through config, CLI, or HTTP bodies.

## 9. Test strategy and fixtures

Tests should prove behavior at boundaries, not mirror method bodies. All default tests run offline without credentials.

- **Fake provider:** scripted Generate responses, tool calls, usage, errors, and call recording. Assert the exact loop contracts: assistant calls before results; multiple calls; invalid arguments; repeated call IDs; stalled agent; completion leading to independent verification; cancellation; unknown usage; budget reservation; context exhaustion; refusal/truncation. The fake runner and fake GitHub client make publication assertions deterministic.
- **Repair table tests:** baseline pass/fail; initial verification pass; failure then repair success; N=0; exhaustion producing draft; no diff; changed file after tests invalidating result; infrastructure failure; failed safety check; cancelled run; dry-run and investigate causing zero forbidden external mutations. Assert final status, verification count, repair count, and publication calls.
- **Adapter wire tests:** local `httptest.Server` checks headers, paths, query params, version settings, schema translation, and exact conversation continuation. Fixtures cover each provider's mixed content, parallel calls, missing/duplicate IDs, Gemini signatures, malformed arguments, blocked/empty result, missing usage, 429/Retry-After, 5xx, auth error, oversized body, pagination, and unknown blocks. Use dummy secrets and synthetic signatures that test byte preservation; no production transcripts. Fixture README records source protocol version and expected normalization.
- **Recorded fixtures:** opt-in recording tool/procedure against operator-owned trivial prompts, never CI credentials. Redact at recording time, manually review before commit, and use an explicit update flag. Replay fixtures through real adapters, not just fake-provider objects. A scheduled manual conformance run may detect upstream drift but must not be required for offline tests.
- **Workspace security:** table tests for `../`, absolute/UNC/volume paths, sibling-prefix tricks, NUL, trailing separators, Unicode/case policy, symlink outside root, dangling/loop links, aliases into `.git`/secret paths, parent-component swaps, and atomic writes. Exercise list/search/edit too. Fuzz path validation and tool JSON parsing; run race-oriented traversal tests on macOS and Linux. Ensure the patched runtime regression is covered.
- **Runner:** helper subprocess fixtures exercise timeout, cancellation, grandchildren, split secrets, output flood, inherited env removal, literal argv/no-shell behavior, unapproved command IDs, cwd rejection, and output-cap draining. Document platform-specific process-group limitations. Never run destructive real-host probes.
- **SQLite:** temp on-disk databases, migrations from empty and prior version, foreign keys, concurrent CLI/server admission, terminal lock release, OS-lock recovery, interrupted publication, rollback, usage totals, event ordering and retention. In-memory DB alone will miss file-lock/WAL behavior.
- **Git/GitHub:** temp bare remotes and tiny generated repos; test branch collision, default-branch protection, no force-push, hook/config suppression, exact tree verification, no empty PR, and ambiguous PR/comment success reconciliation through a fake API. Tests must never push to a real repository.
- **Web:** `httptest` for escaping/XSS payloads, CSRF/session/Origin/Host, unauthorized reads/SSE, input limits, discovery fallback, stale selection behavior, 409 admission, SSE replay/reconnect/gaps/terminal/slow clients. Include a manual browser checklist for Vue CDN failure, two tabs, cancellation, and reconnection; no Node build dependency.

A small CLI end-to-end test combines fake HTTP provider/GitHub servers, a disposable local Git remote, real SQLite, and an approved helper test executable. Assert one complete success and one draft flow. This supplies more confidence than many mocked happy-path unit tests.

## 10. CI, developer workflow, and documentation

Makefile targets: `fmt`, `fmt-check`, `vet`, `test`, `test-race`, `lint`, `build`, `check`, and opt-in provider conformance. Pin a golangci-lint version compatible with the chosen patched Go toolchain. Start with actionable checks (`govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`), adding stricter checks only with clear benefit. Check HTTP body/file/process cleanup and error handling.

GitHub Actions on pushes/PRs: least-privilege `contents: read`; pinned action commit SHAs; job timeouts; concurrency cancellation; patched supported Go toolchain and module cache; format check, vet, lint, unit/integration tests, race tests on a suitable runner, and `govulncheck` with a documented handling process. Build with `CGO_ENABLED=0` for macOS arm64 and Linux amd64/arm64 to verify pure-Go SQLite and embedded assets. Run platform-sensitive workspace/runner tests on both macOS and Linux. No provider keys, write tokens, or network integration tests on PR workflows, and no `pull_request_target` execution of contributed code.

README must include setup, provider examples, model selection/fallback, config precedence, trusted-repository restriction, local-runner limitations, dry-run semantics, cost estimate caveats, test command policy, CLI/server examples, cancellation/recovery, retention/backup, and later container work. Include MIT license and provenance notes for fixture sources. Keep original prompt files versioned and reviewable.

## 11. Ordered milestones and acceptance criteria

- [ ] **M0 — Freeze contracts and threat model.** Record local trust boundary, dry-run/investigate semantics, retry definitions, publication eligibility, and provider contract. Acceptance: every requirement in BRIEF maps to a milestone/test; no claim of hostile-code isolation; unresolved API capabilities have explicit failure behavior.
- [ ] **M1 — Foundation and configuration.** Add original Go module, CLI skeleton, config validation/provenance, slog/redactor, embedded resources, Makefile, MIT, and CI scaffold. Acceptance: binary builds without CGO; invalid/unknown config fails cleanly; credentials cannot appear in effective config/log tests; pinned patched toolchain passes checks.
- [ ] **M2 — Persistence, state, and admission.** Add migrations, RunService shell, transactional events, issue locks/OS locks, cancellation and recovery states. Acceptance: competing processes cannot admit the same issue; killed-run recovery cannot steal a live run; event/state rollback and migration tests pass.
- [ ] **M3 — Workspace and runner boundary.** Implement private clone, jailed file tools, protected paths, exact command policy, minimal env, time/output caps, and Git controls. Acceptance: path/symlink/protected-alias tests pass on macOS/Linux; flood/timeout/cancel tests finish boundedly; no credential reaches helper processes; agent cannot invoke Git publication.
- [ ] **M4 — Fake-provider agent engine.** Add normalized messages, tool registry, loop, budgets, termination, baseline/final verification, and repair cycles. Acceptance: deterministic success, N+1 verification bound, draft eligibility, no-change, dry-run, investigate, cancellation, malformed calls, and budget tests pass without network.
- [ ] **M5 — Three real adapters and discovery.** Implement Anthropic, Gemini, and OpenAI-compatible transport with fixture replay and fallback models. Acceptance: each adapter completes a two-turn tool interaction through recorded/synthetic HTTP fixtures, preserves continuation metadata, handles discovery failure, and rejects unsupported tool capabilities clearly. Optional manual smoke test per configured provider is documented, not required in CI.
- [ ] **M6 — Complete local CLI and GitHub publication.** Wire exact issue/base fetch, full local loop, safe commit/push/PR/comment and reconciliation records. Acceptance: disposable-remote E2E success and failing-test draft paths work; default branch never receives a push; ambiguous publication is reconciled; dry-run has zero mutations and investigation creates only its analysis comment.
- [ ] **M7 — Web page and SSE.** Add embedded template/Vue page, provider/model picker, admission form, run list/details, authenticated replayable logs and cancellation. Acceptance: start-to-terminal manual browser flow works; reconnect loses no retained events; slow/disconnected browsers do not affect execution; auth/CSRF/XSS/selection tests pass; no JS build step.
- [ ] **M8 — Release hardening and operations.** Complete cross-platform tests, quotas/retention/backup, clean shutdown, documentation, vulnerability review, CI and reproducible builds. Acceptance: clean checkout passes `make check`; all default tests are offline; CGO-free target builds succeed; README lets a new operator run an allowed test repo safely and understand costs/local isolation limits.
- [ ] **Later — Webhooks, durable queue, worker pool, container runner.** Add verified webhook signatures and delivery dedupe, durable admission/lease/fencing, per-repo concurrency policy, resource/network-isolated execution and credential separation. Acceptance before hostile-repo support: malicious fixture code cannot read host data, contact disallowed networks, retain child processes, or reach publishing credentials. Do not enable broad public intake merely because webhook handling exists.

## Top 10 risks and how to avoid them

1. **Calling the local workspace a sandbox.** Tests execute arbitrary code with host-user privileges. Restrict Step 1 to trusted repositories, isolate the OS account, disclose the limit, and require a real isolated runner before public intake.
2. **Losing provider-specific continuation data.** A text-only common interface breaks signed Gemini parts and other structured replies. Preserve ordered typed parts and private opaque metadata; verify full multi-turn wire fixtures and fail explicitly on unsupported blocks.
3. **Path checks that pass tests but race in production.** String-prefix/EvalSymlinks checks allow traversal races and protected-file aliases. Use patched Root-relative operations, enforce policy through aliases, bound all file tools, and test component swaps on both target platforms.
4. **An allowlist that effectively permits a shell.** Broad executable prefixes, test scripts, Git helpers, and argument injection defeat command restrictions. Use operator-owned exact argv command IDs, constrained env/cwd, deterministic Git plumbing, and acknowledge that approved tests still need OS isolation.
5. **Runaway costs despite a nominal cap.** Post-response accounting, retries, unknown rates/usage, and reasoning tokens overspend. Reserve conservatively before every attempt, require prices, retain uncertain reservations, and stop when usage cannot be bounded.
6. **False confidence in “tested PRs.”** Agent claims, changed tests, baseline failures, and post-verification edits can mislead. Run authoritative tests outside the loop, bind results to the final tree, protect verification configuration, and state exact failure/baseline evidence in drafts. A green configured test command is evidence, not proof of correctness.
7. **Duplicate or unsafe publication after partial failure.** A timeout may follow successful push/PR/comment creation. Persist intents and stable markers, reconcile remote state, never force-push, and stop on unknown branch ownership or changed default-branch assumptions.
8. **Secrets leaking through secondary paths.** Child env, URLs, split chunks, DB events, fixtures, and PR bodies all bypass naive log masking. Minimize credentials, centralize redaction before every observable sink, test split secrets, prohibit raw payload recording, and inspect final outbound artifacts.
9. **Two processes acting on one issue or stale work after restart.** UI-only checks and expiring heartbeats are insufficient. Use transactional uniqueness plus per-run OS locks, terminal lock release, explicit interruption/reconciliation, and document that separate stores cannot coordinate.
10. **Polished UI hiding unreliable operational behavior.** Lost SSE events, unbounded logs, CDN failure, dependency drift, and host-specific subprocess behavior undermine trust. Persist replayable bounded events, keep execution independent of viewers, pin dependencies, test macOS/Linux, and ship recovery/retention instructions before expanding scope.
