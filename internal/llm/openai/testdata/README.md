# Synthetic Chat Completions fixtures

Every response and request in this directory is synthetic. They contain no
recorded prompts, real model output, credentials or production request IDs.
`fixture-model` is an invented model ID, not a shipped model default.

The protocol baseline is the non-streaming Chat Completions mapping in
`docs/PLAN.md` section 3 and `docs/planning/PLAN-fable.md` section 3.4.
The API documentation shapes used are:

| Family | API prefix/version | Documentation |
| --- | --- | --- |
| OpenAI | `/v1`, Chat Completions and Models, unversioned REST schemas | https://platform.openai.com/docs/api-reference/chat/create and https://platform.openai.com/docs/api-reference/models/list |
| OpenRouter | `/api/v1`, OpenAI-compatible Chat Completions and Models | https://openrouter.ai/docs/api-reference/chat-completion and https://openrouter.ai/docs/api-reference/list-available-models |
| Ollama | `/v1`, OpenAI compatibility API; no Ollama release pin | https://docs.ollama.com/api/openai-compatibility |
| Groq | `/openai/v1`, Chat Completions and Models | https://console.groq.com/docs/openai and https://console.groq.com/docs/api-reference |

These are protocol fixtures, not assertions of current server/model support.
No documentation or service was fetched when implementing this offline task.
There is no dated OpenAI API-version header. Responses API payloads are outside
the adapter's scope. LM Studio uses the same local `/v1` compatibility profile
as Ollama; it has no separate response fixture family.

Each family has text, one-call, two-call, mixed text/call, result-continuation
and model-list fixtures. Request goldens verify `system` role, JSON-string
function arguments, a null assistant content with calls, tool schemas,
temperature, output cap naming, and two separate `tool` messages in provider
order. The first result is empty and the second carries the `ERROR: ` prefix.
Ollama responses exercise object-valued arguments and unknown usage.
OpenRouter exercises a tool call with `finish_reason: stop` and extended model
metadata. Groq exercises the profile omitting `parallel_tool_calls`.

Golden tests use Go 1.27's `httptest.NewTestServer` with its default in-memory
network. They do not listen on sockets or call external services. Update only
requests, with an explicit flag:

```sh
go test ./internal/llm/openai -update
```

Response fixtures are maintained by hand. Malformed envelopes, oversized and
interrupted bodies, HTTP errors, cancellation, redirects, cursor limits, ID
collisions and credential echoes have additional inline/transport tests. The
shared `.resp.json` fixtures cover malformed arguments, a partial call at the
token limit, refusal, content filtering, legacy function calls, missing usage,
HTTP statuses and two model pages.

## Compatibility choices and limits

`openai.New` is the registry constructor. `NewWithOptions` also accepts an
`HTTPClient` and a `*Quirks` override. Settings are copied per instance. The
existing shared/TOML config has no quirks fields, so this milestone does not
change that contract. Default URL profiles are:

| Host | Output cap | Parallel field with tools |
| --- | --- | --- |
| `api.openai.com` | `max_completion_tokens` | `true` |
| `openrouter.ai` and other unknown hosts | `max_tokens` | `true` |
| `api.groq.com` | `max_tokens` | omitted |
| `localhost`, `127.0.0.1`, `::1`, `ollama`, `lmstudio` | `max_tokens` | omitted |

An override replaces the detected profile; an empty output-cap field means
`max_tokens`. Local servers behind other hostnames need an override if they
reject the parallel field. Omission also supports servers that ignore it;
several returned calls are always retained in order. The adapter does not
retry or probe alternative parameters, silently remove tools, or switch models.

* Invalid nonempty arguments return `llm.ErrMalformed`. Empty, missing or JSON
  null arguments become `{}`; JSON strings and object arguments are accepted.
  The older plan's proposed recovery via `{}` plus a tool error is unsupported
  by the current normalized contract: it has no argument-error field. Replacing
  malformed input with an executable empty object could run the wrong action.
* `length` takes precedence over tool calls and suppresses every call, even if
  its arguments look complete. It returns `StopMaxTokens` and any text. Refusal
  and content filtering likewise suppress calls and return `StopRefusal`.
* Modern calls with `stop` still normalize to `StopToolUse`. Legacy
  `function_call` responses are converted to modern normalized calls, which
  are sent back using `tool_calls`/`tool` rather than the deprecated protocol.
* Missing/repeated IDs are repaired deterministically using transcript length,
  call position and a collision suffix. Supplied IDs are reserved before
  synthesis; history IDs are never reused. Calls retain provider order.
* Missing, null or incomplete usage is unknown (`nil`), not zero. Known usage
  subtracts cached input from prompt tokens; completion tokens already include
  reasoning tokens. Negative/inconsistent known counts are malformed.
* The optional `has_more`/`last_id` pagination extension is synthetic. It uses
  the same `/models?after=...` endpoint, escapes cursors, deduplicates models,
  rejects loops and caps discovery at 100 pages under one two-minute context.
  Unpaginated `data` lists work for all four families. Arbitrary provider links
  or other pagination dialects are not followed. Config fallback and caching
  remain in `llm.ModelLister`.
* Own-instance continuation metadata is rejected because this adapter cannot
  replay it faithfully. Foreign opaque blocks are dropped; text/calls remain.
  Audio, image, streaming and custom (non-function) tools are unsupported.
* HTTP errors contain fixed diagnostic text. Raw bodies, URLs and decoder or
  transport messages are never printed; causes are retained with `WithCause`.
  Known finish reasons are retained in `RawStop`; unknown reasons map to
  `StopOther` with an empty `RawStop` to avoid logging arbitrary body text.
  Request IDs come only from a bounded, sanitized `X-Request-ID` header.
* Base URLs must include their API prefix. Keys are optional, URLs with userinfo,
  queries or fragments are rejected, and all redirects are refused. Responses
  are capped at 10 MiB (including error bodies). There is a two-minute request
  deadline, further constrained by the caller and injected HTTP client.

Live conformance, model-specific capability detection, custom OpenRouter
headers, `developer` system role, other pagination dialects and TOML quirks
settings remain outside this milestone. No network smoke test was run.
