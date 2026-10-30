# backend/internal/server: Merge-candidate verification and the merge queue

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

## Merge-candidate held-pass queue (`merge_candidate_queue.go`, ADR-092 / E83.85 #4200)

Serializes ADR-090 merge-candidate verify passes per `(repository, base ref)`:
ONE live pass per base, every other one held FIFO with no runner slot. The
store is `backend/internal/mergequeue` (its README is the persistence
contract); this module owns WHEN each store method runs and what reason a
settle carries.

- **Wiring.** `Config.MergeQueue` (`mergequeue.Store`), set in
  `cmd/fishhawkd/serve.go` to `mergequeue.NewPostgresStore(pool)` next to
  `Config.Concurrency`. nil (or no `Config.GitHub`) = serializer OFF:
  `startMergeCandidateVerifyPass` takes ADR-090's direct start unchanged, so
  every fake-backed test is unaffected.
- **Enqueue.** `startMergeCandidateVerifyPass` routes `base_advance` and
  `conflict_resolution` through `serializeMergeCandidateVerifyPass`: no
  declared verify command → `not_required`, nothing queued; otherwise Enqueue
  (new → held at the tail; same base → re-anchored in place) and pump the
  base, then answer from the run's entry. HELD → start state `held` with its
  position and the live run (no trigger, no re-open, no slot). Admitted by
  this pump → the admission's answer. Already live → the pass is started for
  the new head directly. `fan_in` is NOT serialized (it gates the
  consolidated review that precedes approval); it still routes through
  `local-verify:<host>` because routing keys on the live trigger. The D5
  fan-in hold starts its pass as `fan_in` whatever the head classifies as
  (#4812, § "Consolidated fan-in verify hold").
- **Pending replacement (D2).** `beginMergeCandidateVerifyPass` (the direct
  start): a live trigger for an OLDER head whose implement stage is
  `pending`/`awaiting_host_dispatch` is replaced by appending a trigger for
  the new head carrying the old `prior_state`/`reparked_review_stage_id`;
  no second re-open. A dispatched/running pass keeps the "stage is in state"
  refusal.
- **Pump + admission.** `pumpMergeCandidateQueue(base)` loops: a live entry
  whose run is terminal is dropped `run_terminal`; otherwise `AdmitNext`.
  `admitMergeCandidateEntry` re-reads the run and PR: run terminal → dropped
  `run_terminal`; PR merged → dropped `pr_merged_externally`; PR closed →
  dropped `pr_closed`; behind → `advanceRunBranch` under the system actor
  (`system:merge-candidate-verify`, so `branch_rebased` + the lineage vouch
  carry system provenance); conflict → ejected `advance_conflict`
  (`fishhawk_rebase_run_branch` resolves and re-queues); then re-anchor and
  act on `mergeCandidateVerifyState`: unverified → start (refusal → ejected
  `pass_not_startable`, except a `Transient` refusal — begin's
  superseded-fix-up arm missing its bounded admission fence, #4391 — which
  `Unadmit`s like the transient failures below), in_flight → phase `verifying`, passed/not_required →
  phase `passed`, failed → ejected `verify_failed`. A TRANSIENT failure (run
  or PR unreadable, behind-probe error, non-conflict advance refusal,
  re-anchor error) `Unadmit`s the entry back to held at its original place
  and stops the pump. A crash mid-admission is recovered by the store's
  stale-admission re-claim on the next pump.
- **Admission context (#4303).** Each admitted entry (its `admitted` row and
  `admitMergeCandidateEntry`) runs on `mergeCandidateAdmissionContext`:
  detached from the caller's cancellation and bounded by a FRESH
  `mergeCandidateQueueHandOffBudget`, on every trigger path (periodic pump,
  webhook PR-resolution hook, verdict and cancel hooks, the re-open release,
  a rebase's serialize). So neither a departed webhook sender nor fishhawkd's shutdown
  signal can cut the re-anchor, the pass start or the recovery `Unadmit` and
  strand an entry live in phase `admitted`. The caller ctx gates only the
  TOP-LEVEL `AdmitNext` (and the live-entry check): a cancelled pump claims no
  new entry. An admission that settles its entry pumps the base again on its
  own detached context, so a nested chain's admissions keep running after a
  shutdown signal until `Server.Shutdown`'s drain closes (a cut admission is
  re-claimed after `mergequeue.AdmissionStaleAfter`). A webhook hand-off chain
  is therefore no longer bounded as a whole: each admission is, and the chain
  is bounded by its number of held entries (and
  `mergeCandidateQueueMaxAdmissions` per pump). `mergeCandidateQueueOnPRResolved`
  keeps its own detached, budgeted context for its entry read and settle.
- **Verdict / merge / close hooks (D4).** `recordMergeCandidateVerified`
  (non-duplicate append) → `mergeCandidateQueueOnVerdict` for the run's LIVE
  entry anchored at that head: failed → ejected `verify_failed` + pump;
  passed → phase `passed` (holds the line until merge, or until a re-open
  releases it, below); not_executed →
  phase `unverified` (holds the line; the operator's next
  `fishhawk_rebase_run_branch` re-triggers). `resolveReviewStageOnMerge`
  calls `mergeCandidateQueueOnPRResolved` FIRST, before the ADR-036
  implement-review early return (a forge-side merge is a fact regardless of
  review bookkeeping): merged → live entry `merged` / held entry dropped
  `pr_merged_externally`; closed → dropped `pr_closed`; settling a live entry
  pumps its base.
- **Release + read seams.** `releaseMergeCandidateQueueForRun(ctx, runID,
  reason)` drops the run's active entry and pumps (the cancel hook passes
  `run_cancelled`); `mergeCandidateQueueForRun` returns the run's active
  entry, held position and live run for `GET /v0/runs/{id}`'s
  `merge_candidate_queue` block (omitted with no entry, a nil store or a read
  error) and the merge gate's `409 merge_candidate_held` (a HELD run's merge
  is refused before the behind check, naming its position, the live run and
  that admission is automatic; next step `fishhawk_get_run_status`). With
  ADR-093 slice 1 that refusal answers only a run that is NOT merge-queue
  eligible: an eligible held run's merge call records merge intent and
  answers `202 queued` instead (§ "Merge queue").
  `releaseMergeCandidateQueueOnReopen(ctx, runID, reason, heldToo)` is the
  re-open release below.
