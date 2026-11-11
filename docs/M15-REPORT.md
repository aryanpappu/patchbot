# M15 release readiness report

Implemented from scratch in this worktree on `m15-release`. No commit, tag or
push was created. No dependency, workflow or live credential was added.

## Delivered

- Configurable trusted directories, source reporting and matching child PATH.
- Process-group exit polling with TERM/KILL escalation and fast/slow regressions.
- Offline doctor with pass/warn/fail output, read-only migration inspection and
  explicitly enabled model-list/GitHub identity probes.
- Audit fixes below, release docs, complete example and real-loader coverage.
- `scripts/smoke.sh`, a `smoke` Go integration test and `make smoke`.

## Files

- `CHANGELOG.md`
- `Makefile`
- `README.md`
- `SECURITY.md`
- `cmd/patchbot/main.go`
- `cmd/patchbot/permissions_other.go`
- `cmd/patchbot/permissions_unix.go`
- `cmd/patchbot/permissions_unix_test.go`
- `docs/ARCHITECTURE.md`
- `docs/DECISIONS.md`
- `docs/M15-REPORT.md`
- `docs/PROGRESS.md`
- `internal/cli/cli.go`
- `internal/cli/config.go`
- `internal/cli/config_test.go`
- `internal/cli/doctor.go`
- `internal/cli/doctor_test.go`
- `internal/cli/models.go`
- `internal/cli/runs.go`
- `internal/cli/serve.go`
- `internal/config/config.go`
- `internal/config/defaults.toml`
- `internal/config/load.go`
- `internal/config/load_test.go`
- `internal/config/release_test.go`
- `internal/config/validate.go`
- `internal/execx/README.md`
- `internal/execx/env.go`
- `internal/execx/local_unix.go`
- `internal/execx/local_unix_test.go`
- `internal/execx/path.go`
- `internal/execx/path_other.go`
- `internal/execx/path_test.go`
- `internal/execx/path_unix.go`
- `internal/execx/policy.go`
- `internal/gitx/version.go`
- `internal/llm/anthropic/anthropic.go`
- `internal/llm/anthropic/hardening_test.go`
- `internal/llm/gemini/edge_test.go`
- `internal/llm/gemini/gemini.go`
- `internal/runner/hardening_test.go`
- `internal/runner/phases.go`
- `internal/runner/runner.go`
- `internal/runner/smoke_test.go`
- `internal/secret/redactor.go`
- `internal/secret/redactor_test.go`
- `internal/server/README.md`
- `internal/server/server.go`
- `internal/store/inspect.go`
- `internal/store/inspect_test.go`
- `internal/store/sqlite.go`
- `internal/tools/run_cmd.go`
- `internal/tools/tools_test.go`
- `internal/web/README.md`
- `internal/web/cmd/vendor-vue/main.go`
- `internal/web/web_test.go`
- `internal/workspace/local.go`
- `internal/workspace/local_test.go`
- `internal/workspace/wstest/wstest.go`
- `patchbot.example.toml`
- `scripts/smoke.sh`

## Audit findings and fixes

1. New workspace files/directories used 0644/0755. Rewrites preserved public
   bits. File tools now create 0600/0700 and retain only owner permission bits
   on existing files. Owner execute permission survives. The shared workspace
   test helper also uses private directories. Existing file-tool tests now
   assert private modes and retained owner execution.
2. `MkdirAll(..., 0700)` accepted an existing public data directory; opening an
   existing database with 0600 did not narrow its permissions. CLI startup now
   refuses public data directories, symlink data directories, nonregular or
   public database files. Doctor reports these failures. Existing owners can
   correct permissions explicitly; diagnostics never silently chmod their data.
3. Git and test processes inherited the shell's default creation mask. The Unix
   binary now sets umask 077 once, before starting work. A regression creates
   files/directories with permissive defaults and verifies 0600/0700 results.
   Build directories/binaries also get 0700. Trusted code can still call chmod
   or change its mask; that is outside the repository-code threat boundary.
4. The Anthropic SDK read success, error and model-list bodies without an
   application cap. A response-body wrapper now limits every read to 10 MiB
   plus one detection byte and reports malformed oversized responses. Streaming
   read errors retain the response/request ID. Oversized success/error/model
   bodies and the existing interrupted-body regressions pass.
