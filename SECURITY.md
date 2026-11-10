# Security policy

patchbot runs an AI agent against code and holds GitHub and model-provider credentials, so security reports are welcome
and taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private vulnerability reporting:
**Security → Report a vulnerability** on this repository. You will get a reply within 7 days.

In scope: escaping the workspace jail, running commands outside the allowlist, leaking tokens or API keys, pushing to a
branch other than `patchbot/*`, bypassing the spec-approval gate, and prompt injection that changes what patchbot
publishes.

## Supported versions

Only the latest release and the `development` branch receive fixes during early development.

## Known limits

Until the Docker sandbox lands (Step 4 in `docs/PLAN.md`), a repository's test commands run on the host as the
operator's user. Only use patchbot on repositories you trust.
