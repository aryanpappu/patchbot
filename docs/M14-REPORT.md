# M14 implementation report

Implemented from scratch in the `m14-web` worktree, without committing. The
embedded Go html/template + Vue interface covers new runs, filtered/paged runs
and run detail. It uses the M13 JSON API, artifact downloads and resumable SSE.
No Go dependency, npm package or frontend build step was added.

## Files

New interface:

- `internal/web/web.go`: embedding, template rendering, bootstrap state, exact
  Vue version/URL and single integrity constant, mode-specific CSP and fixed
  asset delivery.
- `internal/web/shell.html`: accessible page shell, escaped JSON data block,
  plain-text preview, pinned SRI/crossorigin scripts and external app/style.
- `internal/web/assets/app.js`: new-run and retry model selection with live
  fallback warnings, budget/dry-run controls, filters/cursor paging and global
  approval count, detail timeline/counters, spec editor/history/version diff,
  status-bound actions, artifacts, token entry and fetch-based resumable SSE.
- `internal/web/assets/app.css`: responsive layout, text status chips, visible
  focus and keyboard-scrollable output.
- `internal/web/assets/vue.global.prod.js.placeholder`: clearly named offline
  placeholder; it is never served as the Vue runtime.
- `internal/web/cmd/vendor-vue/main.go`: bounded download, SHA-384 verification
  and atomic vendor installation, sharing the shell's version/URL/hash.
- `internal/web/cmd/vendor-vue/main_test.go`: successful verification, hash
  mismatch, HTTP failure and size-limit checks; failures preserve an old file.
- `internal/web/web_test.go`: XSS/state round trip, plain markdown safety,
  pinned SRI/crossorigin and forbidden app HTML/code sinks.
- `internal/web/README.md`: safety, state/auth, CSP, UI and vendoring contracts.

Server and integration:

- `internal/server/web.go`: page routes, unauthenticated detail bootstrap and
  preview, protected data-free shells, static asset dispatch.
- `internal/server/web_test.go`: each page, issue-title script/XSS injection,
  both CSP/source modes, fixed asset media types/nosniff, absent vendor
  diagnostic, protected shells without token/private data and Host/API guards.
- `internal/server/api.go`: register page/asset routes; extract the existing
  detail read model so API and initial detail state share exactly the same data.
- `internal/server/server.go`: select the CSP and exempt only exact GET shell
  and asset paths from bearer auth, retaining Host and cross-origin checks.
- `internal/server/security_test.go`: update the previous root-page auth
  expectation; API/SSE/artifact/unknown-route authentication coverage remains.
- `internal/server/README.md`: document browser bootstrap and auth changes.
- `internal/cli/serve.go`: update serve help for the page/token flow.
- `Makefile`: `vendor-vue` target and help entry.
- `docs/M14-REPORT.md`: this report and local dry-run instructions.

The `[server] vue_src` field, `cdn` default, `local` validation and example
configuration already existed. This milestone connects them to rendering,
asset serving and CSP rather than duplicating the configuration key. `go.mod`
and `go.sum` are unchanged.

## Decisions where the plan left details open

1. Specs use the explicitly permitted **plain preformatted text** alternative
   to markdown rendering. Go html/template escapes the server preview; Vue text
   children display all model/issue/test output. HTML and javascript links in
   markdown never become elements or links. Only http/https PR URLs can create
   external links. No HTML injection directive or DOM HTML sink is used.
2. Vue is pinned to **3.5.13**, `vue.global.prod.js` from jsDelivr. Handwritten
   render functions avoid invoking its runtime compiler, so CSP needs neither
   unsafe-inline nor unsafe-eval. The app configures the requested `[[ ]]`
   delimiters. App JS, CSS and API connections use self; only CDN mode adds
   `https://cdn.jsdelivr.net` to script-src.
3. Browser navigation cannot add an Authorization header. Exact GET shell and
   asset routes are therefore public. When token protection is enabled, shells
   include only route/auth metadata, even if a navigation supplies valid auth.
   Every API, SSE and artifact request still requires the bearer token. Host
   validation and security headers apply to public shells/assets as well.
4. Tokens are entered once per tab, stored only in sessionStorage, and sent in
   bearer headers. Rejected credentials restore the entry form. Lock clears
   session storage and displayed data. Fetch streaming is used in both modes,
   with Last-Event-ID resume, incremental frame parsing and deduplication.
   A run that becomes terminal gets a final replay before the log completes.
5. Detail data refreshes on events and every 15 seconds; lists refresh every
   15 seconds. The log retains the latest 1,000 events in the tab and replays
   persisted history on reload. Filters and the opaque cursor are in the list
   URL; First page and Older runs implement forward cursor paging. The approval
   badge counts all awaiting-approval API pages, independent of list filters.
6. The spec editor exposes the existing canonical fields as labelled controls;
   list fields are edited one per line. Save uses M13 PUT and creates a new
   version with prior approval cleared. History diffs use a linear replacement
   diff with prefix/suffix context. API lifecycle/freshness/revision checks
   remain authoritative when a control races another action.
7. New runs default to dry-run checked. Unknown cost/token totals stay unknown.
   A dry run shows diff, harness output and PR title/body previews; it does not
   create a PR link. Retry navigates to the new run created by the existing API.

## Checks

Final command, exit **0**:

```sh
GOCACHE=/private/tmp/patchbot-m14-go \
GOLANGCI_LINT_CACHE=/private/tmp/patchbot-m14-lint make ci
```

Output (build metadata and the module-cache warning path abbreviated):

