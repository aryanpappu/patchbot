# patchbot: implementation plan (Step 1)

Status: proposal, 2026-11-10, revised the same day for owner decisions 6 to 8 in the brief (no Managed
Agents in the core, required refine phase with spec approval, full control from the web UI).
Source: `docs/planning/BRIEF.md`. No code exists yet.

Assumptions made so the plan is executable without questions (all listed again in section 12):

- Module path is `github.com/aryanpappu/patchbot`. Change it once in `go.mod` if the GitHub owner differs.
- GitHub auth in Step 1 is a fine-grained personal access token in `GITHUB_TOKEN`. A GitHub App comes in Step 5.
- Step 1 runs commands on the host with no sandbox, so it is only for repositories and issues the owner trusts.
- Provider API details below come from the public API docs as I know them. Each adapter milestone starts by
  recording real responses, and the recorded fixtures are the source of truth if they differ from this plan.

---

## 1. Goals, non-goals, roadmap

### 1.1 Step 1 goals

1. `patchbot run --repo owner/name --issue 12 [--provider P --model M] [--dry-run]` runs the flow in the
   foreground in two stages with a human gate between them. **Refine stage:** fetch issue, clone, read-only
   exploration, write a spec, post it as an issue comment, stop at `awaiting_approval` (or post questions
   and stop at `needs_info`). **Code stage**, only after approval: branch `patchbot/issue-12`, agent loop,
   tests, push, PR, issue comment. Section 4.7 has the gate in full.
2. Three modes, picked by `--mode` or from the issue label: `refine` (`ai-refine`: spec or questions only),
   `fix` (`ai-fix`: refine, approval, code, test, PR), `investigate` (`ai-investigate`: one analysis comment,
   no code, no spec).
   - Approval works through three channels that all end in the same `Runner.Approve` call: an `/approve`
     comment from an allowed GitHub user, `patchbot approve <run-id>`, and the Approve button in the web UI.
     `/revise <notes>` and UI edits create a new spec version, which needs a new approval.
   - Everything the CLI or a GitHub comment can do, the web UI can do: start (with budget), view and edit
     the spec, approve, revise, reject, answer questions, cancel, retry with another provider or model, and
     view the log, diff, test output, tokens and cost. Every action is written to the run history.
3. Three provider adapters behind one `Provider` interface: Anthropic, Gemini, OpenAI-compatible. Several
   named OpenAI-compatible instances can exist at once (OpenAI, OpenRouter, Ollama, LM Studio, Groq).
4. Provider and model are chosen per run by flag, config default or web UI. Models come from the provider's
   list API, with the config list as fallback.
5. `patchbot serve` gives one server-rendered page with Vue 3 from a CDN: provider/model picker, start-run
   form, runs list, live run log over SSE.
6. SQLite (modernc, no CGO) stores runs, steps, tokens and cost.
7. Safety controls from the brief are all enforced in code and each has a test: path jail, command allowlist,
   timeouts, output caps, token and dollar budgets, never push to the default branch, env-only secrets with
   redaction, one active run per issue.
8. Professional packaging: Makefile, golangci-lint, CI, README, MIT license, single static binary with
   templates, static files and migrations embedded.

### 1.2 Step 1 non-goals

- No webhooks, no label polling, no durable queue, no Docker. The design leaves seams for them (section
  1.3). The one exception is a narrow comment poller under `serve` that watches only the issues of runs
  waiting at `awaiting_approval` or `needs_info`, because `/approve` comments are a requirement.
- No Managed Agents and no other vendor-hosted agent runtime in the core. patchbot runs its own
  provider-neutral loop (section 4.3) on all three providers. The Runner reaches the loop through a
  one-method `agent.Engine` interface, so an optional Claude-only engine can be added later without
  touching the Runner.
- No token-level streaming from providers. The UI streams per-step events, which is enough for a live log.
- No login. The server binds to loopback and refuses a non-loopback address unless a UI token is set.
- No multi-repo monorepo awareness, no PR review-comment follow-up, no resuming a crashed run.
- No GitLab or Bitbucket.
- No provider switch in the middle of a run.

### 1.3 Roadmap for Steps 2 to 5

| Step | Adds | Seam that Step 1 already provides |
|---|---|---|
| 2. Webhooks + poll | `POST /webhooks/github` with `X-Hub-Signature-256` HMAC check and delivery-ID dedupe table. `issues.labeled` events call `Runner.Start`, and `issue_comment` events call `Runner.HandleComment`, which replaces the Step 1 comment poller. A poller that lists issues by label with ETags as fallback. | `Runner.Start` and `Runner.HandleComment` are the only entry points. `runs.trigger` column already has `webhook` and `poll` values. |
| 3. Queue + workers | Durable queue on the `runs` table: workers claim `queued` rows with `UPDATE ... RETURNING`, hold a lease, heartbeat, retry on lease expiry. Configurable worker count. | `Dispatcher` interface (Step 1 implementation is a goroutine plus semaphore). `runs.heartbeat_at` and `queued` status exist from day one. |
| 4. Docker sandbox | `DockerExecutor`: workspace bind-mounted, `--network none`, read-only rootfs, non-root user, CPU, memory and PID limits, per-language images, a dependency-install phase with network and a test phase without. | `Executor` interface is separate from `Workspace`. The agent and tools never call `os/exec` directly. |
| 5. Deploy | Distroless Dockerfile, compose file, Caddy for TLS, GitHub App auth in place of the PAT, UI login, SQLite backup (Litestream), `/metrics`. | `github.Client` takes a `TokenSource`. HTTP middleware chain has an auth slot. |
| Later, optional | A Claude-only engine built on Managed Agents, selectable per run. Not part of the core. | `agent.Engine` interface. |

---

## 2. Repository tree

```
patchbot/
├── cmd/patchbot/main.go            # only calls cli.Main(os.Args, os.Stdout, os.Stderr) and exits
├── internal/
│   ├── cli/
│   │   ├── cli.go                  # subcommand dispatch on stdlib flag: run, serve, models, config, version
│   │   ├── run.go                  # `patchbot run`: flags, builds RunRequest, runs in foreground, prints log
│   │   ├── serve.go                # `patchbot serve`: wires store, runner, bus, HTTP server, graceful shutdown
│   │   ├── actions.go              # approve, revise, reject, answer, cancel, retry, spec, sync, runs (section 4.8)
│   │   ├── models.go               # `patchbot models [--provider P]`: prints the model list and its source
│   │   ├── config.go               # `patchbot config check|show`: validates and prints effective config, secrets masked
│   │   └── cli_test.go             # flag parsing and exit-code tests
│   ├── config/
│   │   ├── config.go               # Config structs and Default()
│   │   ├── load.go                 # file -> env -> flags layering, unknown-key rejection, validation
│   │   ├── defaults.toml           # embedded defaults: budgets, allowlist, Anthropic model IDs
│   │   └── load_test.go            # precedence table tests
│   ├── secret/
│   │   ├── secret.go               # type Secret string with redacting String, LogValue, MarshalJSON
│   │   ├── redactor.go             # Redactor: known values plus token-shaped regexes
│   │   └── redactor_test.go
│   ├── logging/
│   │   ├── logging.go              # slog setup (text or JSON), redacting handler wrapper, run_id attr helper
│   │   └── logging_test.go
│   ├── llm/
│   │   ├── types.go                # Message, Part, ToolCall, ToolResult, ToolSpec, Usage, StopReason
│   │   ├── provider.go             # Provider interface, ChatRequest, ChatResponse, ModelInfo
│   │   ├── errors.go               # *Error with Kind (auth, rate_limit, overloaded, context_too_long, ...)
│   │   ├── registry.go             # builds providers from config, lookup by instance name
│   │   ├── models.go               # ModelLister: live list with TTL cache, config fallback, merge
│   │   ├── pricing.go              # Price table, Cost(usage) in micro-USD
│   │   ├── retry.go                # retry wrapper: backoff with jitter, honours RetryAfter
│   │   ├── schema.go               # the JSON-Schema subset tools may use, and per-provider schema cleaning
│   │   ├── *_test.go
│   │   ├── anthropic/
│   │   │   ├── anthropic.go        # adapter on the official SDK
│   │   │   ├── convert.go          # normalized <-> Anthropic params and response
│   │   │   ├── anthropic_test.go   # golden tests against recorded JSON
│   │   │   └── testdata/           # *.req.golden.json, *.resp.json
│   │   ├── gemini/
│   │   │   ├── gemini.go           # raw net/http client for generateContent and models.list
│   │   │   ├── wire.go             # request and response structs
│   │   │   ├── convert.go
│   │   │   ├── gemini_test.go
│   │   │   └── testdata/
│   │   ├── openai/
│   │   │   ├── openai.go           # raw net/http client for /chat/completions and /models
│   │   │   ├── wire.go
│   │   │   ├── convert.go          # includes the compat quirks table (section 3.4)
│   │   │   ├── openai_test.go
│   │   │   └── testdata/           # fixtures from OpenAI, OpenRouter, Ollama, Groq
│   │   └── fake/
│   │       ├── fake.go             # scripted Provider for deterministic tests and demos
│   │       └── fake_test.go
│   ├── tools/
│   │   ├── tool.go                 # Tool interface, Env, Result
│   │   ├── registry.go             # Registry: Specs(), Dispatch(), argument validation
│   │   ├── read_file.go            # read_file with offset and limit, line numbers
│   │   ├── write_file.go           # write_file: create or overwrite, atomic
│   │   ├── edit_file.go            # edit_file: exact string replacement
│   │   ├── list_dir.go             # list_dir: one level or bounded recursive
│   │   ├── search.go               # search: RE2 over tracked and untracked-not-ignored files
│   │   ├── run_cmd.go              # run_cmd: argv through the allowlist to the Executor
│   │   ├── submit.go               # the terminal tools: submit, submit_spec, ask_questions
│   │   └── *_test.go
│   ├── workspace/
│   │   ├── workspace.go            # Workspace interface, Manager (create, cleanup)
│   │   ├── local.go                # local implementation on os.Root
│   │   ├── jail.go                 # path validation, protected paths, size limits
│   │   ├── git.go                  # clone, branch, status, diff, commit, push through the git CLI
│   │   ├── gitguard.go             # .git/config integrity hash, hook check, branch-name guard
│   │   ├── detect.go               # test-command detection (go.mod, package.json, composer.json, ...)
│   │   ├── jail_test.go            # escape attempts incl. symlinks
│   │   └── git_integration_test.go # against local bare repositories
│   ├── execx/
│   │   ├── executor.go             # Executor interface, Spec, Result
│   │   ├── local.go                # LocalExecutor: process group, timeout, capped output, scrubbed env
│   │   ├── allowlist.go            # Policy: binary and subcommand rules, argument denylist
│   │   ├── capwriter.go            # head+tail bounded buffer
│   │   └── *_test.go
│   ├── github/
│   │   ├── client.go               # Client interface and raw net/http implementation
│   │   ├── types.go                # Repo, Issue, Comment, PullRequest
│   │   ├── errors.go               # status mapping, rate-limit handling
│   │   └── client_test.go          # httptest server tests
│   ├── agent/
│   │   ├── loop.go                 # the tool-calling loop
│   │   ├── limits.go               # Limits and Budget (turns, tokens, micro-USD, wall clock)
│   │   ├── prompt.go               # system prompts and issue framing (written from scratch)
│   │   ├── compact.go              # history trimming when near the context window
│   │   ├── stuck.go                # repeated-call detector
│   │   └── loop_test.go            # table tests on the fake provider
│   ├── runner/
│   │   ├── runner.go               # Runner: Start, Execute, Cancel
│   │   ├── phases.go               # preflight, clone, baseline, agent, verify, publish
│   │   ├── refine.go               # refine stage: explore, spec or questions, post, park the run
│   │   ├── spec.go                 # Spec type, markdown rendering, canonical hash, comment marker
│   │   ├── actions.go              # Approve, Revise, Reject, Answer, EditSpec, Retry and the approval gate
│   │   ├── comments.go             # slash-command parsing, HandleComment, the waiting-run comment poller
│   │   ├── publish.go              # PR and comment bodies, criteria checklist, mention neutralising
│   │   ├── dispatcher.go           # Dispatcher interface and the in-process implementation
│   │   ├── recover.go              # marks stale running runs as failed on startup
│   │   └── runner_test.go          # full flow with fakes
│   ├── events/
│   │   ├── event.go                # Event type and the type constants
│   │   ├── bus.go                  # in-process pub/sub keyed by run ID
│   │   ├── emitter.go              # redact -> persist -> publish
│   │   └── bus_test.go
│   ├── store/
│   │   ├── store.go                # Store interface and row types
│   │   ├── sqlite.go               # modernc implementation
│   │   ├── migrate.go              # embedded forward-only migrator
│   │   ├── migrations/
│   │   │   └── 0001_init.sql
│   │   └── sqlite_test.go
│   ├── server/
│   │   ├── server.go               # http.Server, mux, middleware chain
│   │   ├── middleware.go           # request log, recover, host check, cross-origin protection, security headers
│   │   ├── pages.go                # GET / and GET /runs/{id}
│   │   ├── api.go                  # JSON API handlers
│   │   ├── sse.go                  # GET /api/runs/{id}/events
│   │   └── server_test.go
│   └── version/version.go          # version, commit, date set by -ldflags
├── web/
│   ├── embed.go                    # //go:embed templates static
│   ├── templates/
│   │   ├── layout.html             # <html>, head, CDN script tag with SRI, boot JSON, footer
│   │   ├── index.html              # the page body, which is also the Vue in-DOM template
│   │   └── partials/
│   │       ├── new_run.html        # provider/model picker and start-run form
│   │       ├── runs_list.html      # runs table
│   │       ├── run_detail.html     # header, counters, action bar and the tab strip
│   │       ├── spec_panel.html     # spec view, edit form, approve / revise / reject / answer controls
│   │       └── run_tabs.html       # log, diff, tests and history tabs
│   └── static/
│       ├── app.js                  # the Vue app, plain ES2020, no build
│       ├── app.css                 # hand-written CSS, light and dark
│       └── favicon.svg
├── testdata/
│   └── repos/                      # tiny fixture repositories used by integration tests
├── docs/
│   ├── planning/                   # BRIEF.md and this plan
│   ├── ARCHITECTURE.md             # short package map and the run flow diagram
│   └── SECURITY.md                 # the threat model from section 8, user-facing
├── .github/workflows/ci.yml
├── .golangci.yml
├── .gitignore
├── patchbot.example.toml
├── Makefile
├── LICENSE                         # MIT
├── README.md
├── go.mod
└── go.sum
```

