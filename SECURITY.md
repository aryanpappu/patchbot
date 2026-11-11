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
are passed to git only for clone/push. Redaction runs at persistence and output
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
CLI/UI decisions use a configured actor name; there is no user login.

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
