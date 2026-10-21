---
id: ADR-106
title: "Run-bound agent tokens get a subject-filtered audit read: how cross-run audit categories are withheld"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4575
supersedes: []
superseded_by: []
applies_to: ["backend/internal/audit/categories.go"]
---

# ADR-106: Run-bound agent tokens get a subject-filtered audit read: how cross-run audit categories are withheld

**Status: accepted (ratified by the Captain 2026-10-21).** This record is tracker-only for now: docs/adr is frozen until 2026-11-01, so the in-repo record follows the upkeep rule after ratification. The implementation follow-up goes under E89 #4537.

## Context

#4118 (run 5702aa2e) adds the `plan_scope_overlap` audit category. Its payload names OTHER in-flight runs: their ids, issue refs and file paths. Approval condition 1 withholds that data class from run-bound (`mcp:run:*`) subjects on every surface. Three reviewers independently found that the audit log was the last surface still exposing it.

The fix makes the audit read an access-control boundary for run-bound subjects, for the first time. Until now the per-run and global audit list reads did no subject-keyed filtering. `withholdDelegationShadow` is a render preference that any caller bypasses by naming the category; it is not access control.

#4118 ships it as a **deny-list** in `backend/internal/server/reads.go` (`withholdScopeOverlapFromRunBound`):
- `plan_scope_overlap` is withheld from run-bound subjects on the non-chain per-run read and on the global feed, before pagination and regardless of the requested category;
- a run-bound `chain=true` read is refused with 403 `run_token_forbidden`, because a chain read must stay complete for hash verification.

**The design question.** A deny-list fails open for the future. Any later audit category whose payload names another run leaks to run-bound readers until someone remembers to add it to reads.go. Nothing in the audit-category checklist (AGENTS.md, `backend/internal/audit/categories.go`) prompts that.

## Options

- **(A) Keep the deny-list and add a guard test.** The test fails when a registered category whose payload can name another run's id is not classified as withheld. Smallest change. Cross-run-ness is hard to detect mechanically, so the test needs a hand-maintained classification anyway.
- **(B) Registry-driven withhold.** Add a `cross_run` (or `run_bound_readable`) flag to each entry in `backend/internal/audit/categories.go`, and drive the withhold from the registry. Registering a new category forces the decision, which mirrors the existing audit-category checklist (`TestKnownCategoriesCoversEmittedCategories`). Still fails open if the flag defaults to readable, unless the flag is required.
- **(C) Allow-list.** Run-bound readers see only categories explicitly marked readable. Fail-closed by construction. Every category an in-run agent legitimately reads today must be enumerated (for example review and stage outcomes awaited through `fishhawk_await_audit`), so it risks breaking agents until the list is complete.

## Recommendation

(B), with the flag REQUIRED on registration so there is no silent default. That gives the fail-closed property of (C) at registration time, without an up-front enumeration of everything agents read today. The overseer also leans (B).

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer): **option (B)**, with the classification flag REQUIRED on registration and no default.
- Every entry in `backend/internal/audit/categories.go` declares whether run-bound (`mcp:run:*`) subjects may read it. A category registered without the declaration fails `scripts/test verify`.
- The run-bound audit withhold (per-run non-chain read, global feed, MCP paths) is driven from that registry rather than the `withholdScopeOverlapFromRunBound` deny-list in `reads.go`. The deny-list is retired once the registry drives it.
- A run-bound `chain=true` read stays refused (403 `run_token_forbidden`).
- Implemented as a follow-up under E89 #4537. The in-repo `docs/adr/` record lands after the 2026-11-01 catch-rate re-pin under the upkeep rule.

## Consequences

- Implementation is a follow-up slice under E89 after ratification. #4118's scope is not widened for it.
- A run-bound `chain=true` read stays refused under every option.
- The audit-category checklist in AGENTS.md gains one step: classify run-bound readability.

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

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2801} -->
