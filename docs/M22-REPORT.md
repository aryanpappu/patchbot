# M22: GitHub webhook and label ingress

Implemented an opt-in `[webhook]` receiver, disabled by default. The configured
secret comes only from `secret_env`, participates in redaction, and must be set
when ingress is enabled. No new Go dependencies were added.

POSTs to the configured path bypass UI bearer and browser-origin checks. The
receiver caps the raw body at 1 MiB and verifies `X-Hub-Signature-256` with HMAC
SHA-256 and constant-time comparison before parsing JSON. Valid, relevant hints
are persisted by delivery ID before acknowledgement; the server's existing
maintenance worker reconciles them at startup, on wake-up, and periodically.
Failures leave receipts pending. Completed delivery IDs remain deduplicated.

Reconciliation checks the allowlist, current repository identity, issue number,
open state, PR shape and issue URL when supplied. Comment hints use the existing
runner comment sync, preserving actor/collaborator, edit, waiting-window,
current-spec and freshness checks. Labels only call `Runner.Start`; they cannot
approve. Existing active-run uniqueness still applies.

Label polling scans open issues in allowlisted repositories using paginated REST
requests. Selection among multiple configured trigger labels is lexical and
stable. A durable observed-label generation and unique run ingress key prevent
poll/delivery races or persistent labels from repeatedly starting completed
runs. An observed removal/change rearms a trigger. A removal and reapplication
entirely between observations is intentionally indistinguishable from an
unchanged label; an operator can explicitly retry. Polling comments remains on
whether or not webhooks are enabled.

Migration 0005 adds durable delivery/label state and the unique run ingress key.
0003 and 0004 are single-comment reservations requested for the separate M20/M21
branches. Their checksummed placeholders must be reconciled with the real
migrations before sharing an already-migrated database with those branches.

Tests cover missing/bad/raw-body signatures, oversize requests, duplicate and
completed deliveries, PR-shaped/non-allowlisted payloads, startup receipt replay,
current-state/reordered hints, missed label/comment delivery recovery, concurrent
poll/webhook starts, trigger rearming, and labels never approving. Existing
approval and revision tests continue to exercise the reused security boundary.

Validation passed: `GOCACHE=/private/tmp/patchbot-m22-gocache
GOLANGCI_LINT_CACHE=/private/tmp/patchbot-m22-lintcache GOMAXPROCS=2
GOFLAGS=-p=1 make ci` (format, vet, lint, all race tests and CGO-free build).
The concurrency settings avoid Xcode/git fixture timeouts in the restricted
macOS environment. The build completed despite a nonfatal warning about writing
module-version metadata to the shared module cache.
No push or live GitHub/provider actions were performed.

## Review fixes

A security review of the ingress found five issues; all are fixed with tests.
Migrations 0001-0005 were already deployed and are unchanged; new state lives
in `0006_webhook_attempts.sql`.

- **Replay/burst amplification (medium).** `reconcileDeliveries` groups pending
  receipts by (repo, issue) and calls `Runner.ReconcileIssue` once per group,
  fetching the repo and issue once and applying comment and/or label
  reconciliation, then marks every receipt in the group processed. The receiver
  refuses new receipts with 503, without recording, once 1000 are unprocessed
  (`RecordDelivery` enforces the cap in the insert itself; known IDs are still
  accepted as duplicates).
- **Label polling cost (medium).** `SyncLabels` reads each repository once per
  sweep, uses the labels already in the open-issue listing (no per-issue
  `GetIssue`), and skips issues whose trigger label is absent and unlatched or
  already handled. A `handled` flag on `webhook_labels` records that the current
  generation already started (or conflicted on its ingress key) or was baselined.
  Sweeps run on a separate `webhook.label_poll_interval` (default `5m`) instead of
  the 60s sync tick. Maintenance errors are now logged (redacted) instead of
  discarded.
- **Mass start on enable (medium).** A repository's first completed sweep marks
  existing trigger labels handled without starting anything and records the
  repository in `webhook_repos`; only later additions/changes start runs. A sweep
  with no labelled issues still records the baseline. Repositories with
  `webhook_labels` rows from 0005 are seeded as baselined by the migration.
- **Endless retry (low).** 404/410 from GitHub means the issue or repository is
  gone; the group is treated as done. Other failures increment `attempts`; a
  receipt is abandoned (marked processed, logged) after 10. Processed receipts
  get `processed_at` and are pruned after 14 days on the sync tick, so delivery
  dedupe is now bounded to that window (redelivery only reconciles current state).
- **Secret whitespace (low).** The webhook secret is trimmed like provider keys.
- **Maintenance blocking (check).** `Runner.Start` on the ingress path cannot
  block for long in the server: admission (`manager.reserve`) fails fast with
  "queue full" rather than waiting, and dispatch only enqueues; the rest is local
  SQLite. The surrounding GitHub reads can be slow (30s per request), so each
  coalesced issue reconcile is bounded at 2 minutes and each label sweep at
  `max(label_poll_interval, 1m)`.

Known edge: a webhook label delivery for a repository that has not yet completed
its first sweep can still start a run; this is a genuine new label event, since
GitHub only sends deliveries for changes after the hook was configured.

Tests: backlog cap and duplicate acceptance, per-issue coalescing (21 deliveries
for 2 issues cost 2 issue reads), give-up after 10 attempts, 404 done, 404/410 in
the runner, first-sweep baseline (with and without labels present), steady-state
sweep request counts, handled/rearm latch and stale-generation marking, prune,
secret trimming and interval validation.
