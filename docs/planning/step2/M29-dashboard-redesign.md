# M29 Dashboard redesign (owner-approved 2026-11-14)

Runs after phase 2 (M24, M26) is merged, because M23/M28/M24/M26 all edit internal/web.
Constraints: Vue 3 via SRI-pinned CDN or vendored copy, no build step, native ES modules, existing CSP,
no new CDN libraries, inline SVG icons, SSE for live updates.

## Scope, in priority order
1. Approval inbox landing page ("Waiting for you": specs to approve, questions to answer) + run-detail
   phase stepper (Refine → Approval → Code → Tests → Review → PR) with failure reason.
2. Run detail tabs: Spec (criteria checklist, files, risks; sticky Approve/Revise/Reject, approve shows
   version + short hash), Live log (grouped by turn, collapsed tool calls, follow toggle, highlighted
   failing tests), Changes (file tree + diff + test result + PR link). Budget meters (tokens/cost/time).
3. Sidebar nav: Runs, New request, Repositories, Memory, Settings/GitHub, signed-in email; badge counts
   and `(n) patchbot` tab title.
4. New request form (with M26) incl. templates and dry-run; Repositories page cards + add-repo wizard
   (with M24) showing App install/permission status and a sandbox test run before save.
5. Design system: CSS variables, dark + light toggle, consistent status colours with icon + text,
   system + monospace fonts, 4/8px spacing; mobile-friendly inbox and spec view.
6. Safety/feedback: confirm dialogs for Reject/Cancel/Forget, toasts, empty states, plain-language
   error panel with next step.
7. Accessibility: full keyboard use (a approve, r revise, g r runs), focus rings, ARIA labels,
   prefers-reduced-motion.
8. Code structure: split app.js into ES-module components (RunList, RunDetail, SpecView, LogStream,
   DiffView, RepoCard) and composables (useApi, useRunStream with Last-Event-ID, useSession).

## Done when
All flows above work in Chrome against the Mumbai deploy; web tests updated; CSP/SRI tests green;
Codex review of the XSS surface (model output, diffs and logs rendered as text, never HTML); make ci green.

Design rules: follow `.claude/rules/frontend.md` (loaded automatically when editing internal/web).
