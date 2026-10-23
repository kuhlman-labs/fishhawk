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
