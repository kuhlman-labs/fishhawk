---
id: ADR-109
title: "Amend ADR-093 D1: the merge queue owns the merge — one database-enforced per-run dispatch ledger across inline, delegated and queue merges, plus the admission-epoch CAS protocol"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4601
supersedes: []
superseded_by: []
amends: ["ADR-093"]
applies_to: []
---

# ADR-109: Amend ADR-093 D1: the merge queue owns the merge — one database-enforced per-run dispatch ledger across inline, delegated and queue merges, plus the admission-epoch CAS protocol

**Status: accepted (ratified by the Captain 2026-10-21).** Tracker-only until the 2026-11-01 catch-rate re-pin; the docs/adr/ record then lands under the upkeep rule. It amends ADR-093.

## Context

ADR-093 slice 2 (#4296, run f27f4635) makes the merge queue merge the head of the line itself (merge-on-pass). Two other paths can also dispatch a merge for the same run:
- the operator's inline `merge_run`;
- the delegated `may_merge` arm (`dispatchAcceptanceGatedMerge`).

Plan review found two HIGH double-dispatch interleavings while a queue entry is in phase verifying:
- an inline retry that reuses the older verdict under the 0062 once-index;
- the delegated arm arming auto-merge.

In both cases merge-on-pass dispatches again afterwards. The same review asked for the new admission-epoch concurrency protocol to be recorded.

ADR-093 D1 says an up-to-date PR with a valid pass "may merge inline, as today". This amendment narrows that.

## Options

1. A head pin alone. This still leaves two writers.
2. Serialize on the shared merge-intent lock alone. This depends on every path taking the lock correctly.
3. **A database-enforced, single, run-scoped dispatch ledger, written under the shared lock.**

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer), as stated below (first-officer ruling of the same day). Implemented by #4296 (run f27f4635).

Option 3:
- **The queue owns the merge.** While any queue entry, live or held, carries merge intent for a run:
  - inline `merge_run` refuses with 409 `merge_queue_owns_merge`, unless the PR already merged;
  - the delegated arm is observe-only.
- **One dispatch ledger.** Each run has one record with a UNIQUE constraint on run_id (path, head SHA, state `dispatched` or `dispatch_failed`).
  - Every dispatch path claims it under the shared pglock merge-intent lock (ADR-108) BEFORE any forge merge or auto-merge arm.
  - A unique violation means "already dispatched", and the forge is never called.
  - A failed dispatch is marked `dispatch_failed`, never deleted. A retry needs an explicit, operator-only re-arm.
  - The constraint holds even without the lock.
- **Settle inside the lock.** A successful merge-on-pass settles the entry merged before the lock is released.
- **Admission-epoch CAS.** Every queue admission increments a monotone `admission_epoch`.
  - Admission-owned writes are compare-and-swap on the epoch: Unadmit, SetPhase, SettleAt and ReanchorAt. A stale decision gets ErrStateConflict.
  - Writes that record external facts are unconditional: PR merged or closed externally, run terminal.

## Consequences

- A future merge path must claim the dispatch ledger under the lock, or it reopens double dispatch.
- The inline `merge_run` merges only when the queue holds no intent for the run.
- The record lands in `docs/adr/` under the upkeep rule after the catch-rate freeze lifts on 2026-11-01.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- #4291 [E87] Merge-path throughput: verify once, transfer verdicts across independent base deltas, absorb flakes, parallelize acceptance (ADR-096) — low confidence (0.18); shared: across, adr, merge

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"epic_suggestion":{"number":4291,"url":"https://github.com/kuhlman-labs/fishhawk/issues/4291","title":"[E87] Merge-path throughput: verify once, transfer verdicts across independent base deltas, absorb flakes, parallelize acceptance (ADR-096)","score":0.1810810810810811,"confidence":"low","basis":"across, adr, merge"},"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2423} -->

## Amendment A1 (2026-10-23): an orphaned queue dispatch claim is released on forge evidence (#4298)

