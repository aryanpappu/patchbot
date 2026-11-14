# M23: Login-based dashboard (Cloudflare Access)

No new Go dependencies and no migration (0008 unused): `actor` columns already
store free text.

## What changed

- **Config.** `[server.auth] mode = "token" | "cloudflare_access"` (default
  `token`, unchanged behaviour including no-token loopback). Access mode needs
  `team_domain` (`<team>.cloudflareaccess.com`, no scheme or path), `audience`
  (the application AUD tag) and a non-empty `allowed_emails` list of plain
  addresses (no wildcards). Access fields in token mode are an error. A
  non-loopback bind no longer needs `PATCHBOT_UI_TOKEN` in Access mode.
- **Verifier** (`internal/cfaccess`). RS256 only, against
  `https://<team_domain>/cdn-cgi/access/certs`. Keys are cached by `kid`; an
  unknown `kid` refreshes at most once per minute (also after a failed fetch),
  and a refresh replaces the whole set so retired keys stop working. The set is also refreshed after 1 h; if the refresh fails the old keys serve until 24 h, then verification fails closed. The shared fetch runs under its own timeout, so a cancelled client request never consumes the cooldown (failed fetches back off 15 s). Fetches
  have a 5 s client and 10 s context timeout, no redirects, a 1 MiB body cap,
  and accept only RSA keys of at least 2048 bits. Claims: `iss` equals
  `https://<team_domain>`, `aud` contains the AUD tag, `exp` and `iat`
  required, `nbf` checked when present, all with 60 s skew, `email` in
  `allowed_emails` (case-insensitive). `alg` none or HS256 is rejected.
- **Server.** All requests except the exact static asset paths need a valid JWT
  (pages included). 401 for missing/invalid, 403 for a disallowed email, with a
  generic message; pages get a plain-text 401. The JWT is never logged or echoed.
  Host check, cross-origin protection and body limits are unchanged.
- **Actor.** `runner.WithActor` carries the verified email; approve, revise,
  reject, cancel, retry, spec edit, run start and memory add/decide record it on
  the ui channel. CLI keeps `approval.local_actor`.
- **UI.** `GET /api/session` returns `{email, mode}`; the header shows "Signed
  in as <email>"; no token form in this mode.
- **Doctor.** Reports the mode and counts; with `--online` fetches the JWKS and
  fails unless it holds RSA keys. Warns if `PATCHBOT_UI_TOKEN` is set but ignored.

## Finding the AUD tag and team domain

1. Cloudflare dashboard, Zero Trust, Access, Applications.
2. Open the self-hosted application that protects the patchbot hostname
   (Edit).
3. On the Overview tab copy **Application Audience (AUD) Tag**: a 64-character
   hex string. That is `audience`.
4. The team domain is under Zero Trust, Settings, Custom Pages ("Team domain"),
   shown as `<team>.cloudflareaccess.com`. That is `team_domain`.
5. The Access policy must allow only the owner's email; list the same address
   in `allowed_emails` (defence in depth).

## Deployment notes

- Keep `addr` on loopback behind the tunnel. Remove `PATCHBOT_UI_TOKEN`.
- If the tunnel rewrites the Host header, set `httpHostHeader` to the bound
  address so the host check passes (unchanged requirement).
- GitHub webhooks cannot log in: add an Access bypass policy for the webhook
  path only; that path stays HMAC-protected.
- Service tokens carry no email claim and are refused.

## Tests

`internal/cfaccess` (httptest JWKS, generated RSA keys, fake clock): valid,
wrong signature, aud, iss, expired, not-yet-valid, future iat, missing
exp/email/kid, alg none and HS256, unknown kid refetch limited to once per
minute, key rotation, skew tolerance, JWKS failures (status, JSON, no RSA keys,
oversize, redirect). `internal/server`: approve records the email as actor and
channel ui, reject and memory decisions, every failure mode on API and pages,
public assets only, `/api/session`, host and origin protection, token-mode
regression. `internal/config`: strict validation. `internal/cli`: doctor.
