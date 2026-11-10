These fixtures are **synthetic**, written from the public Generative Language
REST API **v1beta** shapes. They contain no recorded API traffic, real keys,
real prompts, or real model IDs. No network is used to generate or test them.

Protocol references (not fetched during this offline implementation):

- https://ai.google.dev/api/generate-content — generateContent, Content/Part,
  finishReason, promptFeedback, usageMetadata, generationConfig.
- https://ai.google.dev/api/caching#Content — shared Content/Part definitions.
- https://ai.google.dev/gemini-api/docs/function-calling — functionDeclarations,
  functionCall, functionResponse, ordered tool results and optional IDs.
- https://ai.google.dev/gemini-api/docs/thought-signatures — signed continuation
  parts, replayed in their original positions.
- https://ai.google.dev/api/caching#Schema — the shared OpenAPI Schema subset.
- https://ai.google.dev/api/models — models.list pagination and capability filtering.
- https://cloud.google.com/apis/design/errors — google.rpc.Status JSON envelope.
- https://cloud.google.com/php/docs/reference/common-protos/latest/Rpc.RetryInfo —
  google.rpc.RetryInfo.retryDelay (protobuf duration, e.g. `2.500s`).

`*.req.golden.json` are reviewed JSON request bodies; `*.resp.json` are synthetic
responses. `body_cut.resp.json` is intentionally incomplete. `signature.resp.json`
intentionally includes unusual whitespace, an escaped character, exponent
notation, unknown nested fields, and opaque parts. Tests compare full normalized
responses with go-cmp and check exact original part bytes and positions on replay.
Request goldens compare normalized JSON; exact bytes are checked separately.

Update request goldens offline with:

```sh
go test ./internal/llm/gemini -update
```

Decisions at the adapter boundary:

- A configured base URL includes the API version; the default ends in `/v1beta`.
  Model IDs accept either `id` or `models/id`. Redirects are refused. The default
  HTTP client has a two-minute timeout; injected clients retain their timeout.
- Schema conversion uses the existing validated llm subset, recursively strips
  `$schema`, `additionalProperties`, and `default`, and uppercases type enums.
  Numeric bounds and arrays of any supported item type are retained. Unknown
  keywords, unions, and references fail before HTTP. Local tool validation still
  enforces the original schema, including additionalProperties.
- Synthetic call IDs are `call_<assistant-turn>_<call-index>` (zero based).
  Explicit IDs are reserved first; deterministic underscores avoid collisions.
  Repeated function names without IDs are allowed only with the normalized
  transcript's exact ID/name/order correlation. Duplicate explicit IDs, including
  reused historical IDs, fail as malformed. Swapped or mismatched results fail
  before HTTP. Synthetic IDs are never added to originally ID-less wire calls or
  responses; actual wire IDs are echoed in both.
- ProviderMeta holds the entire raw part when metadata is present, and for every
  function call (to remember whether it had an ID). Unknown parts remain opaque.
  Text parts marked `thought:true` also remain opaque, so thought summaries are
  excluded from answer text and from requests to other provider instances.
  Only the originating instance replays its metadata. Replay rejects changed
  normalized text/call fields. In-memory raw bytes, including signatures and
  unknown fields, are copied directly into requests, bypassing JSON compaction.
- Only STOP permits executable function calls. Calls under MAX_TOKENS, refusals,
  MALFORMED_FUNCTION_CALL, OTHER, or future finish reasons remain opaque; the
  terminal stop reason wins. Invalid completed arguments fail as malformed,
  rather than substituting executable empty arguments. Missing args become `{}`.
  Blocked prompts return a refusal even if candidates were supplied. Empty STOP
  responses and multiple candidates are malformed; empty refusals are allowed.
- Usage is unknown (nil) unless both prompt and candidate counts are present.
  Missing cache/reasoning fields mean zero; reasoning is added to output, cached
  input is subtracted from prompt input. Invalid/overflowing counts are malformed.
  The shared Usage type has no separate reasoning-token field.
- HTTP status controls error classification; RPC message text is inspected only
  to recognize context overflow. Raw API diagnostics are never exposed, because
  they can echo secrets or request bodies. Transport/decoder causes are retained
  behind llm.Error.WithCause and llm.SafeText remains safe. RetryInfo and the
  Retry-After header use the longer positive delay for all retryable error kinds.
  Invalid error JSON retains its HTTP classification and any Retry-After delay;
  invalid success JSON is malformed. Early body-read failures are unavailable.
  Every response body (including errors) is capped at 10 MB. Pagination cycles
  fail explicitly.

Limits: these tests establish the adapter's wire contract without live API/model
validation. No model capabilities are inferred beyond models.list's
`generateContent` method. Multimodal fields can be preserved opaquely, but this
adapter only constructs normalized text and tool parts. Raw-part byte fidelity
assumes the in-memory transcript; an external JSON serialization of ProviderMeta
may compact it. No integration with the concurrently developed Anthropic adapter
is attempted here.
