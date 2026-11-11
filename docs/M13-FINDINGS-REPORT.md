# M13 findings fixes

All six findings from `findings-m13.md` are addressed without committing.
Existing workspace changes are preserved.

| Finding | Change | Deterministic regression |
| --- | --- | --- |
| 1. GET routes bypass Host validation | Every request passes Host validation, including page/API reads, artifacts and SSE. The policy accepts the configured hostname and bound numeric address at the bound port. Wildcard binds accept the connection's local numeric address. Loopback aliases require a loopback bind. | `TestReadRoutesValidateHost` sends same-origin GETs with an attacker Host to all read route types and requires 403. `TestHostPolicyForConfiguredAndBoundAddresses` covers public/loopback/wildcard binds, configured names, ports and aliases. |
| 2. Healthy queued jobs are recovered as interrupted | Starts persist an initial admission heartbeat. Revision and approval/execution decisions establish ownership when committed. One manager ticker refreshes every admitted queued/running job, continuing through graceful shutdown draining. Store heartbeats now include queued runs. | `TestQueuedJobsKeepOwnershipBeforeWork` holds both workers, queues start/revise/approve, observes a heartbeat after admission, then constructs another runner against a separate SQLite connection with a controlled recovery cutoff. The queued job must remain active. |
| 3. Reapproval commits without a worker | Approval denial resets the phase to `refine`. `ApproveAndExecute` reserves queue capacity before committing approval and its clone execution claim together. The HTTP API and GitHub sync use this combined operation; the reservation guarantees dispatch even if a request disconnects or graceful shutdown starts. | `TestReapprovalDispatchesAfterQueuedExpiry` advances the clock while approved work queues, then verifies phase reset and a second accepted dispatch. `TestReapprovalAfterCloneOrBaselineDenial` covers expiry and issue-freshness denial at clone and baseline, followed by successful reapproval/execution. `TestApprovalAdmissionFailureLeavesDecisionUnchanged` and queue-full API assertions verify no undispatchable decision is recorded. |
| 4. Quick restarts strand active runs | Every server maintenance pass runs the same `RecoverStale` operation as startup before syncing waiting runs. Orphaned queued, running and approved-clone rows become failed/interrupted and release their active-issue lock. | `TestMaintenanceRecoversRunsThatBecomeStaleAfterRestart` restarts with fresh orphan rows, proves startup preserves them, advances a controlled clock, and verifies periodic recovery and new admission for each issue. |
| 5. Pending work has no memory bound | `server.max_queued_runs` is a validated positive setting, default 16. Pending jobs live in a bounded list; goroutines start only when work can execute. Admission reserves capacity before lifecycle mutations. Full admission returns HTTP 429, kind `queue_full`, with a clear retry message. Cancellation immediately frees queued capacity. | `TestPendingQueueRejectsExcessWork` occupies both workers and all sixteen pending places, then verifies start/approve/revise/retry return 429 without creating runs or changing decisions. `TestManagerBoundsReservationsAndReusesCancelledCapacity` uses a configured capacity of one, exercises release/cancellation, and proves existing reservations survive disconnect/shutdown. Config tests verify the default and reject invalid capacities. |
| 6. Configured localhost bind rejects legitimate mutations | `Serve` retains access to the configured hostname while recording the actual numeric bound address and port. Loopback-bound listeners accept localhost and numeric loopback aliases. | `TestConfiguredLocalhostSurvivesNumericBind` uses a deterministic listener with a configured `localhost:0`, then sends mutations using localhost, case variants and both loopback address families at the actual bound port. |

The six regressions were observed failing against the unfixed behavior. The
corrected expiry/clone/baseline regressions were additionally run in an isolated
copy with the old retained-phase behavior restored: the queued test retained
`clone`, and all four clone/baseline cases retained `baseline`. They pass with
the phase reset. Tests use channels, observed heartbeat writes and controlled
clocks rather than waiting for production stale/approval durations. No external
network, provider keys or live GitHub data are required.

The current API/ownership contract is documented in
[`internal/server/README.md`](../internal/server/README.md).

Validation completed successfully:

- `make ci`: formatting check, `go vet ./...`, `golangci-lint run` (`0 issues`),
  `go test -race ./...`, and the production build all passed.
- The final listener-synchronization assertion also passed under `-race`;
  formatting, vet and lint were checked again afterward.
- `git diff --check` passed. No commit was created.

Go's build cache and golangci-lint's cache were placed under `/private/tmp`
because the shared workspace profile restricts cache writes outside the
workspace.