Dependency direction, enforced by the `depguard` linter: `cli -> server, runner`; `runner -> agent, workspace,
execx, github, store, events, llm`; `agent -> llm, tools, events`; `tools -> workspace, execx, llm (types only)`.
`llm`, `store`, `events`, `secret` import nothing from the other internal packages except `secret`.

---

## 3. Key interfaces and types

Go note for the owner: interfaces are declared where they are consumed and kept small. The `llm` package is
the exception because three packages implement `Provider`.

### 3.1 Normalized LLM types (`internal/llm/types.go`)

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool" // carries only tool_result parts
)

type PartType string

const (
    PartText       PartType = "text"
    PartToolCall   PartType = "tool_call"
    PartToolResult PartType = "tool_result"
    PartOpaque     PartType = "opaque" // provider block that must be echoed back unchanged
)

type Part struct {
    Type       PartType
    Text       string
    ToolCall   *ToolCall
    ToolResult *ToolResult
    // ProviderMeta is written by an adapter and read only by the same adapter.
    // Anthropic: the raw thinking or redacted_thinking block. Gemini: the thoughtSignature of a part.
    ProviderMeta json.RawMessage
}

type Message struct {
    Role   Role
    Parts  []Part
    Origin string // provider instance that produced an assistant message; adapters drop foreign opaque parts
}

type ToolCall struct {
    ID   string          // provider ID, or synthesized "call_<turn>_<index>" when the provider gives none
    Name string
    Args json.RawMessage // always a JSON object; "{}" when the provider sent nothing
}

type ToolResult struct {
    CallID  string
    Name    string // needed by Gemini, which matches results by name
    Content string // text only in Step 1
    IsError bool
}

type ToolSpec struct {
    Name        string
    Description string
    Schema      json.RawMessage // JSON Schema subset, see schema.go
}

type StopReason string

const (
    StopEndTurn   StopReason = "end_turn"
    StopToolUse   StopReason = "tool_use"
    StopMaxTokens StopReason = "max_tokens"
    StopRefusal   StopReason = "refusal" // safety block, content filter, refusal
    StopOther     StopReason = "other"
)

type Usage struct {
    InputTokens      int64 // uncached input only
    OutputTokens     int64 // includes reasoning tokens
    CacheReadTokens  int64
    CacheWriteTokens int64
    ReasoningTokens  int64 // informational subset of OutputTokens
}

func (u Usage) Add(o Usage) Usage
func (u Usage) Total() int64
```

There is no system role. The system prompt is a field of `ChatRequest`, because two of the three APIs take it
outside the message list.

Invariants that `llm.Validate(messages)` checks before every call, so adapters can rely on them:

1. Messages alternate user-side (`user` or `tool`) and `assistant`, starting user-side.
2. Every `tool_call` in an assistant message has exactly one `tool_result` in the next `tool` message, in the
   same order.
3. `Args` is a valid JSON object.

### 3.2 Provider interface (`internal/llm/provider.go`)

```go
type Provider interface {
    Name() string // instance name from config, e.g. "anthropic", "openrouter", "ollama"
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    ListModels(ctx context.Context) ([]ModelInfo, error)
}

type ChatRequest struct {
    Model       string
    System      string
    Messages    []Message
    Tools       []ToolSpec
    MaxTokens   int      // output cap for this call
    Temperature *float64 // nil means do not send the field
}

type ChatResponse struct {
    Message    Message // role assistant
    StopReason StopReason
    RawStop    string // provider's own value, kept for logs
    Usage      Usage
    Model      string // model the provider says it used
    RequestID  string
}

type ModelInfo struct {
    ID            string
    DisplayName   string
    ContextWindow int    // 0 when unknown
    Source        string // "api" or "config"
}

type ErrorKind string

const (
    ErrAuth           ErrorKind = "auth"             // 401, 403: never retried
    ErrRateLimit      ErrorKind = "rate_limit"       // 429
    ErrOverloaded     ErrorKind = "overloaded"       // 5xx, 529
    ErrContextTooLong ErrorKind = "context_too_long" // prompt exceeds the window
    ErrBadRequest     ErrorKind = "bad_request"      // other 4xx: never retried
    ErrTransport      ErrorKind = "transport"        // network, timeout, EOF
    ErrMalformed      ErrorKind = "malformed"        // response we cannot parse
)

type Error struct {
    Kind       ErrorKind
    Provider   string
    Status     int
    RetryAfter time.Duration
    Message    string // redacted
    Err        error
}

func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Retryable() bool // rate_limit, overloaded, transport
```

`llm.WithRetry(p Provider, policy RetryPolicy) Provider` wraps any provider: at most 4 retries, base 1s,
factor 2, full jitter, cap 30s, `RetryAfter` wins when present, and it stops when the context is done. The
Anthropic SDK's own retries are turned off (`option.WithMaxRetries(0)`) so there is one retry policy.

`ModelLister.List(ctx, providerName)` returns the live list cached for 10 minutes, merged with the config
list. Config entries win on `DisplayName` and `ContextWindow`. When the live call fails it returns the config
list alone and a non-fatal warning that the UI shows next to the picker.

### 3.3 Anthropic mapping

Library: `github.com/anthropics/anthropic-sdk-go`. Endpoint `POST /v1/messages`, models `GET /v1/models`.

| Normalized | Anthropic |
|---|---|
| `ChatRequest.System` | top-level `system`, sent as one text block with `cache_control: ephemeral` |
| `RoleUser` text | `user` message, `text` block |
| `RoleAssistant` text / tool_call | `assistant` message, `text` / `tool_use {id, name, input}` blocks |
| `PartOpaque` with `Origin` = this provider | the raw `thinking` or `redacted_thinking` block, byte for byte |
| `RoleTool` message | `user` message whose blocks are `tool_result {tool_use_id, content, is_error}` |
| `ToolSpec` | `tools[] {name, description, input_schema}`, `cache_control` on the last tool |
| `MaxTokens` | `max_tokens` (required by the API; default 8192, see note below) |
| Usage | `input_tokens` -> Input, `output_tokens` -> Output, `cache_read_input_tokens` -> CacheRead, `cache_creation_input_tokens` -> CacheWrite |

Stop reasons: `end_turn`, `stop_sequence` -> `StopEndTurn`. `tool_use` -> `StopToolUse`. `max_tokens`,
`model_context_window_exceeded` -> `StopMaxTokens`. `refusal` -> `StopRefusal`. `pause_turn` and anything
unknown -> `StopOther`.

Details that matter:

- A `tool` message followed by a `user` message (the test-failure feedback) is merged into one Anthropic
  `user` message with the `tool_result` blocks first.
- A third cache breakpoint goes on the last block of the last message, so each turn reads the previous
  turn's prefix from cache. This is the main cost lever for a long loop.
- No sampling parameters are sent unless the config sets them. Newer Claude models reject some of them.
- Thinking is off unless `thinking = "adaptive"` is set for the model in config. When on, thinking blocks are
  stored as `PartOpaque` and echoed, because the API requires them back unchanged during tool use.
- Requests are non-streaming. Keep `max_tokens` at 16384 or below, because the SDK refuses non-streaming
  requests that could run past its long-request limit.
- Error mapping: 401, 403 -> auth. 429 -> rate_limit with `retry-after`. 500, 529 -> overloaded. 400 whose
  message mentions the prompt being too long -> context_too_long. Other 400 -> bad_request.

### 3.4 OpenAI-compatible mapping

Library: none, raw `net/http`. Endpoints `POST {base_url}/chat/completions` and `GET {base_url}/models`.
The Chat Completions shape is chosen, not the Responses API, because it is the one that OpenRouter, Ollama,
LM Studio and Groq all implement.

| Normalized | Chat Completions |
|---|---|
| `ChatRequest.System` | first message `{role: "system"}` (`system_role = "developer"` is a per-provider config option) |
| `RoleUser` text | `{role: "user", content: "..."}` |
| `RoleAssistant` | `{role: "assistant", content: text or null, tool_calls: [{id, type: "function", function: {name, arguments}}]}` where `arguments` is a JSON string |
| `RoleTool` message with N results | N messages `{role: "tool", tool_call_id, content}`; `IsError` becomes a `ERROR: ` prefix because the format has no error flag |
| `PartOpaque` | dropped |
| `ToolSpec` | `tools[] {type: "function", function: {name, description, parameters}}` |
| `MaxTokens` | `max_tokens`, or `max_completion_tokens` when `max_tokens_field = "max_completion_tokens"` in config |
| Usage | Input = `prompt_tokens - prompt_tokens_details.cached_tokens`, CacheRead = `cached_tokens`, Output = `completion_tokens`, Reasoning = `completion_tokens_details.reasoning_tokens` |

Stop reasons: `stop` -> `StopEndTurn`. `tool_calls`, `function_call` -> `StopToolUse`. `length` ->
`StopMaxTokens`. `content_filter` -> `StopRefusal`. Others -> `StopOther`. One override: if the message has
`tool_calls`, the result is `StopToolUse` whatever `finish_reason` says.

Compat quirks, each covered by a fixture:

| Quirk | Handling |
|---|---|
| `arguments` is an object, not a string (some Ollama builds) | accept both with a custom `UnmarshalJSON` |
| `arguments` is `""` or invalid JSON | empty -> `{}`. Invalid -> keep the call, set `Args` to `{}`, and the registry returns an `IsError` result quoting the parse error so the model can retry |
| missing or duplicate tool-call `id` | synthesize `call_<turn>_<index>` |
| no `usage` object | zero usage, `usage_missing` flag on the step, cost shown as unknown |
| `/models` missing or 404 | config fallback |
| no API key (Ollama, LM Studio) | no `Authorization` header when `api_key_env` is unset |
| model without tool support | provider returns 400, mapped to bad_request with a hint naming the model |
| extra headers (OpenRouter `HTTP-Referer`, `X-Title`) | `headers` table in provider config |

Context-overflow detection: 400 with `code == "context_length_exceeded"`, or a message matching
`(?i)context (length|window)|maximum context|too many tokens`.

### 3.5 Gemini mapping

Library: none, raw `net/http` (reason in section 10). Endpoints
`POST {base}/v1beta/models/{model}:generateContent` and `GET {base}/v1beta/models`, key in the
`x-goog-api-key` header, never in the URL.

| Normalized | Gemini |
|---|---|
| `ChatRequest.System` | `systemInstruction {parts: [{text}]}` |
| `RoleUser` text | `contents[] {role: "user", parts: [{text}]}` |
| `RoleAssistant` | `{role: "model", parts: [{text} or {functionCall: {name, args}}]}`; `ProviderMeta` restores `thoughtSignature` on the same part |
| `RoleTool` message | `{role: "user", parts: [{functionResponse: {name, response: {"output": "..."}}}]}`, or `{"error": "..."}` when `IsError`, in call order |
| `ToolSpec` | `tools: [{functionDeclarations: [{name, description, parameters}]}]` with the schema cleaned (below) |
| `MaxTokens` | `generationConfig.maxOutputTokens` |
| Usage | Input = `promptTokenCount - cachedContentTokenCount`, CacheRead = `cachedContentTokenCount`, Output = `candidatesTokenCount + thoughtsTokenCount`, Reasoning = `thoughtsTokenCount` |

Stop reasons: any `functionCall` part -> `StopToolUse`, because Gemini reports `STOP` for tool calls too.
Otherwise `STOP` -> `StopEndTurn`. `MAX_TOKENS` -> `StopMaxTokens`. `SAFETY`, `RECITATION`, `BLOCKLIST`,
`PROHIBITED_CONTENT`, `SPII`, or a `promptFeedback.blockReason` with no candidates -> `StopRefusal`.
`MALFORMED_FUNCTION_CALL`, `UNEXPECTED_TOOL_CALL` and others -> `StopOther`, and the loop treats those two
as a recoverable turn (section 4.4).

Details that matter:

- Function calls often have no ID. The adapter synthesizes `call_<turn>_<index>` and, on the way back,
  matches results to calls by position and name. If the response does carry `id`, it is used and echoed.
- `thoughtSignature` on a part is stored in `ProviderMeta` and sent back unchanged. Dropping it breaks
  multi-turn tool use on thinking models.
- Schema cleaning in `llm/schema.go`: Gemini accepts an OpenAPI subset, so `additionalProperties`, `$schema`
  and `default` are stripped for this provider. Tool schemas are written in the common subset anyway:
  `type: object`, `properties`, `required`, `description`, `enum`, and scalar types `string`, `integer`,
  `boolean`, plus `array` of `string`.
- Model IDs from the list API look like `models/<id>`. The prefix is stripped, and only models whose
  `supportedGenerationMethods` contains `generateContent` are kept. `inputTokenLimit` fills `ContextWindow`.
- Errors use the `{error: {code, status, message}}` envelope: `UNAUTHENTICATED`, `PERMISSION_DENIED` ->
  auth. `RESOURCE_EXHAUSTED` -> rate_limit. `UNAVAILABLE`, `INTERNAL`, `DEADLINE_EXCEEDED` -> overloaded.
  `INVALID_ARGUMENT` with a token-limit message -> context_too_long.

### 3.6 Tools (`internal/tools`)

```go
type Tool interface {
    Spec() llm.ToolSpec
    // Execute returns a Result for anything the model can react to, including its own mistakes.
    // A non-nil error means the harness itself is broken and the run must stop.
    Execute(ctx context.Context, env *Env, args json.RawMessage) (Result, error)
}