- **Re-open release (#4352).** A run re-opened for agent work, or whose head
  was reset, stops holding the line: `releaseMergeCandidateQueueOnReopen`
  reads the run's active entry and settles it `dropped` through
  `settleMergeCandidateQueueEntry` (the `merge_candidate_queue_settled` row
  carries `reason`, `from_state`, `phase`, `anchored_head_sha`; a LIVE entry
  pumps its base so the next held entry is admitted at once). Call sites, each
  on `context.WithoutCancel` like the cancel hook: `fixupStageAs` AFTER
  `run.FixupStage` admits the re-open (`reopened_for_fixup`; covers the HTTP
  verb, campaign auto-drive, acceptance triage and the ADR-090 D6 route, which
  finds the entry already ejected `verify_failed` and is a no-op);
  `startConflictResolutionPass` AFTER its re-open
  (`reopened_for_conflict_resolution`); `handleResetRunBranch` AFTER the
  force-update (`head_reset`). The two agent re-opens drop a HELD entry too
  (admitting it mid-pass would auto-advance the branch under the running agent
  and eject it `pass_not_startable`); the reset drops a LIVE entry only, since
  admission re-anchors a held one to the PR's current head (ADR-092 D2). A
  refused re-open or reset returns before the call and touches no entry; an
  unreadable entry WARN-logs and changes nothing. The verdict is untouched: a
  no-op fix-up's head still reads `passed`, only the line POSITION is lost
  (the release fires at routing time, before the agent can say whether the
  head moves). The run re-enters at the TAIL on its next serialized pass (a
  rebase or base advance), per ADR-093 Q2. A rebase of the live holder is NOT
  a release: it re-verifies the new head as the line's own work (D2).
  **Latency.** The pump is synchronous: releasing a LIVE entry runs the next
  admission (auto-advance merge + pass start) before the verb returns, and an
  admission that ejects its entry admits the next, so the fix-up's
  orchestrator dispatch (the release sits BEFORE `Orchestrator.Advance` in
  `fixupStageAs`) and the HTTP fix-up 200 / rebase 202 / reset 200 can each
  be delayed by up to (number of held entries ejected in the chain + 1) ×
  `mergeCandidateQueueHandOffBudget` (one budget per admission the chain
  runs, the final non-ejected admission included), not one budget. Each admission has its
  own detached budget (#4303); the chain is bounded by the held entries and
  `mergeCandidateQueueMaxAdmissions`. Tests: `merge_candidate_queue_test.go`
  (`TestMergeCandidateQueue_FixupReopen*`, `RefusedFixupKeepsTheLine`,
  `FixupNoOpKeepsPassedVerdict`, `ReleasedHolderReentersAtTail`,
  `ConflictResolutionReopenReleasesTheLine`) and `reset_branch_test.go`
  (`TestResetRunBranch_*MergeCandidate*`).
- **Audit categories** (INTERNAL — not issue-comment activity; registered in
  `audit/categories.go`, pinned by `TestKnownCategories_MergeCandidateQueue`):
  `merge_candidate_queue_held`, `merge_candidate_queue_admitted` (`readmitted`
  on a stale re-claim), `merge_candidate_queue_reanchored` (`at_admission` on
  the auto-advance), `merge_candidate_queue_settled` (state + `reason` +
  `from_state`). Best-effort, system actor, written after the store's
  transaction committed; a lost row never unwinds a queue move.
- **Periodic pump.** fishhawkd pumps every base `mergequeue.Store.ActiveBases`
  reports (a base with a held or live entry) once at boot and then every
  `MergeCandidateQueuePumpInterval` (1 minute) via
  `RunMergeCandidateQueuePump` / `PumpMergeCandidateQueues`, started from
  `backend/cmd/fishhawkd/serve.go`. The signal ctx it runs on gates only the
  ticker and NEW admissions; an admission the loop already claimed runs
  detached to completion. The loop joins `Server.bgMergeQueuePump`, so
  `Server.Shutdown` drains it (and the admission it is running) bounded by
  `Config.ShutdownTimeout` (default 15s, not the 120s admission budget): an
  admission whose tail outlasts the drain window is still cut at process
  exit, as on the webhook/cancel paths `http.Shutdown` drains under the same
  timeout. Boot-window residual: the `Add` runs inside the goroutine serve.go
  launches, so a SIGTERM landing between the `go` and that `Add` lets
  Shutdown miss the loop (no drain, not a panic: `Wait` at zero returns at
  once), the exposure the other bg groups share. Each tick reuses `pumpMergeCandidateQueue`,
  which drops a LIVE entry whose run is terminal (`run_terminal`) and re-claims
  a stale admission, then admits the next held entry. So a run that fails or
  is swept by any path other than the REST cancel (which releases at once)
  holds its base for at most ONE pump tick, and a crash mid-admission
  self-heals within `mergequeue.AdmissionStaleAfter`. Manual pump:
  `fishhawk_rebase_run_branch` on any held run of that base (its re-enqueue
  pumps the base). Not covered: a merge that lands without the
  `pull_request.closed` webhook leaves a `passed` entry live until a pump
  settles it some other way (#4289).
- **Tests.** `merge_candidate_queue_test.go` runs the REAL rebase handler,
  `mergequeue.PostgresStore`, Postgres run AND audit repositories over ONE
  pgtest pool with a multi-PR fake GitHub: the end-to-end wave (A live, B
  held, re-anchor, hand-off on merge with auto-advance under the system
  actor, B admitted through `local-verify:<host>`), pending replacement, the
  hand-off under an unsettled implement review, and one test per eject/drop
  mode. #4303 adds: the periodic pump's admission surviving a cancelled signal
  ctx (`PeriodicPumpSurvivesSignalCancellation`), a strictly later deadline
  per chained admission (`AdmissionBudgetIsPerEntry`), a cancelled pump
  admitting nothing (`CancelledPumpAdmitsNothingNew`), Shutdown draining the
  loop and staying bounded (`TestShutdown_DrainsMergeCandidateQueuePump`,
  `TestShutdown_MergeCandidateQueuePumpDrainBoundedByDeadline`, each taking
  the `entered` handoff so the loop's `Add` happens-before `Shutdown`), and
  `AdmissionBranches` (already failed / in_flight / passed at admission, an
  unresolved post-merge head, a re-anchor error, no installation, and a
  pending-replacement trigger-append failure, which TODAY ejects
  `pass_not_startable`).

## Merge queue (ADR-093 slice 1, E87.4 / [#4295](https://github.com/kuhlman-labs/fishhawk/issues/4295))

ADR-093 D1, D2 and D7 on the ADR-092 queue above: the same store rows, FIFO
order, one-live-per-base rule and lock order, plus the operator's merge
intent (`merge_intent_at`, migration 0107; `backend/internal/mergequeue`
README). Merge-on-pass is slice 2 (§ "Merge-on-pass (ADR-093 D3/D6)"
below); eject/promote and durability are slices 3-4.

- **Eligibility (`merge_queue_eligibility.go`, D1).** One predicate decides
  queue ADMISSION, not only the merge. It never writes; every read error is
  `undeterminable`, which is NOT eligible. Four legs, each on the existing
  single-source helper: (a) review gate approved — the LATEST implement
  review round (`implementReviewRound`, read-only) has a TERMINAL entry for
  every configured agent reviewer (no started round, fewer terminals than
  configured: `review_pending`; an unreadable round: `undeterminable`),
  review coverage complete, and when the review stage declares an approval
  gate a LIVE forge approval by an `approvals.members` identity (any live
  approval when members is empty) and no reviewer whose latest
  non-comment review is CHANGES_REQUESTED (`changes_requested`, approval
  gate declared only). Those approval sub-legs apply to the ENTRY variant
  and the anchor-less ADMISSION variant (the ADR-092 rebase-path enqueue);
  an intent-bearing ADMISSION evaluates NO approval, only the ADR-112 R2
  veto through the forge-approval fold (merge-on-pass's in-lock fold judges
  the approval and waits, then yields, ADR-112 R3), and the MERGE variant
  reads no forge review at all. A
  spec that was READ and declares no review stage or no approval gate passes;
  an absent, unreadable or unparseable spec is `undeterminable` (fail
  closed). (b) acceptance merge-eligible and not stale. (c) required checks
  green at the PR head, classified by `mergeoutcome.ClassifyMergeCI` against
  `runs.required_checks_snapshot`; **zero observed check runs is never green
  — it is `undeterminable`, and a nil snapshot is judged on the observed set
  only when at least one check run exists;** with a nil snapshot an observed
  red is NOT GREEN, not red: the admission variant admits, the merge variant
  holds `checks_not_green`, the entry variant keeps `checks_red`. (d) no open blocking concern: an
  open concern whose provenance is `concern.ProvenanceFirstOfficer`
  (`"first_officer"`) or whose severity is `planreview.SeverityHigh`
  (`"high"`); medium/low stay advisory (ADR-027). No requirement-arbitration
  concern CATEGORY exists (`requirement_arbitration` is a page event), so the
  leg matches none. Reason codes: `not_approved`, `changes_requested`, `review_pending`,
  `review_coverage_incomplete`, `acceptance_not_eligible`,
  `acceptance_stale`, `checks_not_green`, `checks_red`,
  `blocking_concern_open`, `undeterminable`. The ADMISSION variant differs
  only in leg (c): it refuses an observed RED required check and nothing else
  (a freshly advanced head is legitimately pending; the verify pass is the
  gate there). The entry-aware variants (`mergeQueueAdmissionEligibilityAt`,
  `mergeQueueMergeEligibility`, which requires checks GREEN) take the entry's
  intent anchor. `ApprovalBasis` is `live` when the entry or anchor-less
  admission variant passed leg (a) on a declared gate through a live
  approval, and `""` otherwise: an intent-bearing admission and the merge
  variant evaluate no approval (the ADR-093 D6 approval transfer at
  admission is removed, #4632; merge-on-pass takes the basis from the fold).
- **Enqueue arm (`merge_run.go::mergeQueueEnqueueArm`, D2).** With
  `Config.MergeQueue` wired, a `merge_base_behind`,
  `merge_candidate_unverified` (`unverified`, or `in_flight` for a run that
  already has an active entry) or `merge_candidate_held` refusal of an
  ELIGIBLE run becomes `202 queued:true` (`queue_state`, `queue_position`,
  `live_run_id`, `entry_id`, `base_ref`, `anchored_head_sha`,
  `intent_sequence`, `already_queued`, `message`); `merge_queued` stays false
  and NO `merge_verdict_recorded` row is written. Every pre-gate refusal keeps
  its order; `merge_candidate_verify_failed` / `merge_candidate_check_failed`
  are never enqueued; an up-to-date verified (or ADR-096 transferred) head
  merges inline as before. An ineligible or undeterminable run gets today's
  refusal plus the additive `details.merge_queue_ineligible` codes, with
  nothing written.
- **Record before commit.** The chained `merge_queue_entered` row (operator
  actor; payload `run_id`, `repo`, `base_ref`, `anchored_head_sha`,
  `gate_code`, the verdict prose slice 2's merge-on-pass reads, `pr_url`)
  is appended BEFORE `Enqueue{MergeIntent}` / `MarkMergeIntent`. A failed
  append answers `503 merge_queue_unrecorded` with no entry and no
  `merge_intent_at`; a retry appends again. Ordering, not the store's
  in-transaction `RecordIntent` callback, is the guarantee (the chained
  append's `FOR UPDATE` on the runs row would wait on the insert's own
  `FOR KEY SHARE`). Residual: an append whose queue write (or its read-back)
  then fails leaves an orphan row (`503 merge_queue_write_failed`; a retry
  appends a second row and records the intent;
  `TestMergeRun_QueueWriteFailureAfterAppend`). An entry already carrying
  intent answers `already_queued` and writes nothing.
- **One row per intent (merge-intent lock).** The decision (re-read the
  entry, append, write, read back; `recordMergeQueueIntent`) runs under
  `mergequeue.Store.WithMergeIntentLock`, a per-run session-level Postgres
  advisory lock on the pool-wide `pglock` holder budget (its contract and
  connection budget: `backend/internal/mergequeue/README.md` §
  "Merge-intent lock"). The decision runs on the lock's `fn` context
  (`r.WithContext(fctx)`), so every DB call in it carries the intent `fn`
  deadline even when the request has none
  (`TestMergeRun_IntentDecisionRunsOnLockDeadline`); a pump it spawns
  detached starts from `pglock.Detach`, so it is not refused as a nested
  lock. Overlapping merges of one run, including across
  fishhawkd replicas, take turns. The second sees the first's committed intent and answers
  `already_queued` with the SAME `intent_sequence`, so one intent
  transition has exactly one `merge_queue_entered` row
  (`TestMergeRun_ConcurrentMergesRecordOneIntent`, both write paths). A
  lock that cannot be taken, or an entry unreadable under it, gets today's
  refusal with nothing written. A lock that lapsed while the decision ran
  (`ErrIntentLockLapsed`) turns its 202 into a retryable `503
  merge_queue_write_failed`, after pumping the base so an entry the decision
  just created on an idle base is admitted without another queue event; a
  decision that already wrote its own error keeps that one answer
  (`TestMergeRun_LapsedIntentLockNeverAnswers202`). An `already_queued`
  entry whose `merge_queue_entered` row cannot be read back answers a
  retryable `503 merge_queue_write_failed` with nothing written, never a 202
  naming `intent_sequence` 0
  (`TestMergeRun_AlreadyQueuedAuditReadFailureIs503`).
  `intent_sequence` on a repeat is the NEWEST
  row whose `base_ref` is the entry's base. Rows are appended only under the
  lock and only while the entry has no intent, so an orphan is always older
  than the row that recorded the intent, and the orphan of a failed base
  change names the other base
  (`TestMergeRun_RepeatNamesTheIntentRowNotACrossBaseOrphan`). Slice 2's
  merge-on-pass must identify the verdict row by the same rule, never by
  "newest row of the run".
- **Detached pump.** After the write the base is pumped on a detached,
  bounded `mergeCandidateAdmissionContext` tracked by `bgMergeQueuePump`, so
  the 202 never waits on an admission's auto-advance or pass start; it is
  answered from the store state read right after the write.
- **Admission gating (`merge_candidate_queue.go`).**
  `serializeMergeCandidateVerifyPass` (the ADR-092 rebase / base-advance
  path) enqueues a run with NO active entry only when the admission variant
  says eligible; otherwise it starts nothing and answers start state
  `not_queued` with the reasons and a note naming `fishhawk_merge_run`
  (`rebase_branch.go` renders it in the merge-candidate block). An existing
  entry keeps its re-anchor semantics. `admitMergeCandidateEntry` evaluates
  the admission variant on the PR's current head BEFORE the behind probe:
  determined-ineligible → settled `dropped` / `not_merge_eligible` (the pump
  admits the next; the run re-enters at the TAIL on its next merge call,
  ADR-093 Q2); an intent-bearing entry whose ONLY ineligibility is the R2
  veto `forge_approval_vetoed` YIELDS at its admission epoch instead
  (`admitYielded`, ADR-112 R3, #4632); undeterminable → `Unadmit`, held at
  its place. A live,
  passed, intent-bearing entry runs merge-on-pass (§ below). No new automated rebase is added
  (#4574 stays out of scope; the admission auto-advance is ADR-092's).
- **Run and gate-view projection (`runs.go`).**
  `runMergeCandidateQueuePayload` carries `merge_intent` (always on the wire)
  and `merge_intent_at` (omitempty), plus the ADR-112 R5 yield fields and a
  yield note (#4632); GET /v0/runs/{id} and the gate view share it, so
  `gateview.go` needs no edit.

### Merge-on-pass (ADR-093 D3/D6)

E87.5 / [#4296](https://github.com/kuhlman-labs/fishhawk/issues/4296); closes
[#4289](https://github.com/kuhlman-labs/fishhawk/issues/4289) (the hand-off no
longer waits on the PR webhook). The queue merges its own head of line:
`mergeQueueMergeOnPass` (`merge_queue_merge.go`) decides, and the pump
(`merge_candidate_queue.go`) applies the outcome. The store contract (epoch
CAS, `merge_intent_sequence`, the dispatch ledger, the 90s intent fn
deadline): `backend/internal/mergequeue/README.md`.

- **Gate.** A LIVE entry in phase `passed` (a `not_required` head counts as
  passed) carrying `merge_intent_at`, with a head-pinned `GateMerger`
  (`HeadPinnedMerger`; `forge_merger.go`). Anything else holds.
- **Pre-lock checks.** Run missing/terminal → drop `run_terminal`; PR merged
  → settle merged `pr_merged`, closed → drop `pr_closed`, head ≠ anchored
  head → readvance; verify failed → eject `verify_failed`, not
  passed/not_required → hold; `mergeQueueMergeEligibility` at the anchor
  {recorded intent sequence, base}: undeterminable or only
  `checks_not_green` → hold, any other reason → drop `not_merge_eligible`
  (re-enter at the tail with `fishhawk_merge_run`, ADR-093 Q2). The merge
  variant makes NO forge review read (ADR-112 variant restriction): the
  approval is judged only in-lock, by the fold (step 6b below). Then a
  best-effort `fishhawk_audit_complete` republish at the head.
- **Locked steps** (`withRunMergeLock` = `WithMergeIntentLock`; in
  merge-on-pass every forge read is bounded 10s and the pinned merge 30s,
  under the 90s fn deadline):
  re-read the entry (same id, live, phase `passed` OR `merging` — ADR-093
  D8 ruling R3, so a decision after an uncertain dispatch reaches the claim
  and holds `merge_already_dispatched` — intent, same head) and the PR;
  a non-queue `merge_verdict_recorded` newer than the intent → hold
  `inline_merge_dispatched`; the merge overlap guard (#4119) → a 409 holds
  and appends a deduped, counts-only `merge_queue_overlap_refused`, a 502
  holds, no override; the base re-probe (`CompareCommits(head, base)`
  behind > 0 → readvance); the `merge_queue_entered` row at the RECORDED
  intent sequence (missing → hold); **step 6b, the ADR-112 fold** at the
  verified head (§ "ADR-112 forge-approval fold"): a snapshot transport
  failure holds `transient` writing nothing, a snapshot head ≠ the verified
  head readvances `head_moved`, any other non-approved result returns
  `await_approval` or `yield` (§ "ADR-112 R3 yield and R5 visibility"),
  approved writes `merge_queue_authorized` (an append
  failure holds `authorization_unrecorded`); record-before-commit
  `merge_verdict_recorded` {verdict prose and actor from the intent row,
  `via: merge_queue`, `intent_sequence`, `head_sha`, `entry_id`} (a 0062
  once-index duplicate is a resume); claim the
  run's ONE dispatch on the D1 ledger (path `queue`; an existing claim →
  hold `merge_already_dispatched`, forge never called); the pinned merge
  `MergePullRequestAtHead(head)`.
- **Classification.** nil → settle merged `merged_by_queue` INSIDE the lock
  (approval condition D3: a later inline caller reads the PR merged and
  answers `already_merged`, never 409) and append `merge_queue_merged`; a
  failed append after the forge accepted still settles merged and logs at
  ERROR. `ErrMergeConflict` (409 head mismatch) → re-read: merged → merged,
  else readvance, no `merge_queue_merged` row. 405 / 422 →
  re-read: merged → merged, else ONE fresh review snapshot (no permission
  reads): reviewDecision `REVIEW_REQUIRED` / `CHANGES_REQUESTED` or
  mergeStateStatus `BLOCKED` → YIELD `forge_review_required` (ADR-112 R3 row
  4, next_step `approve_pr`, the claim released), else (a null
  reviewDecision, any other state, a snapshot read error) eject
  `forge_merge_refused` naming the error (row 5; never retried in a loop). This applies with or without a
  declared gate. Any other error → re-read: merged →
  merged, else hold. After a claim, a failed dispatch is classified once
  (`classifyMergeDispatchOutcome`): 405 / 409 / 422 (and a checks_pending
  refusal) RELEASE the claim (an exact-claim compare-and-swap DELETE), so
  a re-advanced entry's next merge-on-pass claims again; anything else
  (5xx, timeout, cancel, 403, 404, a generic GraphQL error; only typed
  sentinels release, never a bare `ErrValidation`) is uncertain →
  `dispatch_failed` and the entry holds (mergequeue README §
  "Merge-dispatch ledger"). A PR the forge reports merged settles
  `pr_merged` only when the run has no claim or its claim names the merged
  head, else holds `merged_claim_head_mismatch`. A lock never taken → hold; a lapse after fn → WARN, its
  writes stand.
- **D8 step stamps and the in-flight registry (ADR-093 D8 /
  [#4298](https://github.com/kuhlman-labs/fishhawk/issues/4298)).** After
  the verdict row and the nil-ledger check, and BEFORE the D1 claim, the
  locked decision records phase `merging` at the entry's epoch (a CAS miss
  holds `entry_changed` with no claim and no forge call) and marks the run
  in the per-Server, process-local `mergeInFlight` registry
  (`merge_queue_resume.go`). After the forge accepts (nil, or observed
  merged) it records `settling`, best-effort (a miss is logged; the merge is
  a fact), then runs the merged tail. The pump clears the registry mark only
  AFTER `applyMergeOnPassOutcome` returns (ruling R8c), so the mark spans
  the whole decision-plus-apply and `GET /v0/restart-blockers` lists it as
  `merge_in_flight` (§ "Restart blockers"). The registry is a refcount and
  the pump clears only a mark its own decision made
  (`mergeOnPassOutcome.InFlightMarked`), so a decision that never marked,
  or finished before an overlapping one marked, cannot erase another
  decision's mark while its forge call is in flight. The pre-lock check stays
  `passed`-only: the pump routes a `merging`/`settling` entry to the restart
  resume (§ "Merge-queue durability (ADR-093 D8)"), skipping it while this
  process's mark is set.
- **Outcomes the pump applies.** `merged` → continue; `settle` → `SettleAt`
  (epoch CAS; a conflict stops the pump); `readvance` → `Unadmit(epoch)`,
  the only post-Unadmit continue, capped at `mergeQueueMaxReadvancesPerPump`
  (3); `held` → stop; `await_approval` → `ApprovalWait(epoch, window)`
  OUTSIDE the merge-intent lock: not expired → hold and stop, expired →
  `Yield` + `merge_queue_yielded` and continue; `yield` → `Yield` +
  `merge_queue_yielded` and continue (a yield does not count against the
  readvance cap). Otherwise the pump continues only after a settle, a
  live admission or an admission yield (`stop := res.outcome != admitSettled
  && res.outcome != admitLive && res.outcome != admitYielded`). Admission-owned writes (`Unadmit`, `SetPhase`, `ReanchorAt`,
  `SettleAt`) are compare-and-swaps on the entry's `admission_epoch`, so a
  stale decision changes nothing. A passed verdict pumps the base detached.
- **Ownership and the three dispatch paths (approval conditions D1/D2).**
  The queue OWNS the run's merge while the run has an active entry, live OR
  held, in any phase, carrying merge intent, and a head-pinned seam is bound
  (`mergeQueueOwnsMerge`). Merge-on-pass, the inline `merge_run` tail and the
  delegated `may_merge` arm all run under the run's merge-intent lock and
  claim the run's ONE dispatch on `merge_dispatch_claims` (UNIQUE run_id)
  BEFORE the forge. Inline: owned → `409 merge_queue_owns_merge` with nothing
  recorded (unless the PR already merged: `already_merged`); an unreadable
  queue → `503 merge_queue_unreadable`; a lock not taken within
  `mergeRunLockAcquireTimeout` (30s) → `503 merge_lock_unavailable`; an
  existing claim → `409 merge_already_dispatched` (or `already_merged`).
  Delegated (`autodrive.go::delegatedMergeUnderLock`): owned, an unreadable
  queue, a lock not taken within `autoDriveMergeLockAcquireTimeout` (30s), an
  existing claim and a claim error are each observe-only with the merger
  never called; a dispatch failure after the claim is classified (release or
  `dispatch_failed`) and surfaces the error, except a head-PINNED dispatch
  the forge refused for a head mismatch, which settles the claim by its
  refusal basis (released on precheck / rest_sha, `dispatch_failed` on
  graphql_reread, #4749) and is observe-only (`autoDriveNoteHeadMoved`,
  #4641). The inline path re-observes a
  landed merge BEFORE settling the claim (as merge-on-pass does), so a
  merge that landed answers `already_merged` and leaves the claim
  `dispatched`. Inline: a `merge_checks_pending`
  refusal releases the claim, so the re-invoke once the checks pass claims
  again; so does a pinned head-mismatch refusal, `409 merge_head_moved`
  (#4641). Inline and delegated dispatches are PINNED to the gate's admitted
  head when it is non-empty (§ "ADR-112 forge-approval fold", head-pinned
  bullets), as merge-on-pass is to its verified head. Every ledger write
  after the forge call runs on `context.WithoutCancel` with a bounded
  timeout and appends
  `merge_dispatch_settled`. A `dispatch_failed` claim is cleared only by
  reconciliation (the PR reads merged) or the operator re-arm (mergequeue
  README § "Merge-dispatch ledger"); the merge-queue read surfaces it on the
  entry as `dispatch_claim`.
- **D6 approval transfer: removed (#4632).** Intent-bearing ADMISSION no
  longer evaluates approval legs (it keeps only the R2 veto, § "ADR-112 R3
  yield and R5 visibility"), so the ADR-093 D6 transfer walk
  (`mergeQueueApprovalTransfers`) had no caller and was deleted. R1's
  clean-advance chain inside the fold (`cleanChainReaches`) is the surviving
  transfer rule, applied to every MERGE in-lock.
- **Audit rows** (system actor, INTERNAL, registered in
  `audit/categories.go`). `merge_queue_advanced` {entry_id, prior_head_sha,
  new_head_sha, merge_commit_sha, clean, admission_epoch};
  `merge_queue_verified` {entry_id, head_sha, source pass | transfer |
  already_passed | not_required}; `merge_queue_merged` {entry_id, base_ref,
  head_sha, verdict_sequence, verdict_head_sha, intent_sequence,
  approval_basis (the fold's live | transferred | not_required),
  authorization_sequence and qualifying_reviewer {login, actor_id,
  permission} when a gate is declared}; `merge_queue_overlap_refused` {head, code, reason,
  counts}. **`merge_verdict_recorded.head_sha` is the FIRST merge-on-pass
  attempt's head** (the 0062 once-index keeps one verdict row per run);
  `merge_queue_merged.head_sha` is authoritative for the merged head, and its
  `verdict_head_sha` shows any mismatch.
- **ETA basis.** A line turn now ends at the queue's own merge, so
  `eta_seconds` no longer includes the operator's merge step. The read's
  `eta_basis.note` string in `merge_queue_read.go` still carries the slice-1
  wording; that edit is an operator follow-up.
- **Operator note: a head moved after the final read.** GitHub refuses the
  pinned merge with 409 when the PR head no longer matches the verified SHA;
  the dispatch claim is RELEASED (the forge applied nothing), the entry
  re-advances and no `merge_queue_merged` row is written, and the next
  merge-on-pass claims again and merges
  (`TestMergeQueueMergeOnPass_ForgeErrors`,
  `TestMergeQueueMergeOnPass_RealLedger_HeadMismatchThenMerges`). This is
  pinned by fake-forge tests, not the live walk. A forge timeout or 5xx in
  the same window is uncertain instead: the claim is `dispatch_failed`, the
  head of line holds until the PR reads merged or the operator re-arms it,
  and `dispatch_claim` on the merge-queue read names why.
- **Residuals.** GitHub pins only the head: a base push between the locked
  re-probe and the merge call lands the merge on a base the pass did not
  see. An `inline_merge_dispatched` hold waits on the inline auto-merge; if
  it never fires the entry holds the line until slice 3's bounded hold. In a
  dismiss-stale repository requiring an approving review (this one) every
  advance dismisses the approval, so a behind PR's fold answers dismissed:
  the entry WAITS at the head for the approval window, then yields
  `forge_approval_dismissed` and the line continues; a re-approval at the
  anchored head clears it (ADR-112 states the cost: up to one window per
  such entry with nothing merging). A lock fn cut by its 90s deadline is
  re-observed by the next pump; a merge that landed settles merged and is
  never applied twice.
- **Tests.** `merge_queue_merge_test.go` (the decision), `merge_queue_pump_test.go`
  (epoch CAS, continuation, readvance cap), `merge_queue_e2e_test.go` (the H1
  race, inline vs merge-on-pass in three orders), `autodrive_merge_queue_test.go`
  (the delegated arm: ownership, read error, bounded lock, the D1 claim, the
  H2 race), `merge_queue_eligibility_test.go`.

### ADR-112 forge-approval fold (`forge_approval.go`, [#4619](https://github.com/kuhlman-labs/fishhawk/issues/4619))

ADR-112 (#4622, ratified 2026-10-22, v4.1) R1, R2, R4 and R6: ONE
forge-approval predicate, `forgeApprovalFold(ctx, run, verifiedHead)`, for
every basis and every merge path. R3 (bounded wait, yield) and R5 (yield
surfaces) landed with #4632: § "ADR-112 R3 yield and R5 visibility".

- **Rules.** A review qualifies only if (a) it is the reviewer's latest
  opinionated `APPROVED` at the verified head, or at a head reaching it only
  through CLEAN advances (`clean_advance.go`: a `branch_rebased` row with
  `clean_verified` true; seeded at the review's commit; a
  `conflict_resolution_pushed` row clears; a legacy row with no
  `clean_verified` is not clean); (b) its author satisfies the review gate's
  approvals (fired escalations, `members`, `member_of`, `not: author`, the
  single-member voucher exception); (c) its author's permission, read FRESH
  with the run's installation token, is ≥ max(min_permission, write);
  (d) its author is a real user. A `CHANGES_REQUESTED` vetoes only from a
  write-or-higher identity-valid author (the same floor); an unresolvable
  veto is undetermined (`veto_undetermined`, next step `dismiss_review`).
- **Precedence.** provider_unsupported > reviews_truncated > vetoed >
  undetermined (veto) > [qualifying approval: reviewDecision
  `REVIEW_REQUIRED` → review_required, else approved] > undetermined
  (approval) > dismissed > absent.
- **Cost and cap.** One GraphQL snapshot (≤ 3 pages) + at most 10
  permission reads under ONE 15s deadline; needing more than 10 reads is
  undetermined. Per decision: merge-on-pass makes ZERO REST review listings
  (the merge variant reads none) and one in-lock snapshot (+1, no
  permission reads, on a typed forge refusal); inline and delegated make
  one snapshot + ≤ 10 permission reads in-lock, only when a gate is
  declared; the entry and admission variants are unchanged (one REST
  listing when a gate is declared, zero GraphQL).
- **In-lock order, all three paths.** Inside the merge-intent lock, after the
  ownership check and before every verdict, override, observe, republish
  and claim write. Merge-on-pass: after the base re-probe (step 6b).
  Inline (`merge_run.go::inlineForgeApproval`): before
  `recordOverlapOverride`. Delegated
  (`autodrive.go::delegatedForgeApproval`): after the acceptance and
  nil-merger checks, before the republish and the claim.
- **Head binding (H1).** Inline folds at `gate.HeadSHA` (the head the
  merge-candidate gate verified), the delegated arm at its gate head (EMPTY
  → undetermined `head_unknown`), merge-on-pass at the anchored head. On
  every path the fold compares the in-lock snapshot's `headRefOid` with that
  head: inline → 409 `forge_approval_required` (fold undetermined,
  `head_moved`); delegated → observe-only (`mergeDispatchApprovalNotQualified`);
  merge-on-pass → readvance `head_moved`. An inline merged-PR admit (empty
  gate head, the PR merged) skips the fold so the observe path answers
  `already_merged`.
- **Merge-on-pass on a non-approved fold.** The #4619 interim DROP/EJECT
  table is gone: the fold returns `await_approval` or `yield` with no
  verdict, no authorization row and no claim (§ "ADR-112 R3 yield and R5
  visibility"). The ONE hold is still a snapshot TRANSPORT failure
  (`transient`, nothing written).
- **Authorization row (R4).** An approved fold on a declared gate appends a
  chained `merge_queue_authorized` row (path queue | inline | delegated)
  BEFORE any verdict row or claim. Append failure: queue holds
  `authorization_unrecorded`, inline answers 503
  `merge_authorization_unrecorded` (retryable), delegated is observe-only
  (`mergeDispatchAuthorizationUnrecorded`); none records a verdict or claims.
  A gate-less merge writes no row. On a resume whose verdict row already
  exists, THIS attempt's authorization row still precedes its claim.
  `review_gate_approval_seq` (`reviewGateApprovalSequence`, #4764) is 0 for
  legitimately absent evidence: the run has no review stage row, or its
  review stage has no `approval_submitted` row. A review-stage READ failure
  (`ListStagesForRun` errors) or no wired run repository is an evidence-read
  error, refused exactly like an `approval_submitted` or
  `merge_candidate_verified` read error or an append failure (inline 503
  `merge_authorization_unrecorded`, queue holds `authorization_unrecorded`,
  delegated observe-only), BEFORE any append: it never writes
  `review_gate_approval_seq` 0. `verify_pass` (`verifyPassReference`,
  #4749) resolves the SAME predicate that admitted the head
  (`mergeCandidateVerifyState`): a recorded pass is `{source: recorded,
  merge_candidate_verified_sequence}`, an enforce-mode transfer is `{source:
  transferred, merge_candidate_verdict_transferred_sequence,
  transferred_from, root_head}`, and `not_required` appears ONLY on a head
  that predicate determined needs no verify (no verify command, a
  runner-gated or otherwise unclassified head). Any other state (unverified,
  in_flight, failed, a transfer ignored in off/shadow mode) fails closed
  before any append, refused exactly like an evidence-read error.
- **Unresolvable veto names `dismiss_review` (#4764).** Every
  `veto_undetermined` fold (a `CHANGES_REQUESTED` author whose permission
  read failed, under ANY error class: 404, 500, timeout;
  `classifyForgePermission` does not tell them apart) names next step
  `dismiss_review` and sets `VetoReviewer` to that author, so the inline 409
  `forge_approval_required` carries `next_step` `dismiss_review` and
  `veto_reviewer`, and its message says the review could not be judged. The
  admission veto leg still answers undeterminable on it, and a queue yield
  records the mapping's `retry_after` for its `forge_approval_undetermined`
  reason (§ "ADR-112 R3 yield and R5 visibility").
- **Spec postures (C4).** A run that NEVER recorded a spec (an empty
  `WorkflowSHA` AND no cached `WorkflowSpec`: an ad-hoc run), or whose review
  stage declares no approval gate → no gate (R6): no fold read, admit. A spec
  that WAS recorded (`WorkflowSHA` set) but whose cached bytes are absent (the
  legacy row shape that predates the `workflow_spec` column), read back empty,
  do not parse, or lack the run's workflow → undetermined
  (`spec_undeterminable`, no forge read): inline 409, delegated observe-only,
  queue waits then yields `forge_approval_undetermined`. This DIVERGES from the queue's entry and
  admission variants (`reviewApprovalGateForRun`), which hold
  (undeterminable) on any spec they cannot determine, absent included.
- **GitLab (behaviour change).** On a non-GitHub forge whose review stage
  declares an approval gate the fold answers `provider_unsupported` by name
  (GitLab approvals carry no SHA, so R1(a) cannot be determined): inline and
  delegated merges now FAIL CLOSED (409 / observe-only). A gate-less GitLab
  run is unchanged.
- **Removed legacy check.** `approvalDismissedCheck` (#4086) no longer runs
  on merge-on-pass (pre-lock step 5 and in-lock step 8, with its REST
  `changes_requested` re-read and the `transferred_approval_undetermined`
  hold) or on the inline merge endpoint; the gate view keeps it as an
  ADVISORY display, so the gate view and the merge endpoint deliberately
  differ on that leg (ADR-112 R3).
- **Gate-less changes_requested (C5).** The pre-change gate-less in-lock path
  did NOT enforce changes_requested: step 8's re-read and the eligibility
  leg both ran only when an approval gate was declared, and the inline
  `approval_dismissed` check never read a veto. So no veto enforcement is
  lost on a gate-less run. What IS lost there is the #4086 inline
  `approval_dismissed` refusal: a branch-protected PR whose approval was
  dismissed now queues an auto-merge that does not fire until re-approved
  (the gate view still shows the advisory; on merge-on-pass the typed
  refusal yields `forge_review_required`).
- **Fake fidelity.** The mcq fake's GraphQL route IGNORES `writersOnly`
  (real GitHub pre-filters), so the tests exercise the write floor and the
  permission filter; whether GitHub includes DISMISSED in
  `latestOpinionatedReviews` is unconfirmed, and both shapes fail closed.
- **Head-pinned inline and delegated dispatch
  ([#4641](https://github.com/kuhlman-labs/fishhawk/issues/4641), closes
  residual H3).** The `GitHubMerger` seam is
  `MergePullRequest(ctx, runRow, expectedHeadSHA)`. The inline tail
  dispatches at `gate.HeadSHA`, the delegated tail
  (`dispatchAcceptanceGatedMerge`) at `liveHead`: both are the head the
  merge-candidate gate admitted, the head the D1 claim records and, on a
  declared approval gate, the head the fold judged. So
  `merge_queue_authorized.head_sha` (written only on a declared gate), the
  settled claim's `head_sha` and the pin the forge receives are one value
  (`merge_head_pin_e2e_test.go`). A push landing after the in-lock snapshot
  is refused by the forge instead of merged without a qualifying approval.
- **Both GitHub arms are pinned.** `ForgeMerger` threads the head to the
  github leaf (`serve.go::githubAutoMerger` → `githubclient.QueueMerge`). The
  auto-merge enable carries GraphQL `expectedHeadOid` ("The expected head OID
  of the pull request",
  <https://docs.github.com/en/graphql/reference/input-objects#enablepullrequestautomergeinput>).
  The clean-status REST fallback carries `sha` ("SHA that pull request head
  must match to allow merge"; 409 when it does not,
  <https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request>). Ladder
  contract: `backend/internal/githubclient/README.md` § "Head-pinned
  auto-merge ladder".
- **Typed refusal; the basis decides the claim (#4749).** A head mismatch is
  `forge.ErrMergeConflict` carrying a `*githubclient.HeadMismatchError` that
  names its basis. `classifyMergeDispatchOutcome` releases the claim
  (`merge_dispatch_settled` outcome `released`) ONLY on `precheck` or
  `rest_sha`; `graphql_reread`, or an empty or unrecognised basis, is
  uncertain (`mergeDispatchHeadMismatchUncertain`): the claim is
  `dispatch_failed` and a re-invoke answers `merge_already_dispatched` until
  reconciliation or the operator re-arm. The settled row carries
  `refusal_basis`, `expected_head_sha` and `observed_head_sha`. The reported
  claim state is the SETTLE outcome (a release that did not land is
  `dispatch_failed` too, worded as exactly that). Inline answers 409
  `merge_head_moved` with details `{run_id, pr_url, verdict_sequence,
  head_sha (the pin), refusal_basis, next_step: "fishhawk_merge_run"}`; the
  re-observe still runs first, so a PR that landed answers `already_merged`.
  Delegated returns `mergeDispatchHeadMoved`, which is observe-only
  `autoDriveNoteHeadMoved` with an INFO log (`pinned_to`, `refusal_basis`,
  `forge_guarantees_nothing_applied`, `claim_outcome` = the settle outcome),
  never "merge dispatch failed"; on a released claim the next tick re-gates
  at the new head, on a `dispatch_failed` one every later tick is
  observe-only already-claimed. The message and the log name "the head
  the merge-candidate gate admitted". They add "the head the approval was
  evaluated at" (and the fold, for the re-invoke) ONLY when the review stage
  declares an approval gate (`forgeApprovalFolded`): a gate-less run has no
  evaluated approval.
- **What `refusal_basis` guarantees.** `precheck` (the client's GET showed a
  different head; no mutation was sent) and `rest_sha` (GitHub's documented
  sha-mismatch 409) are forge guarantees that nothing applied, and only
  these say "Nothing merged". `graphql_reread` is INFERRED: the pinned enable
  answered a GraphQL error and a fresh GET then showed a different head.
  GitHub documents no wording for an `expectedHeadOid` mismatch, so if that
  mutation had in fact applied, auto-merge may be armed at the pinned head.
  `unknown` (a leaf with no `HeadMismatchError`) is treated the same. For
  both, the 409 message says the basis is not a forge guarantee and tells the
  operator to inspect the pull request. For `graphql_reread` the claim is
  NOT released (#4749): it is `dispatch_failed`, the message says the
  refusal_basis does not prove the forge applied nothing, and a re-invoke
  answers `merge_already_dispatched` until the operator re-arm (GitHub's
  ordinary refusal of a stale `expectedHeadOid` also lands here, so that
  case now costs a re-arm). `unknown` from a bare REST 409 (no
  `HeadMismatchError`) still releases: a re-invoke re-runs the gate (and, on
  a declared gate, the fold) at the new head, so no Fishhawk re-dispatch
  merges the moved head unapproved.
- **Empty head keeps the unpinned ladder.** An empty head means no head was
  evaluated: an ungated D7 admit (every GitLab-family run, any run the gate
  cannot probe) or a merged-PR admit. An unpinned `ErrMergeConflict` keeps
  the inline 502 `merge_dispatch_failed` and the delegated "merge dispatch
  failed". A declared-gate approval cannot reach the dispatch with an empty
  head, because the fold refuses `head_unknown`.
- **Behaviour change.** Gate-less GitHub runs are now pinned to the head the
  merge-candidate gate admitted too (one rule: pin whenever an evaluated head
  exists). A head that moves between the gate and the dispatch answers 409
  `merge_head_moved` instead of merging a head the gate never verified.
- **GitLab (R6).** `ForgeMerger`'s non-github arm refuses a non-empty pin with
  `ErrHeadPinnedMergeUnsupported` before resolving or calling any forge, so a
  pin is never silently dropped. It is unreachable today: GitLab runs carry
  no gate head.
- **Residuals.** (Auto-merge persistence) The pre-dispatch window is closed
  (pre-check plus forge pin), but GitHub does not document whether a QUEUED
  auto-merge re-checks `expectedHeadOid` when it later fires. Its docs say
  auto-merge is disabled when someone WITHOUT write permission pushes
  (<https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/incorporating-changes-from-a-pull-request/automatically-merging-a-pull-request>),
  so a write-access push after a successful pinned enable may leave
  auto-merge armed at a head the fold never judged. Tracked as
  [#4676](https://github.com/kuhlman-labs/fishhawk/issues/4676).
  Merge-on-pass is synchronous (`MergePullRequestAtHead`), so it has no
  such window. A late approval (for example dismiss-stale after a queue
  advance) clears the yield on the next re-probe (no re-invoke needed).
- **Tests.** `forge_approval_test.go` (the fold;
  `TestFoldSnapshot_VetoUndeterminedNamesDismissReview` over three read
  error classes; `TestReviewGateApprovalSequence_ReadErrorRefuses` and
  `TestWriteMergeQueueAuthorized_StageReadErrorWritesNothing`, which reads
  the audit chain back), `forge_approval_e2e_test.go`
  (inline, delegated and merge-on-pass, including the H2 head-moved row per
  path, the unresolvable-veto 409 row and
  `TestForgeApprovalE2E_InlineStageReadErrorRefuses`), `merge_queue_merge_test.go` (the wait/yield table, evidence
  order, failure modes, post-refusal rows 4/5),
  `merge_queue_eligibility_test.go` (variant restriction),
  `merge_head_pin_e2e_test.go` (the #4641 pin: precheck, GraphQL and REST
  arms on the inline and delegated paths, plus a gate-less pinned merge).

### ADR-112 R3 yield and R5 visibility ([#4632](https://github.com/kuhlman-labs/fishhawk/issues/4632))

ADR-112 (#4622, ratified v4.1) R3 and R5 replace #4619's interim DROP/EJECT
posture. R3: "On window expiry (including for REVIEW_REQUIRED, which follows
row 3: wait, then yield), or immediately on vetoed or provider_unsupported,
the entry YIELDS." A yielded entry is held with a marker the queue's
admission skips; clearing the marker is the only way back in. Store
contract (columns, CAS keys, order): `backend/internal/mergequeue/README.md`
§ "Yield".

- **Vocabulary (`merge_queue_yield.go`).** `yield_reason` is closed:
  `forge_approval_vetoed`, `_review_required`, `_dismissed`, `_absent`,
  `_undetermined`, `_reviews_truncated`, `_provider_unsupported`,
  `_identity_provider_unconfigured` (a `member_of` gate with no identity
  provider, keyed on the fold's REASON), and `forge_review_required` (row 4).
  `next_step` is DERIVED through the total mapping `mergeQueueYieldNextStep`
  (`approve_pr` | `dismiss_review` | `retry_after` |
  `configure_identity_provider` | `none`), never stored on the entry; the
  `merge_queue_yielded` row's `next_step` is the same mapping value
  (`mergeOnPassFoldYield` never takes the fold's own `NextStep`, #4764), so
  the recorded row and every read surface agree. The mapping matches the
  fold's own `NextStep` for every result except `veto_undetermined`, whose
  fold names `dismiss_review` (the inline 409) while its yield
  `forge_approval_undetermined` maps to `retry_after`. `yield_detail` is
  forge-derived and UNTRUSTED: THE sanitiser `mergeQueueYieldDetail` (tokens
  redacted, first line only, control characters dropped, bounded) runs at
  the queue write and at every read sink.
- **Wait, then yield (merge-on-pass).** `mergeOnPassFoldYield`: vetoed,
  provider_unsupported and identity_provider_unconfigured yield at once;
  review_required, dismissed, absent, undetermined and reviews_truncated
  WAIT. A wait is `Store.ApprovalWait(entry, epoch, window)` OUTSIDE the
  merge-intent lock: the anchor `approval_wait_started_at` is persisted
  once per episode (never reset by a readvance, `Unadmit` or a
  re-admission) and expiry is judged on the database clock. Not expired →
  the entry holds and the pump stops; expired → `Store.Yield` (one
  transaction: held, marker stamped, `enqueued_at` and the anchor kept,
  under the `admission_epoch` CAS) and the pump CONTINUES. Window:
  `Config.MergeQueueApprovalWait` (zero = 30m), fishhawkd
  `--merge-queue-approval-wait` / `FISHHAWKD_MERGE_QUEUE_APPROVAL_WAIT` (a
  non-positive or unparseable value refuses startup). Row 4 (a typed RELEASE
  refusal whose fresh snapshot reads REVIEW_REQUIRED, CHANGES_REQUESTED or
  BLOCKED; a null reviewDecision is row 5's eject) yields
  `forge_review_required` with the claim released.
- **Base change (first-officer ruling D).** A base change ends the
  approval-wait episode. The row re-enters the new base's FIFO at the tail,
  unmarked, with a fresh approval_wait_started_at. R3's no-reset rule applies
  within one base. (The store's base-change arm drops the old row
  `base_changed` and inserts a new one, so neither the marker nor the wait
  anchor crosses; a same-base head move keeps both, approval condition C2.
  `TestEnqueue_BaseChangeStartsNewWaitEpisode`.)
- **Admission.** An intent-bearing admission evaluates no approval leg; it
  keeps the review-round / coverage legs and runs the R2 veto through the
  fold. Row 1 first: a determined non-approval ineligibility drops as before,
  even with a veto present; a veto ALONE yields at the admission epoch
  (`admitYielded`, the pump continues); `veto_undetermined` is
  undeterminable. The entry and anchor-less (ADR-092 rebase-path) variants
  keep their approval requirement.
- **Entry (`merge_run` queue arm).** With a head-pinned merger bound, an
  ineligibility made ONLY of approval-leg reasons (`not_approved` →
  `forge_approval_absent`, `changes_requested` → `forge_approval_vetoed`)
  enqueues a YIELDED held row with intent: `merge_queue_entered` (payload
  `yielded`, `yield_reason`) is appended first, then the queue write and
  `merge_queue_yielded` (site `merge_run`); 202 `queued` names the yield and
  the next step. A same-base LIVE entry is never marked; any other
  ineligibility keeps the 409 with nothing written; the inline path still
  refuses and writes nothing.
- **Re-probe (`merge_queue_yield_reprobe.go`).** `RunMergeQueueYieldReprobe`
  ticks at `MergeCandidateQueuePumpInterval` beside the pump. Per base it
  re-probes AT MOST ONE row, the oldest due by `(yielded_at, id)` under the
  exponential backoff `yieldReprobeDue(yield age, since, interval)`: points
  at interval·2^k up to 16 intervals, then every 16 intervals, and a row is
  due when a point fell since THIS process's previous sweep, `(age − since,
  age]` (`(0, age]` on the first sweep after start). The window is the actual
  time since the previous sweep, so a sweep that overran its tick (dropped
  ticks) never skips a point and no row past the cap starves on a
  tick-parity stride (concern 8b3f29f5). The fold runs at the anchored head
  with no store transaction open: approved AND the ONE clear predicate
  `mergeQueueYieldClears` holds → `ClearYield` (CAS on
  `anchored_head_sha` and `yielded_at`) → `merge_queue_yield_cleared` → a
  detached pump (the row rejoins FIFO at its `enqueued_at`). The predicate
  clears every reason on the approved fold EXCEPT `forge_review_required`:
  that row yielded BECAUSE the fold approved and the forge still refused (a
  gate-less fold is always approved), so it clears only when a FRESH review
  snapshot, read even on a gate-less run (`forgeReviewRequiredSnapshot`, the
  row-4 yield's own read), shows reviewDecision neither `REVIEW_REQUIRED` nor
  `CHANGES_REQUESTED` (null is not the condition) and mergeStateStatus not
  `BLOCKED`; an unreadable or undetermined snapshot keeps it yielded (fail
  closed). Every automatic path (the sweep, the targeted re-probe, the
  webhook trigger) judges that predicate unforced, so an unchanged forge state
  never loops clear → re-admit → refused PUT → re-yield; the operator's
  explicit `merge_run` re-invoke forces ONE re-attempt (`force`). The same
  reason → nothing; a changed reason → `UpdateYield` + a `merge_queue_yielded`
  transition row; the PR head moved → `ReanchorYielded` to the current head
  KEEPING the marker and the episode (a head move never clears a marker),
  then the new head is judged on the same probe; merged, a transport error
  or a CAS conflict → nothing. Targeted, no-backoff re-probes: a
  `merge_run` re-invoke on a queued yielded entry (and the inline
  `merge_queue_owns_merge` refusal on one), and an approved
  `pull_request_review` whose `commit_id` equals the anchored head, at most
  once per run per pump interval; the webhook is a trigger only, the fold
  (and, for `forge_review_required`, the fresh snapshot) is the sole
  decision.
- **One episode per same-reason re-yield.** A live entry re-yielding for the
  SAME reason on the same anchored head and base as its LAST cleared episode
  (`mergeQueueYieldContinuation`, read from the last
  `merge_queue_yield_cleared` row, which carries `base_sha`) keeps that
  episode's `yielded_at` (`mergequeue.YieldMarker.Continue`, judged against
  the anchor in the store's own statement): the backoff age keeps growing
  and the episode is posted once. A moved head or base, another reason, or a
  missing cleared row is a new episode. Residual: the continued episode
  reuses the marker's CAS key, so a re-probe racing the re-yield with the
  PRIOR marker may clear it once more (one re-attempt, re-yielded in the same
  episode).
- **Surfacing and audit.** `merge_queue_yielded` (INTERNAL: entry_id,
  base_ref, head_sha, yield_reason, yield_detail, next_step, site
  `merge_on_pass` | `admission` | `merge_run` | `reprobe` | `reprobe_heal`,
  precedence_row, yielded_at, transition_from?), `merge_queue_yield_posted`
  (operator-visible on the run's ISSUE thread activity timeline, once per
  `(entry_id, yielded_at)` episode, for a yield older than one tick, under
  the run's merge-intent lock so two replicas cannot both post) and
  `merge_queue_yield_cleared` (INTERNAL, the resolution). A `Yield` /
  `ClearYield` CAS that committed is never rolled back when its row fails to
  append: the failure logs at ERROR with entry_id and yielded_at, and the
  sweep re-appends a missing `merge_queue_yielded` row for a still-marked
  episode (site `reprobe_heal`).
- **Visibility (R5).** ONE projection, `mergeQueueYieldProjectionFor`
  (`merge_queue_read.go`), feeds the queue read's `entries[]` and
  `head_of_line` and `runMergeCandidateQueuePayload` (run read and gate
  view): `yield_reason`, the sanitised `yield_detail`, `yielded_at` and the
  derived `next_step` on a yielded entry; `approval_wait_started_at`
  whenever the episode's wait anchor is set (a live entry waiting at the
  head too). Entries follow the store order (live, unmarked held, yielded);
  a yielded entry carries no `eta_seconds`; the run read's `note` names the
  yield and its remedy from the closed vocabulary only
  (`mergeQueueYieldRemedy`), never the detail. MCP: the three mirrors in
  `mcpserver/client.go` carry the five fields, and `next_actions`'
  `foldMergeQueueYield` adds the remedy per `next_step` (`approve_pr`,
  `dismiss_review`, `retry_after` → `fishhawk_merge_run` (an already
  offered `fishhawk_merge_run` is rewritten in place, never duplicated),
  `configure_identity_provider`; `none` adds nothing), built only from the
  closed vocabulary; an out-of-vocabulary `yield_reason` renders generically
  and `yield_detail` never reaches it.
- **Rollback.** Revert the slice PRs together (store, eligibility,
  merge-on-pass, re-probe), or first clear the markers with `UPDATE
  merge_candidate_queue_entries SET yield_reason=NULL, yield_detail=NULL,
  yielded_at=NULL, approval_wait_started_at=NULL WHERE state='held'`: a
  partial revert above the store otherwise strands every yielded row. The
  0109 down migration needs the binary reverted first.
- **Tests and counterfactual vehicles.** Store: `mergequeue/store_test.go`
  (wait anchor survives Unadmit/re-admission; AdmitNext skips a yielded
  row that is OLDEST by `enqueued_at`; Yield under the epoch CAS; ClearYield
  CAS read-back; a marker never written on a live row). Merge-on-pass and
  pump: `merge_queue_merge_test.go` (the wait/yield table, row 4),
  `merge_queue_yield_test.go` (`TestMergeOnPassFoldYield_Table`,
  `TestMergeQueueYieldNextStep_AgreesWithFold`, and the cross-layer
  `TestMergeOnPassVetoUndetermined_YieldRowAgreesWithReadSurface`: the
  `merge_queue_yielded` row's `next_step` equals the read projection's),
  `merge_queue_e2e_test.go` (dismissed waits then yields and B merges in the
  SAME pump; gate-less BLOCKED yields; the anchor survives a readvance),
  `merge_queue_eligibility_test.go` (no approval leg on intent admission;
  veto alone yields; non-approval + veto drops). Re-probe:
  `merge_queue_yield_reprobe_test.go` (due table, clear, transition,
  head-moved re-anchor, posted-once under two concurrent sweeps, heal).
  Entry/triggers: `merge_run_test.go`, `pullrequest_review_events_test.go`
  (a non-anchored review SHA triggers nothing), `forge_approval_e2e_test.go`.
  Visibility: `merge_queue_read_test.go`
  (`TestMergeQueueRead_YieldedEntryOrderedLastNoETAAndSanitised`,
  `TestMergeQueueRead_HeadOfLineYieldFields`), `runs_get_test.go`
  (`TestGetRun_MergeCandidateQueue_Yielded`), and `mcpserver`'s
  `TestMergeQueueYieldMirrors_DecodeServerWireLiterals` (strict decode of
  the server's spelling into all three mirrors) and
  `TestFoldMergeQueueYield_*`.

### Merge queue read (`merge_queue_read.go`)

`GET /v0/repos/{owner}/{name}/merge-queue[?base_ref=]`.

- **Scope tier.** A run-bound `mcp:run:<uuid>` token is refused
  `403 run_token_forbidden` (`isRunBoundSubject`) — the payload names other
  runs and their heads. Every other caller needs `read:audit`
  (`scopeMergeQueueRead` = `scopeGateViewRead`, via `requireWriteScope`: 401
  anonymous, 403 `insufficient_scope`); a cookie-session operator passes.
  Repo visibility is a point-read DENY (`enforceRepoVisibility`).
- **Account scope.** The caller identity's `AccountID` is the
  `mergequeue.Base` account, so another account's queue on the same
  repository and base is structurally unreadable; an untenanted caller reads
  the untenanted queue; a non-UUID account fails closed 500
  (`callerAccountUUID`). Without `base_ref` the bases are `ActiveBases`
  filtered to that account and repository.
- **Body.** Per base: `entries` from `ListForBase` (live first, then
  unmarked held FIFO unless promoted, then yielded held) with `position`
  (1-based among held), `merge_intent` / `merge_intent_at`, `promoted`,
  anchors, `eta_seconds` (never on
  a yielded entry) and the ADR-112 R5 yield fields (§ "ADR-112 R3 yield and
  R5 visibility"); `head_of_line` = the first
  entry with `current_head_sha` read best-effort from the forge (one read per
  base; `current_head_unavailable` = `run_read_failed` | `forge_unwired` |
  `pull_request_unresolvable` | `forge_read_failed`); `eta_basis`
  `{samples, median_seconds?, note}`; `ejections[]` (§ "Ejection, re-entry
  and promote (ADR-093 slice 3)").
- **ETA basis.** `eta_seconds` = `position` x the median of
  `RecentLineTurns(base, 10)` (admission to merge of the base's newest merged
  entries, database clock, account-scoped), rounded to the second; omitted
  for the live entry and with no history. A line turn ends at the queue's
  own merge-on-pass (§ "Merge-on-pass (ADR-093 D3/D6)"), not the operator's
  merge step. The `eta_basis.note` string still carries the slice-1 wording
  (an operator follow-up).
- **Fail posture.** `503 merge_queue_unconfigured` unwired, answered only
  AFTER the run-bound refusal and the scope check (the sibling merge verb's
  order), so an anonymous or under-scoped caller cannot learn whether a queue
  is wired (`TestMergeQueueRead_Unconfigured`); any store read error is
  `500 internal_error` with no partial body; the forge read never fails the
  read.
- **Tests.** `merge_queue_read_test.go` over the real store (mcq fixture):
  FIFO positions, intent, head of line and ETA together; account isolation
  (A / B / untenanted on one repo and base); the auth ladder (including the
  shipped route, 401 not 404); unconfigured; non-UUID account; repo
  forbidden; each store read error; each head-of-line degrade; the held head
  of line; `medianLineTurnSeconds`; the yielded entry (ordered last, no
  ETA, sanitised detail, every yield key on the wire) and the head-of-line
  yield / wait fields. `runs_get_test.go` / `gateview_test.go` pin
  `merge_intent` (and `runs_get_test.go` the yield fields and note) on the
  run read and the gate view.

### Ejection, re-entry and promote (ADR-093 slice 3)

ADR-093 D4 plus the D2 promote verb (E87.6 / [#4297](https://github.com/kuhlman-labs/fishhawk/issues/4297)). Four parts, one per file.

- **Ejection record (`merge_queue_eject.go`).** Every ejection appends ONE
  `merge_queue_ejected` row (actor system, INTERNAL — not an issue-comment
  surface) `{entry_id, repo, base_ref, reason, merge_intent,
  anchored_head_sha, next_step, remedy, fixup_routed? (verify_failed),
  refusal?, forge_reason?, forge_detail?}`, written ONLY by the call whose
  `Settle`/`SettleAt` settled the entry (a redelivered hook writes nothing).
  `next_step` is a TOTAL mapping of the reason (`mergeQueueEjectNextStep`):
  `verify_failed` → `await_delegated_fixup` when ADR-090 D6 routed a
  delegated fix-up, else `fishhawk_fixup_stage`; `advance_conflict` →
  `fishhawk_rebase_run_branch` (it starts the bounded conflict-resolution
  pass; the pump never auto-starts one); `forge_merge_refused` →
  `fishhawk_merge_run`; `pass_not_startable` → `fishhawk_get_run_status`;
  anything else → `fishhawk_merge_run`. `remedy` is built from that closed
  vocabulary only; `forge_detail` is untrusted forge text through the yield
  sanitiser. The verdict hook defers the row so
  `succeedMergeCandidateVerifiedStage` writes it AFTER
  `routeMergeCandidateFailure` and it states what D6 did; every other
  `verify_failed` arm (the failed start, a failed verdict at admission,
  merge-on-pass seeing a failed verdict) derives `fixup_routed` from the
  run's `stage_fixup_triggered` row for that failed verdict, matched by
  CHAIN SEQUENCE (`mergeQueueVerifyFailedRouted`), and fails closed to not
  routed. The row is best-effort (after the store commit): a lost row costs
  the automatic re-entry, never the operator's.
- **Forge refusals eject exactly once (ADR-112 R3 rows 4–5, ADR-093 D4).** In
  `mergeOnPassLocked` every typed refusal of the pinned merge PUT
  (`ErrPullRequestNotMergeable` → `not_mergeable`, `ErrMergeUnprocessable` →
  `unprocessable`, `ErrPullRequestUnstableStatus` → `checks_not_settled`)
  first runs the row-4 snapshot (`mergeOnPassRefusalNeedsReview`): review
  required or `BLOCKED` → the entry YIELDS `forge_review_required` /
  `approve_pr`. Only otherwise does it eject `forge_merge_refused` naming the
  code. The unstable-status arm used to hold as transient, so the next pump
  claimed and dispatched again. **The asymmetry is deliberate — do not
  "fix" one arm to match the other:** pending checks HOLD before dispatch
  (the pre-lock `onlyChecksNotGreen` rule), so slow CI never ejects; only the
  forge's own refusal of the merge PUT ejects, and its `forge_detail` /
  remedy say the checks were not settled at the merge attempt (remedy:
  `fishhawk_merge_run` once they settle). The `checks_not_settled`
  `forge_detail` is a short fixed lead (`mergeQueueChecksNotSettledDetail`)
  plus the forge's text sanitised BEFORE the lead is added, so a realistic
  forge message survives the 300-byte `forgeApprovalDetailMaxBytes` bound
  both sinks re-apply; the remedy (closed vocabulary) carries the asymmetry
  (#4705).
- **Automatic re-entry (`merge_queue_reentry.go`).** `sweepMergeQueueReentries`
  runs on the yield re-probe ticker after `sweepMergeQueueYields`. Each
  `mergequeue.ReentryReader` candidate (an ejected `verify_failed` row with
  merge intent, still its run's newest row, on a non-terminal run) is judged
  only when DUE on the `yieldReprobeDue` schedule over the row's
  database-clock settled age, process-local (ADR-112 A1.4: after a restart
  the window is `(0, age]`). One tick reads ONE page of at most
  `mergeQueueReentryLimit` (50) candidates after a process-local keyset
  cursor (`mergeQueueReentryPager`, `(settled_at, id)`); a full page moves the
  cursor to its last row and a short page wraps it to the oldest, so 50
  candidates that never pass a gate (unrouted, never pushed) cannot hide every
  newer one from the read (#4297). Across several pages a page's due window
  reaches back to the start of the previous cycle (the last read of that
  page at the latest), so no schedule point falls between two reads; with one
  page it is the time since the previous sweep. It re-enters only when: no
  stage of the run is `dispatched` or `running` (ADR-115 D4 compliance); its
  `merge_queue_ejected` row says `fixup_routed: true`; a `fixup_pushed` row's
  chain sequence follows that row's; the `merge_queue_entered` row AT the
  entry's `MergeIntentSequence` exists (never the newest such row); the PR
  is open on the base with a moved head; and the ENTRY-variant eligibility
  passes at the new head with a LIVE forge approval read there (an approval
  the fix-up push dismissed means no re-entry, and an approval-only failure is
  SKIPPED, never enqueued yielded; unifying it with the A1.3 clear predicate
  is #4686). Under the run's merge-intent lock `LatestForRun` must still be
  that ejected row; then a `merge_queue_entered` row is appended BEFORE the
  write (actor SYSTEM, `reentry: true`, `reentry_basis
  delegated_fixup_passed`, `reentry_of_entry_id`, `original_intent_sequence`,
  the original verdict prose — merge-on-pass still binds to the verified
  head, never a SHA in that prose) and `Enqueue` inserts a FRESH row at the
  TAIL carrying that row's sequence. A failed append writes no entry.
- **Promote (`merge_queue_promote.go`).** `POST
  /v0/runs/{run_id}/merge-queue/promote` (`write:runs`,
  `requireRunAccount(memberWrite)`): 401 anonymous, 403
  `run_token_forbidden`, 403 `insufficient_scope`, 400 `validation_failed`
  (the strict `{"reason"}` body), then 503 `merge_queue_unconfigured` (no
  queue, no `mergequeue.Promoter`, or no run repository: without it
  `requireRunAccount` cannot check the run's account and falls through, so
  the verb fails closed), then the store's one transaction: 404
  `run_not_found`, 409 `merge_queue_not_queued` / `merge_queue_entry_live`
  (the live entry is never preempted) / `merge_queue_entry_yielded`
  (`details.next_step`), 500 `merge_queue_promote_failed` (rolled back).
  `merge_queue_promoted` is appended INSIDE the transaction; an entry already
  first answers `promoted: false` with no row. The MCP tool is
  `fishhawk_promote_merge_queue_entry`; a stage concurrency waiter's promote
  stays `fishhawk_promote_queued_stage`.
- **Visibility (`merge_queue_read.go`, `runs.go`, `gateview.go`).** ONE
  projection, `mergeQueueEjectionProjectionFor`, feeds the queue read's
  per-base `ejections[]` (`UnresolvedEjections(base, 10)`, newest first,
  account-scoped, unresolved only) and the top-level `merge_queue_ejection`
  block on `GET /v0/runs/{id}` and the gate view (present only while the
  run's newest queue row — `LatestForRun` — is ejected, so never beside an
  active entry). It takes from the row only `fixup_routed`, a `forge_reason`
  inside the closed code set and `forge_detail`, sanitised AGAIN on read;
  `next_step`, `remedy` and `reentry` (`operator_merge_run` |
  `automatic_after_delegated_fixup`) are derived. An absent or unreadable row
  degrades to the reason default (`next_step_basis: reason_default`,
  `fixup_routed` omitted) and never fails the read. Store-read posture: the
  queue read fails closed (500) on an `UnresolvedEjections` error like every
  other store read; the run read omits the block; the gate view omits it
  with a `merge_queue_ejection` `history_gaps` entry. The reads go through
  the OPTIONAL `mergequeue.EjectionReader` capability
  (`mergeQueueEjectionReader`), so a fake embedding only `mergequeue.Store`
  degrades to absent / empty instead of panicking. Queue-read entries and
  `merge_candidate_queue` carry `promoted` (order key set), always on the
  wire. Residual: without `base_ref` the queue read lists only bases with an
  ACTIVE entry, so a base whose line is empty shows its ejections only to a
  `base_ref` read (the run read and gate view still show each run's own).
- **Tests.** Ejection: `merge_queue_eject_test.go`, `merge_queue_merge_test.go`
  (`TestMergeOnPass_ForgeRefusalEjectsExactlyOnce`, rows 4 and 5 per arm),
  `merge_candidate_queue_test.go`. Re-entry: `merge_queue_reentry_test.go`
  (one test per gate, the schedule and the live-stage skip) and the
  cross-layer `merge_queue_e2e_test.go`. Promote: `merge_queue_promote_test.go`
  (one per rung), `backend/internal/integration/mcp/merge_queue_promote_test.go`.
  Visibility: `merge_queue_read_test.go` (`TestMergeQueueRead_EjectionsUnresolvedNewestFirstSanitised`,
  `TestMergeQueueEjection_*`, `TestRunReadAndGateView_MergeQueueEjectionOnlyWhileNewestRowEjected`,
  `TestOpenAPIDocumentsMergeQueuePromote` / `...Ejection`) and
  `mcpserver`'s `TestMergeQueueEjection_WireShape` (real backend and store).

### Merge-queue durability (ADR-093 D8)

E87.7 / [#4298](https://github.com/kuhlman-labs/fishhawk/issues/4298),
delivery PARTIAL. A live head-of-line entry records its LAST RECORDED step in
the free-text `phase` column (no migration): `admitted` → `advancing`
(stamped right before the behind-probe) → `verifying` → `passed` /
`unverified` → `merging` (in-lock, before the D1 claim) → `settling` (after
the forge accepts). The phase is NOT a liveness signal. After a restart the
pump resumes each live entry from its step; the boot pump
(`resumeMergeCandidateQueues`, called by `RunMergeCandidateQueuePump` instead
of its first plain pump) runs that resume first. Code:
`merge_queue_resume.go` (merging/settling, boot pass, registries),
`merge_queue_resume_verifying.go` (verifying/unverified, the shared
`merge_queue_resumed` writer), `merge_candidate_queue.go` (routing, the R7
gate). Store half: `backend/internal/mergequeue/README.md` (`Resumer`,
`AdmitNextReclaiming`, § "Crash-left claims (ADR-093 D8)").

| Step after a restart | Resume |
|---|---|
| `admitted` / `advancing`, admitted before the boot instant | re-claimed by the boot pump's first admission (`AdmitNextReclaiming`, epoch +1, a `merge_queue_resumed` `reclaim` row written INSIDE the queue transaction — a failed append re-claims nothing), then the admission re-runs; the behind-probe and advance are idempotent |
| `admitted` / `advancing`, stale | the 10-minute `AdmissionStaleAfter` re-claim (now covering `advancing`), same re-run |
| `verifying` / `unverified` | `mergeQueueResumeVerifying`: reads verdict ROWS at the anchored head (never the derived `not_required`): passed → `apply_verdict` (the lost hook); failed → `eject` `verify_failed` with `fixup_routed=false`, next_step `fishhawk_fixup_stage` (fail closed, R8b); a live trigger → hold, no row; newest `not_executed` → `record_unverified`, then `redispatch_pass` AT MOST ONCE (intent, reason `runner_failed:`, exactly one such verdict at the head, R7 gate clear, implement stage succeeded/awaiting_approval, PR head = anchor); otherwise hold for `fishhawk_rebase_run_branch` as ADR-092 does |
| `passed` | ordinary merge-on-pass (§ "Merge-on-pass") |
| `merging` / `settling`, THIS process mid-merge | skipped (`mergeInFlight.has`) |
| `merging` / `settling`, PR merged at the QUEUE claim's head | `settle_merged` row (`merge_actor_verified:false`, C6b), a strict `merge_dispatch_settled` `reconciled_merged` for a `dispatch_failed` claim, then the merged tail with `Resume=true` (`merge_queue_merged` only when no row names the entry; a failed dedup read or append holds `resume_unrecorded`). A DECLARED approval gate with no `merge_queue_authorized` row (path `queue`, claim head) holds `authorization_unrecorded` (C6a) |
| `merging` / `settling`, PR merged, no claim or another path's claim | `settle_pr_merged`, settled merged `pr_merged`, claim untouched |
| PR merged at a head the claim does not name | hold `merged_claim_head_mismatch` |
| `merging`, PR open or unreadable, QUEUE claim still `dispatched` and crash-left (C2) | strict `merge_dispatch_settled` {`dispatch_failed`, `orphaned_by_restart`}, then the EXISTING `MarkMergeDispatchFailed(orphaned_by_restart)` (C3), then a hold row {`merge_dispatch_uncertain`, next_step `operator_rearm`, remedy}: confirm on the forge nothing merged, DELETE the `dispatch_failed` claim (mergequeue README operator re-arm); the queue-path re-arm needs NO `fishhawk_merge_run` re-invocation, the next pump re-decides from scratch (C8a). PR closed: `drop_pr_closed` |
| the same claim younger than the crash-left age | hold `merge_dispatch_in_progress`, nothing written; a later tick re-decides |
| `settling`, PR open (queue claim `dispatched`) or unreadable | hold `settling_unconfirmed`, no ledger write (an unreadable PR never orphans a settling claim: the step recorded that the forge accepted); depends on the forge converging: once it shows the PR merged, `fishhawk_reconcile_merge` or `fishhawk_record_merge_observation` (or the next pump) settles it; otherwise the forge must converge (C8b) |
| an inline/delegated claim, a GitLab claim, or a non-orphaned `dispatch_failed` claim | hold `merge_already_dispatched`, no ledger write (C1: only a `queue`-path claim is ever reconciled) |
| `merging`, no claim | `redecide`: back to `passed` at the epoch, ordinary merge-on-pass decides from scratch with the fold and veto in-lock; no dispatch was ever handed to the forge, so this is not a retry |

- **ADR-109 governs the ledger.** The resume adds NO ledger transition and
  dispatches NO merge: it makes ONE forge re-read (`merged := pr.Merged //
  ADR-093 D8: the resume re-read`) under the run's merge-intent lock, and
  the only claim write is the existing `MarkMergeDispatchFailed` the
  uncertain classification already uses.
- **Crash-left age (C2).** `mergeQueueCrashLeftClaimAge` =
  `mergequeue.IntentFnTimeout` (the ADR-108 merge-intent lock fn deadline,
  90s) + `mergeOnPassMergeCallTimeout` (the pinned merge call, 30s) +
  `mergeQueueCrashLeftClaimMargin` (30s) = 150s. Judged on the DATABASE
  clock (`Resumer.DatabaseNow` against `claimed_at`), never Go
  `time.Now()`, and only while HOLDING the merge-intent lock; there is no
  boot-time cutoff. An unreadable clock holds.
- **R2 note (binds any follow-up).** Any later automatic retry of a
  crash-left merge MUST re-run the ADR-112 forge-approval fold and veto
  in-lock, failing closed, before calling the forge. The retry-once and the
  moved head/base release wait for the ADR-109 amendment; this resume
  dispatches no merge PUT.
- **`merge_queue_resumed` (INTERNAL audit category).** Payload `{entry_id,
  base_ref, step, action, reason?, head_sha, admission_epoch, next_step?,
  remedy?, claim_state?, claim_path?, claim_head_sha?}`; every id is the
  run's own (run-bound-readable under ADR-106 once its registry flag lands,
  E89). Actions, closed: `reclaim`, `apply_verdict`, `eject`,
  `record_unverified`, `redispatch_pass`, `settle_merged`,
  `settle_pr_merged`, `drop_pr_closed`, `redecide`, `hold`. Every row is
  appended BEFORE its action and fails closed (held `resume_unrecorded`,
  nothing changed, R8d). Only `hold` rows are deduped (same entry, step,
  action, reason as the run's newest row), so a held entry re-decided every
  tick writes one row; a state-changing action always appends its own (C7).
- **Verdict hook guard (C4).** `mergeCandidateQueueOnVerdict` and the
  transfer re-anchor (`rebase_branch.go`, R8g) leave a `merging`/`settling`
  entry's phase alone: a late or duplicate verdict never resets,
  unverifies or ejects an entry whose queue claim may still be dispatched.
- **R7 stage-live gate.** A RE-CLAIMED admission (boot instant or stale) and
  the verifying re-dispatch never advance or start work while a stage of the
  run is `dispatched`/`running` or the implement stage carries a live
  merge-candidate trigger (`mergeQueueRunStageLive`; a read error counts as
  live). A gate-held admission stays live at its new epoch with no write and
  is recorded in the process-local `mergeQueueGateHolds`; the next periodic
  pump tick re-checks the gate for exactly that entry (C5), so the base
  waits one tick, not `AdmissionStaleAfter`. Stricter than ADR-115 D4 (it
  counts the queue's own pass); fresh admissions are unchanged.
- **Boot instant (R8a).** `runServe` captures `Config.MergeQueueBootAt` with
  `captureMergeQueueBootAt` (`backend/cmd/fishhawkd/serve.go`,
  `Resumer.DatabaseNow`) right before `newServer`, i.e. before every
  goroutine that can admit; zero (no `Resumer`, a read error) turns the boot
  re-claim off and leaves the stale re-claim. Multi-replica residual: a
  sibling replica's admission made before this replica's boot instant is
  re-claimed; the epoch CAS no-ops the sibling's later writes and its side
  effects are idempotent.
- **ADR-112 A1.4 sweep times reset BY DESIGN.** The process-local re-probe
  and re-entry sweep times restart from zero on a restart (the window
  becomes (0, age]); the boot re-claim never touches `yield_reason`,
  `yielded_at` or `approval_wait_started_at` (R6).
- **Restart blocker.** A merge THIS process has in flight is `merge_in_flight`
  on `GET /v0/restart-blockers`; a merging entry a PRIOR process left is not
  (§ "Restart blockers").

## Merge overlap guard (`merge_overlap_guard.go`, E83.61 / [#4119](https://github.com/kuhlman-labs/fishhawk/issues/4119))

`POST /v0/runs/{run_id}/merge` refuses while ANOTHER live run's implement
stage is writing files this pull request changes: merging moves the base
under that agent and lands its run in the category-B dead-end class. Unlike
the advisory #4118 surfaces it FAILS CLOSED.

- **Placement (`handleMergeRun`).** AFTER the #3109 conflict guard (a
  conflicting PR keeps its own, more specific refusal) and BEFORE the ADR-090
  merge-candidate gate, so the ADR-093 enqueue arm (#4295) can never enqueue
  an overlap-blocked run: an overlap-blocked, otherwise queue-eligible run is
  refused, not enqueued. Every refusal records nothing: no verdict, no
  override row, no merge intent, no dispatch.
- **Placement (delegated `may_merge`, #4582).** `AutoDriveRunGate`'s
  may_merge arm calls `mergeOverlapAutoDriveHold` AFTER the first-officer
  hold and BEFORE the merge-candidate gate (the operator path's
  guard-before-gate order), so the merger is never called on a refusal. A 409
  (`overlapping_implement_in_flight` / `overlap_undetermined`) returns
  `decision_required` with the code as `decision_state` and action `merge`; a
  502 `overlap_check_failed` is observe-only fail-closed (the delegated arm's
  rule for every read error, like the first-officer and D7 read errors).
  There is NO delegated override: the path has no `allow_overlap` input and
  never records `merge_overlap_overridden` — `allow_overlap` is operator-only
  (`fishhawk_merge_run`). Every delegated refusal appends a
  `delegated_merge_overlap_refused` row (approval condition C5): payload
  `run_id, pr_url, head_sha, refused_code, refused_reason, blocking_count,
  undetermined_count` — counts only, never another run's id, stage id, issue
  ref or path, so it needs no run-bound withholding. It is deduped against the
  run's LATEST such row (same code, reason, head and counts appends nothing),
  so a campaign driver polling a parked run writes one row per distinct
  decision, not per tick; a failed read or append is WARN-logged and the
  refusal stands. `decision_required` stops `fishhawk_drive_run`; the campaign
  `GateActor` does not carry it and keeps polling, merging once the other
  implement settles. Registered in `audit/categories.go`; NOT an
  issue-comment activity category.
- **Candidates.** `overlapCandidates` — the #4118 enumeration (`ListRuns` per
  live state, same repo and tenant, lineage and one-hop recovery exclusions).
  A `ListRuns` error is 502 `run_list`; a page at `scopeOverlapListLimit`
  (truncated listing) is 409 `run_list_truncated`.
- **Stage-state filter.** A candidate is LIVE when a stage of type
  `implement` is `dispatched` or `running` (`overlapBlockingStageState`).
  Fix-up, conflict-resolution and verify-only merge-candidate passes re-open
  that same implement row, so they block too (a verify-only pass writes no
  code; it is conservative, and it settles on its own). `pending`,
  `awaiting_host_dispatch`, `awaiting_approval`, `awaiting_scope_decision`
  and every terminal state do not block — a planned or parked run is not
  writing files. A stage-list error is 502 `stage_read`. A candidate with no
  implement stage (or no stages at all) is simply not live. A stranded
  `running` implement (runner died, not reaped) blocks until
  `fishhawk_reap_stage` or an override; the refusal names its stage id.
- **No live candidate → admit with ZERO forge reads.** Only past that point
  does the guard read the forge, so a quiet repo pays nothing and fixtures
  without a compare stub keep their outcome.
- **Changed paths.** GitHub anchors exactly like `mergeCandidateGate`
  (`cfg.GitHub`, a non-zero installation, a parseable repo, a PR number); any
  unresolved anchor is 409 `changed_paths_unavailable` — GitLab runs land
  here until a GitLab changed-path read exists. `GetPullRequest` error or an
  empty head/base is 502 `forge_read`; a PR the forge reports MERGED admits
  (the #3622 `already_merged` resume still answers 200). Then
  `ComparePatch(base_ref, head)` — three-dot, the PR's own diff (a compare
  error is 502 `forge_read`). The changed set is every file `Path` plus every
  non-empty `PreviousPath` (a rename's SOURCE counts). It is INCOMPLETE when
  `forge.ComparePatchResult.Truncated` is set (`changed_files_capped`) or a
  `renamed` file carries an EMPTY `PreviousPath` — "source unknown, not no
  source" per `githubclient/README.md` (`rename_source_unknown`).
  **`Truncated` is read deliberately instead of a file count (approval
  condition C6):** it covers BOTH the 300-file compare cap and an omitted
  oversized patch body. The second over-refuses (the file LIST is complete
  when only a patch body was dropped); that is accepted as fail-closed with
  the override as the route, and keeps the cap value out of this package. Do
  not "simplify" this to `len(Files) >= 300` without exporting the cap.
- **Decision.** Each live candidate's effective scope via
  `resolveOverlapScope`: unresolved → undetermined; a non-empty
  intersection with the changed set → blocker (sorted by `issue_ref` then
  `run_id`). Blockers → 409 `overlapping_implement_in_flight` (wins;
  `undetermined` is still carried). Else, with an incomplete changed set,
  every non-blocking live candidate is undetermined → 409
  `overlap_undetermined` reason `changed_files_capped` /
  `rename_source_unknown`; else unresolved scopes → reason
  `scope_unresolved`. Else admit.
- **Refusal body.** 409 details: `blocking[]` / `undetermined[]`
  (`{run_id, issue_ref, decomposed_from, stage_id, stage_type, stage_state,
  paths}`), `head_sha` (when read), `reason` (undetermined), and for the
  blocker refusal `next_step: fishhawk_await_stage`, `blocking_run_id` and
  `stage_id` of the first blocker (the MCP `NextAction`). **Those cross-run
  details render ONLY when the caller may read cross-run overlap (#4582,
  `overlapDetailsVisible`):** not a run-bound subject, and a cookie session or
  a token holding `read:audit` — the posture of `GET
  /v0/runs/{run_id}/scope-overlap` (the merge verb itself requires only
  `write:approvals`). Otherwise `mergeOverlapResult.withheld` rebuilds the
  details as `blocking_count`, `undetermined_count`, `head_sha` (when read),
  `reason` (undetermined) or `next_step` (blocker), `details_withheld: true`
  and `required_scope: read:audit`, with NO `blocking` / `undetermined` /
  `blocking_run_id` / `stage_id` key, and the blocker message is re-rendered
  naming no run, stage or path (the undetermined message already names only a
  count). The override path keeps the unredacted result for its audited row.
  The delegated arm's note and log use the same detail level for its
  identity (the campaign operator identity holds no `read:audit`, so its note
  is the withheld form). 502
  `overlap_check_failed` carries only the allow-listed `reason` +
  `retryable:true` (+ `run_id`); the raw cause is logged, never in the
  message. Messages are static wording plus ids, states, path COUNTS and the
  head; they say "No new merge verdict was recorded" (approval condition C5:
  on a resume a verdict row may already exist).
- **Override.** `allow_overlap: true` + `overlap_reason` (400
  `validation_failed {field: overlap_reason}` when blank after trimming or
  over 2000 bytes, decided before any read) overrides EVERY refusal the guard
  produces, 502s included — otherwise a GitLab or degraded-forge merge would
  have no way through. The recorded `overlap_reason` is the ORIGINAL string
  (the trim only rejects blank input), stored as JSON data and never
  interpolated into a message, prompt or log line. With the guard admitting,
  `allow_overlap` records nothing.
- **`merge_overlap_overridden` row, record-before-commit.** Payload:
  `run_id, pr_url, head_sha, overridden_code, overridden_reason, blocking,
  undetermined, overlap_reason, fingerprint`. It is appended at the FIRST
  commit point the request reaches: immediately before the
  `merge_queue_entered` append on the enqueue path (`mergeQueueEnqueueArm`'s
  `preCommit`, under the run's merge-intent lock) and immediately before the
  `merge_verdict_recorded` read/append otherwise. On a FIRST POST its row
  therefore precedes the verdict (or intent) row; on a resume the verdict row
  already exists and only a changed judgement appends a new override row. A
  failed dedupe read or append is 503 `overlap_override_unrecorded`: nothing
  merged or enqueued, no new verdict.
- **Dedupe.** `fingerprint` = sha256 over (overridden code, reason token,
  head, sorted `b:`/`u:` run_id:stage_id pairs, `overlap_reason`); a row
  carrying it already means "already durable", so the
  `fishhawk_merge_run` checks_pending re-POST loop writes one row per
  distinct judgement. A changed head, blocker set or reason writes a new row.
- **Access.** Withheld from run-bound subjects with `plan_scope_overlap`
  (`withholdScopeOverlapFromRunBound`, § "Cross-run scope overlap"): it names
  other runs' ids, issue refs and paths. Registered in
  `audit/categories.go`; NOT an issue-comment activity category.
- **Residuals.** ADR-093 slice 2's merge-on-pass (#4296) runs this guard
  under the run's merge-intent lock with no override, recording a refusal as
  `merge_queue_overlap_refused` (§ "Merge-on-pass (ADR-093 D3/D6)");
  `dispatchAcceptanceGatedMerge` does not run it (its delegated caller runs
  it first). TOCTOU: a push or a newly dispatched implement
  between the guard read and GitHub's merge is not caught. Cost per POST (or
  delegated pass) with a live candidate elsewhere: one stage list per
  candidate plus one `GetPullRequest` and one compare. **Fail-closed
  consequence for the delegated arm, accepted (#4582, approval condition
  C1):** a deployment where the guard cannot read this run's changed paths
  (no GitHub client, no installation, an unparseable repo, no PR number;
  GitLab runs land here) LOSES the delegated `may_merge` arm whenever another
  same-repo run has a live implement stage: the run parks `decision_required`
  `overlap_undetermined`, the campaign `GateActor` keeps polling it, and only
  an operator `fishhawk_merge_run` with `allow_overlap` clears it. This is
  accepted under the issue's no-delegated-override rule.
- **Counterfactual anchors** (`merge_overlap_guard_test.go` unless noted):
  the call `overlap := s.mergeOverlapGuard(r.Context(), runRow)`
  (`TestMergeRun_OverlappingImplementRefused`); `overlapBlockingStageState`
  (`TestMergeRun_OverlapNonLiveStageAdmits`, pending subcase); `return
  appendErr` (`TestMergeRun_OverlapOverrideAppendFailureRefuses`); the
  empty-rename-source check (`TestMergeRun_OverlapRenameSourceUnknownFailsClosed`);
  the `Truncated` read (`TestMergeRun_OverlapCompareTruncatedFailsClosed`);
  the `preCommit` call in `recordMergeQueueIntent`
  (`TestMergeRun_OverlapOverrideUnrecordedNeverEnqueues`); the withhold
  predicate (`reads_test.go`
  `TestListRunAudit_RunBoundWithholdsMergeOverlapOverride`). #4582
  (`autodrive_merge_overlap_test.go` for the delegated arm): the may_merge
  call line `} else if out, held := s.mergeOverlapAutoDriveHold(ctx, runRow,
  id); held {` (`TestAutoDriveRunGate_MergeOverlap_RefusesAndParks`); the
  `overlapDetailsVisible` body (`TestMergeRun_OverlapDetailsWithheldWithoutReadAudit`,
  `TestOverlapDetailsVisible`); `shown = overlap.withheld()` in
  `merge_run.go` (`TestMergeRun_OverlapDetailsWithheldWithoutReadAudit`);
  `shown = o.withheld()` in `autodrive.go`
  (`TestAutoDriveRunGate_MergeOverlap_NoteWithheldWithoutReadAudit`); the
  502 classification `if o.Code == mergeCodeOverlapCheckFailed {`
  (`TestAutoDriveRunGate_MergeOverlap_CheckFailedObservesOnly`); the
  `recordDelegatedMergeOverlapRefusal` call and its latest-row comparison
  (`TestAutoDriveRunGate_MergeOverlap_RefusalRecordedOncePerDecision`).

## Merge-candidate verdict transfer (ADR-096 D1/D2/D5, ADR-098 D3, E87.2 / #4293)

At a CLEAN base advance Fishhawk performed (the rebase verb's performed forge merge, and the ADR-092 queue's admission auto-advance), `merge_candidate_transfer.go` decides whether the prior head's PASSED merge-candidate verdict may transfer to the new merge commit H' instead of running a verify pass.

- **Mode.** `Config.MergeCandidateTransfer.Mode` is `off` | `shadow` | `enforce`. The Config ZERO VALUE is `off` (an in-process test server is unchanged); fishhawkd's `--merge-candidate-transfer` / `FISHHAWKD_MERGE_CANDIDATE_TRANSFER` defaults to `shadow` and refuses startup on any other value (`backend/README.md`). `Server.New` WARNs naming an explicitly configured mode that is not `shadow`, and — when not `off` — WARNs per missing `git` / `go` binary (without both every decision is `undecidable`, so the distroless image yields no shadow data).
- **Ladder** (`decideMergeCandidateTransfer`, first failure wins; the classifier is reached only past every precondition): no verify command or mode `off` → nothing computed, no row; cause `conflict_resolution` → `conflict_resolution`; not a performed merge, cause ≠ `base_advance`, or a new head that is not the decoded merge commit (the post-merge re-read observed a concurrent push; rebase verb and queue admission alike) → `not_clean_advance`; the prior head's state unreadable → `source_unreadable`, not `passed` → `source_not_passed:<state>` (a runner-gated head is `not_required`, so the FIRST advance after a PR opens always runs a pass); no working dir → `no_working_dir`; inputs → `undecidable:deadline_exceeded` | `undecidable:incomplete_export` (an incomplete or over-limit tree, an unsafe path, or a symlink resolving outside the repository) | `inputs:<sentinel>`; then the classifier's own code (`independent`, `global_surface`, `doc_check`, `shared_artifact`, `reach_overlap`, the closed undecidable sub-codes `undecidable:surface_undeclared` | `undecidable:surface_incomplete` | `undecidable:invalid_surface` | `undecidable:unsupported_language` | `undecidable:unclassified_path` | `undecidable:opaque_path` — matched before the generic row, the remainder after one `:` kept as UNTRUSTED `reason_detail` — else the generic `undecidable`). Only `independent` transfers.
- **Surface (ADR-124 / #4657).** `verdictTransferSurface` resolves the repository's workflow-v2 `verdict_transfer` declaration from the run's cached ADMISSION spec (`run.WorkflowSpec`) ONLY, through `basedelta.ResolveSurface`, never from the checkout, so a PR cannot change the surface its own decision uses. It runs right after the mode-off rung, so every computed row (ladder refusals included) records `classifier_version` (`basedelta.ClassifierVersion`), `surface_digest` (omitted when undeclared) and `surface_provenance` {`source: admission_spec`, `workflow_sha` (may be empty), `spec_sha256` of the cached bytes, `state`: `no_spec` | `unparseable` (logged WARN) | `undeclared` | `declared`}. An unparseable or undeclared spec passes a nil surface, which the classifier refuses (`surface_undeclared`). After a classification that did not hit the deadline, the prospective-coverage DIAGNOSTIC lists the declared patterns matching no file of either materialized tree (`surface_unmatched`, at most 16, each bounded and UNTRUSTED, plus `surface_unmatched_count`); a listing error or a spent budget skips it, and it never changes the verdict.
- **Enforce withheld (ADR-124 D4).** A declaration PRESENT without a `completeness_check` keeps enforce in shadow for that run: `mergeCandidateTransferEnforce` writes nothing and returns false, and `scheduleMergeCandidateTransferShadow` runs the decision as shadow (mode `shadow`) stamping `enforce_withheld: completeness_check_undeclared`. An ABSENT declaration is not withheld — the classifier refuses it, so no transfer happens either way. The rule is uniform for every repository.
- **Inputs.** `basedelta.GitDelta` on the run's LOCAL checkout (B' = the merge commit's second parent; from_base = the unique merge base of the chain's ROOT head and B'), then B' and H' materialized with `basedelta.MaterializeTreeLinks` — never `git archive`, so `export-ignore` cannot hide a dependent package — and every symlink of either tree whose resolved target is, or contains, a delta or PR path added to that list (`basedelta.ExpandSymlinkAliases`). PR paths are the union of from_base..root head and B'..H', so a merge-commit-only edit is seen. A transfer chain anchors on the ROOT recorded pass (`root_head`), so successive deltas are re-judged as one cumulative delta against the base the real pass verified.
- **Budgets.** The classification runs on its own context: `min(Deadline (30s default), remaining − Reserve)`. Queue admission reserves `mergeCandidateTransferHandOffReserve` (60s) for the re-anchor, verdict reads, pass start and Unadmit; the rebase verb's enforce arm runs on a DETACHED context bounded by `rebasePostMergeTailBudget` (that outer bound wins over basedelta's 5-minute `DefaultDeadline`) and starts a refused pass on a FRESH detached context. Exhaustion is `undecidable:deadline_exceeded`, a refusal, so the pass still starts.
- **Rows** (all run-level, system actor, every one carrying `mode`): `merge_candidate_verdict_transferred` {from_head, to_head, from_base, to_base, root_head, delta_paths_count, pr_paths_count, reason, reason_code, reason_detail, classifier_version, surface_digest, surface_provenance, surface_unmatched, surface_unmatched_count, enforce_withheld; `surface_version` was removed by #4657 — older rows keep it and nothing reads it} — enforce only, the caller skips the pass and re-anchors the run's active queue entry; `merge_candidate_verdict_transfer_refused` — enforce only, the pass runs; `merge_candidate_verdict_transfer_shadow` (INTERNAL; adds `would_transfer`, `duration_ms`) — shadow only, decided ASYNC after the unchanged pass start behind a one-slot semaphore; `merge_candidate_verdict_false_transfer` (ADVISORY, never a gate) — a shadow `would_transfer` whose head's pass recorded `failed`, appended by whichever of the two rows lands second, with a WARN log.
- **Fail-closed verdict (the rollback contract).** `newestMergeCandidateVerdict` reads transferred rows ONLY in `enforce`; in `off` / `shadow` they are ignored and the head fails closed to `unverified`, so switching away from enforce revokes every transfer already written. The merge gate, the queue, the fan-in hold and the rebase up-to-date arm all read through it; a read error on either category fails closed.
- **Untrusted reason.** The classifier's Reason names repository paths, so it is stored split: a closed `reason_code` plus a bounded, control-stripped `reason_detail` flagged `reason_untrusted: true`. It is neutralized by ONE helper, `issuecomment.NeutralizeUntrustedInline` (inside a code span on the comment timeline; as plain JSON on the gate view and the MCP mirror, whose description marks it UNTRUSTED DATA), and never rendered into an agent prompt.
- **Gate view `merge_candidate_verdict`** (`gateViewMergeCandidateVerdictFor`, handler only). ALWAYS present: `transfer_mode`; `head_sha` — the run's current head as the chain records it (`gateViewCurrentHead`: the highest-sequence head across `pull_request_opened`, `child_pushed`, `fixup_pushed`, `acceptance_scenarios_pushed`, `conflict_resolution_pushed`, `operator_commit_vouched` and a performed `branch_rebased`) — with `state` / `cause` / `source` (`recorded` | `transferred`) / `transferred_from` / `root_head` from `mergeCandidateVerifyState` for EXACTLY that head, so a fix-up or conflict-resolution push after a transfer reports the new head and never the older transferred verdict; `last_transfer_refusal` (the newest refused row); `shadow[]`, each paired AT READ TIME with the newest recorded `merge_candidate_verified` result for its `to_head` (`pass_result`, `false_transfer = would_transfer && pass_result == failed`); `false_transfers[]`. Any read failure (or no audit repository) omits the block and adds a `merge_candidate_verdict` `history_gaps` entry — never a partial block.
- **Residuals.** A delta REMOVING a package H' imports makes H' fail `go list`, so the decision is `undecidable`, not `reach_overlap` — fail closed and deliberate. A shadow row lost to a restart is a measurement gap, never a skipped pass. The gate view's head is chain-derived: an out-of-band push the operator never vouched is not seen there (the merge gate reads the forge head). A transfer sets the queue entry's phase `passed`, which ADR-093 merge-on-pass merges like a pass.
- **Tests.** `merge_candidate_transfer_test.go` (one per ladder arm, false transfer in both arrival orders, the prompt-render exclusion), `merge_candidate_transfer_e2e_test.go` (real git + real classifier through the real rebase handler), `merge_candidate_transfer_surface_e2e_test.go` (the same path over the DECLARED surface: a non-Go repository refused `unsupported_language` in enforce and never `would_transfer` in shadow, a nested Go module not a workspace, `surface_undeclared`, `unclassified_path`, a declared-inert transfer carrying `surface_unmatched` / `surface_digest` / provenance, a checkout declaration never read, and a workflow-spec change forcing a pass), `rebase_branch_test.go`, `merge_candidate_queue_test.go`, `gateview_test.go` (`TestGateView_MergeCandidateVerdict*`, `TestGateView_ShadowPairingFalseTransfer`, `TestGateView_LastTransferRefusalHostileReasonInert`) and `mcpserver`'s `TestMergeCandidateVerdict_WireShape`.

## Consolidated fan-in verify hold (`fanin_verify.go`, ADR-090 D5 / #4018, #4812)

`holdConsolidatedReviewForFanInVerify` runs whenever `DispatchConsolidatedReview` has resolved a decomposed parent's consolidated head: the first consolidated round, its re-run (`fishhawk_rerun_review`), the release Advance after a pass reports, and a post-review re-fire. It holds the round until the head's merge-candidate state is `passed` or `not_required`; with acceptance ahead of review, `FanInVerifySettled` holds acceptance on the same head.

- **Forced cause.** An `unverified` head starts its pass with cause `fan_in`, whatever `classifyMergeCandidateHead` answers (logged as `head_cause`). This applies ADR-090 D5 (the consolidated review waits for a passed verify of the fan-in head) and ADR-092 D1 (`fan_in` precedes approval, so it is not serialized); no amending ADR is needed. Before #4812 the hold forwarded the classified cause: a head carrying a base merge classifies `base_advance`, a serialized cause, so the start went into `serializeMergeCandidateVerifyPass`, which for a parent with no active entry and no merge-queue eligibility (its consolidated review has not run) answers `not_queued` and starts nothing. The hold logged that as a started pass and held forever.
- **Untouched.** ADR-093 queue admission (`merge_candidate_queue.go`) is unchanged. The merge gate still requires a verdict for exactly the merge-time head, and a later base advance at merge goes through the queue as before. Verdicts bind by head, not by cause (`newestMergeCandidateVerdict`, `mergeCandidateVerifyState`), so a `fan_in` pass on a `base_advance` head satisfies every reader of that head.
- **Start answers** (`fanInHoldOnStart`). Only an `in_flight` answer with `Triggered` true is a start.

  | Answer | Hold | Log |
  |---|---|---|
  | refusal | hold | WARN `could not be started`, the refusal |
  | nil answer | hold | WARN `returned no answer` |
  | `not_required`, `passed` | release | none |
  | `in_flight`, triggered | hold | INFO `pass started`, `stage_id`, `triggered=true` |
  | `in_flight`, untriggered | hold | INFO `already in flight`, `stage_id` |
  | `failed` | hold | WARN `FAILED`, route `fishhawk_fixup_stage` |
  | `not_queued` | hold | WARN `start_state=not_queued`: nothing enqueued or started |
  | `held` | hold | WARN `start_state=held`, `position`, `live_run_id` |
  | any other state | hold | WARN `start_state=<state>` |

  Only the triggered `in_flight` record contains `pass started`.
- **Recovery for a parent wedged before #4812** (e.g. c4d9aa3e / #4810): re-run the first consolidated round (`fishhawk_rerun_review`) or Advance the run. Either runs the hold, which now starts the `fan_in` pass; dispatch the re-opened parent implement stage.
- **Residual: a direct pass beside a queue entry.** The forced `fan_in` start takes the direct `beginMergeCandidateVerifyPass` path and skips `applyMergeCandidateStartToEntry` in two cases: (1) the parent holds an ACTIVE HELD queue entry, and (2) the parent holds a LIVE entry whose head went unverified (e.g. a `not_executed` settle after admission). No queue state is written. Per-head idempotence covers both: a later pump or admission reads `in_flight` or `passed` for the same head, and an admission that advances the branch produces a new head that needs its own pass.
- **Tests** (`fanin_verify_test.go`). `TestFanInHold_BaseAdvanceHead_StartsFanInPass` (one `fan_in` trigger, implement re-opened, zero `Enqueue`), `TestFanInHold_BaseAdvanceHead_ActiveHeldEntry_StartsDirectPass` (residual case 1: a direct pass, no queue write), `TestFanInAcceptanceHold_BaseAdvanceHead_EndToEnd` (`FanInVerifySettled` false then true, acceptance released, one consolidated round), `TestFanInHold_BaseAdvanceHead_PassedVerdictReleases` and `TestFanInHoldOnStart_Answers` (one row per answer, `not_queued` taken from the real start). Each base-advance test first asserts that the head classifies `base_advance` and that a `base_advance` start routes through the queue.
