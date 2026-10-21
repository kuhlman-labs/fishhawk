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
