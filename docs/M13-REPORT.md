# M13 implementation report

The six review corrections and their regressions are documented in
[M13-FINDINGS-REPORT.md](M13-FINDINGS-REPORT.md). That follow-up updates the
admission, Host validation, heartbeat and recovery behavior described below.

Implemented in the `m13-server` worktree without committing. All new code was
written for this milestone. HTTP and SSE use only the Go standard library;
`go.mod` and `go.sum` are unchanged.

## Files

New server implementation and API documentation:

- `internal/server/server.go`: authentication, bind/Host checks, security
  headers, HTTP timeouts, maintenance ticker, serving and graceful shutdown.
- `internal/server/api.go`: providers/models, run list/detail/start, spec edit,
  all five controls, and public artifact downloads.
- `internal/server/json.go`: strict bounded JSON, uniform errors, structured
  credential redaction and safe public run diagnostics.
- `internal/server/manager.go`: configurable concurrency, serialized work per
  run, request-independent contexts, cancellation, draining and shutdown limits.
- `internal/server/broadcast.go`: bounded nonblocking event notifications and
  lagging-subscriber disconnection.
- `internal/server/sse.go`: sequence replay, reconnect/live delivery, polling
  fallback, heartbeat, bounded socket writes and terminal drain.
- `internal/server/README.md`: payloads, filters, response shapes, ownership,
  security and timing contracts for M14.
- `internal/server/fixture_test.go`: fake provider/GitHub fixtures, real SQLite,
  local bare remotes and HTTP response secret checks.
- `internal/server/api_test.go`: endpoint happy/error paths, start through PR,
  spec approval invalidation, revisions/retry, cancellation and pagination.
- `internal/server/security_test.go`: bind refusal, bearer auth on every route,
  cross-origin/Host rejection, response headers and secret scans.
- `internal/server/sse_test.go`: gap/duplicate-free reconnect acceptance test,
  replay across multiple pages, heartbeat, polling fallback, slow clients,
  terminal closure and final worker event drain.
- `internal/server/lifecycle_test.go`: concurrency and per-run ordering,
  cancellation, disconnected browsers, graceful serving/shutdown and timeout,
  GitHub sync/expiry and recoverable queued approved execution.

CLI, runner, persistence and secret integration:

- `internal/cli/serve.go`: serve flags/config, credentials, SIGINT/SIGTERM,
  runner/store wiring and bounded shutdown.
- `internal/cli/serve_test.go`: invalid usage and flag/config bind refusal.
- `internal/cli/cli.go`: serve dispatch/help and injectable lifetime context.
- `internal/cli/cli_test.go`: replace the obsolete serve placeholder test.
- `internal/runner/runner.go`: dispatch/channel/redactor seams, durable start
  admission, safe worker snapshots and optional stage-specific models.
- `internal/runner/lifecycle.go`: dispatch revisions, preserve UI actor/channel
  on controls, audit cancellation before waking workers (including idempotent
  cancels), and classify invalid operator spec input.
- `internal/runner/phases.go`: dispatch Execute after a durable heartbeat claim;
  every original code/approval/publish gate remains in place.
- `internal/runner/README.md`: asynchronous server integration contract.
- `internal/store/events.go`: notifications for committed event writes, outside
  SQLite transactions, with thread-safe subscribe/unsubscribe.
- `internal/store/events_test.go`: committed-event visibility, rollback silence
  and idempotent unsubscribe.
- `internal/store/views.go`: ordered spec/approval history and artifact metadata
  without loading artifact content.
- `internal/store/store.go`: those read contracts and snake_case JSON row tags.
- `internal/store/sqlite.go`: collect transactional events and publish only
  after commit; publish direct event appends after successful persistence.
- `internal/store/records.go`, `internal/store/decisions.go`,
  `internal/store/migrate.go`: use the tracked transaction wrapper without
  changing SQL constraints, CAS rules, migrations or gate checks.
- `internal/secret/redactor.go`, `internal/secret/redactor_test.go`: exact
  credential registration, including short UI tokens, with a regression test.
- `docs/M13-REPORT.md`: this report.

## Decisions where the plan left details open

1. JSON uses snake_case. Start/action responses wrap `run`, spec edits wrap
   `spec`, and detail includes `run`, nullable `latest_spec`, `spec_versions`,
   `approvals`, transcript-free `steps` and public artifact metadata. The full
   request/response contract is in `internal/server/README.md`.
2. Starts, approvals/execution, revisions and retries return 202 with a durable
   run ID; starts/retries also return Location. Reject/cancel and spec edits
   return 200. Invalid input is 400, missing resources 404, lifecycle/active
   issue conflicts 409, large bodies 413, unavailable model listing without a
   fallback 502, shutdown 503, and unexpected failures a generic 500 envelope.
3. POST start accepts `max_cost_micros`, optional common `model`, and separate
   `refine_model`/`code_model`. A stage-specific model overrides the common
   model. The runner resolves defaults and clamps the frozen budget as before.
   PUT spec accepts the existing canonical structured `tools.SpecDraft`.