type Result struct {
    Content string
    IsError bool
    Submit  *Submission // set only by the submit tool
}

type Submission struct {
    Summary   string
    PRTitle   string            // code stage
    PRBody    string            // code stage
    Criteria  []CriterionResult // code stage: one entry per acceptance criterion of the approved spec
    Report    string            // investigate mode
    Spec      *SpecDraft        // refine stage, from submit_spec
    Questions []string          // refine stage, from ask_questions
}

type SpecDraft struct {
    Summary            string
    AcceptanceCriteria []string     // rendered as AC1, AC2, ...
    Files              []FileChange // Path, Change (one line on what changes and why)
    Risks              []string
    TestPlan           string
}

type CriterionResult struct {
    ID       string // "AC1"
    Met      bool
    Evidence string // file, test name or command that shows it
}

type Env struct {
    WS     workspace.Workspace
    Exec   execx.Executor
    Policy execx.Policy
    Limits Limits // read cap, write cap, command timeout, output cap
    Log    *slog.Logger
}

type Registry struct{ /* ordered map name -> Tool */ }

func NewRegistry(tools ...Tool) *Registry
func RefineTools() *Registry      // read_file, list_dir, search, submit_spec, ask_questions (read-only)
func FixTools() *Registry         // read_file, write_file, edit_file, list_dir, search, run_cmd, submit
func InvestigateTools() *Registry // read_file, list_dir, search, submit
func (r *Registry) Specs() []llm.ToolSpec
func (r *Registry) Dispatch(ctx context.Context, env *Env, call llm.ToolCall) (llm.ToolResult, *Submission, error)
```

`Dispatch` handles unknown tool names and argument decoding errors itself and returns them as `IsError`
results. Arguments are decoded with `json.Decoder.DisallowUnknownFields` into a struct per tool.

| Tool | Arguments | Behaviour and limits |
|---|---|---|
| `read_file` | `path`, `offset?`, `limit?` | Line-numbered text. Default 400 lines, max 2000 lines or 64 KB per call. Binary files (NUL in first 8 KB) are refused. Says how many lines remain. |
| `write_file` | `path`, `content` | Creates parents. Atomic temp file plus rename inside the root. Max 1 MB. Keeps the mode of an existing file. |
| `edit_file` | `path`, `old_string`, `new_string`, `replace_all?` | `old_string` must occur exactly once unless `replace_all`. The error states the count found. |
| `list_dir` | `path?`, `depth?` | Depth 1 by default, max 3, max 500 entries, skips `.git`. |
| `search` | `pattern`, `glob?`, `max_results?` | Go RE2 regex. File set comes from `git ls-files -co --exclude-standard`, so `.gitignore` is respected for free. Skips binary files and files over 1 MB. Default 50 matches, max 200, as `path:line: text`. |
| `run_cmd` | `argv` (array of strings), `timeout_seconds?` | No shell. Passes through `Policy.Check`, then the `Executor`. Returns exit code and capped output. |
| `submit` | code stage: `summary`, `pr_title`, `pr_body`, `criteria` (array of `{id, met, evidence}`). investigate: `report` | Ends the agent phase. It does not publish anything. The Runner does. In the code stage it is rejected with an `IsError` result unless `criteria` has exactly one entry per criterion ID of the approved spec. |
| `submit_spec` | `summary`, `acceptance_criteria` (array of strings, 1 to 12), `files` (array of `{path, change}`), `risks` (array of strings), `test_plan` | Refine stage only. Ends the stage with a spec. Every `path` must pass the lexical jail check. |
| `ask_questions` | `questions` (array of strings, 1 to 5) | Refine stage only. Ends the stage with questions for the issue author. |

The refine registry has no write tool and no `run_cmd`, so the refine stage cannot change the checkout or
run code. That is enforced by which tools exist, not by the prompt. These terminal tools use arrays of flat
objects, so the schema subset in 3.5 allows `array` of `string` and `array` of flat `object`.

### 3.7 Workspace and Executor

```go
// internal/workspace
type Workspace interface {
    Dir() string // absolute host path of the checkout
    ReadFile(path string) ([]byte, error)
    WriteFile(path string, data []byte) error
    Stat(path string) (fs.FileInfo, error)
    ReadDir(path string) ([]fs.DirEntry, error)
    ListFiles(ctx context.Context) ([]string, error) // tracked plus untracked-not-ignored

    DefaultBranch() string
    Branch() string
    HasChanges(ctx context.Context) (bool, error)
    Diff(ctx context.Context) (string, error) // against the base commit, capped
    DiffStat(ctx context.Context) (DiffStat, error)
    Commit(ctx context.Context, msg string) (sha string, err error)
    Push(ctx context.Context, token secret.Secret, force bool) error

    Close(keep bool) error
}

type Manager interface {
    Create(ctx context.Context, spec Spec) (Workspace, error)
    Sweep(ctx context.Context, olderThan time.Duration) error
}

type Spec struct {
    RunID    int64
    Repo     RepoRef // Owner, Name
    CloneURL string  // https URL without credentials
    Token    secret.Secret
    Base     string // default branch
    Branch   string // patchbot/issue-<n>
}

var (
    ErrPathEscape    = errors.New("path escapes workspace")
    ErrPathProtected = errors.New("path is protected")
    ErrTooLarge      = errors.New("file too large")
)
```

```go
// internal/execx
type Executor interface {
    Run(ctx context.Context, spec Spec) (Result, error)
}

type Spec struct {
    Argv      []string
    Dir       string // absolute, inside the workspace
    Env       []string
    Timeout   time.Duration
    MaxOutput int // bytes kept, head plus tail
}

type Result struct {
    ExitCode  int
    Output    string // stdout and stderr interleaved
    Truncated bool
    TimedOut  bool
    Duration  time.Duration
}

type Policy struct{ /* rules loaded from config */ }

func (p Policy) Check(argv []string) error // nil when allowed
```

`Executor.Run` returns an error only when the process could not be started. A non-zero exit is a `Result`.

### 3.8 Store (`internal/store`)

```go
type Store interface {
    CreateRun(ctx context.Context, r NewRun) (Run, error) // ErrActiveRunExists on the unique index
    GetRun(ctx context.Context, id int64) (Run, error)
    ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
    SetRunStatus(ctx context.Context, id int64, from []Status, to Status, patch RunPatch) error
    Heartbeat(ctx context.Context, id int64) error
    AddUsage(ctx context.Context, id int64, u llm.Usage, costMicros *int64) error
    FailStaleRuns(ctx context.Context, staleAfter time.Duration) (int, error)

    AddStep(ctx context.Context, s NewStep) (int64, error)
    FinishStep(ctx context.Context, id int64, patch StepPatch) error
    ListSteps(ctx context.Context, runID int64) ([]Step, error)

    AppendEvent(ctx context.Context, e NewEvent) (seq int64, err error) // NewEvent: RunID, Type, Data, Actor, Channel
    EventsSince(ctx context.Context, runID, afterSeq int64, limit int) ([]events.Event, error)
    History(ctx context.Context, runID int64) ([]events.Event, error) // only rows with type "action.*"

    CreateSpec(ctx context.Context, s NewSpec) (Spec, error) // assigns the next version, supersedes the previous one
    GetSpec(ctx context.Context, id int64) (Spec, error)
    LatestSpec(ctx context.Context, runID int64) (Spec, error)
    ListSpecs(ctx context.Context, runID int64) ([]Spec, error)
    SetSpecPosted(ctx context.Context, id int64, commentID int64, commentURL string) error
    LatestApprovedSpecForIssue(ctx context.Context, repo string, issue int) (*Spec, error)
    AddApproval(ctx context.Context, a NewApproval) (Approval, error) // also sets the spec status, one transaction
    ApprovalFor(ctx context.Context, specID int64) (*Approval, error)
    AddQuestions(ctx context.Context, q NewQuestions) (Questions, error)
    AnswerQuestions(ctx context.Context, id int64, answer string, by, channel string) error
    ListWaitingRuns(ctx context.Context) ([]Run, error) // awaiting_approval and needs_info

    SaveArtifact(ctx context.Context, a NewArtifact) error
    GetArtifact(ctx context.Context, runID int64, kind string, attempt int) (Artifact, error)

    Close() error
}

var (
    ErrNotFound        = errors.New("not found")
    ErrActiveRunExists = errors.New("an active run already exists for this issue")
    ErrStaleStatus     = errors.New("run is not in the expected status")
)
```

`SetRunStatus` is a compare-and-set (`WHERE id = ? AND status IN (from...)`). This is what makes cancel and
finish race-free, and it is the hook the Step 3 queue uses to claim work.

### 3.9 GitHub client (`internal/github`)

```go
type Client interface {
    GetRepo(ctx context.Context, repo RepoRef) (Repo, error) // default branch, clone URL, permissions
    GetIssue(ctx context.Context, repo RepoRef, number int) (Issue, error)
    ListIssueComments(ctx context.Context, repo RepoRef, number int, max int) ([]Comment, error)
    FindOpenPR(ctx context.Context, repo RepoRef, headBranch string) (*PullRequest, error)
    BranchExists(ctx context.Context, repo RepoRef, branch string) (bool, error)
    CreatePR(ctx context.Context, repo RepoRef, in NewPR) (PullRequest, error) // Draft flag inside NewPR
    CreateIssueComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error)
    GetIssueComment(ctx context.Context, repo RepoRef, commentID int64) (Comment, error)
    UpdateIssueComment(ctx context.Context, repo RepoRef, commentID int64, body string) error
    ListIssueCommentsSince(ctx context.Context, repo RepoRef, number int, since time.Time, etag string) (comments []Comment, newETag string, notModified bool, err error)
}

type TokenSource interface {
    Token(ctx context.Context) (secret.Secret, error)
}

func New(httpClient *http.Client, baseURL string, ts TokenSource) Client
```

`Issue` carries `Number, Title, Body, Labels, State, IsPullRequest, Author, AuthorAssociation, HTMLURL`.
`Comment` carries `ID, Author, AuthorAssociation, Body, CreatedAt, UpdatedAt, HTMLURL`. `baseURL` is configurable for tests and
GitHub Enterprise.

### 3.10 Agent loop (`internal/agent`)

```go
type Loop struct {
    Provider llm.Provider
    Model    string
    Tools    *tools.Registry
    Env      *tools.Env
    Pricing  llm.PriceTable
    Emit     events.Emitter
    Clock    func() time.Time
}

type Limits struct {
    MaxTurns        int           // per stage: refine 20, code 40 (test-retry re-entries share the code cap)
    MaxOutputTokens int           // per call, default 8192
    MaxTotalTokens  int64         // default 2_000_000
    MaxCostMicros   int64         // default 2_000_000 (2 USD); 0 disables
    ContextWindow   int           // from the model registry, 0 when unknown
    Deadline        time.Time     // run wall clock
}

// Session holds the conversation so the Runner can re-enter the loop after a test failure.
type Session struct {
    System   string
    Messages []llm.Message
    Usage    llm.Usage
    Cost     int64 // micro-USD, -1 when the model has no price
    Turns    int
}

type EndReason string

const (
    EndSubmitted EndReason = "submitted"
    EndMaxTurns  EndReason = "max_turns"
    EndBudget    EndReason = "budget"
    EndDeadline  EndReason = "deadline"
    EndStuck     EndReason = "stuck"
    EndRefusal   EndReason = "refusal"
)

type Outcome struct {
    Reason     EndReason
    Submission *tools.Submission
}

// Run drives the loop until an EndReason. It returns an error only for failures that are not a normal
// ending: provider errors after retries, harness errors, context cancellation.
func (l *Loop) Run(ctx context.Context, s *Session, lim Limits) (Outcome, error)
```

### 3.11 Runner and Dispatcher (`internal/runner`)

```go
type RunRequest struct {
    Repo     github.RepoRef
    Issue    int
    Mode     Mode   // "refine", "fix", "investigate", or "" to derive from the issue labels
    Provider string // "" means config default
    Model    string // "" means the provider's default model
    DryRun   bool
    Trigger  string // "cli", "ui", later "webhook", "poll"
    Limits   *LimitOverrides
}

type Runner struct {
    Store      store.Store
    GitHub     github.Client
    Providers  *llm.Registry
    Workspaces workspace.Manager
    Executor   execx.Executor
    Emitter    events.Emitter
    Config     *config.Config
    Log        *slog.Logger
}

func (r *Runner) Start(ctx context.Context, req RunRequest) (store.Run, error) // validates, inserts as queued
func (r *Runner) Execute(ctx context.Context, runID int64) error               // runs the next stage to a terminal or waiting status
func (r *Runner) Cancel(ctx context.Context, runID int64, by Actor) error      // works on running and waiting runs

// Human actions. Each one checks the run's status with a compare-and-set, writes an "action.*" history
// event with the actor and channel in the same transaction, and returns ErrStaleStatus or ErrSpecChanged
// when it lost a race.
func (r *Runner) Approve(ctx context.Context, runID int64, d Decision) error // the caller then dispatches the code stage
func (r *Runner) Revise(ctx context.Context, runID int64, notes string, by Actor) error
func (r *Runner) Reject(ctx context.Context, runID int64, reason string, by Actor) error
func (r *Runner) Answer(ctx context.Context, runID int64, text string, by Actor) error
func (r *Runner) EditSpec(ctx context.Context, runID int64, baseVersion int, draft tools.SpecDraft, by Actor) (store.Spec, error)
func (r *Runner) Retry(ctx context.Context, runID int64, o RetryOverrides, by Actor) (store.Run, error)

// HandleComment turns one issue comment into an action, or ignores it. The Step 1 poller and the
// Step 2 webhook both call it.
func (r *Runner) HandleComment(ctx context.Context, repo github.RepoRef, issue int, c github.Comment) error
func (r *Runner) SyncComments(ctx context.Context, runID int64) error // one poll for one waiting run

