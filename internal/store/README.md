The store implements the persistence boundary for the runner and HTTP server.
`Open` creates a 0600 on-disk file, applies embedded migrations, and configures
WAL, foreign keys, a five-second busy timeout, NORMAL synchronization, and
immediate transactions on every connection. Each SQLite handle has one writer
connection; multiple processes can use the same file.

`Store` groups run lifecycle operations, execution records and SSE replay,
versioned specs and decisions, questions, actions, and artifacts. Row structs
serve as inputs and outputs: the store assigns IDs, sequences, versions, and
missing creation timestamps. Optional counts, cost, and times are pointers;
NULL is unknown and an explicit zero remains a known zero. Times are UTC at
millisecond precision. Costs are integer micro-USD.

Decisions where the plans do not specify an exact contract:

- Run pagination uses an opaque `(created_at, id)` keyset cursor, newest first,
  with a default page size of 50 and a maximum of 500. Continue with the same
  filters. Event replay uses ascending per-run sequence numbers with the same
  page-size defaults. Empty collections are successful reads; missing singular
  reads return `ErrNotFound`.
- `SetRunStatus` compares one expected status, updates phase and optional
  metadata, manages lifecycle timestamps, and appends `run.status` atomically.
  A same-status transition can update phase. The runner enforces the legal
  transition graph. `Heartbeat` only touches running runs.
- `FailStaleRuns` takes an absolute cutoff, uses a strict `<` comparison, and
  ignores NULL heartbeats. It fails running runs with `error_kind=interrupted`
  and appends their events in one transaction. It never expires waiting runs.
- The caller supplies the SHA-256 hash of its canonical spec representation.
  `SaveSpec` appends an immutable version (zero requests automatic numbering;
  a supplied version must be the next one). Only the latest version can have
  a valid approval, even if a new version has the same hash. The latest decision
  wins, so revise/reject also revoke an earlier approve. Saving a spec while
  approved moves the run back to awaiting approval and emits an event atomically.
- Specs retain history without a separate mutable superseded flag. The maximum
  version defines the current spec. Decisions are immutable audit records.
  Actor authorization, issue freshness, revision limits, expiry, and transitions
  after a decision belong to the runner. A valid stored approval alone does not
  authorize coding: the runner must also check status and issue freshness.
- `RecordStep` allocates a sequence for a new step; a nonzero sequence replaces
  an existing record. Run totals are recomputed from completed LLM/test steps
  in the same transaction, so finishing a step twice cannot double count. If
  any completed LLM step has unknown usage or price, that total stays NULL.
- Step input/output caps are 16/64 KiB, with UTF-8 boundaries preserved; error
  messages are capped at 2000 bytes. Callers redact secrets before every write
  and cap artifacts at their source. Artifact saves replace the same
  `(run_id, kind, attempt)`; question updates only change answer metadata.

The schema uses STRICT tables, CHECK constraints, foreign keys, and a partial
unique index covering all five active statuses, including `approved`. The merged
plan's phases (`code`, without `agent` or `spec`) take precedence. Migration
checksums and names are checked on every open; a binary refuses unknown versions
and a failed migration rolls back its DDL and ledger entry together. New schema
changes belong in new numbered migration files, never edits to shipped files.
