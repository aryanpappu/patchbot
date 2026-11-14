# Security policy

**patchbot is not a sandbox. Repository commands run as your user.** They can
read your host files, use your network and create processes outside the tool
jail. Use only repositories, test commands and toolchain directories you trust.

## Threat model

The model, issue text, repository files, test output and upstream HTTP responses
may be wrong or hostile. The operator, configuration, local account and trusted
toolchain are trusted. The intended deployment is one operator on one machine.

The approval gate binds code access to the current spec version and hash, issue
content and expiry. Repo/command allowlists, an `os.Root` file jail, protected
paths, explicit child environments, output caps, process groups and guarded git
publication limit what model tools can do. Tokens are absent from test envs and
are passed to git only for remote operations (clone, fetch, ls-remote, push) as an
ephemeral host-scoped `http.extraheader` in `GIT_CONFIG_*`, never in argv, the
URL or `.git/config`; local git commands never receive them. Redaction runs at persistence and output
boundaries. Every CLI/server runner receives provider, GitHub and configured UI
credentials. Registered values shorter than eight characters cannot be substring
masked; `config check` and `doctor` warn without printing their values. Harness
phases, statuses and JSON/TOML keys are preserved during redaction. HTTP reads and
outbound deadlines are bounded. Adapter deadlines are retryable `unavailable`
errors; caller cancellation and deadlines stop retries.

Trusted PATH entries must exist and resolve to directories. Every directory
from `/` to the entry, including unresolved symlink locations, must be owned by
root or the current user and have no group/world write bits. Sticky directories
are rejected too. Data and workspace roots are excluded. Doctor validates config
before running anything and shares git resolution with production through the
configured trusted path. Its version checks use `execx` with process groups, a
five-second timeout, a 4 KiB output cap and WaitDelay.

Process-group cleanup observes leader exit without reaping. The zombie reserves
its PID/PGID until the final group signal, preventing cleanup from signaling a
reused group. Timeout/cancellation sends TERM, waits the full grace period, then
KILL; ordinary exits clean remaining members before Wait. Git shares this
lifecycle. Escaped descendants remain outside this guarantee.

The web server defaults to loopback. Other binds require `PATCHBOT_UI_TOKEN`.
Bearer auth protects data routes when enabled. Host validation, Go cross-origin
mutation checks, CSP, SRI and plain-text rendering protect the browser. Local
CLI decisions use a configured actor name.

With `[server.auth] mode = "cloudflare_access"` (M23) the UI/API no longer use
the bearer token. Every request except the three fixed static asset paths must
carry `Cf-Access-Jwt-Assertion`; patchbot verifies the RS256 signature against
the team JWKS (`https://<team_domain>/cdn-cgi/access/certs`, keys cached by
`kid`, one refresh per minute on an unknown `kid`), `iss`, `aud`, `exp`/`nbf`/
`iat` (60 s skew) and that the `email` claim is in `allowed_emails`. Only RS256
is accepted (`none` and HMAC tokens are rejected). Failures return a generic
401/403 and the token is never logged or echoed. The verified email is the
recorded actor on the ui channel. The Access JWT is the sole credential in this
mode: keep the listener on loopback behind the tunnel, and rely on the Access
policy to limit who can sign in (`allowed_emails` is a second check). The webhook path keeps its own
HMAC check and needs a Cloudflare Access bypass policy so GitHub can reach it.

GitHub approval identity: comments by the token owner (`/user`) are patchbot's
own and normally never count. With a single-user PAT the owner may also be an
approver ("shared identity"). Such a comment counts only if the login is a
configured approver and live collaborator, the comment is unedited, inside the
current waiting window, carries no `<!-- patchbot` marker, and its ID is not in
patchbot's `posted_comments` record. Every comment patchbot posts carries that
marker and its ID is recorded right after posting, so either check alone keeps
patchbot's own text (even a spec starting with `/approve`) from approving.

GitHub App mode (`[github] auth = "app"`): comments by the App bot
(`<slug>[bot]`) never count, so the shared-identity rule above applies only to
PAT mode. Each run's REST calls and every git network operation use an
installation token minted for that repository's immutable ID only, with
contents/issues/pull_requests write and metadata read, cached in memory, never
written to SQLite, logs, artifacts or the container environment, and registered
for redaction (with its git header form) before first use. App failures fail
closed; there is no fallback to a PAT.

Key custody on a shared host: the App private key is a long-lived credential
that can mint tokens for every repository the App is installed on. patchbot
keeps it in a file readable only by the service user (0600, owner checked,
symlinks refused, never inside the data directory, workspaces or backups) and
parses it once into memory. This is not a boundary against root, against other
processes running as the patchbot user, or against debuggers and core dumps of
the patchbot process. Install the App only on the repositories patchbot needs,
run repository code with the container executor so it does not run as a process
that can read the key, and rotate the key after any suspected host compromise
(outstanding installation tokens stay valid until they expire, at most 1 hour,
unless the installation is suspended).

Connect GitHub (App manifest flow, M28): `GET /setup/github` is a data-free
shell (like the other pages); the manifest and a 32-byte random `state` are
issued only by the bearer-protected `POST /api/github/setup/start`. The state
is kept as a SHA-256 digest in server memory, expires after 10 minutes, works
once, is compared in constant time, and is consumed even when the exchange fails.
Because GitHub redirects the browser to `/setup/github/callback`, which cannot
carry a bearer header, that one route is authenticated by the state alone; an
attacker who cannot obtain a state cannot make patchbot adopt their App. The
code exchange is a bounded (64 KiB), 20-second, no-redirect, unauthenticated
call; its response and the code are never logged or echoed, and errors are fixed
strings. The key is written atomically (exclusive temp file, `0600`, then a
hard link that fails if the path exists), owned by the service user, and an
existing file, symlink or dangling link is refused, so a key is never replaced.
The key directory must pre-exist; the check runs before the single-use code is
spent so a key cannot be lost after the App was created. The database stores
only the App ID and slug. `client_secret` and `webhook_secret` are discarded
(the App is created with the webhook inactive). The setup page relaxes only
`form-action` to `https://github.com`. Setup is disabled once connected, and a
public, unauthenticated visitor can learn only whether GitHub is connected.

In scope: file-tool jail escapes, command-policy bypass, leaks through logs,
SSE, artifacts, errors or PR text, unsafe git publication, browser protection
bypass and code execution without valid spec approval. Prompt injection that
crosses these enforced boundaries is in scope.

Out of scope: containing arbitrary repository code, a compromised host or
operator account, malicious trusted binaries, intentional credential exfiltration
by tests, children that escape process groups, denial of service by host code,
multi-user authorization and secure public deployment. Redaction cannot identify
every secret or arbitrary encoding. The Unix binary starts
with umask 077. Git and tests inherit it, but trusted code can change modes.
Embedders must set their own mask. Workspace parents are private. Dry runs still execute
host code and may incur provider costs. Containers, CPU/memory isolation,
network isolation, TLS termination, webhooks and durable leases are future work.

See [architecture](docs/ARCHITECTURE.md) for the boundaries and known limits.

## Reporting

Do not open a public issue. Use this repository's GitHub **Security → Report a
vulnerability** feature. Include the version, platform and a minimal reproduction
with dummy credentials. Expect a reply within seven days. Never include live
keys in a report or test fixture.

## Supported versions

The latest release and `development` receive security fixes during early
development. Use Go 1.27 or newer and git 2.50.1 or newer. Review provider pricing
and credentials before enabling publication. Review every generated PR.