type Actor struct {
    Name    string // GitHub login, or approval.local_actor for CLI and UI
    Channel string // "cli", "ui", "github_comment", "system"
}

type Decision struct {
    SpecVersion int
    SpecSHA     string // must equal the stored hash of that version
    By          Actor
    CommentID   int64 // for the github_comment channel
}

type RetryOverrides struct {
    Provider, Model string
    Limits          *LimitOverrides
}

// Declared in internal/agent. The Runner depends on this, not on *Loop, so another engine can be added later.
type Engine interface {
    Run(ctx context.Context, s *Session, lim Limits) (Outcome, error)
}

type Dispatcher interface {
    Submit(runID int64) error
    Shutdown(ctx context.Context) error
}
```

`patchbot run` calls `Start` then `Execute` on the main goroutine. `patchbot serve` calls `Start`, then
`Dispatcher.Submit`. The Step 1 dispatcher is a goroutine per run behind a semaphore of size
`server.max_concurrent_runs` (default 2). Runs waiting for a slot stay `queued`.

### 3.12 Event bus (`internal/events`)

```go
type Event struct {
    RunID int64           `json:"run_id"`
    Seq   int64           `json:"seq"` // per run, starts at 1, assigned by the store
    Type  string          `json:"type"`
    Time  time.Time       `json:"time"`
    Data  json.RawMessage `json:"data"`
}

type Emitter interface {
    Emit(ctx context.Context, runID int64, typ string, data any)
}

type Bus struct{ /* mutex, map[runID]map[*subscriber]struct{} */ }

func NewBus() *Bus
func (b *Bus) Publish(ev Event)
func (b *Bus) Subscribe(runID int64, buffer int) (ch <-chan Event, cancel func())
```

The real `Emitter` does three things in order: redact the payload, `Store.AppendEvent` (which assigns
`Seq`), `Bus.Publish`. The database is the source of truth and the bus is only a wake-up path. `Publish`
never blocks: a subscriber whose 256-slot buffer is full is dropped and its channel closed. The browser's
`EventSource` then reconnects with `Last-Event-ID` and the handler replays from the database. `Emit` does
not return an error. A failed write is logged and the run goes on.

---

## 4. The run and the agent loop

### 4.1 Run statuses and outcomes

`status` has eight values. Four are active (they hold the one-run-per-issue slot) and four are terminal.

```
                 +---------------- answer -----------------+
                 v                                         |
queued --> running (refine) --+--> needs_info -------------+
                 ^            |
                 |            +--> awaiting_approval --+-- approve --> queued --> running (code) --> succeeded
                 |                    |    ^   |       |                                        \--> failed
                 +----- revise -------+    +---+       +-- reject  --> rejected
                                           edit (new spec version, no LLM call)

any active status -- cancel or expiry --> cancelled        running -- error --> failed
```

| Status | Kind | Meaning |
|---|---|---|
| `queued` | active | Waiting for a worker slot. Entered at start, after approve, after revise, after answer |
| `running` | active | A stage is executing. Only this status has a heartbeat |
| `needs_info` | active, waiting | Questions posted. Waiting for an answer |
| `awaiting_approval` | active, waiting | Spec posted. Waiting for approve, revise, edit or reject |
| `succeeded`, `failed`, `cancelled`, `rejected` | terminal | |

A waiting run uses no process, no workspace and no tokens. It is only a database row, so it survives
restarts. Which stage `Execute` runs is derived from the database, not from memory: investigate mode runs
investigate; otherwise, if the mode is `fix` and the run's spec in force has a valid approval, it runs the
code stage; otherwise it runs the refine stage.

The current phase is a separate column for display: `preflight, clone, refine, spec, baseline, agent,
verify, publish`.

`outcome` (set with the terminal status):

| Outcome | Status | Meaning |
|---|---|---|
| `pr_opened` | succeeded | Tests passed, normal PR |
| `pr_draft` | succeeded | PR opened as draft: tests failed, were not run, or the agent stopped early |
| `investigated` | succeeded | Analysis comment posted |
| `spec_approved` | succeeded | Refine mode only: the spec was approved and no code was asked for |
| `rejected` | rejected | The spec was rejected by an allowed user |
| `expired` | cancelled | Waited longer than `approval.ttl` (default 14 days) |
| `no_changes` | succeeded | Agent finished without changing files. Explanation posted as a comment |
| `dry_run` | succeeded | Everything except push, PR and comment |
| `error` | failed | See `error_kind` and `error_message` |
| `cancelled` | cancelled | User cancel or shutdown |

### 4.2 Runner phases

The list below is the **code stage** and the investigate flow. In `fix` and `refine` modes a run first goes
through the **refine stage** (section 4.7), which shares steps 1 to 3, replaces 4 to 7 with read-only
exploration and a spec, and parks the run. The code stage starts with a fresh clone and a fresh `Session`
whose task is the approved spec. Nothing but database rows is carried across the wait.

1. **Start (before the run row exists).** Validate `owner/name` and the issue number. Resolve provider and
   model. Check the provider's API key is present. Insert the run as `queued`. The partial unique index
   rejects a second active run for the same issue with `ErrActiveRunExists` (CLI exit code 3, HTTP 409).
2. **Preflight.** `GetRepo` and `GetIssue`. Fail if the issue is closed, is a pull request, or the token has
   no push permission (fix mode). Derive the mode from labels when not given. With several labels the one
   that does least wins: `ai-investigate`, then `ai-refine`, then `ai-fix`. **Approval gate:** the code
   stage refuses to start unless `checkApproval` (4.7) passes. It is checked here even though the action
   handler checked it already. In fix mode, fail if an open PR already exists from `patchbot/issue-<n>`,
   or the remote branch exists and `--overwrite-branch` was not passed. Load up to 20 issue comments.
3. **Clone.** `git clone --depth 1 --single-branch --no-tags` of the default branch into
   `<data_dir>/workspaces/<run_id>/repo`, then `git switch -c patchbot/issue-<n>`. Record the base SHA and
   the `.git/config` hash (section 8.6). Resolve the test command: per-repo config, else detection, else none.
4. **Baseline** (fix mode, when a test command exists and `baseline_tests = true`). Run the tests once on the
   untouched checkout. The result goes into the first prompt and into the PR body, so a suite that was
   already red is not blamed on the patch.
5. **Agent.** Build the `Session` and call `Loop.Run`.
6. **Verify** (fix mode). Covered in 4.5.
7. **Publish.** Covered in 4.6.
8. **Cleanup.** Workspace is deleted on success, kept on failure for `keep_failed_hours` (default 24).
   `Manager.Sweep` runs at startup and hourly under `serve`.

A goroutine heartbeats `runs.heartbeat_at` every 10 seconds while `Execute` is alive.

### 4.3 One agent turn

```
loop:
  1. ctx cancelled?                      -> return ctx.Err()
  2. deadline passed?                    -> EndDeadline
  3. s.Turns >= MaxTurns?                -> EndMaxTurns
  4. budget pre-check (below) fails?     -> EndBudget
  5. compact history if needed (below)
  6. resp, err = Provider.Chat(...)      -> error handling in 4.4
  7. s.Turns++; add usage and cost; persist the llm step; emit llm.response and usage
  8. append resp.Message to s.Messages
  9. switch:
       tool calls present:
         run each call in order through Registry.Dispatch (sequential, never parallel)
         persist a tool step and emit tool.call / tool.result for each
         append one RoleTool message with all results
         if a call was submit and it validated -> EndSubmitted
         stuck detector trips -> see 4.4
       StopEndTurn with no tool calls:
         first time: append user message "Call the submit tool to finish, or continue working."
         second time in a row: treat the text as the summary -> EndSubmitted with a synthesized Submission
       StopMaxTokens:
         drop any tool call with truncated arguments from the stored message
         append user message "Your last reply was cut off at the output limit. Make smaller edits."
       StopRefusal -> EndRefusal
       StopOther   -> append user message "Your last reply could not be processed. Try again."
                      (two in a row -> error kind provider_malformed)
