# M12 implementation report

Implemented on `m12-runner` in this worktree, without committing. All new code
was written for this task and uses the existing agent, store, git, GitHub,
workspace, executor, provider, config and CLI packages.

## Files

New runner implementation and documentation:

- `internal/runner/runner.go`: construction, startup recovery, frozen run options,
  provider resolution, admission, status CAS and artifacts.
- `internal/runner/lifecycle.go`: approvals, revisions, UI edits, rejection,
  cancellation, heartbeats, expiry, failure handling and retries.
- `internal/runner/phases.go`: preflight, disposable clones, refine/investigate,
  approval-gated code, baseline isolation and harness integration.
- `internal/runner/accounting.go`: durable conservative accounting and active
  time across revisions, phases and separate CLI processes.
- `internal/runner/publish.go`: final integrity/path checks, PR rendering, local
  commits, guarded push, draft selection and issue notification.
- `internal/runner/records.go`: redacted git/publication execution records.
- `internal/runner/sync.go`: a single waiting-run command/expiry pass and advice
  for edited GitHub spec comments.
- `internal/runner/README.md`: API, execution, accounting and CLI contracts.
- `internal/runner/runner_test.go`: GitHub HTTP fixture, local bare repositories,
  full fix/read-only/dry flows, approval invalidation and authorization,
  rejection, cancellation and three failed test attempts.
- `internal/runner/recovery_test.go`: budgets, expiry, crash recovery, revisions,
  retries, pinned bases, moved branches, salvage and publication safety.
- `internal/runner/heartbeat_test.go`: deterministic heartbeat/parking race.
- `internal/runner/cli_test.go`: CLI end-to-end flows using the same real store,
  fake provider, HTTP server and bare remote.

CLI and supporting integration:

- `internal/cli/runs.go`: all requested run/control/list/sync/retry commands,
  signal handling, masked output, flags and exit codes.
- `internal/cli/runs_test.go`: command usage and parsing coverage.
- `internal/cli/cli.go`: dispatcher and help integration.
- `internal/cli/cli_test.go`: replace obsolete placeholder expectations.
- `internal/agent/stages.go`: lifecycle hooks after the unchanged approval
  checks, and untrusted baseline evidence in the code prompt.
- `internal/store/decisions.go`: atomic latest-spec decisions and code claims;
  immutable spec-comment publication provenance.
- `internal/store/store.go`: those store contracts and issue-title patches.
- `internal/store/records.go`: refuse spec edits during code/verify/publish;
  count verification attempts separately from baseline tests.
- `internal/store/sqlite.go`: issue-title updates, executing approved-phase
  heartbeats, and CAS/event recovery of stale execution or abandoned queue rows.
- `internal/github/identity.go`: authenticated `/user` identity for command sync.
- `internal/config/config.go` and `internal/config/defaults.toml`: the planned
  `defaults.comment_on_failure` switch, enabled by default.
- `docs/M12-REPORT.md`: this report.

The pre-existing untracked task brief, `docs/tasks/M12.md`, was left unchanged.

## Decisions where the plan left implementation details open

1. `Start`, `Revise`, and `Execute` are synchronous. `Approve` records the
   decision and leaves `approved`; CLI approve and a successful GitHub approval
   then call Execute. This also gives M13 a separate dispatch point. Refine's
   Execute finishes `spec_approved` without installing code tools.
2. The real agent gate still requires `approved` at all existing checks. Clone
   and baseline therefore keep that status, carry heartbeats, and participate
   in stale recovery. A new hook calls an atomic exact-spec code claim only
   after the gate and registry checks. Spec edits are refused after the claim.
   Approval decisions, their status changes/events and their decision action
   audit are transactional.
3. Full single-branch clones avoid relying on an unauthenticated shallow fetch
   to obtain a days-old approved base, including private repos. Code always
   checks out the approved SHA before creating `patchbot/issue-N`.
4. Workspaces use private, unique per-phase directories and the existing jail.
   They are removed after work, including failures; redacted artifacts remain
   in SQLite. Startup uses Manager.Sweep for orphan directories older than the
   greater of 24 hours or run timeout + test timeout + stale threshold. No
   periodic scheduler is introduced in M12.
5. A baseline that dirties its checkout causes that disposable checkout to be
   deleted and re-cloned at the same pinned SHA before coding. Baseline output
   is untrusted prompt data, is recorded separately, and does not consume the
   maximum three verification attempts.
6. Usage events plus an accounting artifact restore conservative cost/token
   charges, uncertain usage, and elapsed active time. Waiting time does not
   consume the execution timeout. Nullable measured totals remain unknown when
   usage/pricing is unknown. Limits are frozen per run; a tighter current hard
   dollar ceiling still applies on resume. Unpriced models use the token cap,
   as the existing agent requires.
