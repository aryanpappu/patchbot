# M21 opt-in reviewer report

Built in the `m21-reviewer` worktree from `development` at `26e319b`. One
conventional commit, not pushed. No new Go dependency.

## Delivered

- `[review]` config: `enabled = false`, `model = ""` (run's refine model),
  `max_repairs = 1` (0 or 1), `max_turns = 15`, `reserve_fraction = 0.15`
  (0 to 0.5), `salvage_draft = false`. Validated, shown by `config show`,
  documented in `patchbot.example.toml`.
- Frozen selection: the approve decision writes provider, model, repairs,
  turns, reserve and salvage policy to `runs.review_json` in the same
  transaction as the approval (UI, CLI, GitHub `/approve` and retry reuse).
  Execution reads only that snapshot; a run approved with review off, or before
  this migration, stays unreviewed.
- Migration `0004_review.sql`: `runs`/`steps` rebuilt so phase allows `review`,
  `runs.review_json`, new `reviews` table (round, spec version/hash, diff hash,
  base SHA, tree ID, verdict `approve|request_changes|incomplete`, reason,
  findings, criteria, provider, model). The migrator gained a
  `-- patchbot:foreign_keys=off` mode (SQLite's documented table rebuild):
  enforcement off on its one connection, transaction, `foreign_key_check`
  before commit, enforcement restored.
- `submit_review` tool: verdict, findings (`file`, `line`, `severity`
  critical/major/minor/info, `summary`, at most 30), criteria by ID (every AC
  exactly once). `approve` is rejected with any unmet criterion or blocking
  finding; `request_changes` needs a finding.
- `agent.Loop.Review`: refine read-only registry plus `submit_review`, its own
  prompt and conversation. Input is the approved spec, the bounded diff (64 KiB,
  truncation flagged) and harness evidence (status, attempts, exit code,
  timeout, 8 KiB output), all as untrusted JSON-string blocks. The coder's
  summary is never supplied. Accounting is forked from and absorbed back into
  the run session. Refusal, deadline, budget, turn limit, stuck loop, provider
  errors and synthesized/malformed submissions return `Incomplete`.
- `agent.CodeRequest.Review` hook: called for every publishable result.
  `request_changes` starts at most one repair cycle inside the same approved
  code stage (no new write capability is created), with findings as untrusted
  feedback, a fresh turn allowance, exactly one more harness attempt and a
  fresh review of the new diff. A hook error clears `CanPublish`.
- Runner: `review` phase transitions and `review.verdict` events, one `review`
  artifact per round, `reviews` rows. Diff hash = SHA-256 of
  `[base SHA, git write-tree of the complete worktree, bounded diff]`.
  Publication recomputes it before commit and compares the commit tree with the
  reviewed tree after commit. No approve verdict: run fails with
  `review_failed`, or with `salvage_draft` a draft PR headed "Draft: independent
  review did not pass". A changed diff fails with `review_stale`. The PR body
  and issue comment carry verdict, round, model, hashes, criteria and findings
  as advisory evidence.
- Shared budget: coding and repair get the run limits minus
  `reserve_fraction` of tokens, cost and remaining active time; the reviewer
  uses the full remainder. Review LLM steps carry phase `review` and count in
  run totals, usage events and the accounting artifact.
- API detail includes `reviews`; `review` is a public text artifact
  (`attempt` = round); CLI `runs show` prints review rounds; the UI timeline has
  the `review` phase and an "Independent review" panel.
- Cancellation, heartbeats and stale recovery cover `running/review`; specs
  cannot be saved while a run is in `review`.

## Tests (fake provider, `-race`)

- Runner: approve path (frozen selection, hashes, PR evidence, reviewer input
  excludes coder summary, read-only tools, shared totals);
  request_changes -> repair -> approve (published bytes are the repair, second
  diff hash cited, two verify/review phases); still failing after repair (fails
  without salvage, marked draft with salvage); malformed, refusal and provider
  auth failure fail closed; workspace changed after review is refused
  (`review_stale`, nothing pushed); cancel during review; disabled review is
  unchanged even if config is enabled after approval.
- Agent: approve path keeps the coder conversation and accumulates accounting;
  timeout, budget, refusal, malformed, provider error and invalid verdicts
  (unmet approve, major finding with approve, request_changes without findings)
  are incomplete; repair is capped at one; hook error blocks publication.
- Store: rebuild keeps child rows (no cascade) and restores foreign keys,
  frozen selection, review round/spec binding and conflicts, stale `review`
  recovery. Gitx: tree ID covers untracked files and equals the committed tree.
  Config: review validation. Server: detail `reviews`, review steps and
  artifact over the API.
- Follow-up tests: truncated diff cannot approve, sequence preserved by the
  rebuild, marker parsing and unmarked `DROP TABLE` refusal, PR findings
  neutralized, `max_repairs = 0` fails on request_changes.
- `make ci` (fmt, vet, golangci-lint, race tests, build) is green.

## Open issues

- `0003_reserved_m20.sql` is a placeholder because the loader requires
  contiguous versions. Delete it when M20's `0003` lands; two `0003` files fail
  loudly. Any local database that applied the placeholder must be recreated.
- The repair cycle reuses the coder's conversation and remaining coder budget;
  if coding consumed it, repair stops early and the second review sees a
  salvage result (draft at best).
- No seeded-defect comparison against live models yet (quality, cost and
  latency versus review off); review stays off by default until that evidence
  exists.
- `0004` rebuilds `runs` and `steps` from their 0002 shape. If M20's `0003`
  adds columns to either table, `0004` must be updated to carry them over
  before merging (merge M20 first, then rebase this migration).
- Second-round review can still run short of budget after an expensive repair;
  that fails closed rather than publishing.

## Review follow-ups applied

An independent go-reviewer pass found no contract violation; these medium
items were fixed before commit:

- An approve verdict on a truncated diff (gitx cap or the 64 KiB reviewer cap)
  is recorded as incomplete (`diff_truncated`) and cannot pass.
- The foreign-keys-off marker is parsed from the first line (BOM, CRLF and
  whitespace tolerant), and a migration containing `DROP TABLE` without it is
  refused, so a rebuild can never cascade-delete child rows.
- The rebuild preserves the `runs` AUTOINCREMENT sequence, so deleted run IDs
  (which name branches) are never reused.
- Findings in the PR body are rendered inside code spans: no mentions, links,
  images, HTML or issue references are interpreted.
- A repair is skipped (failing closed at once) when the coder's share of the
  budget is already spent; the repair feedback shows the raised attempt cap.