```

**Budget pre-check.** The cost of the next call is not known in advance, so step 4 reserves a worst case:
`estimate = (last call's input tokens + MaxOutputTokens)` priced at the model's rates, or just the token
count for the token budget. The run stops if `spent + estimate > cap`. Both budgets are checked. When the
model has no price entry, only the token budget applies, and the run logs one warning at the start.

**Compaction.** When `ContextWindow` is known and the last call's input exceeded 70% of it, tool results
older than the last 6 turns are replaced by `[output removed to save context; call the tool again if
needed]`. Message structure and call IDs stay intact, so all three adapters still see valid pairs. It is
deterministic and costs no extra LLM call.

**Submit validation.** In fix mode `submit` requires a non-empty `summary` and `pr_title`. If the working
tree has no changes the Runner still accepts it and ends with `no_changes`.

### 4.4 Failure paths inside the loop

| Failure | Detection | What happens |
|---|---|---|
| Rate limit, 5xx, network error | `llm.Error.Retryable()` | Retried by `WithRetry` up to 4 times. Then run fails, `error_kind = provider_unavailable`. Salvage rule applies (4.6). |
| Bad or missing API key | `ErrAuth` | Fail at once, `provider_auth`. No retry, no issue comment (the problem is local). |
| Prompt too long | `ErrContextTooLong` | Force compaction keeping 3 turns and retry once. Second failure: `context_overflow`, salvage rule. |
| Other 4xx | `ErrBadRequest` | Fail, `provider_bad_request`. The redacted provider message is stored. |
| Unparseable response | `ErrMalformed` | Retry once, then fail `provider_malformed`. |
| Unknown tool name | registry | `IsError` result: `unknown tool "x"; available: ...`. Loop continues. |
| Invalid tool arguments | registry | `IsError` result with the decode error. Loop continues. |
| Tool-level error (file missing, command denied, timeout, non-zero exit) | tool | `IsError` result or normal result with the exit code. Loop continues. |
| Path escape or protected path | workspace | `IsError` result, plus a `security.denied` event and a warning log. Loop continues. Five denials in one run end it with `policy_violations`. |
| Harness error (disk full, executor cannot start) | `Execute` returns error | Fail, `internal`. |
| Same tool call with identical arguments 3 times in a row | `stuck.go`, hash of name plus args | First trip: append user message saying the call is repeating. Second trip: `EndStuck`. |
| Max turns | counter | `EndMaxTurns`, salvage rule. |
| Token or dollar budget | pre-check | `EndBudget`, salvage rule. |
| Wall-clock deadline (default 30 min) | clock | `EndDeadline`, salvage rule. |
| Model refuses or is safety-blocked | `StopRefusal` | `EndRefusal`. Nothing is pushed. Comment on the issue says the model declined. |
| User cancel or SIGINT | context | Stop at once, kill the running command's process group. Status `cancelled`. No push, no comment. Workspace kept. |
| Process killed | heartbeat older than 60s | `FailStaleRuns` at next startup sets `failed`, `error_kind = interrupted`. The stale workspace is swept later. |

### 4.5 Verify and the test-retry loop

```
attempt = 0
loop:
  if no changes in the working tree            -> outcome no_changes
  if no test command                           -> verified = unknown, go to publish (draft)
  attempt++
  result = run test command (timeout 10 min, output cap 256 KB stored, 8 KB sent to the model)
  if exit code 0                               -> verified = passed, go to publish
  if attempt >= max_test_attempts (default 3)  -> verified = failed, go to publish (draft)
  if agent already ended on a hard limit       -> verified = failed, go to publish (draft)
  append user message: "Tests failed (attempt k of N)." + head 2 KB + tail 6 KB of output
  outcome = Loop.Run(same Session, same Limits)      // turns and budget carry over
  if outcome.Reason != EndSubmitted            -> verified = failed, go to publish (draft)
```

The harness runs the tests, not the model. The model may also run them through `run_cmd`, but only the
harness run decides whether the PR is a draft. A timed-out test run counts as a failure, and the feedback
message says it timed out.

### 4.6 Publish and the salvage rule

**Salvage rule.** When the agent phase ends for any reason other than submit, cancel, refusal or auth, and
the working tree has changes, the Runner still runs the tests once and publishes a **draft** PR whose body
starts with why the run stopped. Work that cost money is not thrown away, and a draft cannot be merged by
accident. With no changes, it posts a failure comment instead.

Fix mode, in order. Each step is recorded as a step row.

1. Check `.git/config` hash and hooks (section 8.6). Mismatch: fail `workspace_tampered`, nothing is pushed.
2. `git add -A`, commit as `patchbot <patchbot@users.noreply.github.com>` (configurable) with the message
   `fix: <pr_title> (#<n>)`.
3. Dry run stops here: print the diff stat, the PR title and body, and the comment text. Outcome `dry_run`.
4. Push `HEAD:refs/heads/patchbot/issue-<n>` after the branch guard (section 8.7). One retry on a transport
   error. Failure: `push_failed`, workspace kept, failure comment posted.
5. Create the PR against the default branch. `draft = verified != passed`, or any acceptance
   criterion is reported unmet. The body has: `Fixes #<n>`, a link to the approved spec comment with its
   version and approver, the acceptance-criteria checklist (4.7), the
   model's summary, test status with attempt count, baseline status, provider and model, tokens and cost,
   and a fixed footer saying the change was generated and needs review. If GitHub rejects draft PRs for the
   repository (422), retry as a normal PR with `[DRAFT - tests failing]` in the title. Failure after a
   successful push: `pr_failed`, and the comment links the branch.
6. Comment on the issue with the PR link and test status. A failed comment does not fail the run. It is
   logged and stored as a warning on the run.

Investigate mode: post `Submission.Report` as one comment, capped at 60,000 characters. A failed comment
here does fail the run (`comment_failed`), because the comment is the whole product. The report is stored
in the run row either way.

Failure comments (one per failed run, never on cancel, dry run, or auth errors) contain the error kind in
plain words, how far the run got, and no stack traces or raw provider messages.

---

### 4.7 Refine stage, spec and approval gate

**Refine stage steps.**

1. Start, preflight and clone as in 4.2. No branch is created, and the checkout stays on the default branch.
2. **Spec reuse.** If the issue already has an approved spec from an earlier run (a `refine` run that ended
   `spec_approved`, or the run this one retries) and the hash of the issue title and body still equals the
   hash stored on that spec, the new run adopts it (`runs.spec_id` points at it) and goes straight to the
   code stage. The approval was given for exactly that text, so the gate still holds. If the issue changed,
   the run refines again.
3. **Explore.** `Engine.Run` with `RefineTools()` and the refine limits (20 turns). The prompt holds the
   issue, any earlier questions and answers and, on a revise, the previous spec plus the reviser's notes.
4. The stage ends on one of two terminal tools:
   - `ask_questions`: post the questions as one issue comment, status `needs_info`.
   - `submit_spec`: store spec version N, render it to markdown, post it as an issue comment, status
     `awaiting_approval`. The comment ends with a harness-written footer that names the three commands.
5. Delete the workspace. In dry-run mode nothing is posted: the spec or questions are printed and stored,
   and the run still parks, so the gate is exercised the same way.

Refine-stage failures follow the table in 4.4. A refine stage that ends on a limit without a terminal tool
fails the run with `refine_incomplete` and posts a failure comment. There is nothing to salvage.

**The spec.** `Spec` has `Version`, the five fields of `SpecDraft`, `BodyMD` (the rendered comment),
`SHA256` (hash of a canonical JSON encoding of the five fields), `IssueSHA256`, `Source` (`agent`,
`revise`, `ui_edit`), `CreatedBy` and `Status` (`draft`, `posted`, `approved`, `superseded`, `rejected`).
The database row is canonical. The GitHub comment is a rendering of it and carries a hidden marker
`<!-- patchbot:spec run=7 v=2 sha=ab12... -->`. A run has at most one spec that is not `superseded`.

**Actions on a run.**

| Action | Allowed from status | Effect |
|---|---|---|
| Approve | `awaiting_approval` | Insert an `approvals` row bound to the spec's hash. Mode `fix`: status `queued`, and the code stage is dispatched. Mode `refine`: status `succeeded`, outcome `spec_approved`. A short confirmation comment names the approver and the version |
| Revise (notes required) | `awaiting_approval` | Status `queued`. The refine stage runs again with the notes and produces version N+1. Capped by `approval.max_revisions` (default 5), after which the action is refused |
| Edit spec (UI) | `awaiting_approval` | Store version N+1 with source `ui_edit` and no LLM call. Status stays `awaiting_approval` |
| Reject (reason optional) | `awaiting_approval`, `needs_info` | Status `rejected`. Comment on the issue |
| Answer | `needs_info` | Status `queued`. The refine stage runs again with the answer in the prompt |
| Cancel | any active status | Status `cancelled` |
| Retry | `failed`, `cancelled`, `rejected` | Creates a **new** run with `retry_of` set, the same repo, issue and mode, and optional provider, model and budget overrides. Spec reuse (step 2) decides whether it needs a new approval. The old run is never reopened |

Whenever a new spec version is created (revise or edit), the old version becomes `superseded` in the same
transaction and its approval, if any, stops counting. The old GitHub comment is edited down to one line
that links the new one, and the new version is posted as a new comment. **A new version always needs a new
approval.**

**The gate.** One function decides whether code may be written. It is called by `Approve`, at the top of
the code stage, and again right before the write tools are registered:

```go
// checkApproval returns nil only when the run's spec in force is the latest version, has status
// "approved", has an approvals row whose spec_sha256 equals the spec's current hash, and the issue
// hash recorded on the spec equals the issue as fetched now.
func (r *Runner) checkApproval(ctx context.Context, run store.Run, issue github.Issue) error
```

If the issue text changed after approval, the code stage does not start. The run goes back to
`awaiting_approval` with a comment saying the issue changed and the spec must be approved again or revised.

**GitHub comment commands.** `HandleComment` acts on a comment only when all of these hold:

- the issue has a run in a waiting status,
- the author is in `approval.approvers` **and** has `author_association` OWNER, MEMBER or COLLABORATOR,
- the author is not the bot account,
- the comment was created after the current spec (or questions) comment, and `updated_at == created_at`
  (an edited comment is never a command),
- the first line is exactly `/approve` or `/reject`, or starts with `/revise ` or `/reject `. The rest of
  the comment is the notes or the reason.

On `needs_info`, any other comment from an allowed user is taken as the answer. Everything else is ignored
without a reply, so a stranger cannot make the bot post. Before an `/approve` is accepted, the spec comment
is fetched and its body compared with the stored rendering. If someone edited the comment on GitHub, the
approval is refused with a reply that says edits on GitHub are not read and that `/revise` or the UI must
be used. This keeps "an edited spec needs a new approval" true for edits made outside patchbot.

With an empty `approval.approvers` list, comment commands are off and only the CLI and UI can approve.

**Comment poller (Step 1 only).** Under `serve`, a goroutine lists waiting runs every
`approval.poll_interval` (default 60s) and calls `SyncComments` for each: one conditional request per
issue (`since` plus ETag, so an unchanged issue costs no rate limit). `patchbot sync` does the same once
from the CLI for people who do not run the server. The same loop expires runs past `approval.ttl`.

**Acceptance criteria in the PR.** The code-stage prompt lists the criteria as AC1 to ACn. `submit` must
report each one. The PR body renders them as a task list, `- [x] AC1: <text>. Evidence: <evidence>`, or
`- [ ]` when not met, under a heading that says this is the agent's own assessment, next to the test result
measured by the harness. Any unmet criterion makes the PR a draft.

### 4.8 CLI commands

| Command | Does |
|---|---|
| `patchbot run --repo R --issue N [--mode M] [--provider P] [--model M] [--max-cost-usd X] [--dry-run]` | Starts a run and executes the next stage in the foreground. In `fix` and `refine` modes it prints the spec and the run ID and exits 0 at `awaiting_approval` or `needs_info`. There is deliberately no flag that approves in advance |
| `patchbot spec <run-id> [--version N]` | Prints the spec and its status |
| `patchbot approve <run-id> [--detach]` | Prints the version and hash it approves, records the approval, then runs the code stage in the foreground. With `--detach` it only records it, and a running `serve` picks the run up |
| `patchbot revise <run-id> --notes "..."` | Runs the refine stage again in the foreground |
| `patchbot answer <run-id> --text "..."` | Answers `needs_info`, then refines again in the foreground |
| `patchbot reject <run-id> [--reason "..."]` | Rejects the spec |
| `patchbot cancel <run-id>` | Cancels a waiting run, or signals a run executing in another process through the `cancel_requested` column, which the loop reads at each turn |
| `patchbot retry <run-id> [--provider P] [--model M]` | New run from a failed, cancelled or rejected one |
| `patchbot sync [<run-id>]` | Reads new issue comments once for waiting runs and applies commands |
| `patchbot runs [--status S]` | Lists runs |
| `patchbot serve`, `models`, `config check\|show`, `version` | As described in section 2 |

Exit codes: 0 success or parked at a waiting status, 1 failed, 2 usage, 3 an active run already exists,
4 action not allowed in the run's current status, 130 cancelled.

---

## 5. Configuration

### 5.1 Format: TOML

TOML, parsed with `github.com/BurntSushi/toml`. Reasons:

- The config is tables of tables (providers, models, prices, per-repo settings). TOML handles that without
  significant whitespace, so a pasted block cannot change meaning through indentation.
- Types are explicit. YAML's implicit typing (`no`, `on`, `1.10`) is a real hazard in a file that holds
  model IDs and prices.
- The Go YAML ecosystem is unsettled: `gopkg.in/yaml.v3` is no longer maintained and the replacements are
  young. `BurntSushi/toml` is small, stable and has no dependencies.
- `toml.MetaData.Undecoded()` gives unknown-key detection for free, which the loader turns into an error.

### 5.2 Search order and precedence

Config file: `--config` flag, else `PATCHBOT_CONFIG`, else `./patchbot.toml`, else `~/.patchbot/config.toml`.
A missing file is fine unless it was named explicitly.

Value precedence: **flag > env > file > built-in default**. The loader applies the layers in reverse:
`config.Default()` (decoded from the embedded `defaults.toml`), then the file, then an explicit table of env
bindings, then only the flags the user actually set (`flag.FlagSet.Visit`). `patchbot config show` prints
the effective config with the source of each value.

### 5.3 Example file (`patchbot.example.toml`)

```toml
data_dir = "~/.patchbot"          # database and workspaces live here

[defaults]
provider = "anthropic"
mode     = "fix"

[limits]
max_turns          = 40           # code stage
refine_max_turns   = 20           # refine stage
max_output_tokens  = 8192
max_total_tokens   = 2000000      # per run, across refine, revisions and code
max_cost_usd       = 2.00         # per run, across all stages; 0 disables the dollar cap
hard_max_cost_usd  = 10.00        # ceiling for the per-run budget chosen in the UI or by flag
max_test_attempts  = 3
run_timeout        = "30m"
cmd_timeout        = "2m"
test_timeout       = "10m"
cmd_output_bytes   = 8192         # sent to the model per command
baseline_tests     = true

[server]
addr                = "127.0.0.1:8080"
max_concurrent_runs = 2
vue_src             = "cdn"       # or "local" to serve web/static/vendor/vue.global.prod.js

[git]
author_name  = "patchbot"
author_email = "patchbot@users.noreply.github.com"
branch_prefix = "patchbot/"

[github]
api_url = "https://api.github.com"
trusted_associations = ["OWNER", "MEMBER", "COLLABORATOR"]

[approval]
approvers     = ["your-github-login"]   # who may /approve, /revise, /reject by comment; empty disables comment commands
local_actor   = "local"                 # name recorded for CLI and UI actions (there is no login in Step 1)
poll_interval = "60s"
ttl           = "336h"                  # 14 days, then a waiting run is cancelled as expired
max_revisions = 5

[providers.anthropic]
type          = "anthropic"
default_model = "claude-sonnet-5-5"

  [[providers.anthropic.models]]
  id = "claude-opus-5-5"
  [[providers.anthropic.models]]
  id = "claude-sonnet-5-5"
  [[providers.anthropic.models]]
  id = "claude-haiku-5-5"
  [[providers.anthropic.models]]
  id = "claude-fable-5-2"

[providers.gemini]
type          = "gemini"
default_model = ""                # set this to a model ID from `patchbot models --provider gemini`

[providers.openai]
type        = "openai"
base_url    = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"

[providers.ollama]
type     = "openai"
base_url = "http://localhost:11434/v1"   # no api_key_env: no Authorization header is sent

  [[providers.ollama.models]]
  id             = "your-local-model"
  context_window = 32768
  input_per_mtok = 0.0                   # an explicit zero price means "free", not "unknown"
  output_per_mtok = 0.0

[repos."owner/name"]
test_cmd = ["go", "test", "./..."]
allow    = [["make", "test"], ["make", "lint"]]   # extra allowlist entries for this repo only
```

Per-model keys: `id`, `display_name`, `context_window`, `thinking` (`off` or `adaptive`, Anthropic only),
`input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`, `cache_write_per_mtok`.

Per-provider keys: `type` (`anthropic`, `gemini`, `openai`), `base_url`, `api_key_env`, `default_model`,
`headers` (table), `max_tokens_field`, `system_role`, `models`.

### 5.4 Environment variables

Secrets, read only from the environment:

| Variable | Used for |
|---|---|
| `GITHUB_TOKEN` | GitHub API and git push |
| `ANTHROPIC_API_KEY` | providers of type `anthropic` |
| `GEMINI_API_KEY` (fallback `GOOGLE_API_KEY`) | providers of type `gemini` |
| the name in `api_key_env` | providers of type `openai` |
| `PATCHBOT_UI_TOKEN` | required when `server.addr` is not a loopback address |

Settings: `PATCHBOT_CONFIG`, `PATCHBOT_DATA_DIR`, `PATCHBOT_PROVIDER`, `PATCHBOT_MODEL`, `PATCHBOT_ADDR`,
`PATCHBOT_MAX_COST_USD`, `PATCHBOT_MAX_TURNS`, `PATCHBOT_LOG_LEVEL` (`debug|info|warn|error`),
`PATCHBOT_LOG_FORMAT` (`text|json`).

Rules the loader enforces:

- A config key named `api_key`, `token`, `secret` or `password` at any depth is an error that tells the user
  to use the environment. Secrets cannot enter through the file at all.
- There is no `.env` loading. The README recommends `direnv` or the shell profile.
- A provider is "configured" when its key is present, or when it is type `openai` with no `api_key_env`.
  Unconfigured providers are listed in the UI as disabled, with the missing variable name.

### 5.5 Provider and model registry

`llm.NewRegistry(cfg, env)` builds one `Provider` per `[providers.<name>]` table, each wrapped in
`WithRetry`. The instance name is the key used by `--provider`, the UI and the `runs.provider` column.

Model resolution for a run: `--model` flag, else `PATCHBOT_MODEL`, else the provider's `default_model`. If
none is set the run is rejected with a message that names the `models` command. The chosen ID is not checked
against the list, because local servers and routers accept IDs they do not list. A wrong ID surfaces as the
provider's 404 on the first call.

The four Anthropic IDs from the brief ship in `defaults.toml`. No Gemini or OpenAI model names ship anywhere
in the binary.

### 5.6 Pricing and cost

```go
type Price struct { // micro-USD per million tokens, so integer math throughout
    Input, Output, CacheRead, CacheWrite int64
    Known bool
}

type PriceTable map[string]Price // key "provider/model"

func (t PriceTable) Cost(provider, model string, u Usage) (micros int64, known bool)
```

`cost = (Input*p.Input + Output*p.Output + CacheRead*p.CacheRead + CacheWrite*p.CacheWrite) / 1_000_000`.
Missing cache prices fall back to the input price.

Prices are config data, not code. **The plan deliberately does not list prices**: I do not have verified
prices for the model IDs in the brief, and wrong numbers in a cost tracker are worse than none. Milestone
M3 includes filling `defaults.toml` for the four Anthropic IDs from the official pricing page, with a
`# checked YYYY-MM-DD` comment. Everything else is priced by the user in their own file.

Behaviour without a price: cost is stored as NULL and shown as "n/a", the dollar cap is not enforced for
that run, the token cap still is, and the run logs a warning at the start. Costs are always labelled as
estimates in the UI, since the provider's invoice is the real figure.

---

## 6. SQLite schema and migrations

Driver `modernc.org/sqlite` through `database/sql`. DSN pragmas: `journal_mode(WAL)`, `busy_timeout(5000)`,
`foreign_keys(1)`, `synchronous(NORMAL)`, and `_txlock=immediate`. `db.SetMaxOpenConns(1)`: one connection
removes every `SQLITE_BUSY` case, and the write volume here is tiny. Times are INTEGER Unix milliseconds
UTC. Money is INTEGER micro-USD. Database file: `<data_dir>/patchbot.db`, created with mode 0600.

```sql
-- 0001_init.sql
CREATE TABLE runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    repo             TEXT    NOT NULL,                 -- "owner/name", lower-cased
    issue_number     INTEGER NOT NULL,
    issue_title      TEXT    NOT NULL DEFAULT '',
    mode             TEXT    NOT NULL CHECK (mode IN ('refine','fix','investigate')),
    retry_of         INTEGER REFERENCES runs(id),      -- the run this one retries
    spec_id          INTEGER,                          -- spec in force: own latest version, or an adopted one
    cancel_requested INTEGER NOT NULL DEFAULT 0,       -- set by another process, read at each turn
    waiting_since    INTEGER,                          -- when the run entered needs_info / awaiting_approval
    comments_etag    TEXT    NOT NULL DEFAULT '',      -- for the conditional comment poll
    comments_seen_at INTEGER,                          -- newest issue comment already handled
    trigger          TEXT    NOT NULL CHECK (trigger IN ('cli','ui','webhook','poll')),
    provider         TEXT    NOT NULL,
    model            TEXT    NOT NULL,
    dry_run          INTEGER NOT NULL DEFAULT 0,
    status           TEXT    NOT NULL CHECK (status IN ('queued','running','needs_info','awaiting_approval',
                                                    'succeeded','failed','cancelled','rejected')),
    phase            TEXT    NOT NULL DEFAULT '',
    outcome          TEXT    NOT NULL DEFAULT '',
    error_kind       TEXT    NOT NULL DEFAULT '',
    error_message    TEXT    NOT NULL DEFAULT '',      -- redacted, max 2000 chars
    branch           TEXT    NOT NULL DEFAULT '',
    base_sha         TEXT    NOT NULL DEFAULT '',
    head_sha         TEXT    NOT NULL DEFAULT '',
    pr_number        INTEGER,
    pr_url           TEXT    NOT NULL DEFAULT '',
    pr_draft         INTEGER NOT NULL DEFAULT 0,
    comment_url      TEXT    NOT NULL DEFAULT '',
    summary          TEXT    NOT NULL DEFAULT '',      -- model summary or investigation report
    tests_status     TEXT    NOT NULL DEFAULT '',      -- '', passed, failed, not_run
    test_attempts    INTEGER NOT NULL DEFAULT 0,
    turns            INTEGER NOT NULL DEFAULT 0,
    input_tokens     INTEGER NOT NULL DEFAULT 0,
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros      INTEGER,                          -- NULL when the model has no price
    limits_json      TEXT    NOT NULL DEFAULT '{}',    -- effective limits for this run
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    finished_at      INTEGER,
    heartbeat_at     INTEGER
);

-- The "one active run per issue" rule, enforced by the database.
CREATE UNIQUE INDEX runs_one_active_per_issue
    ON runs (repo, issue_number) WHERE status IN ('queued','running');
CREATE INDEX runs_created_at   ON runs (created_at DESC);
CREATE INDEX runs_status       ON runs (status, created_at);     -- queue claim in Step 3
CREATE INDEX runs_repo_issue   ON runs (repo, issue_number, created_at DESC);

CREATE TABLE steps (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idx           INTEGER NOT NULL,                    -- order within the run, starts at 1
    kind          TEXT    NOT NULL CHECK (kind IN ('phase','llm','tool','test','git','github')),
    name          TEXT    NOT NULL,                    -- phase name, tool name, "chat", "push", ...
    status        TEXT    NOT NULL CHECK (status IN ('running','ok','error')),
    turn          INTEGER,                             -- agent turn for llm and tool steps
    input_json    TEXT    NOT NULL DEFAULT '',         -- tool args or request summary, redacted, max 16 KB
    output_text   TEXT    NOT NULL DEFAULT '',         -- tool result or response text, redacted, max 64 KB
    stop_reason   TEXT    NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros   INTEGER,
    exit_code     INTEGER,
    duration_ms   INTEGER,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER,
    UNIQUE (run_id, idx)
);
CREATE INDEX steps_run_kind ON steps (run_id, kind);

CREATE TABLE events (
    run_id     INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    type       TEXT    NOT NULL,
    data       TEXT    NOT NULL,                       -- JSON, redacted
    created_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, seq)
) WITHOUT ROWID;
```

`steps` is the structured record used for accounting and the run summary. `events` is the append-only UI
log used for SSE replay. They overlap on purpose: one is queried, the other is streamed. `AppendEvent`
computes `seq` as `COALESCE(MAX(seq),0)+1` inside the insert statement, which is safe with one connection.

Run totals on `runs` are updated in the same transaction as the `llm` step that caused them
(`AddUsage` plus `FinishStep`), so the list page never needs an aggregate query.

**Migrations.** Forward-only, numbered SQL files in `internal/store/migrations`, embedded with `embed.FS`.
`migrate.go` creates `schema_migrations(version INTEGER PRIMARY KEY, name TEXT, applied_at INTEGER)`,
applies each missing file in its own transaction in order, and fails startup if the database has a version
the binary does not know. It runs automatically on `store.Open`. No migration library: this is about 60
lines, has no dependency, and a personal tool never needs down-migrations. The rule for later steps:
new tables and nullable columns only, and never edit a shipped migration.

Reserved for later, not created now: `webhook_deliveries` (Step 2) and lease columns on `runs` (Step 3).

---

## 7. Web UI

### 7.1 Routes

Go 1.22 pattern routing on `http.ServeMux`.

| Method and path | Returns | Notes |
|---|---|---|
| `GET /{$}` | HTML | Dashboard |
| `GET /runs/{id}` | HTML | Same page with that run selected (deep link, survives reload) |
| `GET /api/providers` | JSON | `[{name, type, configured, missing_env, default_model}]` |
| `GET /api/providers/{name}/models` | JSON | `{models: [...], source: "api"\|"config"\|"merged", warning}`; `?refresh=1` skips the cache |
| `GET /api/runs` | JSON | `?limit=50&before_id=` newest first |
| `POST /api/runs` | JSON | Body `{repo, issue, mode, provider, model, dry_run}`. 201 with the run, 409 when one is active, 422 on validation |
| `GET /api/runs/{id}` | JSON | Run plus its steps |
| `POST /api/runs/{id}/cancel` | JSON | 202, or 409 when already finished |
| `GET /api/runs/{id}/events` | `text/event-stream` | Replays from `Last-Event-ID`, then live |
| `GET /static/` | files | From `embed.FS`, `Cache-Control` with the build version in the URL query |
| `GET /healthz` | `ok` | Also pings the database |

Errors are always `{"error": {"code": "...", "message": "..."}}`. Request bodies are limited to 64 KB with
`http.MaxBytesReader` and decoded with `DisallowUnknownFields`.

Middleware order: recover, request log (slog, with a request ID), host check, `http.CrossOriginProtection`
(Go 1.25, rejects cross-site state-changing requests), optional bearer check for `PATCHBOT_UI_TOKEN`,
security headers. Server timeouts: `ReadHeaderTimeout` 5s, `ReadTimeout` 15s, `IdleTimeout` 120s, and
`WriteTimeout` 30s. The SSE handler lifts its own write deadline with `http.ResponseController`.

### 7.2 Templates

`html/template` parsed once at startup from `web.FS`. `layout.html` defines the document and a `content`
block. `index.html` fills it and includes the three partials. Template data:

```go
type pageData struct {
    Version string
    VueSrc  string        // CDN URL or /static/vendor/...
    VueSRI  string        // integrity hash, empty for local
    Boot    template.JS   // JSON produced by json.Marshal of bootData, safe for a JSON script block
}

type bootData struct {
    Providers     []providerView
    Runs          []runView // newest 50
    SelectedRunID int64     // 0 on "/"
    Defaults      struct{ Provider, Model, Mode string }
}
```

### 7.3 Vue 3 from a CDN on server-rendered HTML

- `layout.html` loads the **full global build** (it includes the template compiler) pinned to an exact
  version with SRI: `<script src="https://cdn.jsdelivr.net/npm/vue@3.x.y/dist/vue.global.prod.js"
  integrity="sha384-..." crossorigin="anonymous" defer>`. The exact version and hash are fixed at M14.
  `server.vue_src = "local"` serves a vendored copy for offline use. `make vendor-vue` downloads it.
- Go templates and Vue both use `{{ }}`. Go keeps `{{ }}`. Vue is switched to `[[ ]]` with
  `app.config.compilerOptions.delimiters = ['[[', ']]']`. Go never sees `[[`, and Vue never sees `{{`
  because Go has already rendered it. Directives (`v-for`, `:href`, `@click`) need no escaping.
- The server writes the initial state as `<script type="application/json" id="boot">{{ .Boot }}</script>`.
  `app.js` reads it with `JSON.parse(document.getElementById('boot').textContent)`, so the first paint needs
  no API call. `html/template` escapes the JSON for the script context.
- `app.js` calls `Vue.createApp({...}).mount('#app')`. The HTML inside `#app` rendered by Go is the Vue
  template (an in-DOM template). `#app` carries `v-cloak` with `[v-cloak]{display:none}`, and a `<noscript>`
  block says JavaScript is required.
- This is not SSR hydration. The server renders the shell and data, Vue renders the lists. That is the
  simplest thing that meets "server-rendered page, no build step".
- No inline scripts, so the CSP can be strict: `default-src 'self'; script-src 'self'
  https://cdn.jsdelivr.net 'unsafe-eval'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'`.
  `'unsafe-eval'` is needed because the in-browser template compiler uses `new Function`. That is the price
  of no build step, and it is acceptable for a loopback tool that renders all untrusted text through
  `[[ ]]` interpolation (which escapes) and never through `v-html`.
- State lives in one `reactive` object: `providers`, `models`, `form`, `runs`, `selected`, `log`,
  `connection`. No router. Selecting a run uses `history.pushState('/runs/<id>')` and `popstate`.

### 7.4 SSE format

```
id: 42
event: tool.result
data: {"run_id":7,"seq":42,"type":"tool.result","time":"2026-11-10T09:15:02.113Z","data":{"turn":5,"name":"run_cmd","ok":true,"exit_code":0,"preview":"ok  example/pkg 0.21s","truncated":false,"duration_ms":812}}

```

- `id` is the per-run `seq`. On reconnect the browser sends `Last-Event-ID` and the handler replays
  `seq > id` from the database in pages of 500, then switches to the bus. To close the gap between replay
  and subscribe, the handler subscribes first, replays, then drops bus events with `seq` at or below the
  last replayed one.
- `retry: 2000` is sent once at the start. A comment line `: ping` goes out every 15 seconds.
- Headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`.
- After the terminal event the server sends `event: end` and closes. The client calls `close()` on `end`,
  so finished runs do not reconnect forever.

Event types and `data` fields:

| Type | Data |
|---|---|
| `run.status` | `status, phase, outcome, error_kind, error_message, pr_url, comment_url` |
| `phase.started` / `phase.finished` | `phase, ok, duration_ms` |
| `llm.response` | `turn, text` (max 4 KB), `stop_reason, tool_calls` (names only) |
| `tool.call` | `turn, call_id, name, args` (max 2 KB, redacted) |
| `tool.result` | `turn, call_id, name, ok, exit_code, preview` (max 4 KB), `truncated, duration_ms` |
| `test.result` | `attempt, max_attempts, passed, exit_code, timed_out, tail` (max 4 KB), `duration_ms` |
| `usage` | `turn, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros, total_cost_micros, budget_micros, total_tokens, budget_tokens` |
| `security.denied` | `tool, reason` |
| `log` | `level, message` |
| `end` | `{}` |

### 7.5 Screens

One page with three regions. On wide screens: a left column (new run, runs list) and a right column (run
detail). Under 900px they stack.

1. **Header bar.** Name, version, a GitHub token indicator (present or missing, never the value), count of
   running runs.
2. **New run card.** Provider select (unconfigured providers disabled, with the missing variable as hint).
   Model control: a text input bound to a `<datalist>` of listed models, so an unlisted ID can be typed.
   A refresh button and a small label showing the list source (`api`, `config`). Repo input validated
   against `^[\w.-]+/[\w.-]+$`. Issue number. Mode radio: fix, investigate. Dry-run checkbox. Start button.
   Field errors show inline. A 409 shows "a run for this issue is already active" with a link to it.
   Provider and model choices are remembered in `localStorage`.
3. **Runs list.** Table: status badge, repo#issue, mode, provider/model, turns, tokens, cost, started,
   duration, PR link. Newest first, "load more" pagination. Rows that are `queued` or `running` are
   refreshed by polling `GET /api/runs` every 5 seconds while any exist. Clicking a row selects it.
4. **Run detail.** Header with issue title and link, status, phase, outcome, PR and comment links, a cancel
   button while active. A counters row: turns `12/40`, tokens, cost against budget as a bar, test attempts.
   The live log: one entry per event, grouped by turn. Tool calls show name plus a one-line argument
   summary and expand to arguments and output. Test results are coloured by pass or fail. Errors and
   security denials stand out. Auto-scroll sticks to the bottom until the user scrolls up, then a "jump to
   latest" pill appears. A connection indicator shows live, reconnecting or ended. For a finished run the
   same view is filled from the replay.
5. **Empty and error states.** No runs yet, no provider configured (shows the env variable names), and no
   GitHub token each get a specific message with the fix.

---

## 8. Security model

### 8.1 Trust boundaries and the honest limit of Step 1

| Input | Trust |
|---|---|
| Config file, flags, environment, the person at the UI | trusted |
| Issue title, body, comments | **untrusted data** |
| Repository contents (code, READMEs, test output) | untrusted data when read, **executable** when tests run |
| LLM output (text, tool calls, PR text) | untrusted |

Step 1 has no sandbox. Running a repository's tests runs that repository's code, plus whatever the model
wrote, as the local user. The allowlist controls what the model can start directly. It does not contain
what a started process does. So Step 1 is for repositories the owner trusts and issues the owner has read,
and `README` and `SECURITY.md` say so in the first screen. Docker in Step 4 is the real boundary. Every
control below still matters, because each one removes a class of accidents and cheap attacks.

### 8.2 Path jail

All file access by tools goes through `Workspace`, which holds an `*os.Root` opened on the checkout
(`os.OpenRoot`, Go 1.24+, with `MkdirAll`, `ReadFile`, `WriteFile`, `Rename` added in 1.25). `os.Root`
resolves paths with `openat`-style calls, so a path cannot leave the directory through `..`, an absolute
path, or a symlink, and there is no check-then-use race.

Layers, in order:

1. **Lexical check.** Reject empty paths, NUL bytes, absolute paths, and anything where `filepath.IsLocal`
   is false. This gives clear errors. It is not the security boundary.
2. **Protected paths.** After `filepath.Clean`, compared case-insensitively (macOS is case-insensitive):
   any path whose first element is `.git` is denied for read and write. Writes to `.github/workflows/**`
   are denied unless `allow_workflow_edits = true` for the repo, because a changed workflow can run with
   repository secrets when the PR is opened.
3. **`os.Root` operation.** Symlinks that point outside the root fail here. Symlinks that stay inside work.
4. **Symlink alias check.** A symlink inside the checkout could point at `.git`. Before a write, the target
   is opened through the root and compared with `os.SameFile` against the root's `.git`. On a write to an
   existing symlink, the link itself is never replaced: `write_file` refuses when `Lstat` shows a symlink.
5. **Size and type.** Writes over 1 MB and reads of non-regular files (devices, FIFOs, sockets) are refused.

No tool creates symlinks. `os.Root` does not stop a process started by `run_cmd`, which is the limit stated
in 8.1.

### 8.3 Command allowlist

`run_cmd` takes an argv array. There is no shell, so there is no quoting, piping, redirection, globbing or
`$(...)`. `Policy.Check` rules:

- `argv[0]` must be a bare name (no `/`), found through a fixed `PATH`, and present in the allowlist.
- Each allowed binary has an allowed first-argument set. Defaults:

| Binary | Allowed subcommands |
|---|---|
| `go` | `build`, `test`, `vet`, `list`, `fmt`, `mod tidy`, `mod download` |
| `gofmt` | any (flags and paths only) |
| `git` | `status`, `diff`, `log`, `show`, `blame`, `ls-files` |
| `npm`, `pnpm`, `yarn` | `test`, `run test`, `run lint`, `run build`, `ci`, `install` |
| `composer` | `install`, `test`, `validate` |
| `php` | only `vendor/bin/phpunit`, `vendor/bin/pest`, `artisan test` as first argument |
| `python`, `python3` | only `-m pytest`, `-m unittest` |
| `pytest`, `cargo` (`build`, `test`, `check`, `fmt`, `clippy`), `make` (targets listed per repo) | as stated |
| `ls`, `cat`, `head`, `tail`, `wc` | not allowed. The file tools cover them, with the jail |

- Never allowed, whatever the config says: `sh`, `bash`, `zsh`, `env`, `sudo`, `curl`, `wget`, `ssh`, `scp`,
  `nc`, `rm`, `chmod`, `chown`, `ln`, `docker`, `git push|config|remote|clone|fetch|pull|reset|checkout`.
  The config loader rejects an allowlist entry for any of these.
- Any argument that is an absolute path, or contains a `..` element, is rejected. Argument count is capped
  at 64 and total length at 8 KB.
- Per-repo `allow` entries add exact argv prefixes (for example `["make", "test"]`).
- The working directory is always the checkout root. The model cannot choose it.

A denied command returns an `IsError` result that lists what is allowed, so the model can adapt.

### 8.4 Timeouts, process control, output caps

- `run_cmd`: default 2 minutes, model may ask for up to 5. Test command: 10 minutes. Whole run: 30 minutes.
  Every provider HTTP call: 5 minutes. Every GitHub call: 30 seconds. All are contexts from one parent.
- Each command starts in its own process group (`SysProcAttr{Setpgid: true}`). On timeout or cancel:
  `SIGTERM` to the group, 5 seconds grace, then `SIGKILL` to the group, with `cmd.WaitDelay` set so a
  child holding the pipe open cannot hang `Wait`. Windows is out of scope. The build tag keeps it compiling
  with a plain kill.
- Stdin is `/dev/null`. Stdout and stderr go to one `capWriter` that keeps the first 2 KB and the last 6 KB
  with a `[... N bytes omitted ...]` marker between. Memory use is bounded whatever the process prints.
- Other caps: `read_file` 64 KB, search 200 matches, list 500 entries, diff in the PR-body step 200 KB,
  tool arguments stored 16 KB, step output stored 64 KB, events 4 KB of preview each.

### 8.5 Secrets and redaction

- Secrets enter only through the environment (5.4) and are held as `secret.Secret`, whose `String()`,
  `LogValue()` and `MarshalJSON()` all return `[redacted]`. Getting the value needs an explicit `.Reveal()`,
  which is easy to audit with grep.
- **Child processes get a scrubbed environment**: `PATH` (fixed), `HOME` (an empty per-run directory),
  `LANG`, `TMPDIR` (per-run), `CI=1`, `GIT_TERMINAL_PROMPT=0`, plus `NO_COLOR=1`. No API key and no GitHub
  token is ever in the environment of a command the model starts or of the test command.
- The git token is given only to the harness's own `clone` and `push` processes, through
  `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0` setting `http.https://github.com/.extraheader`.
  It is never in argv (visible in `ps`), never in the remote URL, and never written to `.git/config`.
- `secret.Redactor` replaces (a) every known secret value and its base64 form, and (b) token-shaped strings:
  `ghp_`, `gho_`, `ghs_`, `github_pat_`, `sk-ant-`, `sk-`, `AIza`, `Bearer <long token>`, and PEM private
  key blocks. It is applied at four choke points: the slog handler wrapper, the event emitter (before the
  database and SSE), tool results before they go to the LLM, and PR and comment bodies before they go to
  GitHub. A test puts a fake token through each path and asserts it never appears.
- The database stores no secret and no request headers. Provider request bodies are not logged above debug
  level, and at debug level they pass the redactor.
- `patchbot config show` prints only whether each secret is set.

### 8.6 Git integrity

Because the model can run code through tests, the checkout's git metadata is treated as possibly modified
before the harness uses it with a token.

- Every harness git call uses `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`,
  `-c core.hooksPath=/dev/null`, `-c core.fsmonitor=false`, `-c protocol.file.allow=never`,
  `-c credential.helper=`.
- After clone, the Runner stores a SHA-256 of `.git/config`. Before commit and before push it recomputes it
  and also checks that `.git/hooks` has no non-sample files. A mismatch fails the run with
  `workspace_tampered` and nothing is pushed.
- The push URL is built from the validated `owner/name`, not read from the remote config.

### 8.7 Branch protection

Three independent guards, since pushing to the default branch is the worst outcome:

1. The branch name is built by code as `<branch_prefix>issue-<n>`. Neither the model nor the issue text
   can influence it.
2. `Workspace.Push` refuses, before starting git, when the branch does not start with the configured
   prefix, equals the default branch, or equals `main`, `master`, `develop`, `trunk`.
3. The push uses an explicit refspec `HEAD:refs/heads/<branch>` and never `--all`, `--mirror` or `--tags`.
   Force is `--force-with-lease` and only with `--overwrite-branch`.

The README also recommends a fine-grained token limited to the target repositories (contents, pull
requests, issues: read and write, and no workflow scope) and a branch protection rule on the default
branch, so GitHub rejects a bad push even if all three guards failed.

### 8.8 Prompt injection stance

Issue text, comments, file contents and command output are data. Nothing in them is ever treated as an
instruction to the harness, and the model is told the same. The defence does not depend on the model
obeying. It depends on what the model is able to do:

- **Capability limits.** No network tool. No secrets in reach of tools. Files only inside the checkout.
  Commands only from the allowlist. One branch, never the default. Output is a PR or a comment that a
  human reviews. The worst case for a successful injection is a bad draft that a reviewer rejects, plus
  the spend up to the budget cap.
- **Framing.** The system prompt holds the rules. The issue goes into the first user message inside clearly
  marked delimiters with a per-run random suffix (so the text cannot close the block), with the statement
  that it is a bug report to analyse, not instructions to follow.
- **Input filtering.** Only comments whose `author_association` is in `trusted_associations` are included.
  The issue body is always included, because a labelled issue was approved by someone with triage rights.
  Issue body is capped at 20 KB and each comment at 4 KB. HTML comments are stripped, because they are
  invisible to the human who applied the label.
- **Output filtering.** PR and comment bodies pass the redactor. `@mentions` are neutralised by inserting a
  zero-width space after `@`, so a run cannot ping people. Lengths are capped. Bodies end with a fixed
  footer written by the harness, not the model.
- **No instruction channel from the repo.** The harness reads no agent-instruction files from the checkout
  in Step 1. The test command comes from the trusted config or from detection of well-known files, and
  detection only picks from a fixed list of commands. It never executes a string found in the repo.

### 8.9 Server

- Default bind `127.0.0.1:8080`. A non-loopback address without `PATCHBOT_UI_TOKEN` is a startup error.
- The host check accepts only `localhost`, `127.0.0.1`, `[::1]` and configured hosts, which blocks DNS
  rebinding. `http.CrossOriginProtection` blocks cross-site POSTs from a malicious page in the same browser.
- With a UI token set, the API needs `Authorization: Bearer`, compared with `subtle.ConstantTimeCompare`.
  The page asks for it once and keeps it in `sessionStorage`. SSE uses a short-lived signed query parameter
  because `EventSource` cannot set headers.
- Every model-produced string reaches the page as JSON rendered through Vue text interpolation. No
  `v-html`, no `template.HTML` on untrusted data.

### 8.10 One active run per issue

The partial unique index in section 6 enforces it in the database, so the CLI and the server (two
processes on one database file) cannot both start a run for the same issue. A crashed process leaves a
`running` row, and `FailStaleRuns` (heartbeat older than 60 seconds) clears it at the next startup and
every minute under `serve`.

---

## 9. Testing strategy

Rules: table-driven, standard library `testing` only, `t.Parallel()` where there is no shared state,
`t.TempDir()` for files, `t.Context()` for contexts, no network and no sleeps in unit tests (the clock and
backoff are injected).

### 9.1 Unit tests

| Package | What is covered |
|---|---|
| `config` | Precedence table (each of flag, env, file, default winning in turn), unknown keys, secret-named keys rejected, duration and path parsing, banned allowlist entries |
| `secret` | Every known value and pattern redacted, base64 form, no false positive on ordinary hex, `Secret` formatting with `%v`, `%s`, `%+v`, JSON, slog |
| `llm` | `Validate` invariants, `Usage.Add`, cost math with and without cache prices, unknown price, retry policy with a fake clock, schema cleaning |
| `tools` | Each tool's happy path and every error message, argument validation, unknown fields, caps |
| `workspace` (jail) | `..`, absolute path, NUL, symlink to outside, symlink to `.git`, symlinked parent directory, `.GIT` case variant, FIFO, oversized write, workflow path |
| `execx` | Allowlist table (allowed, denied binary, denied subcommand, absolute-path argument), timeout kills the whole process group (a script that spawns a sleeping child), output cap keeps head and tail, scrubbed env (child prints its env, test asserts no secret) |
| `events` | Ordering, slow subscriber dropped without blocking the publisher, cancel removes the subscriber, run under `-race` |
| `store` | Migration from empty, idempotent re-run, unknown newer version rejected, active-run unique index, compare-and-set status, stale-run recovery, event `seq` monotonic |
| `github` | Each call against `httptest.Server`: paths, headers, bodies, pagination, 404, 403 rate limit, 422 on draft |
| `server` | Routes with `httptest`: validation, 409, cancel, cross-origin POST rejected, bad Host rejected, SSE replay from `Last-Event-ID` then live, `end` event |

Fuzz tests (`go test -fuzz`, seeds committed): the path jail (`FuzzJailPath` asserts no accepted path
resolves outside the root) and `Policy.Check`.

### 9.2 The fake provider

`internal/llm/fake` implements `Provider` from a script, so agent tests are deterministic and free.

```go
type Step struct {
    Text      string
    ToolCalls []llm.ToolCall
    Stop      llm.StopReason
    Usage     llm.Usage
    Err       error                          // returned instead of a response
    Expect    func(req llm.ChatRequest) error // optional assertion on the incoming request
}

func New(name string, steps ...Step) *Provider
func FromFile(path string) (*Provider, error) // JSON script, used by the demo mode
func (p *Provider) Requests() []llm.ChatRequest // everything the loop sent, for assertions
```

Running past the end of the script fails the test with the unexpected request. Agent-loop cases, one table
row each: straight submit; read, edit, submit; unknown tool then recovery; invalid arguments then recovery;
end of turn without submit (nudge, then accept); truncated output; refusal; max turns; token budget; dollar
budget; deadline with a fake clock; stuck detector; retryable error then success; auth error; context
overflow then compaction; cancel in the middle of a tool call; test fails twice then passes; test fails
three times (draft); no changes.

With `PATCHBOT_ENABLE_FAKE=1`, `--provider fake --fake-script file.json` works from the CLI, which gives a
free end-to-end demo and is what the README quick start uses.

### 9.3 Golden tests for provider adapters

For each adapter, `testdata/` holds pairs per scenario: `<name>.req.golden.json` (the request body the
adapter must produce from a fixed normalized `ChatRequest`) and `<name>.resp.json` (a recorded real
response). The test starts an `httptest.Server` that asserts method, path and auth header, compares the
request body with the golden file after JSON normalisation, and replies with the recorded response. The
adapter's output is then compared with an expected normalized `ChatResponse`.

Scenarios per provider: plain text, one tool call, two tool calls in one turn, the follow-up request that
carries tool results, max-tokens stop, refusal or safety stop, each error status, list models. Extra:
Anthropic thinking block round trip and cache usage fields. Gemini missing call IDs and thought signature
round trip. OpenAI-compatible fixtures recorded from OpenAI, OpenRouter, Ollama and Groq, covering each
quirk in 3.4.

`go test ./internal/llm/... -update` rewrites request goldens. Responses are recorded by
`make record PROVIDER=...`, a small test helper behind the `record` build tag that calls the real API with
keys from the environment and writes the body after removing headers, request IDs and keys. Recorded files
are reviewed by hand before commit. A CI step greps `testdata/` for token-shaped strings.

### 9.4 Git workspace integration tests

No network. Each test builds a bare "remote" with `git init --bare` in `t.TempDir()`, seeds it from
`testdata/repos/<fixture>`, and uses a `file://` URL (the test manager allows the file protocol, production
does not). Covered: clone and branch creation; commit with the configured author; push lands the right ref
on the bare remote; push refused for the default branch and for a branch without the prefix; existing
remote branch refused without overwrite and replaced with it; `.git/config` tamper detected; a planted
hook is not executed; diff and diff stat; test-command detection for each fixture. These tests are skipped
with a clear message when `git` is not installed.

A full-flow test in `runner` wires the fake provider, the local bare remote, a fake `github.Client` and an
in-memory SQLite database, and asserts the final run row, the steps, the event sequence, the pushed commit
and the PR and comment payloads, for: fix success, fix with failing tests (draft), investigate, dry run,
duplicate active run, cancel.

A manual end-to-end check against a real throwaway repository is a release step, not a CI job. It lives
behind the `e2e` build tag and needs real tokens.

### 9.5 CI (`.github/workflows/ci.yml`)

Triggers: push to `main` and pull requests. Jobs:

1. **lint**: `golangci-lint` (v2 config) with `errcheck`, `govet`, `staticcheck`, `ineffassign`, `unused`,
   `gosec`, `revive`, `errorlint`, `bodyclose`, `contextcheck`, `noctx`, `sloglint`, `depguard`,
   `gocritic`, `misspell`, formatters `gofumpt` and `goimports`.
2. **test**: matrix `ubuntu-latest` and `macos-latest`, Go 1.25.x. `go vet ./...`, then
   `go test -race -shuffle=on -coverprofile=cover.out ./...`. Coverage is reported, with a floor of 75%
   total and 85% for `agent`, `workspace`, `execx`, `secret`.
3. **build**: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X .../version.Version=..."` for
   linux/amd64, linux/arm64, darwin/arm64, which proves the no-CGO claim.
4. **vuln**: `govulncheck ./...`.
5. **tidy**: `go mod tidy` leaves no diff, and the fixture secret grep.

Makefile targets mirror CI: `build`, `test`, `test-race`, `lint`, `fmt`, `cover`, `vuln`, `run`, `serve`,
`record`, `vendor-vue`, `ci` (everything CI runs).

---

## 10. Library choices

| Need | Choice | Reason |
|---|---|---|
| Anthropic | `github.com/anthropics/anthropic-sdk-go` (official) | Solid and maintained by the API owner. Typed tool-use and thinking blocks, model listing, base URL and HTTP client options for golden tests. Its retries are disabled in favour of ours. |
| Gemini | raw `net/http` | The official `google.golang.org/genai` SDK works, but it pulls in the Google Cloud auth stack for Vertex support that this project does not use, which is a lot of weight in a single binary. The surface needed is two endpoints. Raw JSON also makes it trivial to echo `thoughtSignature` and unknown fields unchanged. The adapter is behind `Provider`, so swapping to the SDK later touches one package. |
| OpenAI-compatible | raw `net/http` | The target is the compat ecosystem, not only OpenAI. Servers differ in small ways (3.4) that typed SDK structs reject or hide. Two endpoints, about 300 lines. |
| GitHub | raw `net/http` | Seven REST calls. `google/go-github` is good but large, and a hand-written client behind an interface is easy to fake and to move to GitHub App auth later. |
| Git | the `git` CLI through `os/exec` | Exact behaviour of real git, shallow clone, and auth through env config. `go-git` is a heavy dependency with gaps and would be a second git implementation to trust. Requires git 2.31+ for `GIT_CONFIG_COUNT`, checked at startup. |
| SQLite | `modernc.org/sqlite` | Required by the brief. Pure Go, so cross-compiling stays trivial. |
| Migrations | own code, about 60 lines | Forward-only numbered files. No library earns its place. |
| Config | `github.com/BurntSushi/toml` | Section 5.1. |
| CLI | stdlib `flag` | Five subcommands. A small dispatcher is clearer than Cobra here and adds no dependency. |
| HTTP | stdlib `net/http` | Required by the brief. Pattern routing, `ResponseController`, `CrossOriginProtection`. |
| Logging | stdlib `log/slog` | Required by the brief. |
| Templates | stdlib `html/template`, `embed` | Required by the brief. |
| Frontend | Vue 3 global build from jsDelivr, pinned with SRI | Required by the brief. No other frontend library, hand-written CSS. |
| Tests | stdlib `testing`, `httptest`, `go-cmp` (test only) | `go-cmp` gives readable diffs for normalized structs and golden JSON. No assertion framework. |

Direct runtime dependencies: three (`anthropic-sdk-go`, `modernc.org/sqlite`, `BurntSushi/toml`). The exact
versions are pinned at M0 by `go get` of the latest release, and Dependabot is enabled for `gomod` and
`github-actions`.

---

## 11. Build checklist

Each milestone ends with `make ci` green and one commit (or a small PR). Order is chosen so that the agent
loop is proven with the fake provider before any real API or GitHub call exists.

| # | Milestone | Deliverables | Acceptance criteria |
|---|---|---|---|
| M0 | Scaffold | `go.mod`, `cmd/patchbot`, `cli` dispatcher, `version`, Makefile, `.golangci.yml`, CI, LICENSE, README stub, `.gitignore` | `patchbot version` prints version and commit. Unknown subcommand exits 2 with usage. CI passes on both OSes. |
| M1 | Config, secrets, logging | `config`, `secret`, `logging`, `patchbot config check\|show`, `patchbot.example.toml` | Precedence tests pass. A file with `api_key = ...` is rejected. A log line with a known token prints `[redacted]`. `config show` lists each value's source. |
| M2 | Store | `store`, `0001_init.sql`, migrator | Fresh database migrates. Second open is a no-op. A second active run for the same issue returns `ErrActiveRunExists`. Stale-run recovery works. |
| M3 | LLM core and fake provider | `llm` types, errors, retry, pricing, schema, registry, `ModelLister`, `fake` | `Validate` catches each broken invariant. Cost math is exact in micro-USD. Retry honours `RetryAfter` with a fake clock. Anthropic prices filled from the official page with a checked date. |
| M4 | Workspace jail and file tools | `workspace` (local, jail), `tools` for read, write, edit, list, search, registry | Every escape case in 9.1 is refused. Fuzz test runs 30s clean. Tools work on a fixture directory. |
| M5 | Executor and `run_cmd` | `execx`, `run_cmd` tool, `submit` tool | Allowlist table passes. A command that spawns a child and sleeps is fully killed at timeout. A 10 MB output uses bounded memory. The child env contains no secret. |
| M6 | Agent loop | `agent` | All fake-provider cases in 9.2 pass under `-race`. Steps and events are written through interfaces (fakes in tests). |
| M7 | Anthropic adapter | `llm/anthropic`, fixtures, `patchbot models` | Golden tests pass. `patchbot models --provider anthropic` lists live models with a real key, and falls back to config without network. |
| M8 | OpenAI-compatible adapter | `llm/openai`, fixtures from four servers | Golden tests pass, including every quirk row. Works against a local Ollama model with tools. |
| M9 | Gemini adapter | `llm/gemini`, fixtures | Golden tests pass, including synthesized IDs and signature round trip. A two-turn tool exchange works with a real key. |
| M10 | Git workspace | `workspace/git.go`, `gitguard.go`, `detect.go` | Integration tests in 9.4 pass. Token never appears in `ps` output or `.git/config` (asserted). |
| M11 | GitHub client | `github` | `httptest` suite passes. A manual read of a real issue works. |
| M12 | Runner and `patchbot run` | `runner`, `cli/run.go`, prompts, publish, investigate mode, dry run | Full-flow tests pass. `patchbot run --dry-run` on a throwaway repo prints a diff and PR text and pushes nothing. A real run opens a PR and comments. A failing-test repo yields a draft. Exit codes: 0 success, 1 failed, 2 usage, 3 already active, 130 cancelled. |
| M13 | Events, API, SSE | `events`, `server` without pages | API tests pass. `curl -N` on the events URL shows replay then live events and reconnects correctly with `Last-Event-ID`. SIGINT shuts down cleanly and marks runs cancelled. |
| M14 | Web UI | templates, `app.js`, `app.css`, pinned Vue with SRI | Start a run from the browser, watch the live log, cancel it, reload the page on `/runs/{id}` and see the same state. Works with `vue_src = "local"` offline. No console errors, CSP header present. |
| M15 | Hardening and release | README (quick start with the fake provider, security section), `ARCHITECTURE.md`, `SECURITY.md`, coverage floors, `govulncheck`, manual e2e with all three providers | Checklist in `SECURITY.md` ticked against tests. Fresh clone to first dry run in under 5 minutes following the README. Tag `v0.1.0`. |

Rough size: M0 to M6 is the core and is about half the work. M7 to M9 are independent of each other and of
M10 to M11, so they can be done in any order or in parallel.

---

## 12. Risks and open questions

### Risks

| Risk | Impact | Mitigation |
|---|---|---|
| No sandbox in Step 1 | Tests run arbitrary code as the local user | Stated plainly in the README. Scrubbed env, git integrity checks, least-privilege token. Step 4 fixes it properly. Consider making `serve` refuse non-loopback binds entirely until Step 4. |
| Provider API details drift from this plan | Adapter bugs | Each adapter milestone starts by recording real fixtures. Unknown stop reasons map to `StopOther`, never a panic. |
| The model IDs in the brief behave differently from older ones (rejected parameters, thinking defaults) | 400 errors on the first call | Send the minimum parameter set. Thinking and sampling are opt-in per model in config. |
| Local models call tools badly | Loops, invalid arguments, wasted time | Tolerant parsing, error results the model can read, stuck detector, turn cap. The README names tool-calling support as a requirement. |
| Cost estimate differs from the invoice | Budget cap is approximate | Reserve a worst case before each call. Label all costs as estimates. Cap is per run, so the error is bounded. |
| Context growth on long runs | Overflow or high cost | Output caps on every tool, compaction at 70%, prompt caching on Anthropic. |
| Flaky or slow test suites | False drafts, long runs | Baseline run shows pre-existing failures. Test timeout. Draft, not failure, when tests do not pass. |
| Shallow clone hides history the fix needs | Weaker fixes on some issues | `clone_depth` is configurable (0 means full). Default stays 1 for speed. |
| `os.Root` behaviour differences across platforms | A jail gap | Jail tests run on both Linux and macOS in CI, plus the fuzz test. |
| CDN unavailable or blocked | UI does not load | `vue_src = "local"` with a vendored file and `make vendor-vue`. |
| `'unsafe-eval'` in the CSP | Weaker XSS defence | Required by the no-build constraint. Offset by no `v-html`, no inline script, loopback-only by default. A precompiled render function would remove it, at the cost of a build step. |

### Open questions for the owner

Each has a default, so none blocks the build.

1. **Module path and repo name.** Plan uses `github.com/aryanpappu/patchbot`. Confirm the GitHub owner.
2. **Draft when tests cannot be run.** Plan opens a draft when no test command is found. The alternative is
   a normal PR with an "unverified" note.
3. **Salvage rule.** Plan publishes a draft PR when the run hits a limit with changes in the tree. The
   alternative is to push nothing and only comment.
4. **Failure comments.** Plan posts one comment on the issue when a run fails. On a public repo that may be
   noise. A `comment_on_failure = false` switch is cheap to add.
5. **Baseline test run.** On by default. It doubles test time on slow suites.
6. **Dependency installation.** Step 1 allows `npm ci`, `composer install`, `go mod download` through the
   allowlist, which run install scripts with network access on the host. Keep it, or require the owner to
   prepare dependencies some other way until Docker exists?
7. **Gemini SDK.** Plan uses raw HTTP. If the owner prefers the official SDK for the portfolio signal, it
   is a one-package change at M9.
8. **Pricing defaults.** Should the binary ship prices for the four Anthropic IDs (convenient, can go
   stale), or ship none and require them in the user's config?
9. **Repo allowlist.** Should `serve` only accept repositories listed under `[repos]` in the config? Plan
   accepts any repo the token can push to. An allowlist is safer once webhooks arrive, and may be worth
   adding now.
10. **Transcript storage.** Plan stores capped tool inputs and outputs per step, not the full conversation.
    Full transcripts would help debugging and a later "resume" feature, at the cost of database size and
    more data to redact.