5. Anthropic allowed the supplied client's redirect behavior. The adapter now
   copies the client and refuses redirects, matching the raw HTTP adapters and
   preventing credential forwarding through redirects.
6. Anthropic and Gemini relied on client timeouts when callers gave no context
   deadline. Injected clients could remove that bound. Anthropic now adds
   context deadlines of ten minutes for chat and thirty seconds for model lists;
   Gemini adds two minutes per request. The Vue downloader now uses a request
   context capped at sixty seconds. GitHub already capped requests at thirty
   seconds and OpenAI-compatible calls at two minutes. Regression transports
   inspect deadlines without waiting for production timeouts.
7. Registered provider/GitHub secrets shorter than eight bytes were ignored by
   `AddSecrets`. All explicit credential registration now uses `AddExact`,
   including rejected config-layer keys and injected server GitHub tokens.
   Generic text registration retains its eight-byte heuristic. The short-secret
   regression now requires masking.
8. CLI parsing could echo arbitrary flag/positional text before config keys
   were loaded. Some config summary lines also printed a source path without
   its provider redactor. Usage diagnostics now omit unparsed values. Doctor validates API key variable
   names before displaying them, so a key pasted into `api_key_env` is never
   echoed. Config
   sources are masked. The final CLI stream registers GitHub/UI credentials
   before parsing and provider credentials after loading. Regressions cover
   malformed flags, arbitrary provider secrets, short credentials and config
   diagnostics. The injected config path is also preserved by `config` commands.
9. Upstream PR URLs went into SQLite through run patches before redaction.
   Free-text issue title, error message, PR URL and summary patches now pass the
   runner redactor before persistence. A fake GitHub response echoes a credential
   in a PR URL; the real publication regression verifies stored runs and events.
10. Package docs still described Vue's pin as provisional even though this tree
    already included the verified asset and corrected pin. Updated those docs
    and added an offline SHA-384 test against the embedded bytes. No download
    was made and no new third-party provenance verification is claimed.
11. The existing fixed PATH could not run toolcache installations. Added the
    requested config list and rejected empty/relative/traversing/PATH-injection
    entries, world-writable directories and data/workspace aliases. Validation
    resolves existing prefixes and dangling symlinks. Policy copies the list;
    lookup and child PATH use that same copy. Tests cover mutation, ambient PATH
    exclusion, world writes, files, missing directories and workspace/data paths.
12. Cancellation always consumed the full grace period. The executor now checks
    the whole process group every ten milliseconds and returns when it is gone.
    It retains SIGKILL at the grace deadline. Fast/cooperative and slow/TERM-
    resistant children are tested, alongside existing descendant cleanup tests.

The remaining audit checks found no unfinished production TODO/FIXME and no
leftover application debug prints. `TODO` in golden adapter search queries is
intentional test input; the task brief names the audit terms. Vendoring success
output and smoke PR output are intentional command/test output. Vue's included
production runtime retains its own error diagnostics.

The other persistence and output paths were checked: redacted slog handlers,
agent records/events, tool outputs, specs, accounting, artifacts, SSE replay,
structured API errors and PR/comment text. Numeric accounting and harness-owned
status data contain no secret text. HTTP handlers already use bounded request
bodies. Raw provider/GitHub response reads were already bounded. Git subprocess
output is capped. The SDK body cap and context changes close the gaps above.

## Decisions

- Missing trusted directories are allowed for portable configs. Existing ones
  must be directories and pass the Unix mode check. Doctor fails unresolved
  repo commands. Construction checks the actual workspace root again.
- Doctor reports absent fresh storage as WARN; first use creates/migrates it.
  Existing ledgers with pending versions, gaps, unknown versions or changed
  names/checksums FAIL. Doctor never migrates or creates storage. Missing
  configured key variables FAIL; a keyless OpenAI-compatible endpoint WARNs.
- Online probes call only ListModels and `/user`. They do not chat, clone, push,
  create issues or test repository permissions. Normal constructor seams keep
  offline tests free of real keys and sockets.
- The smoke script builds the real binary and exercises its doctor. The full
  run uses the production CLI dispatcher/loader with injected infrastructure:
  scripted provider, httptest GitHub over net.Pipe, real SQLite and local bare
  git. This avoids adding a production fake-provider mode or bypassing approval.
  The actual Go bin directory is included in the fixture's trusted list for
  setup-go/toolcache installations. The harness verifies `fixed.txt` exists.
