# M29: Dashboard redesign

Frontend-only milestone: no API or lifecycle changes. The server only gained
the routes and the fixed asset list the new UI needs.

## What changed

1. **ES modules.** `assets/app.js` is gone. `main.js` plus `components/*.js`,
   `composables/use*.js`, `ui.js` and `format.js` (about 40 small files) are
   embedded and served from `web.assetNames`, an explicit list. Token mode
   (`web.PublicPath`) and Access mode (`accessPublic`) share `web.AssetPath`.
   Mux routes were added for `/assets/components/{name}` and
   `/assets/composables/{name}`. CSP unchanged; Vue pin and SRI unchanged.
2. **Design system** (`app.css`): tokens on `:root` (colours, `--space-1..6`,
   radii, fonts), dark and light via `data-theme` plus `prefers-color-scheme`,
   status chips with icon + text + colour from one table, monospace for
   IDs/diffs/logs, reduced-motion respected, phone layout with no horizontal
   scroll.
3. **Inbox and stepper.** `/` is "Waiting for you" (awaiting_approval and
   needs_info, plus runs in progress). Run detail has the Refine, Approval,
   Code, Tests, Review, PR stepper and a plain-language failure reason with a
   next step.
4. **Run tabs.** Spec (structured criteria checklist, files, risks, test plan,
   version picker and diff, editor; sticky Approve/Revise/Reject, Approve shows
   `vN` and a short hash), Live log (grouped by turn/phase, collapsed events,
   failing events marked, follow toggle), Changes (file list, text diff with
   "Show all" past 500 lines, test result, PR link, review). Budget meters for
   tokens, cost and time against the frozen limits; unknown stays "unknown".
5. **Navigation.** Sidebar (Inbox, Runs, New request, Repositories, Memory,
   GitHub) with badge counts, signed-in email, theme toggle and Lock; collapses
   to a top bar under 48rem. Tab title is `(n) patchbot`. New `/memory` page.
6. **Dialogs and feedback.** `ConfirmDialog` (focus trap, Escape, focus restore)
   for Reject, Cancel, Forget and Remove repository, and for Approve. Toasts
   (`role="status"`, 6 s). Loading, empty and error states with a retry or next
   step on every list.
7. **Keyboard.** `a`, `r`, `g i`, `g r`, `g n`, `?`; ignored while typing and
   while a dialog is open; focus ring from `--focus`; ARIA tabs, dialogs, live
   regions.

Existing features kept: list filters and cursor in the URL, spec edit/approve/
revise/reject/cancel/retry, SSE with `Last-Event-ID`, memory panel, token unlock,
Access-mode email, Repositories page, write-a-new-request form, Connect GitHub.

## Review fix: approval bound to the shown spec

Codex found that the confirm dialog showed one spec version but the approve call
carried no identity, so an edit or revise in between would approve the newer spec.
`POST /api/runs/{id}/approve` now requires `spec_version` and `spec_hash` (400 if
missing) and calls the new `Runner.ApproveSpecAndExecute`, which passes them as
`expected` to `approveDecision`. The comparison runs under the run's control lock
in the same critical section as the decision; a mismatch returns 409 `spec_changed`
(`runner.ErrSpecChanged`, which wraps `store.ErrConflict` so the GitHub comment path
is unchanged). The CLI path is unchanged. The UI captures version and hash before the
dialog opens and sends exactly those; on 409 it shows the error, reloads the run and
does not retry. Tests: stale version, stale hash, missing fields and zero version
record no approval; a matching body approves; a static UI test checks both fields are
sent from the captured spec.

## Tests

- `internal/web`: asset list equals the embedded tree; every import resolves
  to a listed file; every listed asset is served with the right media type and
  `nosniff`, unlisted and `..` paths are 404; all modules scanned for forbidden
  sinks, HTML in template strings, unvalidated `href`, storage and cookie use;
  untrusted surfaces (diff, log, spec, repo name, memory) still use the text
  helpers; shell has only the JSON state, Vue and the module; CSP has no
  `unsafe-*`.
- `internal/server`: pages (`/`, `/memory`, `/repos`), CSP per Vue mode, every
  asset served, token mode protected shell, Access mode: assets public only on
  the fixed list, everything else (pages, `/assets/other.js`, directory paths,
  non-GET) needs the JWT; email still arrives via state and `/api/session`.
- `node --check` passes on every module.

## Not done / caveats

- **Not checked in Chrome.** No browser was available; all JS was syntax-checked
  with `node --check` and covered by static tests only. Needs a pass on the
  Mumbai deploy (desktop and about 375 px, light and dark, keyboard only).
- Planning-doc item 4 extras (request templates, add-repository wizard with App
  install status and a sandbox test run before save) are not built; the
  existing forms and dry-run checkbox are kept.
- "Highlighted failing tests" in the log is a heuristic over event payloads
  (`ok:false`, failed/error status, non-zero exit, FAIL/panic text).
- The file list in the Changes tab is derived from the diff text, not a tree.
- `/setup/github` stays a separate server-rendered page (its nav was extended).