**Ratified by the Captain 2026-10-23 (v2).** Drafted by the first officer on 2026-10-23. v1 was reviewed by claude-opus-5-5 and claude-fable-5-1, both "revise", and their fixes converge.

**Context.** A queue merge claims the run's dispatch ledger row (`dispatched`) at step 8 of `mergeOnPassLocked`, makes one pinned REST merge call (at most 30 s), and settles. All of this happens inside the per-run merge-intent lock (ADR-108, 90 s fn bound).

A crash between the claim and the settle leaves a `dispatched` row with no live holder. A same-process strand can do the same: when marking `dispatch_failed` does not land, the claim stands as `dispatched`.

Today that run has NO remedy:
- every pump holds the entry `merge_already_dispatched`;
- inline `merge_run` answers 409, because the queue owns the merge;
- the operator re-arm covers only `dispatch_failed`, and "a `dispatched` claim is never re-armed".

The run is stuck until it is cancelled.

**Decision.** This applies to a claim with `path = 'queue'` only.
1. **Orphan predicate.** A queue claim is orphaned when it is observed by a process HOLDING the run's merge-intent lock, and `claimed_at < db_now − (lock fn deadline + merge-call timeout + margin)`, both on the database clock. Every legitimate claim is taken and settled inside that lock, so an orphan has no live holder. The age floor covers a lock that lapsed while a merge call was still in flight. No boot timestamp is used.
2. **Placement.** Reconciliation happens inside `mergeOnPassLocked`, in step 8's not-claimed branch, after steps 1–7 have re-read the entry and the PR and re-run the head, base, fold and R4 checks under the lock. The forge read is the one the lock already performs.
3. **Outcomes:**
   - **PR merged:** reconcile through `mergeOnPassReconcileMerged`, as today.
   - **PR closed unmerged:** settle the entry closed.
   - **PR open and unmerged:** the forge proves the earlier dispatch applied nothing, so the claim is RELEASED. It is deleted by compare-and-swap, exactly as `ReleaseMergeDispatch` does, with a `merge_dispatch_settled` row (outcome `released`, reason `released_orphaned_claim`). The pump then falls through to the ORDINARY merge-on-pass: head and base checks, the ADR-112 fold with its wait or yield, the R4 `merge_queue_authorized` row, the verdict row, a fresh claim, and the pinned merge.
   - **Indeterminate forge read:** the claim moves to `dispatch_failed`, cause `orphaned_by_restart`, with an audit row, and ADR-109's operator-only re-arm applies.
4. **Not in scope.** Inline and delegated claims are not touched: their `dispatched` row is normal while GitHub auto-merge is armed. GitLab claims are not touched either; queue merge-on-pass is `provider_unsupported` there (ADR-112 R6).
5. **Visibility.** An orphaned or released claim is surfaced as a named state in the merge-queue read's `dispatch_claim`, and in `get_run_status.next_actions`.

**Why it is safe.**
- There is no bespoke retry. The only merge after a release is the ordinary pinned one, under every existing check.
- The pinned PUT carries `sha` (409 on a mismatch, 405 on an already-merged PR), so the forge is the compare-and-swap and the PR merges once.
- A crashed process's late PUT is caught by the existing `observedMerged()` re-read before settling.
- A restart costs at most one extra pinned merge call.

**Consequences.**
- **No schema change:** no `released` state and no `resume_retried_at`.
- **Audit:** new reason values on the registered `merge_dispatch_settled` category, plus the `orphaned_by_restart` cause.
- **Tests, each with a counterfactual:**
  - a young claim is not orphaned;
  - an inline claim with auto-merge armed is untouched;
  - an orphan with the PR open is released and then merges through the ordinary path;
  - an indeterminate read leads to `dispatch_failed` and then a re-arm;
  - a merged PR reconciles;
  - a closed PR settles.
- **Implementation:** #4298 already delivers points 1, 3 (indeterminate → `dispatch_failed`) and 4, under ADR-109 as written. The release-on-forge-evidence arm in point 3 is added by #4298 if A1 is ratified before its implement stage, or otherwise by a follow-up child.
- **Record metadata:** an in-body amendment to ADR-109. No new edge.

