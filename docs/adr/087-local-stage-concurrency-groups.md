---
id: ADR-087
title: "Local stage concurrency groups: server-coordinated admission of host-dispatched stages"
status: accepted
date: 2026-10-04
issue: https://github.com/kuhlman-labs/fishhawk/issues/3968
supersedes: []
superseded_by: ["ADR-094"]
applies_to: []
---

# ADR-087: Local stage concurrency groups: server-coordinated admission of host-dispatched stages

## Context

Local runs (`runner_kind: local`) on one host share everything their gates touch: tool locks (golangci-lint), the per-repository verify lock, the shared test Postgres, the Docker daemon and the CPU. Nothing coordinates them. Each operator session checks `pgrep` / `list_runs` before dispatching an implement stage and races every other session.

On 2026-10-03, two sessions' implement stages (runs 8ffaa5d1 and 1a0212a7) overlapped. Both verify gates contended. One burned a verify-fix pass on a lock failure. The other failed category B, wedged its decomposition parent, and lost about 50 minutes of work (#3948).

Tool-level mitigations exist but are toolchain-specific: #3962 (`--allow-serial-runners`, merged) and #3948 (lock contention becomes a retryable category C). #3964 proposes coordinating at the stage level in fishhawkd, which sees every local run, so it generalizes to any language or project.

#3964's first plan (run 7d4ce5a0) was rejected after review found:
- **pool starvation:** a session advisory lock held on one pooled connection while the stage CAS needs a second;
- **non-atomic accounting:** the CAS and the held-row update committed separately, so a crash between them escapes the slot accounting;
- **a slow release** when an admitted stage never gets a runner.

These are architectural choices, so they are recorded here before the replan.

## Options

1. **Status quo plus tool-level mitigations only** (#3962, #3948). Cheap, but every new toolchain needs its own fixes, and CPU and Docker contention remain.
2. **Server-coordinated concurrency groups at the host-dispatch marker** (#3964). A local stage acquires a group slot when it is dispatched; a stage that cannot acquire one is QUEUED (FIFO), not refused. Default group `local-implement:<host>` at limit 1; a workflow-v2 stage field overrides it.
3. **Runner-side host semaphore** (a file lock in the runner). Covers only runners on one host, gives no visibility to other sessions, and cannot queue fairly across MCP processes.
4. **Rely on container isolation** (E51 / #3966 / #2137). Isolates filesystems and locks but not CPU, the Docker daemon or the test database, and costs more CPU per verify; not a substitute.

## Recommendation

Option 2, with these constraints:
- **Atomic admission.** The queue check, the stage transition to `dispatched` and the holder record commit in ONE transaction on ONE connection, under a transaction-scoped advisory lock. Prefer try-lock and answer queued on a miss over a blocking wait, so admission can never starve the pool or leak a session lock.
- **Holding derived from stage state** (`dispatched`/`running`), with a liveness backstop keyed on the column runner heartbeats actually write. Release reuses the existing reap machinery.
- **Spawn failure releases the slot** promptly through the existing reap-failure path. A stage admitted but never spawned must not hold the host for long.
- **Queued is a 409** (`concurrency_slot_queued`), so a pre-change client fails closed and never spawns.
- **Queueing is a queue row plus a response block, not a new stage state.** This avoids touching the stage-state constraint, the transition table and every exhaustive state switch.
- **The waiter lives in the MCP process**, runs on a detached context with a stated cap, and rebuilds its spawn inputs at admission. Its death leaves a stale queue row that is skipped after a TTL, never a wedged group.

Tool-level mitigations stay as defense in depth.

## Decision

**Accepted (2026-10-04).** Adopt option 2: server-coordinated concurrency groups at the host-dispatch marker, with every constraint in the Recommendation above as part of the decision:
- atomic single-transaction admission under a transaction-scoped (try-)lock;
- holding derived from stage state, with a liveness backstop on the real heartbeat column;
- prompt slot release on spawn failure;
- a 409 `concurrency_slot_queued` so stale clients fail closed;
- a queue row plus a response block, not a new stage state;
- a bounded MCP-process waiter on a detached context that rebuilds its spawn inputs at admission.

The tool-level mitigations (#3962, #3948) remain as defense in depth. Implementation: #3964 (core), then #3969 (the remaining host-spawn verbs).

## Consequences

- **A new table and a new wire contract** on the host-dispatch marker: optional `{host}` body, the 409 `concurrency_slot_queued`, and a stage `concurrency` block. Plus a workflow-v2 `concurrency` stage field.
- **Version skew** between slices is fail-closed. A pre-change MCP client sees a queued dispatch as an error and spawns nothing.
- **A background spawner in the MCP process** is a new behaviour: a runner can start after the dispatching tool call returned. It must be visible (status, audit rows `stage_concurrency_queued` / `stage_concurrency_admitted`) and bounded.
- **Residuals to state:** the host key is a client-supplied label (not a security boundary); stages already dispatched before deploy are not counted once; the queue serializes local implement throughput by design.

## Relations

Companion: #3964 (core implementation), and its follow-up for the remaining host-spawn verbs. Related: #3948, #3962, ADR-024, ADR-063.

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

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":1715} -->