- Local CLI/UI identity remains `approval.local_actor`. No login or new approval
  shortcut was added. The spec gate and publication guards remain authoritative.
- Runtime creation is private. Embedders of CLI/runner packages must set their
  own process umask before starting work; the production binary does this once.

## Checks

Final commands both exit **0**. Build metadata and temporary paths are
abbreviated in the output below. All Go/lint caches used `/private/tmp`; module
lookup and checksum network access were disabled. Dependencies were already
present. The PATH prefix selects the installed Apple Git executable rather than
its Xcode-discovery launcher for fixture setup.

```sh
PATH="/Applications/Xcode.app/Contents/Developer/usr/bin:$PATH" \
GOCACHE=/private/tmp/patchbot-m15-go \
GOLANGCI_LINT_CACHE=/private/tmp/patchbot-m15-lint \
GOPROXY=off GOSUMDB=off make ci
```

```text
go vet ./...
golangci-lint run
0 issues.
go test -race ./...
ok  	github.com/aryanpappu/patchbot/cmd/patchbot	1.747s
ok  	github.com/aryanpappu/patchbot/internal/agent	(cached)
ok  	github.com/aryanpappu/patchbot/internal/cli	2.881s
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
ok  	github.com/aryanpappu/patchbot/internal/runner	151.707s
ok  	github.com/aryanpappu/patchbot/internal/secret	(cached)
ok  	github.com/aryanpappu/patchbot/internal/server	(cached)
ok  	github.com/aryanpappu/patchbot/internal/store	(cached)
?   	github.com/aryanpappu/patchbot/internal/testhttp	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/tools	(cached)
?   	github.com/aryanpappu/patchbot/internal/version	[no test files]
ok  	github.com/aryanpappu/patchbot/internal/web	(cached)
ok  	github.com/aryanpappu/patchbot/internal/web/cmd/vendor-vue	(cached)
ok  	github.com/aryanpappu/patchbot/internal/workspace	(cached)
?   	github.com/aryanpappu/patchbot/internal/workspace/wstest	[no test files]
mkdir -p -m 700 bin
chmod 700 bin
CGO_ENABLED=0 go build -trimpath -ldflags '<build metadata>' -o bin/patchbot ./cmd/patchbot
chmod 700 bin/patchbot
```

```sh
PATH="/Applications/Xcode.app/Contents/Developer/usr/bin:$PATH" \
GOCACHE=/private/tmp/patchbot-m15-go \
GOPROXY=off GOSUMDB=off make smoke
```

