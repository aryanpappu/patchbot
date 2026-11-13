# M20 repository memory report

Implemented on `m20-memory` per `docs/planning/step2/PLAN.md` (M20) and the trust
model in `docs/planning/step2/codex-round1.md` section 1. No dependency was added.

## Delivered

- Migration `0003_memory.sql`: `memory_entries` (typed observations and prose
  lessons, scoped by canonical `owner/name`) and `memory_actions` (audit). CHECK
  constraints forbid prose in observations, harness-authored lessons and
  model-authored observations. The forward-only checksummed ledger is unchanged.
- `internal/memory`: command-policy digest, deterministic bounded selection,
  observation/lesson rendering and the only constructors for new entries.
  Observations take an exit code, timeout flag and SHAs; there is no field for
  command output.
- Runner: one selection per refine/investigate/code stage, frozen before the
  model runs and recorded as a `memory_selection` artifact (IDs, kinds, content
  hashes, bytes, policy digest) plus a `memory.selected` event. Observations are
  written after the baseline test and after the final verification commit.
  Model lessons from the code stage are stored `pending`.
- Agent: memory is appended to the stage's first user message as
  `untrusted_data kind="repository memory"` after `agent.MemoryNote`. The system
  prompts are unchanged. `propose_lesson` is registered only when
  `memory.enabled`, collects at most three redacted proposals of 600 bytes, and
  is inert during the run.
- Config `[memory] enabled = true, max_entries = 12, max_bytes = 4096,
  observation_ttl = "720h"`, validated (1-100 entries, 256-65536 bytes, TTL > 0).
- CLI `patchbot memory list|approve|reject|forget|add`.
- API `GET/POST /api/memory`, `POST /api/memory/{id}/approve|reject|forget`,
  using the existing bearer auth, host checks and cross-origin protection, and a
  memory panel on the `/runs` page of the Vue UI.

## Trust properties and their tests

| Property | Test |
| --- | --- |
| Repository separation | `memory.TestSelectEligibility/repo separation`, `store.TestRetrievalCandidatesFilterInSQL` |
| Pending, rejected and revoked never retrieved | `memory.TestSelectEligibility/pending rejected revoked never retrieved`, runner second-run check |
| Stale observations (policy digest or TTL) not retrieved | `memory.TestSelectEligibility/stale observations excluded`, `TestPolicyDigestChangesWithPolicy` |
| Zero-exit stdout with instructions never becomes trusted prose | `runner.TestMemoryTypedObservationsLessonsAndFrozenSelection` (echo of a poisoned message as the test command), `memory.TestObservationIgnoresCommandOutput`, store CHECK tests |
| Memory never in the system prompt | `agent.TestMemoryNeverEntersSystemPromptOrPolicy`, `TestRefineAndInvestigateMemoryInUserTurn`, runner request checks |
| Memory cannot alter policy, scope or approval | `agent.TestMemoryNeverEntersSystemPromptOrPolicy` (identical tool specs), `runner.TestMemoryCannotAlterPolicyOrApproval` |
| Caps respected and deterministic | `memory.TestSelectCapsAndDeterminism` |
| Frozen selection recorded | runner test: per-stage artifacts and events, hashes match entries, approval after selection leaves the record unchanged |
| Disabled mode | `runner.TestMemoryDisabledRetrievesAndWritesNothing`, `memory.TestDisabledReadsNothing`, CLI notice |
| Lesson redaction and bounds | `memory.TestLessonRedactionAndBounds`, `agent.TestProposeLessonCollectsBoundedRedactedProposals`, API 600-byte check |
| Operator controls, audit and auth | `store.TestMemoryLifecycle`, `server.TestMemoryAPI`, `server.TestMemoryAPIRequiresAuthAndSameOrigin`, `cli.TestMemoryCommands` |

`make ci` (fmt-check, vet, golangci-lint, `go test -race ./...`, build) passes.

## What forget leaves behind

`forget` sets status `revoked`, erases lesson text and records the actor and
time. Retrieval eligibility is removed immediately. These remain: the content
hash, `memory_actions` audit rows, `memory_selection` artifacts and
`memory.selected` events (IDs and hashes only), LLM step transcripts of runs
that already received the entry, and any existing SQLite backup or WAL file.

## Other changes

- Store tests and the doctor test no longer hard-code the migration count, so
  0004/0005 can be added without editing them.

## Open issues

- The cap is entries plus bytes; there is no separate token allowance. 4096
  bytes is roughly 1,000 to 1,500 tokens and is charged by the existing budget.
- Observations record the harness test command only (baseline and final
  verification). Agent `run_cmd` results are not observed.
- Lessons can be proposed only in the code stage, and are persisted when the
  code stage ends (including salvage and failure), not at publication.
- No retention or pruning of old revoked or stale entries yet.
- Memory decisions are not routed through GitHub comments; they are local
  CLI/UI only.
