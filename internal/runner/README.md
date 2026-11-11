# Runner and run control

Construct `runner.New(ctx, cfg, db, options)` once per process. It recovers
expired execution heartbeats and waiting runs; the caller owns the store.
`Start` runs preflight and the read-only stage synchronously. Fix/refine runs
return waiting, after their workspace is removed. Investigations save a report
and post one comment, without a specification or code tools.

`Approve(ctx, id, actor)` records an exact-version decision and leaves the run
approved. `Execute(ctx, id)` continues fix work synchronously; for refine mode it
finishes with `spec_approved` without coding. CLI approve calls both. A future
server can dispatch Execute separately. `Revise` reruns refinement immediately;
`EditSpec` appends a canonical UI version. Both invalidate the prior approval.
`Reject`, `Cancel`, `Retry` and `SyncWaiting` share these lifecycle rules.

`SyncWaiting` is one pass, suitable for the M13 server's 60-second timer or
`patchbot runs --sync`. It polls only waiting, non-dry runs and requires an
unedited command newer than the current spec/wait window, an allowlisted actor,
live collaborator membership, and an authenticated bot identity. The production
GitHub client resolves the identity from `/user`; tests can supply `BotLogin`.
The exact version/hash from that poll remains bound to the atomic decision;
an edit during command handling requires a new command for the new spec.

The agent gate runs against the real store while status remains `approved`.
Clone and baseline phases carry heartbeats in that state. After the gate has
validated the spec and built its registry, the agent's phase hook calls the
store's atomic StartCode claim. The claim requires the latest version/hash and
approval and prevents spec writes during code, verification and publication.
Every lifecycle mutation has a CAS and an event in the same transaction. A
persisted generation advances on lifecycle changes, so failures and transitions
from obsolete workers cannot modify a newer revision, even if status repeats.
Operator edits guard the prior status, generation and spec version/hash while
saving the new spec, waiting status, event and audit action in one transaction.
The agent still owns all harness tests and repairs, capped at three attempts.

Usage events retain conservative token/cost reservations, including unknown
usage. An accounting artifact also retains active elapsed time across new CLI
processes, stages and revisions. Measured nullable store totals remain nullable;
unknown prices still use the token cap. Run limits are frozen at creation and
clamped to the current hard dollar cap when resumed.
The remaining active-time budget bounds the work context before preflight,
clone and baseline, as well as the agent. Waiting time remains excluded.

Dry runs use the same approval gate and local commit, store the spec/diff/PR
artifacts, and perform no push or GitHub write. The CLI prints those artifacts.
Publication rechecks issue/spec integrity, validates changed paths through the
workspace policy, checks for existing branches/PRs, and uses gitx's guarded
commit/push. Missing test commands, failed tests or salvage require draft PRs.
Immediately before push and again before PR creation, publication reloads the
approval and persisted status/generation and checks approval TTL and spec/issue
integrity. Cancellation and expired approvals stop the next external write.

Infrastructure comes from `Options`: raw provider constructors, GitHub client,
secret-backed token source, clock, clone and executor functions. Production uses the
existing adapters, executor, workspace jail and git guards. Tests use the fake
provider, the repository's in-memory httptest server, SQLite and local bare git
remotes. No network or real provider keys are needed.

CLI exit codes are 0 for completed controls, green runs and waiting runs; 1 for
errors or draft results; 2 for invalid usage; 3 for the active-issue conflict;
and 130 for a cancelled execution. A successful `cancel` control returns 0.
Failure comments default on; `[defaults] comment_on_failure = false` suppresses
them. Cancellation, dry runs and provider-auth failures never post them.
