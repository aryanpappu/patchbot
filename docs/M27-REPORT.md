# M27: GitHub App authentication

Implements decision 2 of `docs/planning/auth/DECISION.md` and the requirements
of `docs/planning/auth/codex-round1.md`. No new Go dependencies.

## What changed

- **Config.** `[github] auth = "pat" | "app"` (default `pat`, behaviour
  unchanged; `PATCHBOT_GITHUB_AUTH` overrides). App mode needs `app_id` and an
  absolute `private_key_file` outside the data directory; `installation_id` is
  optional (0 resolves per repository with `GET /repos/{o}/{r}/installation`).
  `config show` prints the section with sources; `private_key_file` is exempt
  from the secret-looking-key check because it holds a path, while an inline
  `private_key` value is still refused.
- **Key and JWT** (`internal/github/appkey*.go`). The key file must be a regular
  file (symlinks refused, opened with `O_NOFOLLOW`, re-checked with `SameFile`),
  mode 0600 or stricter, owned by the effective user, at most 64 KiB, and not
  inside the data directory or workspaces (checked on both the given and the
  symlink-resolved path). PKCS#1 and PKCS#8 RSA keys of at least 2048 bits are
  accepted; it is parsed once and only the `*rsa.PrivateKey` is kept. All errors
  are fixed strings. JWTs are RS256 via `crypto/rsa`, `iat = now-60s`,
  `exp = now+9m`, `iss = app_id`.
- **Installation tokens** (`internal/github/app.go`, `github.App`). Run tokens
  are minted with `repository_ids: [id]` and exactly contents/issues/
  pull_requests write plus metadata read; the response's permissions and
  repository list are verified. The immutable repository ID is resolved once
  per process with a metadata-only token narrowed by repository name. Tokens are
  cached per (installation, repo ID, permission set), concurrent mints are
  single-flighted (the mint runs on a detached, 2-minute-bounded context; each
  caller stops waiting on its own cancellation), refreshed with less than
  5 minutes left, and invalidated on a REST 401 with exactly one re-mint. App API
  calls retry at most 4 times: rate limits wait for Retry-After/reset plus
  jitter when it is within 2 minutes, 5xx/network errors back off
  exponentially, 401/403/404/422 are `MintError{Kind: "denied"}` and never
  retried. `OnMint` registers every token before it is used.
- **Repository-bound routing.** New `github.RepoTokenSource` (explicit
  `RepoRef` argument) and `github.StaticRepoTokens` (PAT for every repository).
  `HTTPClient` selects the credential from each call's own repository; PAT-mode
  clients built with `github.New` behave exactly as before. Runs, waiting
  comment sync, label ingress, preflight, publication and failure comments all
  go through these per-repository calls. `runner.Options` gains `RepoTokens`,
  `CommitAuthor` and test seams `GitHubAPI`/`GitHubHTTP`; `runner.New` builds the
  App source from config (`internal/runner/auth.go`) and fails startup on any
  App error. `Options.Tokens` (PAT) is ignored in app mode.
- **Git.** `gitx.CloneOptions.TokenFunc` is called for every remote operation
  (clone, CheckoutSHA fetch, both ls-remote calls, push), so a run longer than
  one hour pushes with a fresh token. Header-only auth and every existing guard
  are unchanged; a TokenFunc error is never quoted and never falls back to
  `Token`.
- **Identity.** App mode resolves `<slug>[bot]` from `GET /app` (JWT) and the
  bot user ID from an unauthenticated `GET /users/<slug>[bot]`; either failing
  stops startup. `EligibleCommand` always rejects comments by a `[bot]` bot
  login, so the owner's `/approve` goes through the normal approver +
  collaborator + unedited path; the shared-identity rule remains for PAT mode.
  Commits are authored as `<slug>[bot] <ID+<slug>[bot]@users.noreply.github.com>`.
- **Doctor.** App mode checks key file safety and local JWT signing, warns that
  `GITHUB_TOKEN` is ignored, and with `--online` checks the App identity and,
  for every allowlisted repository, installation reachability, the configured
  `installation_id`, the four granted permissions, and a single-repository mint.

## Tests (all with `-race`, fake GitHub over httptest)

- `internal/github/app_test.go`: JWT header/claims and signature verified with
  the public key (and rejected with another key); key formats and file safety
  (mode, symlink, inside data, relative, non-PEM, directory) without key
  material in errors; run token has exactly one repository ID and the four
  permissions, ID lookup is metadata-only, every token passes `OnMint`; cache
  reuse and refresh near expiry with a fake clock; 32 concurrent callers share
  one mint; two repositories never share a token and a token for one is refused
  for the other; 401 invalidates and re-mints exactly once; revoked installation
  fails closed even when a PAT source is present; rate limit waits on
  Retry-After, refuses long resets, and stops on cancellation; bot identity and
  commit email; `[bot]` comments never eligible while owner approval is; JWTs
  are not quoted in errors.
- `internal/gitx/auth_remote_test.go`: the HTTPS `git http-backend` remote
  rotates its accepted token after clone; push succeeds because each remote
  operation asks `TokenFunc`; a failing `TokenFunc` fails closed without quoting
  and without using `Token`.
- `internal/runner/app_auth_test.go`: full app-mode fix run. The bot's own
  `/approve` is ignored, the owner's approves through sync; within the execute
  checkout the push token differs from the clone token (mutation-checked: pinning
  the clone token fails the test); every REST call carries a token minted for
  that repository and none the PAT; the commit author is the bot; no minted
  token, its git header form, or the PAT appears in the SQLite files, slog
  output, posted comments, or any repository command's environment, and every
  minted token is registered in the caller's redactor. Revoked installation
  fails the run with no REST call reaching GitHub; an unsafe key mode stops
  `runner.New`.
- `internal/cli/doctor_app_test.go` and `internal/config/github_test.go`:
  doctor offline/online app checks (missing permission, missing installation,
  unsafe key, ignored `GITHUB_TOKEN`, no key or token printed); config
  validation and `config show`.
- PAT regression: all existing github, gitx, runner, cli, server and config
  tests pass unchanged apart from the internal `gitx.command` signature.

## Open issues

- M24 "refuse to save a repository the App cannot reach" is not built (M24 does
  not exist yet); `doctor --online` provides the same check for config repos.
- Git network failures are not classified, so a git 401 does not invalidate
  the cache; the next operation still gets a fresh token once the cached one
  is within the refresh margin, and REST 401s do invalidate.
- Key rotation needs a restart (the key is parsed once); outstanding tokens
  remain valid until expiry (at most 1 hour) unless the installation is
  suspended. Documented in README and SECURITY.md.
- The repository-ID cache lives for the process lifetime; a transferred or
  renamed repository needs a restart.
- Ordinary REST calls still have no rate-limit backoff (only App API calls do);
  `SyncWaiting` still lists comments unconditionally.
- A disposable-repository end-to-end drill against real GitHub has not been run
  (no network in this milestone).
