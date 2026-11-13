# Auth review: Codex round 1 (2026-11-13)

Claude has the right destination but understates both the pilot bug and the migration.
“Zero code” is not the same as “works for this owner.”

## Options A–D
- **A — agree: reject.** `runner.Token` shares one credential across REST and Git;
  `config.Repos` restricts legitimate scheduling, not what a stolen credential can access.
  A classic `repo` PAT gives this shared host an unnecessary account-wide blast radius.
- **B — qualified agreement for a bounded pilot.** Selected repos and a short expiry are
  sensible; `runner.New` already loads `GITHUB_TOKEN`. But a fine-grained PAT targets one
  resource owner, not arbitrary repos across owners. Do not promise universal coverage.
  More seriously, `github.CurrentUser` returns the PAT owner's login and `sync.go` passes
  it to `EligibleCommand`, which rejects that author's commands. If aryanpappu's account
  is both token owner and sole approver, its `/approve` never counts. Do not spoof
  `BotLogin` to bypass this separation.
  A separately authenticated approval channel or distinct bot account is required today.
  Claude's universal “max 1 year” claim is too strong: expiry is policy-dependent ([PAT docs][pat]).
- **C — agree: reject as the default.** `config.Repo` has no credential field, and
  `TokenSource.Token(ctx)` has no repo argument. This needs routing changes, not just a
  SQLite column. Per-token scope helps with one leaked token; service compromise exposes
  all tokens and the encryption key. N manual rotations buy little here.
- **D — agree as the target, disagree that the existing seam makes it straightforward.**
  `TokenSource` is concurrency-safe but repo-blind; `runner.New` builds one global REST
  client. Single-repo tokens need explicit repo-bound clients/sources for runs AND polling,
  comment sync, preflight and future issue creation. Never use a mutable “current repo.”
  Installation tokens expire after one hour and can be narrowed by repo IDs ([App docs][app]).
  The private key remains a long-lived credential able to mint across the App's installations.
  PR/comment identity improves, but commits do not automatically become `patchbot[bot]`:
  `publish.go` passes `gitx.Author{}`; `gitx.CommitAll` hardcodes patchbot's name/email.

## Risks and requirements Claude missed
- **Shared-host key custody:** an env private key is not a security boundary against root,
  same-UID compromise, debugging or dumps. Preserve Step 2's dedicated Unix user and rootless
  executor; keep the key outside workspaces, SQLite, backups and containers. Prefer a protected
  service credential file over a widely inherited env value; document the remaining host trust.
  Single-repo tokens limit token theft, not theft of the signing key on the same host.
- **Expiry/cache:** `phases.go` obtains a token once for `CloneOptions.Token`; `gitx.Repo`
  retains it until `Close`. REST calls reacquire via `http.go`, but push reuses the old token.
  Cache in memory by installation + immutable repo ID + permission set, coalesce concurrent
  minting, refresh before expiry with skew margin, and acquire fresh credentials per Git
  network operation. Cover resumed runs and waiting-window sync, not just one-hour coding.
  Invalidate on revocation/auth failure; bound retries and never blindly replay external writes.
- **Identity/approval:** installation auth must not depend on `/user` (`identity.go`). Resolve
  and verify App bot identity separately; preserve allowlisted approver + live collaborator +
  unedited-comment checks in `commands.go`/`sync.go`. Membership errors must fail closed.
  M23's verified email actor is a separate authorization path, not a GitHub login mapping.
- **Private repos already have a transport gap:** `gitx.Push` does unauthenticated `ls-remote`;
  `CheckoutSHA` may do unauthenticated fetch. A valid PAT/App token does not fix those failures.
  Extend narrowly scoped network auth deliberately, preserving argv secrecy and host/URL guards.
- **Rate limits:** `github/errors.go` classifies limits and reset times; runner/server have no
  corresponding GitHub backoff. `SyncWaiting` uses nonconditional comment listing despite the
  ETag API. Per-repo tokens do not create independent installation quotas. Bound polling/minting,
  honor Retry-After/reset with jitter and cancellation, and distinguish limits from revocation.
- **M24 validation:** after M23 authorization, accept canonical owner/name, not arbitrary URLs.
  Verify installation ownership, selected immutable repo ID, granted/token permissions, default
  branch, issue availability and actual Git read access. Public `GetRepo` success alone proves
  little; `Permissions.Push` in current preflight is not an App permission model. Verify contents,
  issues and PR write grants plus metadata read for collaborator checks ([endpoint docs][collab]);
  report ruleset/workflow restrictions honestly without a destructive test push. Fail closed
  before saving; audit actor/result without secrets, recheck at run/publication time, and define
  repo removal/edit effects on active runs. GitHub installation changes remain a separate action.
- **Storage/rotation:** M24 stores repo policy in SQLite, not credentials. Current config secrets
  are env-backed; encrypted SQLite adds key custody, WAL/backups and restore complications.
  Keep only credential references/App IDs in DB. `runner.New` registers one initial token with
  the redactor: every minted/replacement token and encoded Git form needs equivalent protection.
  PAT replacement currently needs restart/reconstruction; schedule expiry alerts and a tested
  rotation runbook. App key rotation needs overlap, mint verification, old-key removal, cache
  invalidation and explicit outstanding-token revocation handling after compromise.

## Final recommendation and acceptance criteria
Use B only for the small pilot with an explicitly proven approval path and scheduled rotation.
Build D as a prerequisite to enabling M24 broadly; keep PAT mode explicit for local/dev, never
silently fall back to a broader PAT after App denial. M23 login does not grant GitHub access.
M25 images must never receive credentials; M26 needs issue-create API support (absent from
`github.Client` today), issues-write permission and audited authenticated actor attribution.
Accept D only after tests prove concurrent cross-repo isolation, expiry between clone/push/PR,
revocation and key rotation, correct App identity and owner approval, private-repo publication,
rate-limit recovery, denied M24 saves and zero secrets in logs/artifacts/DB/container env.
Preserve `publish.go`'s approval rechecks before push and PR creation; test ambiguous PR failure
without duplicate writes. Run `make ci` and a disposable-repo end-to-end drill before rollout.

[app]: https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation
[pat]: https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens
[collab]: https://docs.github.com/en/rest/collaborators/collaborators