## Amendment A2 (2026-10-31): the resume releases an orphaned queue claim before the fold (#4862)

**Ratified by the Captain 2026-10-31 (v2).** Drafted by the first officer on 2026-10-31 under the Captain's choice of option (a) of #4862, with the step-8 release kept as a backstop. v1 was reviewed by claude-fable-5-1 and gpt-6-astra (codex), both "revise", and their findings are folded in. Neither found a safety hole in the placement.

**Context.** Verified at dc752ab1. All paths are under `backend/internal/`.

A1 releases an orphaned queue claim only in step 8 of `mergeOnPassLocked`. Step 8 runs after the step-6b ADR-112 fold has approved; an unapproved fold returns at `server/merge_queue_merge.go:1056-1062`. The ADR-093 D8 resume's `orphaned_claim_open` arm already establishes, under the run's merge-intent lock (`server/merge_queue_resume.go:260`), every fact A1's safety proof uses:
- the claim is a `queue` claim still `dispatched` (`:317`);
- the PR reads open and unmerged (`:319-375`);
- the claim is crash-left on the database clock (`:420`, `:479-495`).

Even so, that arm writes nothing to the ledger and returns the entry to `passed` (`:424-433`), leaving the release to step 8. When the fold is not approved, step 8 never runs, so the claim outlives the merge attempt. The resulting behaviour is inconsistent:

1. **The fold's yields skip the claim.** The `await_approval` yield at expiry and a veto's immediate yield go through `applyMergeOnPassOutcome` → `yieldMergeQueueEntry` (`server/merge_candidate_queue.go:538-552`), which never reads the claim. `Store.Yield` compares only liveness and the admission epoch (`mergequeue/store.go:885-905`). The entry yields with an orphaned `dispatched` claim still standing.
2. **Two yields refuse on any claim.** Both run `mergeOnPassPreYieldRefusal` (`server/merge_queue_yield.go:402`), which refuses on ANY claim and holds the head of the line with no yield marker:
   - the ADR-112 R3 expired-wait enforcement, on a readvance or a transient hold with an open approval wait (`server/merge_queue_yield.go:338-349`);
   - the ADR-112 A3.2 merge-gate wait, including the immediate red-check yield (`server/merge_queue_merge.go:848`).

   With red checks, approval does not unstick the entry. Only green CI or the operator can.
3. **Withdraw-intent is blocked.** A yielded entry that carries the orphaned claim cannot use ADR-112 R3's withdraw-intent verb (#4685). `withdrawIntentTx` refuses any claim (`mergequeue/store.go:1488-1492`). Such an entry leaves the queue in one of two ways:
   - an automatic clear, which needs an approved fold and passing merge-gate legs, followed by step 8;
   - observing the PR merged or closed, or cancelling the run.

ADR-112 R3's Ledger bullet assumes the opposite: "a wait, and every yield except row 4's, happens BEFORE any dispatch claim".

**Why the resume, not an earlier step of merge-on-pass.** Merge-on-pass's pre-lock steps return before the lock is taken:
- the head-moved readvance (`server/merge_queue_merge.go:741`);
- step 4's `await_gate` (`:773`).

A release placed inside the lock therefore misses the merge-gate case. The resume runs first. Every orphan created after A2 passes through it, because merge-on-pass stamps `merging` under the lock BEFORE it claims (`:1086-1107`), and the pump routes a `merging` entry to the resume (`server/merge_candidate_queue.go:427`).

**Decision.** A1 point 2 (Placement) is amended. Everything else in A1 stands.

