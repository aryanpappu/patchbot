# Local server

`patchbot serve [--addr 127.0.0.1:8080] [--config FILE]` serves a placeholder
page and the M13 API. It uses `net/http`, `encoding/json` and ordinary HTTP
writes for SSE. The caller owns the SQLite store. Call `Shutdown` before
closing it. The command handles SIGINT and SIGTERM and allows 30 seconds for
shutdown, then cancels remaining work.

Without `PATCHBOT_UI_TOKEN`, only loopback IP literals and `localhost` can
bind. With it, **every** request requires `Authorization: Bearer <token>`,
including the page, SSE and artifact downloads. Authentication compares
SHA-256 digests in constant time. No token is rendered in the page or logs.
Every request, including GET and SSE, must use the configured hostname or the
bound numeric address with the bound port. Loopback aliases are accepted only
for loopback binds. On wildcard listeners the actual connection's local numeric
address is accepted. Non-GET requests also pass `http.CrossOriginProtection`.
A reverse proxy must preserve an accepted Host. No trusted origins or
forwarded-host headers are configured.

Responses use snake_case JSON. Errors have the shape
`{"error":{"kind":"invalid_request","message":"..."}}`. Bodies are limited
to 1 MiB; unknown fields, JSON null and trailing JSON values are rejected.
The server masks registered credentials and token-shaped text before output.
Run error messages use a generic public description; local records retain
redacted diagnostic details. There are no external JavaScript dependencies.

## API contract

- `GET /api/providers`: `providers` (`id`, `kind`, `default_model`, `available`),
  allowlisted `repos`, and new-run `defaults`. No provider URLs or credentials.
- `GET /api/providers/{id}/models`: `models` (`id`, `display_name`, `source`),
  `live`, `warning`, `fetched_at`. Uses `llm.ModelLister`, including its five
  minute live cache, configured-model merge and warning on fallback. A failed
  listing with no configured fallback returns 502.
- `GET /api/runs`: `runs`, `next_cursor`. Filters: `repo`, positive `issue`,
  repeated or comma-separated `status`, `limit` (1–500; default 50), `cursor`.
  Reuse the same filters with each opaque continuation cursor.
- `POST /api/runs`: `repo`, positive `issue`, optional `mode`, `provider`,
  `model`, `refine_model`, `code_model`, `max_cost_micros`, `dry_run`.
  The stage-specific model overrides `model`; normal runner defaults and
  hard budget caps apply. Returns 202, `{"run":...}` and a Location header.
- `GET /api/runs/{id}`: `run`, nullable `latest_spec`, ascending `spec_versions`,
  `approvals`, `steps` (summaries without input/output transcripts), and public
  `artifacts` (`kind`, `attempt`, `bytes`, `created_at`).
- `PUT /api/runs/{id}/spec`: a canonical `tools.SpecDraft` object with
  `summary`, `acceptance_criteria`, `files` (`path`, `change`), `risks`,
  `test_plan`, `open_questions`. Returns `{"spec":...}`. Runner content/path,
  issue freshness, revision and generation checks remain authoritative.
- `POST /api/runs/{id}/approve`, `/reject`, `/cancel`: empty body or `{}`.
  Approve returns 202 and dispatches execution; reject and cancel return 200.
- `POST /api/runs/{id}/revise`: `{"notes":"..."}`, returns 202.
- `POST /api/runs/{id}/retry`: empty body, `{}`, or optional `provider` and
  `model`, returns a **new** run with 202 and a Location header.
  Every action response contains `run`. UI actions use `approval.local_actor`
  and channel `ui`; synced GitHub decisions retain their GitHub provenance.
- `GET /api/runs/{id}/artifacts/{name}`: `diff`, `baseline`, `test_output`,
  `pr_title`, `pr_body`; always text/plain with nosniff. `attempt` defaults
  to zero for stored artifacts. Harness verification output is read from
  already-redacted steps, with attempts numbered from one; omitting attempt
  selects the latest verification output. Internal accounting is not exposed.
- `GET /api/runs/{id}/events`: SSE with per-run sequence IDs, event type and
  JSON data. Nonempty `Last-Event-ID` takes precedence over `?after=`.
  Subscribe before replay to avoid a replay/live race. SQLite is authoritative:
  notifications wake a store read, and polling every second recovers missed
  notifications or an unavailable broadcaster. Heartbeats are comments every
  15 seconds. A terminal stream drains finishing worker events before closing.

## Work and stream ownership

The manager defaults to two running jobs (`server.max_concurrent_runs`) and
sixteen pending jobs (`server.max_queued_runs`, a positive integer). Waiting
jobs occupy a bounded list and do not allocate worker goroutines or occupy
execution slots. Admission reserves queue capacity before creating a run or
recording an approval/revision. A full queue returns HTTP 429 with error kind
`queue_full` and message "pending run queue is full; retry when capacity is
available", without changing the run or its decisions. Cancellation releases
pending capacity immediately. Follow-up jobs for one run execute
serially. Work is detached from request cancellation; only the Cancel action
or the shutdown deadline cancels its owned contexts. Start, revise, execute,
retry and GitHub-synced work all use the same runner and dispatch boundary.
All admitted jobs carry durable ownership from admission, and one manager
ticker refreshes their heartbeats throughout queuing, execution and shutdown
draining. Approved execution waiting for a slot keeps the approved status and
clone phase for the ordinary gate. An approval denial resets the phase to
refine, allowing reapproval to dispatch again. Approval and the asynchronous
execution claim are saved in the same transaction after capacity is reserved.
There is no durable worker queue or automatic restart of interrupted work.

One stopped-on-shutdown maintenance ticker calls `RecoverStale` and
`SyncWaiting`, including waiting-run expiry, every 60 seconds. The same stale
recovery runs at startup. A fresh orphan preserved on a quick restart is failed
once its heartbeat expires, releasing its issue lock so it can be retried.

Each SSE subscriber has 32 notification slots. Overflow disconnects that
subscriber; write deadlines also disconnect a blocked socket within five
seconds. Neither path blocks event persistence. Reconnect with the last event
actually consumed. HTTP read-header/read/idle timeouts are 5/30/60 seconds;
there is no global write timeout, and SSE uses a deadline per write instead.

Tests use httptest recorders, the repository's httptest server over net.Pipe,
a fake provider/GitHub client, real SQLite and local bare git repositories.
They require no network or live keys.
