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