```text
# fmt-check: clean; no output
go vet ./...
golangci-lint run
0 issues.
go test -race ./...
?   	github.com/aryanpappu/patchbot/cmd/patchbot	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/agent	(cached)
ok  	github.com/aryanpappu/patchbot/internal/cli	2.095s
ok  	github.com/aryanpappu/patchbot/internal/config	(cached)
ok  	github.com/aryanpappu/patchbot/internal/execx	(cached)
ok  	github.com/aryanpappu/patchbot/internal/github	(cached)
ok  	github.com/aryanpappu/patchbot/internal/gitx	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/anthropic	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/fake	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/gemini	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/openai	(cached)
ok  	github.com/aryanpappu/patchbot/internal/llm/providers	(cached)
ok  	github.com/aryanpappu/patchbot/internal/logging	(cached)
ok  	github.com/aryanpappu/patchbot/internal/runner	167.292s
ok  	github.com/aryanpappu/patchbot/internal/secret	(cached)
ok  	github.com/aryanpappu/patchbot/internal/server	25.992s
ok  	github.com/aryanpappu/patchbot/internal/store	(cached)
?   	github.com/aryanpappu/patchbot/internal/testhttp	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/tools	(cached)
?   	github.com/aryanpappu/patchbot/internal/version	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/web	1.816s
ok  	github.com/aryanpappu/patchbot/internal/web/cmd/vendor-vue	1.830s
ok  	github.com/aryanpappu/patchbot/internal/workspace	(cached)
?   	github.com/aryanpappu/patchbot/internal/workspace/wstest	[no test files]
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags '...' -o bin/patchbot ./cmd/patchbot
go: writing stat cache: ... operation not permitted
```

The module-cache warning is nonfatal: Go could not cache main-module version
metadata in the existing read-only module cache. Compilation, linking and the
CGO-disabled binary build succeeded. Build and lint caches used /private/tmp;
no dependency downloads were attempted.

`node --check internal/web/assets/app.js` and `git diff --check` exit **0**.
The built `patchbot serve --help` and `patchbot config check --help` exit **0**
and agree with the local instructions below. `make -n vendor-vue` prints
`go run ./internal/web/cmd/vendor-vue`; the network download was not invoked.

A temporary Node VM harness with mocked Vue/browser/API objects also exercised
all three render paths, dry-run JSON submission and filter submission, token
entry/session reuse, bearer SSE headers, frames split across chunks, and final
terminal replay using Last-Event-ID. It exited **0**:

```text
new: render and API smoke passed
runs: render and API smoke passed
detail: render and API smoke passed; bearer SSE chunking and Last-Event-ID terminal replay passed
detail (token entry): render and API smoke passed; bearer SSE chunking and Last-Event-ID terminal replay passed
```

This mock check does not substitute for a real browser/Vue runtime smoke test.

## Unfinished and external verification

As authorized, `VueIntegrity` in `internal/web/web.go` is an explicit offline
provisional SHA-384 value. **The supervisor must verify/correct it outside the
sandbox before the page can load Vue.** The constant's comment says that
`make vendor-vue` verifies it. Both CDN and local scripts share this one pin.
The vendoring command refuses a mismatch without installing anything.

The actual vendor file is intentionally absent; its named placeholder and
verified-download target are delivered as requested. No network download,
live GitHub/provider run or real-browser check against the Vue runtime was
performed in this sandbox. No other M14 feature is intentionally deferred.

## Try the page locally with a dry run

1. Have the supervisor correct/verify the single Vue integrity constant. On a
   machine with network access, install and embed that exact build:

   ```sh
   make vendor-vue
   make build
   ```

2. Copy the example and edit it for a repository and issue you control:

   ```sh
   cp patchbot.example.toml patchbot.toml
   ```

   Set the allowlisted `[repos."owner/repo"]` entry and literal test command
   argv, configured provider/model IDs, approvers and budget. Add this table
   (or edit an existing server table):

   ```toml
   [server]
   addr = "127.0.0.1:8080"
   vue_src = "local"
   ```

   For CDN mode, use `vue_src = "cdn"`; the verified pin is still required.
   Local mode only removes the browser's CDN dependency. A dry run still reads
   GitHub, clones the allowlisted repository, calls the selected provider and
   runs repository commands, without publishing comments, pushes or a PR.

3. Set `GITHUB_TOKEN` and the API key environment variable named by your
   provider config using your usual secret setup. Optionally set
   `PATCHBOT_UI_TOKEN` to exercise browser unlock. Validate and start:

   ```sh
   ./bin/patchbot config check --config patchbot.toml
   ./bin/patchbot serve --config patchbot.toml --addr 127.0.0.1:8080
   ```

4. Open `http://127.0.0.1:8080/runs/new`. If prompted, enter the UI token once;
   it remains in sessionStorage for this tab. Select your repo, issue, `fix`
   mode, provider and a listed or typed model. Optionally choose a separate
   refine model, set a budget, leave **Dry run checked**, and click Start run.

5. Watch the detail timeline and log until awaiting approval or needs info.
   Review the plain-text spec. Try Edit latest specification / Save new version
   and compare versions, or enter notes and click Revise. Resolve open questions
   before approving. Approve is enabled only at awaiting approval.

6. Click Approve and follow the live code/test phases. Expand test attempts,
   inspect the diff and PR preview. A successful dry run publishes nothing.
   Reload the detail URL to refetch its stored state and replay the log. Open
   `/runs` to try filters, the approval badge and cursor paging. For a failed,
   cancelled or rejected run, choose provider/model and Retry to get a new ID.

7. Stop the server with Ctrl-C. It drains owned work using the existing M13
   shutdown behavior. Closing the browser alone never cancels a run.
