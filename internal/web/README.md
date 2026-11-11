# Embedded run interface

`internal/server` serves `/` and `/runs` (run list), `/runs/new` (new run),
and `/runs/{id}` (detail). The browser uses only the existing M13 JSON API,
artifact downloads and SSE. No npm, build step, new Go dependency or external
markdown renderer is involved. All assets and the Go html/template shell are
embedded in the binary.

The template serializes an ordinary state value in `#state`; there is no
trusted-JavaScript cast. On an unauthenticated loopback server, the detail
shell includes the same redacted read model as the API plus a plain-text
preview. With a UI token configured, all shells contain only route/auth
metadata; run data is fetched after the user unlocks the tab. New/list screens
bootstrap from their API endpoints. Every load refetches server state. Filters
and the continuation cursor survive reload in the list URL. Unsubmitted form
edits and the displayed log window are transient; the log replays from SQLite
when reloading the detail page.

Vue uses the global production build, pinned to 3.5.13. The app configures
`[[ ]]` delimiters. Its handwritten render functions return text nodes and
never invoke Vue's runtime template compiler, keeping CSP free of
`unsafe-inline` and `unsafe-eval`. The only executable scripts are the app
from self and Vue from `https://cdn.jsdelivr.net`; styles and fetches use self.
Local mode permits only self for scripts. No inline event attributes are used.

Specs use the brief's **plain preformatted text alternative** to markdown
rendering, including the server preview. Model/issue/test output is always
text. Markdown HTML and `javascript:` links stay inert text; the app creates
external links only for validated http/https PR URLs. The tests check script
escape payloads, inert markdown and forbidden HTML sinks in our shell/app.
The canonical spec marker is hidden in the browser's spec view; editing
recovers its structured fields into labelled controls. Criteria, risks, test
plans and questions are edited one per line. Saving calls the existing PUT
endpoint, creating a version and invalidating prior approval. Version diffs
show common context and all replaced lines in linear time, rather than doing
quadratic matching on potentially large generated text.

Button status rules mirror M13: Approve for awaiting_approval; Edit, Revise and
Reject for awaiting_approval, needs_info or approved with a spec; Cancel for
active runs; Retry for failed, cancelled or rejected. The API remains
responsible for freshness, expiry, revision caps and racing lifecycle changes.
Errors are visible alerts. Saving never silently discards an open editor.
Retries choose a provider and a live-listed or typed model ID and navigate to
the new run. Unknown token/cost totals remain visibly unknown.

All POST/PUT requests use JSON, `credentials: "same-origin"`, and, when needed,
a bearer header. Token entry happens once per tab and has no cookie, URL,
localStorage or HTML representation. Rejected credentials clear session storage
and restore the token form. Locking clears displayed data. Fetch-based SSE
works in both auth modes, parses frames across transport chunks, deduplicates
sequence IDs and resumes using `Last-Event-ID`. Only fully consumed frames
advance the cursor. Reconnect delay grows from one to ten seconds. The view
retains the latest 1,000 events, refreshes details on events and polls every
15 seconds as a fallback. Closing the page does not cancel a run.

The awaiting-approval badge counts all matching API pages, independent of the
current list filter. List requests keep filters with each opaque cursor. Older
runs and First page provide forward cursor navigation. Forms have labels,
status chips include text, focus is visible, output panels are keyboard
scrollable, and layout wraps at phone width.

## Vue vendoring

The single `VueIntegrity` constant in `web.go` pins the included Vue 3.5.13
asset. Its comment records verification against jsDelivr and unpkg on 2026-11-11.
An offline test verifies that the embedded bytes match this SHA-384 pin.

To refresh that exact asset from a networked machine:

```sh
make vendor-vue
make build
```

The Go downloader uses the same version/URL/hash as the shell, bounds the
response to 2 MiB, checks SHA-384, and atomically installs
`assets/vue.global.prod.js` only on a match. Errors preserve the previous
vendor file. Rebuilding embeds the installed copy. No build is downloaded by
`make ci`. The historical `vue.global.prod.js.placeholder` is never served as JavaScript. Missing local Vue returns 503
with instructions, and the shell shows a loading diagnostic.

Select the mode in your TOML config (the key already existed in M1):

```toml
[server]
vue_src = "local" # default: "cdn"
```

Both modes retain SRI and crossorigin. Changing mode requires restarting the
server; installing a vendor asset requires rebuilding the binary.