1. **Release at the resume.**
   - **Where.** The ADR-093 D8 resume's `orphaned_claim_open` arm RELEASES the orphaned claim before the entry returns to `passed`.
   - **Guards.** They are unchanged:
     - a live `merging` entry at its admission epoch;
     - a `queue` claim still `dispatched`;
     - crash-left by A1's predicate on the database clock;
     - observed under the merge-intent lock;
     - a forge read showing the PR open and unmerged.
   - **What it never touches.** A `settling` entry, an inline, delegated or GitLab claim, a `dispatch_failed` claim and a young claim are never released. The closed and indeterminate arms are unchanged.
   - **Order:**
     1. the `merge_queue_resumed` {`redecide`, `orphaned_claim_open`} row;
     2. the strict `merge_dispatch_settled` {`released`, `released_orphaned_claim`} row (record before action);
     3. the exact-claim compare-and-swap DELETE (`ReleaseMergeDispatch`);
     4. `SetPhase(passed)` at the epoch, then redecide.

     The implementation reuses `mergeOnPassReleaseOrphanedClaim` (steps 2–3) at the new site, so there is ONE release body.
   - **Failures.** Each one stops the decision and holds. The next tick re-reads the authoritative state; no state guarantee beyond that re-read is claimed.
     - **An append fails** (either row): hold `resume_unrecorded`; the ledger is unchanged. Each tick may write one `redecide` row that is not followed by its action, as A1 already documents for a failing release.
     - **The DELETE fails, or its outcome is unknown:** hold `orphaned_claim_release_failed`. The claim may or may not still stand. The next resume re-judges it: still orphaned means another release attempt, and gone means the no-claim arm.
     - **The phase write fails:** hold. The claim is already gone. If the entry is still `merging` at the same epoch, the next resume takes the no-claim arm (`server/merge_queue_resume.go:397-405`). A genuine CAS conflict means the entry left that epoch, and the next pump handles whatever is live.
   - **Lock lapse.** The resume now logs a WARN when its merge-intent lock lapsed after the judgement ran, as merge-on-pass does (`server/merge_queue_merge.go:798-803`). The DELETE remains an exact-claim CAS.
2. **Step 8 stays as a backstop.** `mergeOnPassReleaseOrphanedClaim` in step 8 is unchanged. It covers:
   - **(a)** an orphan stranded on a non-`merging` entry before A2 shipped. Such a claim releases at step 8 once that entry's fold approves.
   - **(b)** step 1's in-lock acceptance of a `merging` entry after a stale pre-lock read (ADR-093 D8 ruling R3, `server/merge_queue_merge.go:927`).
3. **No claim survives a successful resume release.** An orphaned claim released by the resume never reaches the fold. Every ADR-112 R3 and A3 wait and yield, the pre-yield refusal, and withdraw-intent then apply to the entry unchanged, and R3's premise holds for every orphan created after A2. `mergeOnPassPreYieldRefusal` keeps ADR-109's rule unchanged ("a claimed dispatch is held, never yielded"), and no yield exception is added.
4. **The resume's ledger writes, enumerated.** The resume never CLAIMS and never DISPATCHES a merge. Its ledger writes are exactly two:
   - the existing `dispatch_failed` marking (`orphaned_by_restart`) on the closed and indeterminate arms;
   - A2's release.

   This narrows the #4298 approval rulings R1/R2 and the `merge-queue.md` statement that A1's release "is merge-on-pass's, not the resume's".

**Transition.** A2 does not reach an orphan already stranded on a `passed` or yielded entry when it ships. Such an entry holds or stays yielded until its fold approves and step 8 releases it, or until the operator cancels the run. On 2026-10-31 the `kuhlman-labs/fishhawk` `main` merge queue had zero entries, so no such orphan exists in this deployment.

**Why it is safe.** A1's argument does not depend on the fold, and A2 changes only where the release happens. The proof is A1's, with its assumptions inherited and stated:
- **Assumption 1.** An open, unmerged PR read after the A1 age floor means the crashed dispatch applied nothing.
- **Assumption 2.** A lapsed lock's context deadline bounds the holder's client work. It does not prove the forge stopped, so the floor is a margin, not a proof.

