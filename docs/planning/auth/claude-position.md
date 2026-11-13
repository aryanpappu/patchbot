# GitHub auth model for patchbot: Claude's opening position (2026-11-13)

Context: one owner (aryanpappu), self-hosted patchbot on a shared Mumbai server, repos allowlisted in config,
planned M23 (login dashboard via Cloudflare Access JWT) and M24 (add/edit repos from the dashboard).
Today: one fine-grained PAT in GITHUB_TOKEN (env), used for clone/push (GIT_CONFIG_* header, never argv)
and REST (issues, comments, PRs). `github.TokenSource` seam exists. Owner asks: account-wide token or
per-repo token?

## Options
A. One account-wide classic PAT (repo scope). Simple; blast radius = every repo the owner has, incl. private
   and other people's orgs. Never expires unless set. Reject.
B. One fine-grained PAT covering "selected repos". Owner edits repo list on GitHub when adding a repo.
   One secret, least privilege per repo set, max 1 year expiry. Blast radius = the selected set.
C. One fine-grained PAT per repo, stored per repo (encrypted in SQLite, key from env). Smallest blast radius
   per token, but N secrets to create/rotate by hand, and secrets now live in the DB (today: env only).
D. GitHub App installed on chosen repos. Private key in env; patchbot mints 1-hour installation tokens
   scoped per run to exactly ONE repo (`repositories=[name]`) with only contents/issues/pull_requests.
   Commits/PRs show as `patchbot[bot]`, so `/approve` from the owner is clearly distinct from the bot's own
   comments. Adding a repo = tick it in the App installation (or dashboard deep link). No long-lived repo token.

## My recommendation
Now (pilot): B — one fine-grained PAT, selected repos, 90-day expiry. Zero code.
Next milestone (with M24): D — GitHub App behind the existing TokenSource, per-run single-repo
installation tokens, keep PAT as fallback for local/dev. Not C: per-repo PATs multiply manual secrets and
move secrets into the DB for less benefit than D gives automatically.
Dashboard "add repo" (M24) must verify the App installation (or PAT) can actually reach the repo before
saving, and never accept a token through the web UI.
