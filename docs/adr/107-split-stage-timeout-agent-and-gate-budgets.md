---
id: ADR-107
title: "Amend ADR-025: split the stage timeout into an agent budget and a project-derived verify-gate budget"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4581
supersedes: []
superseded_by: []
amends: ["ADR-025"]
applies_to: []
---

# ADR-107: Amend ADR-025: split the stage timeout into an agent budget and a project-derived verify-gate budget

**Status: accepted (ratified by the Captain 2026-10-21).** This record is tracker-only for now: docs/adr is frozen until 2026-11-01. It **amends** ADR-025 and does not supersede it.

## Context

ADR-025 frames a single spec-resolved stage timeout. In practice an implement attempt's `agent_timeout_seconds` was consumed by BOTH the agent's work and the runner's committed-tree verify gate (scoped, full, absorb re-runs, strict re-verify). A full verify takes 20–25 minutes in this repository and about a minute in others, so whether a stage fits depended on the project, not on the agent. Evidence is in #4023: the #3964 fix-up overran its budget with the gate still running.

#4023 (run 23b2b3fa, plan approved 2026-10-20) implements the split:
- `agent_timeout` covers agent work only, and stops when the agent exits;
- the gate gets its own budget of `N × executor.verify.timeout`, where N enumerates every verify execution the runner may charge to the gate phase (at least 9);
- a gate that exceeds its budget fails category C as a gate timeout, not an agent timeout;
- stage health and `fishhawk_await_stage` report both phases separately.

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer), as stated below and implemented by #4023 (PR #4589). It amends ADR-025 and does not supersede it.


ADR-025's single stage timeout becomes two budgets. The agent budget comes from the spec, as before. The gate budget is derived from the project's declared verify timeout and the runner's enumerated verify forms. Both are reported on the progress wire contract (#2558-registered pair `stage_progress_report`).

## Consequences

- A project with a slow verify no longer starves its agent.
- The gate budget is a new kill. An undercounted N would fail a passing tree, so the enumeration is pinned by a test and documented with call sites.
- Once ratified and the freeze lifts, `docs/adr/` gains the amending record, and ADR-025's front matter gets `amended_by` under the upkeep rule.

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

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2762} -->