4. List filters are `repo`, positive `issue`, repeated/comma-separated `status`,
   `limit` (1–500; default 50) and the store's opaque `cursor`. Clients retain
   filters across continuation requests. Providers also return the allowlisted
   repos and safe new-run defaults so M14 can populate its form.
5. The manager owns detached contexts and queues follow-up work serially per
   run, with a shared execution semaphore. It never stores a durable queue.
   Async worker failures are persisted by the runner rather than returned as
   raw upstream errors. CLI runner calls remain synchronous by default.
   Cancellation intents are audited once before signalling the worker, including
   idempotent cancels, so a fast cancellation cannot erase the UI action.
6. Approved execution queued for a slot is persisted as approved/clone with a
   heartbeat before dispatch. This extends existing stale recovery to death
   before actual execution. The exact-spec approval checks still run inside
   execution and before code registration/publication. Reused retry approval
   goes through that same execution gate in the current worker slot.
7. Shutdown rejects new requests/jobs, stops maintenance/SSE, and waits for
   HTTP handlers and admitted work. The CLI allows 30 seconds and then cancels
   owned work contexts. A caller can use another deadline through Shutdown.
8. SQLite subscriptions notify after every committed event, including events
   inside lifecycle transactions. Notifications are wake-up hints, not event
   payload delivery: streams read ordered SQLite batches of up to 500. This
   tolerates reordered callbacks and prevents a replay/live subscription gap.
9. SSE uses 32 notification slots per subscriber, a one-second polling fallback,
   15-second heartbeat comments and a five-second deadline per socket write.
   Overflow disconnects a lagging subscriber. Last-Event-ID wins over `after`.
   Terminal streams drain events from finishing local workers before closing.
10. Downloads allow `diff`, `baseline`, `test_output`, `pr_title`, `pr_body`.
    Verification output comes directly from already-redacted harness steps:
    attempts start at one; omitted attempt selects the latest. Internal
    accounting/warning artifacts are excluded from detail and download access.
11. Authentication compares fixed-size SHA-256 credential digests in constant
    time. Only loopback IP literals and localhost can bind without a token;
    other hostnames require one, avoiding a DNS-based loopback assumption.
    Wildcard listeners accept their actual local connection Host/port. No
    proxy host headers or additional trusted origins are enabled.
12. Strict JSON also rejects null and trailing values. Security/error headers
    apply to every route, including missing routes and authentication errors.
    JSON string redaction preserves number precision and JSON validity; short
    UI credentials are registered at the runner's persistence boundary too.
    Public run errors use a generic message, and provider warnings retain the
    ModelLister's safe fallback description.
13. HTTP read-header/read/idle timeouts are 5/30/60 seconds. No global write
    timeout interferes with SSE; each stream write has its own deadline. SSE
    clears the request read deadline after reading its request.
14. Offline tests use httptest recorders and the repository's httptest server
    over net.Pipe, a fake GitHub client and provider, real SQLite and local bare
    repositories. No live keys or network calls are used.

## Checks

Final command, exit **0**:

```sh
GOCACHE=/private/tmp/patchbot-m13-go \
GOLANGCI_LINT_CACHE=/private/tmp/patchbot-m13-lint make ci
```

Output (build metadata abbreviated):

```text
# fmt-check: clean; no output
go vet ./...
golangci-lint run
0 issues.
go test -race ./...
?   	github.com/aryanpappu/patchbot/cmd/patchbot	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/agent	(cached)
ok  	github.com/aryanpappu/patchbot/internal/cli	1.944s
ok  	github.com/aryanpappu/patchbot/internal/config	(cached)
ok  	github.com/aryanpappu/patchbot/internal/execx	(cached)
ok  	github.com/aryanpappu/patchbot/internal/github	(cached)
ok  	github.com/aryanpappu/patchbot/internal/gitx	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/anthropic	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/fake	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/gemini	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/openai	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/providers	(cached)
ok  	github.com/aryanpappu/patchbot/internal/logging	(cached)
ok  	github.com/aryanpappu/patchbot/internal/runner	135.549s
ok  	github.com/aryanpappu/patchbot/internal/secret	(cached)
ok  	github.com/aryanpappu/patchbot/internal/server	20.858s
ok  	github.com/aryanpappu/patchbot/internal/store	(cached)
?   	github.com/aryanpappu/patchbot/internal/testhttp	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/tools	(cached)
?   	github.com/aryanpappu/patchbot/internal/version	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/workspace	(cached)
?   	github.com/aryanpappu/patchbot/internal/workspace/wstest	[no test files]
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags '...' -o bin/patchbot ./cmd/patchbot
```

The Go build emitted a nonfatal `writing stat cache ... operation not permitted`
warning while trying to cache main-module version metadata under the existing
read-only module cache. The CGO-disabled binary built successfully; no dependency
downloads or module-cache relocation were needed.

`git diff --check` passes. The built `bin/patchbot serve --help` command returns
0 and documents the address/config flags and bearer-token requirement. No live
HTTP listener, provider or GitHub credentials were used by the checks.


## Unfinished

No M13 work is intentionally deferred. The root page is the requested minimal
placeholder; the full HTML/Vue interface remains M14. No live GitHub/provider
smoke test was run, consistent with the offline/no-live-key requirement.
