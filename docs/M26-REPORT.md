# M26: Create issue from the dashboard

Write a feature request in the dashboard; patchbot opens the GitHub issue and
can start the refine run. No new Go dependencies; tests use an in-memory fake
GitHub only.

## What changed

- **API** `POST /api/issues` (`internal/server/issues.go`), behind the existing
  auth (Access JWT or bearer), cross-origin check and 1 MiB body limit.
  Fields: `repo`, `title`, `body`, `start_run`, `idempotency_key` (required,
  8-128 of `[A-Za-z0-9_-]`, a UUID from the UI) and, when `start_run`, the same
  run options as `POST /api/runs` (`mode`, `provider`, `model`, `refine_model`,
  `code_model`, `dry_run`, `max_cost_micros`).
  - Validation: repo must pass `Server.repoAllowed` (the single allowlist
    accessor, also used by `POST /api/runs`, so M24's registry replaces one
    function); title 1-256 runes after trim with no control characters at all;
    body at most 64 KiB (CRLF normalised) with no control characters except
    newline and tab; valid UTF-8; no `<!-- patchbot` marker in title or body
    (blocks spoofing the marker).
  - Response `201` (created now) or `200` (replay):
    `{"issue":{repo,number,node_id,html_url,requested_by,created},"run":{...}}`.
- **GitHub** (`internal/github/issues.go`): `CreateIssue` and `ListIssuesSince`
  on `HTTPClient` through the same `request` path as every other call, so the
  credential is the App installation token for that repository or the PAT.
  Exposed to the server as `runner.IssueWriter()` (optional interface; returns
  `ErrGitHubNotConnected` in setup mode). Title and body are mention-neutralized
  like all other patchbot text. `Issue` now decodes `node_id`.
- **Attribution.** GitHub shows the bot or PAT user as author. The body ends with
  `---`, a visible line "Created from the patchbot dashboard." (no actor or email:
  the issue is public) and the hidden
  `<!-- patchbot:issue-created v1 key=KEY h=HASH16 -->`, where HASH16 is a prefix of
  the request hash. The actor, issue number and
  node ID are stored in `dashboard_issues`; `GET /api/runs/ID` adds
  `dashboard_issue.requested_by`. patchbot never trusts an issue by its author,
  and comment commands keep the existing marker/`posted_comments` rules (the
  marker is in the issue body, not a comment).
- **Migration** `0011_dashboard_issues.sql`: `idempotency_key` unique,
  `request_hash`, `repo`, `actor`, `state` (`pending`/`created`), `issue_number`
  (unique per repo), `issue_node_id`, `issue_url`, `run_id`, timestamps.
  `0010_reserved_m24.sql` is an empty placeholder so numbering stays contiguous
  on this branch (see Open issues).
- **Idempotency and the crash window.**
  1. The pending row is inserted before GitHub is called. Same key + different
     content is `422 idempotency_conflict`.
  2. A created row is returned as is (`200`), never creating a second issue.
  3. A pending row younger than 2 minutes means another request may still be in
     flight: `409 issue_creation_in_progress`. An older one is atomically claimed
     by one retry, which lists recent issues (any state, up to 300) for the exact
     marker. A candidate counts only if its author is patchbot's own login
     (`<slug>[bot]` in App mode, the `/user` login in PAT mode, via
     `Runner.BotLogin`) and its title and body equal exactly what this request
     produces, marker included; a copied marker in someone else's issue is
     ignored. Two or more matches is `issue_state_unknown`. One match: the row is
     completed, nothing is created.
     A complete search that finds nothing: the issue is created. A failed or
     truncated search: `502 issue_state_unknown` ("may have been created, check
     the repo"), never a blind create.
  4. A GitHub 4xx, a missing credential or a token-mint failure certainly created
     nothing, so the pending row is deleted and the key can be reused at once. A
     5xx, transport error or timeout leaves the row pending (outcome unknown).
  5. The GitHub call and bookkeeping use a context detached from the HTTP client,
     so a disconnecting browser cannot abandon them midway.
- **Run start.** `start_run` calls `Server.startRun`, extracted from the
  `POST /api/runs` handler together with `validStartOptions`; there is no second
  lifecycle path. The run is created with channel `ui` and the dashboard actor,
  lands in `awaiting_approval` and is never approved. The run is created under the
  unique ingress key `dashboard-issue:<key>` (the existing `runs.ingress_key`
  mechanism). A concurrent or later retry gets `ErrConflict` from the store,
  fetches the existing run by that key and links it; a link failure is returned,
  not ignored. This holds even if the first run already finished. If the run cannot start the response carries both the error and the
  `issue`, and the same key resumes.
- **Errors** (fixed messages; GitHub bodies and tokens never relayed):
  `github_not_connected` 409; `github_permission_denied` 403 (GitHub 401/403/404
  all map here, never 401, which the UI reads as "sign in again"); `issues_disabled`
  422 (GitHub 410); `github_rejected` 422; `github_rate_limited` 429;
  `github_unavailable` 502; `issue_state_unknown` 502; `not_supported` 501.
- **UI** (`/runs/new`): a "Write a new request" toggle (`aria-pressed`) with repo,
  title, request text, "Start refine now", and the existing mode/provider/model/
  budget/dry-run controls when checked. The key stays the same while the content
  is unchanged and renews when it changes. The created issue is linked through
  `link()` (https only) and the page navigates to the run after 1.5 s. Render
  functions, text nodes and CSP are unchanged.

## Tests

`internal/server/issues_test.go` (all under `-race`): marker, footer, actor and
row; idempotent retry and key reuse conflict; six concurrent identical requests
create once; crash window recovery by marker, in-progress guard, create-when-
nothing-found, unverifiable search never creates; `start_run` through the run
path ending in `awaiting_approval` with no approvals, linked run and no second
run on retry, requester in run detail; resume after an earlier failure; repo
allowlist; validation table and boundaries; safe GitHub error mapping and
immediate retry after a definite failure; `github_not_connected`; unsupported
client; auth, wrong token, cross-site refused; body limit; Cloudflare Access email
recorded in the footer, row and run actions; hostile text stays inert data (JSON,
nosniff, not in the page shell). Plus `internal/github/issues_test.go`,
`internal/store/dashboard_issues_test.go` and `TestWriteRequestFormUsesSafeSinks`
in `internal/web`. No tokens appear in any response (the fixture asserts it).

## Open issues

- **Migration 0010 placeholder.** M24 owns 0010. At merge, delete
  `0010_reserved_m24.sql` and keep M24's real file; 0011 follows unchanged.
  Also swap `repoAllowed` for M24's registry lookup.
- The actor email is never written to GitHub; it lives in `dashboard_issues`,
  `approvals`/`actions` and the run detail only.
- **Marker search window** is 300 issues updated since shortly before the pending
  row; in a very busy repo it reports `issue_state_unknown` instead of guessing.
- **Eventual consistency.** The marker search uses the issues list (not the
  search API), which is read-after-write consistent, but a pending row stays
  unrecoverable for 2 minutes after an unknown failure by design.
- **Run start error after issue creation** with an existing failed run row
  returns the run (failed) with 200/201 rather than an error.
- No labels or assignees are set; no edit/close of dashboard issues.
- UI was not checked in Chrome on Mumbai (no browser or network here); the form
  is covered by the static sink test only.
