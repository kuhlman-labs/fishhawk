---
id: ADR-108
title: "Shared Postgres lock-holder budget: one pool-wide pglock budget with reserved headroom and enforced no-nesting for every connection-holding advisory-lock helper"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4594
supersedes: []
superseded_by: []
applies_to: ["backend/internal/pglock/**"]
---

# ADR-108: Shared Postgres lock-holder budget: one pool-wide pglock budget with reserved headroom and enforced no-nesting for every connection-holding advisory-lock helper

**Status: accepted (ratified by the Captain 2026-10-21).**

## Context

Several helpers take a session advisory lock on a pooled connection, then run `fn` while that lock is held:
- `mergequeue.WithMergeIntentLock` (#4295);
- `run.WithConsolidatedPROpenLock` (#4110);
- the refinement `WithFilingLock`.

`fn` itself acquires further connections from the SAME pgx pool. Each helper either had no bound or its own independent bound (#4295 shipped a store-local MaxConns/2 semaphore). Holders of different helpers could therefore fill the pool together and wait on each other forever: a hold-and-wait across the whole pool (MaxConns 10, and request contexts carry no deadline). Found reviewing #4295; implemented by #4591 (run c7cf44ee).

## Options

1. **Run fn's DB work on the locked connection or transaction.** No second connection is needed. This requires reshaping every fn (audit appends, queue writes, forge calls).
2. **A dedicated small lock pool, or a try-lock loop.** The lock connection comes from a separate pool.
3. **One shared holder budget per pool** (chosen for #4591).

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer): option 3, with the rules below, as implemented by #4591 (PR #4597).

Option 3, with these rules:
- **One budget per pool.** A new `backend/internal/pglock` package keeps one holder budget per pool in a pool-keyed registry. Every connection-holding lock helper takes a slot BEFORE acquiring its lock connection.
- **Reserved headroom.** HolderLimit = max(1, (MaxConns − R)/2), with a named reserve R ≥ 2. At saturation the holders and their fns use at most MaxConns − R connections, leaving R for health checks, the boot sweep, tickers and handlers.
- **Minimum pool size.** MaxConns ≥ R + 2. Below it, every holder fails closed with a named error.
- **Invariant:** a holder's fn uses at most ONE further pool connection at a time. Detached goroutines spawned from fn are ordinary non-lock load, bounded by their own contexts.
- **No nesting, enforced.** fn receives a marked ctx, and a budgeted acquire on a marked ctx fails closed (`ErrNestedLock`) before taking a slot or a connection. Detached work spawned from fn must clear the marker.
- **Every helper:** an fn deadline that does not depend on the caller's context, a panic-safe deferred unlock, and a lapsed or failed unlock surfaced as `ErrLockLapsed` rather than swallowed.
- **Why it is separate from ADR-094's admission groups:** those govern spawning verbs, meaning runner or host processes. This budget governs connections inside one fishhawkd process's pool.

## Consequences

- Any NEW helper that holds a pooled connection while fn uses the pool must take the pglock budget. Otherwise it reopens the starvation class.
- A pool smaller than R + 2 refuses lock-holding work.
- The record lands in `docs/adr/` under the upkeep rule after the catch-rate freeze lifts on 2026-11-01.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2193} -->
