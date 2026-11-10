# GitHub REST boundary

`New(httpClient, baseURL, tokenSource)` returns `(*HTTPClient, error)`; it
implements `Client`. An empty base URL uses GitHub.com. Enterprise URLs may
include `/api/v3`. The build's `internal/version.Version` supplies the user agent.
Settings are immutable after construction; token sources must support concurrent
calls. No dependencies were added.

The runner can use `ListIssueComments(ctx, repo, number, since)` for ordinary
listing, or `ListIssueCommentsSince(..., since, etag)` for conditional polling.
Both follow `Link` headers and stop at 300 comments. Only single-page results
return an ETag: the first page's ETag cannot validate subsequent pages. The runner
owns polling intervals, persistent cursors, deduplication and any continuation
needed when an issue has more than 300 matching comments. `since` filters by
GitHub's **updated** time, so timestamps must still be checked for edited commands.

`GetIssueComment` supports the runner's spec-rendering comparison. `GetBranch`
returns `(Branch, exists, error)`, and `BranchExists` is available for callers
needing only the boolean. `FindOpenPR` returns a nil pointer when no PR matches
the repository owner's head branch. A collaborator 404 returns `(false, nil)`;
other missing resources return `*ErrNotFound`.

Errors wrap operation names. Use `errors.As` with pointers to `ErrNotFound`,
`ErrUnauthorized`, `ErrForbidden`, `ErrRateLimited`, or `ErrValidation`.
Other statuses return `*APIError`. Each status error exposes `StatusCode` and a
bounded, redacted `Message`. `ErrRateLimited.Reset` uses the later valid reset or
retry time when both are supplied, and zero when neither is usable. Rate-limit
detection includes secondary 403 responses. The client does not retry writes;
the caller decides when retrying is safe. See [GitHub's rate-limit guidance](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).

Request deadlines cover token acquisition, transport and response reads (30 s,
or an earlier caller/client deadline). Success responses are capped at 10 MiB;
error bodies at 1 KiB. Token-source, transport and decoder error details are
discarded because they can echo credentials; context cancellation remains
inspectable with `errors.Is`. The current token and its URL/base64 forms are
redacted in API error messages, including truncated token prefixes. Redirects
are refused; pagination must stay on the same origin and endpoint. The supplied
`http.Client` is copied rather than modified.

Comment parsing is separate from authorization. `ParseCommand(Comment)` accepts
only the first nonempty line, ignoring case and leading whitespace. `/approve`
and `/reject` must occupy that line alone. `/revise` and `/answer` require notes,
which can start on the following line. Notes preserve subsequent line structure,
with CRLF normalized to LF. Quoted and fenced commands are ignored.

`EligibleCommand(comment, approvers, isCollaborator, botLogin)` takes the bot
login explicitly: the three inputs in the task's sketch cannot establish the
bot's identity. Missing bot identity, missing timestamps or callback, an edited
comment, an unlisted author, or a self-authored comment fail closed. The callback
must return false on lookup errors. It can capture the context and repository
when adapting `Client.IsCollaborator`. This uses a live collaborator check,
as required by `PLAN.md`, rather than trusting `author_association`. The runner
still enforces the waiting state, comment ordering, current spec hash and issue
integrity, and calls `ParseCommand` separately.

Outgoing comments and PR titles/bodies automatically pass through
`NeutralizeMentions`. It preserves email addresses and Markdown code spans and
fences, and is idempotent. General secret redaction remains the harness's
responsibility before calling the client.

Tests use Go 1.27's `httptest.NewTestServer`, whose in-memory network exercises
HTTP without requiring sandbox permission to bind loopback ports.