7. Config can tighten the policy, but cannot raise verification above three
   attempts, revisions above five, or waiting/approval TTL above fourteen days.
   A requested per-run dollar budget is clamped to the hard maximum.
8. Retry accepts failed/cancelled/rejected sources and creates a fresh run and
   budget. Reuse copies the exact canonical approved content into version one
   of the new run, records old-run/version provenance, retains the original
   approver and approval timestamp, and uses the ordinary gate. Changed issues,
   edited specs/comments, expired approvals or missing approval require refine
   and a fresh approval. Original models persist unless overridden; selecting
   a different provider resolves models from its effective configuration and
   provider defaults.
9. Existing patchbot branches or open PRs cause a refusal instead of overwrite.
   Draft creation is mandatory for non-green/unverified/salvaged work; there is
   no fallback that silently turns such work into a ready PR. An unsuccessful
   issue notification after PR creation is stored as a warning artifact.
10. Dry runs still require approval, make a disposable local commit, retain
    spec/diff/PR artifacts, and perform zero GitHub writes or pushes. CLI run
    prints the waiting spec; CLI approve prints all publication artifacts.
11. GitHub sync resolves the authenticated login from `/user` when no trusted
    identity is injected. It filters comments by the current spec/wait window
    and current spec comment ID, and uses live collaborator checks. It is one
    pass with no background goroutine or durable comment cursor. A hand-edited
    spec comment receives revise/UI-edit advice once per spec version.
12. UI edits are canonical structured spec versions, checked against the same
    content/path rules and bound to the currently fetched issue hash. GitHub
    comment edits are never imported.
13. Git/publication records use a detached context capped at five seconds so
    cancellation does not discard audit evidence. Revision decision audit and
    redacted revision-note detail are recorded separately.
14. CLI codes are 0 for successful controls, green runs and waiting; 1 for
    operational failure or draft results; 2 for usage; 3 for an active-issue
    conflict; 130 for cancelled execution. A successful cancel action returns
    0. Configuration and GitHub credentials are redacted before output/storage.
15. The test HTTP server is the repository's real httptest server over in-memory
    connections because this environment refuses TCP binds. Git and SQLite
    remain real; provider calls are scripted fakes and remotes are local bare
    repositories under `t.TempDir()`.

## Checks

Final command (exit **0**):

```sh
GOCACHE=/private/tmp/patchbot-m12-go \
GOLANGCI_LINT_CACHE=/private/tmp/patchbot-m12-lint make ci
```

Output (build metadata abbreviated):

```text
# fmt-check: clean; no output
 go vet ./...
 golangci-lint run
 0 issues.
 go test -race ./...
 ?   github.com/aryanpappu/patchbot/cmd/patchbot [no test files]
 ok  github.com/aryanpappu/patchbot/internal/agent (cached)
 ok  github.com/aryanpappu/patchbot/internal/cli 2.484s
 ok  github.com/aryanpappu/patchbot/internal/config (cached)
 ok  github.com/aryanpappu/patchbot/internal/execx (cached)
 ok  github.com/aryanpappu/patchbot/internal/github (cached)
 ok  github.com/aryanpappu/patchbot/internal/gitx (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm/anthropic (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm/fake (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm/gemini (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm/openai (cached)
 ok  github.com/aryanpappu/patchbot/internal/llm/providers (cached)
 ok  github.com/aryanpappu/patchbot/internal/logging (cached)
 ok  github.com/aryanpappu/patchbot/internal/runner 107.495s
 ok  github.com/aryanpappu/patchbot/internal/secret (cached)
 ok  github.com/aryanpappu/patchbot/internal/store 3.188s
 ?   github.com/aryanpappu/patchbot/internal/testhttp [no test files]
 ok  github.com/aryanpappu/patchbot/internal/tools (cached)
 ?   github.com/aryanpappu/patchbot/internal/version [no test files]
 ok  github.com/aryanpappu/patchbot/internal/workspace (cached)
 ?   github.com/aryanpappu/patchbot/internal/workspace/wstest [no test files]
 mkdir -p bin
 CGO_ENABLED=0 go build -trimpath -ldflags '...' -o bin/patchbot ./cmd/patchbot
```

Go emitted a nonfatal `writing stat cache ... operation not permitted` warning
for main-module version metadata under the default read-only module cache. The
CGO-disabled build succeeded. Dependencies were already downloaded; no module
cache relocation or dependency downloads were needed.

`git diff --check` also passes. The first complete race run exposed a
heartbeat/parking CAS race; it was fixed, covered by a deterministic regression,
and the final full race run above passes.

## Unfinished

No M12 implementation items are intentionally deferred. A live GitHub/demo-repo
smoke test is not run: the brief requires offline tests without live keys. M13's
serve loop, JSON API and periodic polling remain outside this task.
