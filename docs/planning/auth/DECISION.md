# GitHub auth decision (2026-11-13, Claude + Codex)

## Decision
1. **Now (pilot): one fine-grained PAT, "only selected repositories"** (patchbot-demo, pappumishra),
   contents/issues/pull_requests write + metadata read, short expiry (30-90 days), env-only.
   Not account-wide (classic `repo` PAT = every repo you own on a shared host). Not one PAT per repo
   (N manual secrets, needs per-repo token routing that does not exist, secrets would move into SQLite).
2. **Next (before M24 "add repo from dashboard" is enabled broadly): GitHub App "patchbot".**
   Installed only on chosen repos; patchbot mints 1-hour installation tokens per run, narrowed to ONE repo
   ID and only contents/issues/pull_requests. Bot identity `patchbot[bot]` separates bot comments from the
   owner's `/approve`. App private key = protected file readable only by user patchbot, never in DB,
   backups, workspaces or containers. PAT mode stays explicit for local/dev; no silent fallback.
3. Repo policy (commands, protected paths, image) goes in SQLite with M24; **credentials never do**.
   The dashboard never accepts a token.

## Found during review — must fix before the pilot on a PRIVATE repo (verified in code)
- `gitx.Push` runs `ls-remote` unauthenticated (internal/gitx/publish.go:71 and :100, `complete(ctx, false, ...)`),
  and `CheckoutSHA` fetches unauthenticated (internal/gitx/gitx.go:276). pappumishra is private, so push/
  re-clone would fail with an auth error. Offline smoke used a local fake remote, so tests never caught it.
  Fix: use the same header-only auth (GIT_CONFIG_*) for these network calls, with tests against an
  auth-requiring fake remote.
  **Status: fixed (branch auth-fixes).** clone, fetch (CheckoutSHA), both ls-remote calls and push all
  use the header-only extraheader; `gitx.command` now refuses a remote subcommand without it and refuses
  credentials for local ones. Tested against an HTTPS `git http-backend` remote that requires the header
  (succeeds with the token, fails without; token absent from argv, .git/config and errors).
- With a PAT owned by `aryanpappu`, GitHub comments by `aryanpappu` are the bot's own identity, so
  `/approve` and `/revise` comments are ignored (sync.go → EligibleCommand). **Approve from the dashboard**
  for the pilot. The App (decision 2) fixes this properly.
  **Status: fixed for the PAT pilot (branch auth-fixes), "shared identity" rule.** A comment by the token
  owner counts only if that login is a configured approver and live collaborator, the comment is unedited
  and in the current waiting window, has no `<!-- patchbot` marker, and its ID is not in `posted_comments`
  (migration 0007). Every comment patchbot posts now carries a marker and its ID is recorded right after
  posting. Other authors keep the old rules. The App remains the target identity separation.
- The token is loaded once per run and reused for push; fine for a PAT, must be refreshed per network call
  with an App (1-hour expiry).

## App milestone acceptance criteria (from Codex)
Concurrent cross-repo isolation; token expiry between clone/push/PR; revocation and key rotation;
correct bot identity + owner approval; private-repo publication; rate-limit backoff; M24 refuses to save
a repo the App cannot reach with the needed permissions; zero secrets in logs/artifacts/DB/container env;
`make ci` + disposable-repo end-to-end drill.
