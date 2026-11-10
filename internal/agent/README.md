# Agent engines

`Loop.Run(ctx, session, limits)` owns provider retries, accounting, compaction,
ordered tool dispatch, and bounded endings. Supply a raw provider with adapter
retries disabled. `Env.Redactor` is shared with the registry and executor; configure
it before constructing them. A session belongs to one goroutine.

The stage methods replace any caller-supplied registry with the appropriate fixed
set. `Refine` saves a spec version and returns `awaiting_approval` or `needs_info`.
`Investigate` returns a report. Neither stage can execute commands or write files.

Coding requires a capability:

```go
token, err := agent.Check(ctx, db, runID, agent.IssueHash(title, body), agent.ApprovalOptions{
    TTL: configuredTTL, MaxRevisions: configuredMaxRevisions,
})
if err != nil {
    return err
}
result, err := loop.Code(ctx, token, agent.CodeRequest{
    RunID: runID, Title: title, Body: body,
    Changes: gitRepo, // *gitx.Repo implements HasChanges
    TestCommand: "test", Session: session, Limits: limits,
})
```

`Check` returns a private concrete type required by `Code`. Omitting the token,
passing nil, or passing a caller-defined substitute fails compilation (tested
with a negative fixture). The token's type and fields are unexported; callers
cannot construct a valid token. A failed check returns a zero token, which is
rejected again at runtime if a caller ignores its error.
Code rechecks the persisted approval at entry and immediately before building its
write registry, and verifies the canonical spec content and absence of questions.
The runner must leave the run `approved` until this entry check has completed.

The harness uses `Changes.HasChanges`, not model claims, and runs the configured
command after each submission. Repair retains the conversation, limits, turn count,
and budget. Only `CodeResult.CanPublish` permits the runner to publish. `Draft`
requires a draft, `Salvage` identifies an early stop with changes and completed
verification (or explicitly unconfigured tests), and `NoChanges` means no PR.
Cancellation, refusal, auth, policy, recorder, and harness failures cannot authorize
publication. A salvageable provider error remains non-nil along with its result.

The runner owns status transitions, questions/comments, baseline testing, final
diff policy/integrity checks, and publication. These engines never commit or push.
Pass a shared `Session` across refine/revisions/code to carry the run's token and
cost charges. Stage turn caps start at stage entry; repairs share that cap. When
restoring after a process restart, seed conservative `Cost`, `Tokens`, and
`AccountingUncertain` from recorded usage events. NULL store totals mean unknown
and must not be restored as zero. A zero cost cap disables that cap; the runner
supplies the configured default of 2,000,000 micro-USD. An unset deadline gets a
30-minute stage deadline that persists across repair re-entry.

Implementation choices beyond the plan's detail:

- Before each provider attempt, reserve at least the last reported input and a
  conservative serialized-byte estimate for the current request, plus maximum
  output, using `llm.Pricing.Reserve`. Missing usage retains that reservation and
  leaves persisted measured usage/cost unknown. Failed requests conservatively
  retain reservations too. Retries count as paid attempts and consume turns.
- Spec arrays preserve order; nil and empty arrays canonicalize identically.
  Markdown includes a deterministic canonical JSON marker so the approved fields
  survive workspace deletion. Code rejects altered rendering or hash mismatch.
- Every call in a batch receives an ordered result. Siblings after a validated
  terminal call or stop are recorded as skipped rather than executed.
- A truncated reply executes no calls, even when some argument prefixes parse.
  Invalid arguments otherwise become error results; subsequent history uses an
  object placeholder where needed to satisfy provider conversation invariants.
- JSON-encoded untrusted blocks escape delimiter-like source text. Redaction
  precedes encoding and truncation. Opaque provider signatures remain untouched
  in live history and are omitted from audit records.
- Completed operations are audited using a detached context capped at five
  seconds, so cancellation does not erase their records. Recording failure stops
  further work. Safety and harness failures are excluded from salvage.
- This checkout's PLAN-fable ends its security sections at 8.10. The approval
  requirements used here are its section 4.7 and the overriding merged PLAN.