```text
./scripts/smoke.sh
mkdir -p -m 700 bin
chmod 700 bin
CGO_ENABLED=0 go build -trimpath -ldflags '<build metadata>' -o bin/patchbot ./cmd/patchbot
chmod 700 bin/patchbot
=== RUN   TestSmoke
RESULT  CHECK                DETAIL
PASS    config               <temporary config>
PASS    go version           go version go1.27.3 darwin/arm64
PASS    git version          git version 2.54.0 (Apple Git-157)
PASS    GitHub credential    GITHUB_TOKEN is set
PASS    provider anthropic   PATCHBOT_SMOKE_KEY is set
PASS    data directory       mode 0700
PASS    database migrations  2 migrations; checksums match
PASS    owner/repo / test    /bin/test
WARN    online checks        skipped; use --online for ListModels and GitHub /user
RESULT  CHECK                DETAIL
PASS    config               <temporary config>
PASS    go version           go version go1.27.3 darwin/arm64
PASS    git version          git version 2.54.0 (Apple Git-157)
PASS    GitHub credential    GITHUB_TOKEN is set
PASS    provider anthropic   PATCHBOT_SMOKE_KEY is set
PASS    data directory       mode 0700
PASS    database migrations  2 migrations; checksums match
PASS    owner/repo / test    /bin/test
WARN    online checks        skipped; use --online for ListModels and GitHub /user
Run 1: awaiting_approval (fix, refine)

spec:
# Specification

Add a fixed file

## Acceptance criteria

- [ ] AC1: fixed.txt exists

## Files

- fixed.txt: create file

## Risks


## Test plan

- Run tests

## Open questions


<!-- patchbot:spec-data eyJzdW1tYXJ5IjoiQWRkIGEgZml4ZWQgZmlsZSIsImFjY2VwdGFuY2VfY3JpdGVyaWEiOlsiZml4ZWQudHh0IGV4aXN0cyJdLCJmaWxlcyI6W3sicGF0aCI6ImZpeGVkLnR4dCIsImNoYW5nZSI6ImNyZWF0ZSBmaWxlIn1dLCJyaXNrcyI6W10sInRlc3RfcGxhbiI6WyJSdW4gdGVzdHMiXSwib3Blbl9xdWVzdGlvbnMiOltdfQ== -->

Approve with: patchbot approve 1
Run 1: succeeded (fix, publish)

spec:
# Specification

Add a fixed file

## Acceptance criteria

- [ ] AC1: fixed.txt exists

## Files

- fixed.txt: create file

## Risks


## Test plan

- Run tests

## Open questions


<!-- patchbot:spec-data eyJzdW1tYXJ5IjoiQWRkIGEgZml4ZWQgZmlsZSIsImFjY2VwdGFuY2VfY3JpdGVyaWEiOlsiZml4ZWQudHh0IGV4aXN0cyJdLCJmaWxlcyI6W3sicGF0aCI6ImZpeGVkLnR4dCIsImNoYW5nZSI6ImNyZWF0ZSBmaWxlIn1dLCJyaXNrcyI6W10sInRlc3RfcGxhbiI6WyJSdW4gdGVzdHMiXSwib3Blbl9xdWVzdGlvbnMiOltdfQ== -->


diff:
diff --git a/fixed.txt b/fixed.txt
new file mode 100644
index 0000000..eaaa2f8
--- /dev/null
+++ b/fixed.txt
@@ -0,0 +1 @@
+fixed


pr_title:
Add fixed file

pr_body:
Fixes #12

Approved spec: stored run 1 specification, version 1, SHA-256 `bd21edb020cd917c2df6d545155200c605c57ba22588d81e776556fdbbc06801`, approved by local-owner.

Harness tests: **passed** (1 attempts). Baseline: **failed**.

Acceptance criteria (agent claims; harness result above):

- [x] AC1: fixed.txt exists — harness: passed. 

Created fixed.txt



Harness exit code: 0; timed out: false.

```text

```

Usage: 300 input / 150 output tokens; cost: 0 micro-USD.

Provider: anthropic; model: test-model.

Generated by patchbot. Human review is required.

SMOKE PASS: refine → approve → code → test; PR text printed; no writes or pushes
--- PASS: TestSmoke (4.23s)
PASS
ok  	github.com/aryanpappu/patchbot/internal/runner	5.540s
```

The final smoke test completes in 4.23 seconds after building. It checks a
matching stored approval, successful verification, consumed provider script,
workspace cleanup, zero GitHub posts and zero remote patchbot branches. Its
baseline fails before the fixture file exists; verification passes after code
creates it. Both facts appear in the PR preview.

`sh -n scripts/smoke.sh`, `node --check internal/web/assets/app.js` and
`git diff --check` also exit **0**. The real example loads and validates in the
config test suite. The local Vue bytes match the shared SRI pin.

Earlier checks caught outdated expectations for public file modes and config
usage text; those assertions now match the hardened behavior. Early overlapping
runs also added load to Apple's launcher and hit Xcode-discovery fixture
timeouts. The final check above passes with the installed Git binary. No test
was skipped or timeout increased to hide those failures.


## Unfinished and external limits

The cached govulncheck v1.9.0 scanner runs, but this environment has no cached
vulnerability database. `GOPROXY=off go run ...@v1.9.0` cannot complete module
metadata lookup. The cached executable reports its version, then a local-only
scan with `-db file:///private/tmp/patchbot-m15-vulndb ./...` fails because no
local database exists. No network database was substituted or fabricated.
**A clean vulnerability scan is not established.** Run the CI vulnerability job
or `govulncheck ./...` with an authoritative database before release.

The smoke flow needs no network or real keys. Fresh dependency downloads,
live model availability/pricing, live GitHub permissions and a real browser
session were not tested here. Repository code still runs as the operator; no
sandbox, multi-user login, webhooks, durable queue, public deployment or
publication reconciliation was added. These limits are documented in README,
SECURITY and PROGRESS. No other requested implementation is intentionally deferred.