Two properties stay separate:
- **At-most-once merge.** The pinned PUT carries `sha`, so the forge is the compare-and-swap and the PR merges once.
- **Authorization.** The only merge Fishhawk dispatches after a release is the ordinary pinned one, after steps 1–9 under the lock (head, base, approved fold, R4 authorization, verdict). Releasing earlier skips no approval. Queue ownership, not the claim, keeps inline `merge_run` out during the longer window in which the claim is gone.

**Late-PUT residual.** A PUT from the crashed process that lands after the release is observed by the next decision's PR read. It settles `pr_merged` with no claim and no `merge_queue_merged` tail (`server/merge_queue_merge.go:139-141`, `:938`). That is safe, but its attribution is weaker than A1's in-lock `observedMerged()`.

**Out of scope (pre-existing, filed as #4876).** Concurrent pumps on one live head of line can apply a stale fold yield, decided under the lock but written outside it, after another pump has claimed a dispatch. `Store.Yield` checks only liveness and the epoch, so the entry can yield with a fresh claim standing (gpt-6-astra review). A2 does not create or close this race. The fix, re-checking the entry and claim under the lock before a fold yield is applied, is tracked in #4876.

**Consequences.**
- **Schema and audit.** No schema change and no new audit category. The existing `merge_dispatch_settled` reason `released_orphaned_claim` is written from a second site.
- **Docs.**
  - `server/docs/merge-queue.md`:
    - the "release is CONDITIONAL on that decision's fold approving … holds the head until it arrives" prose;
    - the resume table row;
    - the "SOLE added automatic ledger transition … merge-on-pass's, not the resume's" bullet.
  - `merge_queue_resume.go` comments: lines 12-14, 106-110, 119-120, 408-416 and 432.
  - `merge_queue_merge.go` comments: lines 273-281, 487 and 1112-1116.
  - `merge_candidate_queue.go` comment: lines 563-564.
  - ADR-112 needs no substantive change. A one-line cross-reference to ADR-109 A2 is optional in R3's Ledger bullet.
- **Tests, each with a counterfactual:**
  - **Unapproved fold.** An orphan on an unapproved fold is released at the resume. The entry then waits and yields at expiry with no claim row, and no fresh claim or PUT happens before approval.
  - **Red checks.** An orphan with red checks yields `merge_checks_red` instead of holding.
  - **Other expiries.** An orphan with an R3 readvance or transient expiry, or with a pending-check or review-gate expiry, yields instead of holding.
  - **Withdraw-intent.** It succeeds on a yielded entry whose orphan the resume released.
  - **Order.** The rows land as resumed → settled → DELETE → phase.
  - **Failures.**
    - Each append failure holds `resume_unrecorded` with the ledger unchanged.
    - A failed DELETE or a CAS miss holds `orphaned_claim_release_failed`, and the next resume re-judges.
    - A crash after the DELETE but before the phase write: the next resume takes the no-claim arm.
    - A phase CAS conflict holds.
  - **Exclusions, all NOT released:**
    - a young claim, including the exact age boundary;
    - a zero `claimed_at`, or an unreadable database clock;
    - a stale entry or epoch, or a `settling` entry;
    - an inline, delegated or `dispatch_failed` claim;
    - the closed and indeterminate arms, which still mark `dispatch_failed`;
    - a run with this process's merge in flight, which is still skipped.
  - **Flipped test.** The aged half of `TestMergeQueueResume_YoungQueueClaimHolds`, which asserted no resume ledger write, flips.
  - **Competing resumes.** A second resume after the release holds `entry_changed`.
  - **Late merge.** A PR that reads merged after a resume release, with no claim, settles `pr_merged` with zero PUTs.
  - **Step-8 backstop.** A yielded entry carrying a pre-A2 orphan, re-admitted and approved, releases at step 8.
- **Implementation:** #4862 is re-scoped to this amendment's implementation once A2 is ratified.
- **Record metadata:** an in-body amendment to ADR-109, with no new edge. ADR-112 A3's "after dispatch, nothing changes" is unaffected, because once the claim is released no dispatch exists.

