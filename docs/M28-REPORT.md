# M28: Connect GitHub (App manifest flow)

Builds on M27. The owner clicks **Connect GitHub**; GitHub creates the App from
a manifest, redirects back with a one-time code, and patchbot exchanges it and
stores the key itself. The owner never handles the private key. No new Go
dependencies; tests use an in-memory fake GitHub only.

## What changed

- **Config** (`[github]`): `setup_public_url` (https origin, http only for
  localhost; no path/query/credentials) and `setup_app_name` (1-34 chars,
  default `patchbot-<6 random>`). `app_id = 0` is now valid in app mode (setup
  mode); negative is rejected.
- **Manifest** (`github.NewManifest`): name, url, `hook_attributes`
  `{url: <public>/hooks/github, active: false}`, `public: false`,
  `default_permissions` copied from `RunPermissions` (contents/issues/
  pull_requests write, metadata read), `redirect_url = <public>/setup/github/callback`,
  no `default_events`. Exact JSON is asserted in a test.
- **Routes** (`internal/server/githubsetup.go`):
  - `GET /setup/github`: data-free shell + `/assets/setup.js`. 409 page when
    already connected (app_id in config, a `github_app` row, or the key file
    exists) or not configured (auth not `app`, no `private_key_file`, no
    `setup_public_url`).
  - `POST /api/github/setup/start` (bearer + cross-origin check): preflights the
    key path, mints a 32-byte random state (kept only as a SHA-256 digest, max
    16 in flight, 10-minute expiry, single use, constant-time compare) and
    returns the `https://github.com/settings/apps/new?state=...` action and the
    manifest. The script builds the form and posts it, so the bearer header is
    available (a plain navigation cannot send it).
  - `GET /setup/github/callback`: exempt from the bearer check (GitHub's redirect
    cannot send it); the state is the credential. Validates and consumes the
    state, preflights the key path (before the code is spent), exchanges the
    code, writes the key, saves the row. Serialized by a mutex.
  - `GET /api/github/status`: `connected`, `slug`, `app_id`,
    `restart_required`, `setup_available`, and installations from
    `GET /app/installations` using a JWT.
- **Exchange** (`github.ConvertManifest`): unauthenticated POST, 64 KiB body cap,
  20 s timeout, redirects refused, code validated against `[A-Za-z0-9_-]{1,128}`.
  Only `id`, `slug`, `pem` are decoded; `client_secret`/`webhook_secret`/
  `client_id` are never read. The PEM must parse as an RSA key >= 2048 bits.
  Errors are fixed strings (status code only); the code and body are never echoed.
- **Key file** (`github.WriteNewKeyFile`): exclusive temp file in the target
  directory, `0600`, fsync, then `os.Link` to the final path (fails if anything,
  including a dangling symlink, is there), temp removed, then the M27 placement
  check (`CheckAppKeyFile`, data dir exclusion) runs on the result. Owner is the
  service user because the server process creates it. The directory is never
  created.
- **Store**: migration `0009_github_app.sql` (single row: app_id, slug,
  installation_id = 0, created_at) plus `0008_reserved_m23.sql` (comment only,
  absent in this branch). `SaveGitHubApp` returns `ErrConflict` on a second
  save; `store.ReadGitHubApp` reads read-only for doctor.
- **Startup**: in app mode with `app_id = 0` the runner loads the ID from the
  table. If none exists the server still starts (setup mode): credentials fail
  closed with `ErrGitHubNotConnected` (never a PAT) and `prepare` refuses new
  runs; the API answers 409 `github_not_connected` ("...open /setup/github...").
- **Doctor**: WARN "GitHub App: not connected, open /setup/github" with no key
  checks; with a stored App it runs the usual key/JWT/online checks.

## Secrets discarded

`client_secret` and `webhook_secret` are not decoded, stored, logged or shown.
The App is created with the webhook inactive, so no webhook secret is needed;
if a webhook is enabled on the App later, set a new secret in the App and in
`[webhook]`. Nothing but `app_id` and `slug` reaches the DB; the success page
shows the slug, ID and install link only. A test greps the response, logs and
the SQLite file for the secrets, the PEM header and the code.

## Tests

`internal/server/githubsetup_test.go` (`-race`): manifest exact; random default
name; state mismatch/empty/malformed/expired/replayed rejected without calling
GitHub; state consumed on a failed exchange; success writes a 0600 key with the
exact content and no temp files, row saved, no leak; existing key refused (and
the code not spent, also when it appears between start and callback); disabled
when connected via DB or config and when unconfigured; bearer rules (start and
status 401, shell and callback reachable, callback relies on state); missing key
directory; runs refused before connection (409, no run row); status endpoint
with JWT; unauthenticated mode. Also `internal/github/setup_test.go`
(key writer, symlink/dangling/relative/excluded-root cases, conversion
hardening), `internal/store/githubapp_test.go`, `internal/cli/doctor_setup_test.go`,
and updated config validation tests.

## Open issues

- **Activation needs a restart.** After connecting, the running process stays in
  setup mode (runs refused) until `systemctl restart patchbot`; the page and
  status endpoint say so (`restart_required`). Hot-swapping the runner's token
  source/bot identity needs locking around ~26 call sites and was left out.
- **Installed repositories** are shown as installations (account and
  `repository_selection`), not a per-repo list: that needs an installation
  token, which patchbot mints only per run and repository. Doctor `--online`
  checks every allowlisted repo.
- **Failure after conversion** (key cannot be written despite the preflight) strands
  an App on GitHub; the page names it so the owner can delete it. A DB failure
  after the key was written tells the owner to set `app_id` manually.
- **Host header behind a proxy/tunnel**: the callback goes through the existing
  host validation, so the proxy must forward a Host patchbot accepts.
- The setup shell is public like the other shells, so an unauthenticated
  visitor can learn whether GitHub is connected (one bit, no identifiers).
- `installation_id` in the table is stored as 0 and not used yet.
- Pagination of `GET /app/installations` is not followed (first 100).
