---
paths:
  - "internal/web/**"
  - "internal/server/web*.go"
---

# patchbot frontend rules (dashboard in `internal/web`)

Adapted from `/frontend-patterns` for this project's stack: **Vue 3.5 global build, no npm, no build step,
handwritten `h()` render functions, Go `html/template` shell, strict CSP.** React/Next/Zustand/Framer
patterns from that skill do NOT apply here — use the Vue equivalents below.

## 1. Hard constraints (never break — tests enforce most of them)

- **No build step, no npm, no new CDN libraries.** Only Vue (pinned + SRI, `VueIntegrity` in `web.go`).
  Icons are inline SVG built with `h("svg", …)`; no icon fonts, no image CDNs.
- **CSP stays as is:** no `unsafe-inline`, no `unsafe-eval`, no inline `<script>`/`<style>`/`style=""`
  strings from the server, no `on*=` attributes. Never use Vue's runtime template compiler
  (no `template:` strings, no in-DOM templates) — render functions only.
- **Untrusted text is text.** Model output, issue bodies, specs, diffs, logs, test output, memory lessons
  and error messages are rendered with text nodes (`text()`, `block()`), never `innerHTML`, `v-html`,
  `insertAdjacentHTML`, `document.write`, or a markdown→HTML renderer.
- **Links:** create `<a href>` only from values parsed with `new URL()` and protocol `http:`/`https:`
  (see the existing `link()` helper); external links get `target="_blank" rel="noopener noreferrer"`.
- **Secrets never touch the DOM, URL, localStorage or cookies.** In Cloudflare Access mode the browser
  holds no credential at all; identity comes from `GET /api/session`.
- **All writes:** JSON body, `credentials: "same-origin"`, go through the single `api()`/`useApi` helper,
  surface errors in a visible `role="alert"` panel. Never swallow a failed request.
- **Server is the source of truth.** The UI never decides lifecycle rules (approval freshness, revision
  caps, spec hash) — it shows buttons by status and lets the API reject.

## 2. Code structure

- Split `app.js` into native ES modules under `assets/` (`<script type="module">` from self):
  - components: `RunList`, `RunDetail`, `SpecView`, `LogStream`, `DiffView`, `RepoCard`, `Inbox`,
    `NewRequest`, `MemoryView`, `Toast`, `ConfirmDialog`.
  - composables (`use*.js`): `useApi`, `useSession`, `useRunStream` (SSE with `Last-Event-ID`,
    frame parsing across chunks, seq dedupe, 1–10 s backoff), `useKeyboard`, `useToast`.
  - `ui.js` for primitives (`button`, `field`, `chip`, `panel`, `icon`) — reuse them, don't re-create.
- **Composition over flags:** small components that take props + slots/children; a component that grows
  more than ~3 boolean "mode" props should be split.
- **One file ≈ one component, < 300 lines.** Pure helpers (formatting, diff, status mapping) live in
  plain functions with no DOM access so they can be unit-tested from `web_test.go`/JS tests.
- **State:** `reactive()` per component; shared state (session, badge counts, toasts) only through a
  composable that returns a single reactive object. No global mutable variables.
- **Cleanup:** every `setInterval`, SSE reader, `AbortController` and key listener is released in
  `onUnmounted`. Fetches on route change are aborted.

## 3. Design system (CSS in `app.css` only)

- **Tokens on `:root`**, never raw colours in rules:
  `--bg --surface --text --muted --edge --accent --focus --ok --warn --err --info` plus `-bg` variants,
  `--radius` (.3rem / .6rem), `--space-1..6` on a 4/8 px scale, `--font-ui` (system-ui),
  `--font-mono` (ui-monospace).
- **Dark + light:** `@media (prefers-color-scheme: dark)` guarded by `:root:not([data-theme="light"])`,
  and repeated under `:root[data-theme="dark"]`; the theme toggle only sets `data-theme` on `<html>`.
  Contrast ≥ 4.5:1 for text, ≥ 3:1 for borders/icons in both themes.
- **Status is never colour alone:** every status chip = icon + text + colour, from one mapping table
  (`awaiting_approval`, `needs_info`, `running`, `succeeded`, `failed`, `rejected`, `cancelled`).
- **Layout:** max width 76rem, wraps at phone width (≥ 320 px, 16 px gutter, no horizontal page scroll).
  Long content (logs, diffs, paths) scrolls inside its own `tabindex="0"` panel, `overflow-wrap: anywhere`.
- **Monospace** for IDs, hashes, paths, diffs, logs; short hashes shown as 7–12 chars with full value
  in `title`.

## 4. UX patterns

- Landing page = **"Waiting for you" inbox** (specs to approve, questions to answer); run list second.
- Run detail = **phase stepper** (Refine → Approval → Code → Tests → Review → PR) with the failure
  reason in plain language and a "what to do next" line.
- **Destructive actions** (Reject, Cancel, Forget memory, Remove repo) use `ConfirmDialog`; Approve
  shows spec version + short hash in the button/dialog so the owner knows what they approve.
- Every list has an **empty state**, every async area a **loading state**, every failure an **error
  panel** with a retry action. Success feedback via toast (`role="status"`, auto-dismiss ≥ 5 s).
- Live updates via SSE; poll every 15 s only as fallback. Keep only the latest 1,000 log events.
- Tab title shows pending count: `(n) patchbot`.
- Unknown numbers (tokens, cost) show "unknown", never 0.

## 5. Accessibility

- Semantic elements first (`button`, `nav`, `main`, `section > h2`, `dl`, `ol` for steppers).
- Every control has a label; icon-only buttons have `aria-label`.
- Visible `:focus-visible` ring using `--focus`; dialogs trap focus, close on Escape, restore focus.
- Keyboard shortcuts (`a` approve, `r` revise, `g r` runs, `?` help) are ignored while typing in
  inputs and never trigger destructive actions without the confirm dialog.
- Respect `prefers-reduced-motion`: no animation beyond opacity fades ≤ 150 ms.
- Live regions: alerts `role="alert"`, toasts/status `role="status"`.

## 6. Performance

- Lists > 200 rows paginate with the API cursor (no client virtualisation library).
- Debounce search/filter input 300 ms; abort the previous request.
- Diff/log rendering stays linear time; collapse large tool outputs and diffs > 500 lines behind
  "Show all".
- No layout-thrashing loops; batch DOM work through Vue reactivity.

## 7. Done checklist for any frontend change

1. `make ci` green (includes CSP/SRI and forbidden-sink tests in `internal/web` and `internal/server`).
2. Add/extend a test when adding a new render path for untrusted data (XSS payload stays inert).
3. Check in Chrome on the Mumbai deploy (not local), at desktop and ~375 px width, light and dark.
4. Keyboard-only pass through the changed flow.
5. Update `internal/web/README.md` if behaviour or security properties change.
