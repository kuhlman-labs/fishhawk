# backend/internal/auditcomplete

Audit-complete derivation (#229, #282, #947, #3092): derives the `fishhawk_audit_complete` blocking-check state on demand via `ComputeResult(ctx, runID, deps) (Result, error)`, which returns `{State, Missing[], Resolved[]}`. `Compute(ctx, runID, deps) (state, missing, error)` is a signature-identical thin wrapper that drops `Resolved`, so every pre-#3092 call site compiles and behaves unchanged.

## Sub-topics (full detail in docs/architecture/audit-complete.md)

- [The six rules](../../../docs/architecture/audit-complete.md#the-six-rules)
- [Normalization for rule 4](../../../docs/architecture/audit-complete.md#normalization-rule-4-specifics) (#302/#308)
- [Rule 2b decomposition trace resolution](../../../docs/architecture/audit-complete.md#rule-2b-decomposition-trace-resolution-3092-details) (#3092)
- [Rule 5 live-HEAD check](../../../docs/architecture/audit-complete.md#rule-5-live-head-check-details) (#282)
- [Rule 6 review-presence gate](../../../docs/architecture/audit-complete.md#rule-6-review-presence-gate-947-details) (#947)
- [State output](../../../docs/architecture/audit-complete.md#state-output)
- [Integration points](../../../docs/architecture/audit-complete.md#integration-points)
- [Republish-on-drift](../../../docs/architecture/audit-complete.md#republish-on-drift)
- [Verifier mirror](../../../docs/architecture/audit-complete.md#verifier-mirror) (ADR-008 / #72)

## Rule 6 (#947): review-presence gate

Rule 6 makes the ADR-027 advisory implement-review a pre-merge **presence** gate: `Compute` appends a pending-flavored `review_pending` missing item (state `pending`, never `fail`) while a configured `reviewers.agent` review is dispatched but not yet terminal, and `runImplementReviewLoop` republishes the Check Run green via `recomputeAndPublishAuditComplete` once it lands.

The present/in-flight decision (`auditcomplete.ReviewPresent`) is shared single-source with the ADR-036 merge-resolution hold (`server.checkImplementReviewSettled`), reusing the same `planReviewBackstop` so a dead reviewer can't wedge the gate; the advisory verdict stays non-blocking (any terminal kind clears it).

The MCP `fishhawk_get_run_status` surfaces a display-only `implement_review_merge_hint` mirroring the same pending condition for the local loop.

## Acceptance short-circuit trace exemption (#1728 / #1748) is BASIS-keyed, not verdict-keyed

An acceptance stage the orchestrator short-circuited pre-spawn ships NO trace bundle yet is legitimately `succeeded`, so `Compute` exempts it from Rule 4's trace-required check. The exemption is keyed on the `acceptance_outcome_recorded` payload's **`basis`** field (`plan.AcceptanceBasisEmptyCriteria` / `plan.AcceptanceBasisAllSkipWithBasis`) — any other basis, or a normal validator-recorded verdict (which never sets `basis` at all), is NOT exempted and still requires its trace.

Because the key is the basis and not the verdict, #2347's change of that short-circuit verdict from `passed` to `not_validated` leaves the exemption firing exactly as before. That independence is a pinned regression test, not an assumption: `auditcomplete_test.go` asserts the exemption still applies to a `verdict: not_validated` entry carrying a known basis, and still does NOT apply to a validator-recorded verdict with no basis field.

A read failure on either the skip-marker or the outcome-entry query is transient and returned to the caller, so the gate never silently under- or over-gates.

## Rule 2b (#3092): decomposition trace resolution is NEVER an exemption

A decomposed run's parent implement stage is the fan-out stage — it parks `awaiting_children`, spawns no agent, and by construction can never carry a trace — so rule 2 was unsatisfiable for every decomposed run and the required check could never go green.

`ComputeResult` now RESOLVES that evidence through the fan-out instead of exempting the stage: when the parent implement stage has a trace gap and the run has decomposition children (`run.ListRunsFilter.DecomposedFrom`), it reads each child run's own implement-stage `trace_uploaded` entries and satisfies the parent's requirement only when every executed child is trace-complete. A child genuinely missing a trace still FAILS, naming the child run id and the child stage id. Only the implement stage is eligible — the misses are partitioned by owning stage id through a parallel slice returned by `missingTraces` (never a `Detail` string match, and never a new field on the wire-serialized `MissingItem`), so a plan/acceptance trace gap passes through verbatim.

The four fail-closed branches:

- **Non-terminal child implement stage** (`pending`, `running`, `awaiting_*`) → the parent's opaque `trace_missing` items are replaced by a single pending-flavored `children_pending` item. State `pending`: the requirement stays UNSATISFIED and the merge stays blocked exactly as `review_pending` blocks it today — it is not-yet, never a pass.
- **Zero contributors** (every child cancelled, or carrying no implement stage) → NO `Resolution`. A `Resolution` is a positive claim naming the child runs that actually supplied the evidence, so it is CONSTRUCTED from a non-empty contributor set rather than checked after the fact; the parent's own items stand.
- **Page-ceiling overflow** — the child query is paginated to exhaustion (`childPageSize` 100, `childPageCeiling` 100 pages) because the plan schema declares no `maxItems` on `decomposition.sub_plans`; reaching the ceiling without a short page means the child set was read only in PART, so no resolution is emitted and the parent's items stand. A truncated read can never become a pass.
- **No children** (a flat run) → unchanged, byte-identically.

Every read the rule performs (child query, child stages, child traces) surfaces a **transient error** so the caller retries — never a silent pass.

`Resolution` is surfaced as evidence: the checks endpoint carries it as `resolved[]` on the audit-complete row and the published Check Run's pass summary names the satisfying child runs.
