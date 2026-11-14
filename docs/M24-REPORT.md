# M24 report: repository management in the dashboard

Date: 2026-11-14. Branch `m24-repo-mgmt`.

## What was done

- Migration `0010_repos.sql`: `dashboard_repos` (name unique, JSON settings, version, created/updated at+by) and
  append-only `repo_audit` (triggers refuse update/delete).
- `internal/repos.Registry` (RWMutex): config repos (read-only, source `config`) united with dashboard repos
  (source `dashboard`). `Allowed/Has/Get/Lookup/Names/List/Add/Update/Remove/Audit`. Config wins on name collision.
- Validation shared with the config file through `Config.ValidateRepo` (name, argv commands, test_command,
  protected paths, M25 image/PATH rules), plus size limits, `workspace.NewPolicy` and a redaction round-trip check.
  `.github/workflows` stays protected for every repo (`workspace.BuiltinProtected`).
- Runner/server/webhook/memory/CLI consult the registry. Each run freezes its repo settings in a
  `repo_settings` artifact at creation; clone, resume, revision, publish and memory digests read it back.
- API: `GET/POST /api/repos`, `GET/PUT/DELETE /api/repos/{owner}/{name}`, `.../audit`. Actor is the Access email
  (else local actor), channel `ui`. PUT needs `version` (If-Match or body; 428 if missing, 409 on mismatch).
  DELETE is 409 `repo_busy` with active runs (checked in the delete transaction). Add reports a non-blocking
  GitHub reachability check (`runner.CheckRepo`).
- Doctor lists every repo with its source and checks dashboard repos like config repos (read-only DB read);
  `config show` lists dashboard repos as comments.
- UI: `/repos` page (list, add/edit form with argv-per-line commands, remove with confirm, audit view); render
  functions and text nodes only, CSP unchanged, `/repos` added to the shell nav and `web.PublicPath`.

## Tests (-race)

`store`: migration, lifecycle/conflicts/busy removal, append-only audit, read-only reader.
`repos`: validation parity with `config.Validate` (local and container), collision, inert colliding row,
limits, credential refusal, builtin protection, concurrency, Merge.
`server`: CRUD+audit, refusals (config repo, shell strings, unknown fields, 413), new run uses a newly added repo
without restart, delete refused with active run, auth required on all endpoints (token and Access), Access email in
audit, XSS payload inert. `runner`: frozen settings survive an edit and a resume.

## Review fixes

- Registry is read-through (no cache): every lookup reads SQLite, so another process's add/edit/remove is seen by the next run start and a removed repo cannot start runs anywhere; the freeze re-reads right before saving.
- Audit `before` is read from the stored row inside the mutation transaction.
- Serialized settings are capped at 64 KiB (413 on add/edit) and the freeze fails closed above the cap; nothing is truncated.
- A blank image/PATH is omitted by the UI and an empty PATH list is treated as inherit.

## Open issues

- CLI has no repo add/edit commands (parity optional).
- Removed config repos mid-run still fail preflight (existing behaviour).
- Frozen settings are not shown in the run UI; UI is compact pending M29.
