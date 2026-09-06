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

## `stage_not_terminal` (E64.59 / #3190): the mid-flight pending carries its cause

`Compute`'s mid-flight guard — "any non-review stage is non-terminal → `pending`" — used to be the ONLY pending return that carried NO missing items. An empty list renders a Check Run whose `output.text` is null, so an operator staring at a permanently `in_progress` `fishhawk_audit_complete` had nothing to read and no action to take. #3190 is the run where that cost an hour and the documented manual heal also failed.

It now emits one pending-flavored `stage_not_terminal` item per non-terminal non-review stage. Two detail shapes:

- **Generic** — names the stage TYPE, its short id and its current STATE (`"acceptance stage 88538e1a is in state pending; the run is not terminal, so the audit chain cannot be assembled yet"`).
- **Action-bearing** — when the non-terminal stage is an ACCEPTANCE stage carrying a stage-scoped `acceptance_reopened` entry (a fix-up push invalidated its verdict, #1682), the detail instead names the re-dispatch that clears the block and states that the check clears on its own once acceptance settles. That is the exact strand #3190 reports: `reopenAcceptanceOnFixupPush` flips a settled acceptance stage back to `pending`, so every later recompute takes this branch forever.

Invariants:

- **The STATE is unchanged.** The mid-flight branch returns `StatePending` DIRECTLY, so a mid-flight run is `pending` before and after — never `fail`. `TestCompute_MidFlightPendingIsNotFail` pins that, and the discriminating mutation is flipping that return to `StateFail`. `stage_not_terminal` is ALSO registered in `onlyPendingFlavored`'s pending-flavored list, but read that honestly: because the branch returns directly, that switch is not consulted on this path, so the registration is a defensive CLASSIFICATION with no reachable discriminating mutation today — correct if a future refactor ever routes this kind through the fail→pending demotion, and measured GREEN under its own deletion rather than assumed load-bearing.
- **The early-return placement is unchanged**, so no rule below the guard starts running on a mid-flight run.
- **The `acceptance_reopened` lookup FAILS OPEN.** A read error degrades to the generic detail and is NEVER returned as a `Compute` error — this lookup only sharpens wording, and turning a legible pending into a 500 would be the opposite of what #3190 is for. Deliberately the opposite posture from the skip-marker and outcome-entry reads above, which decide whether a stage is in scope at all and so must surface transiently.
- The category string `"acceptance_reopened"` is a LITERAL here, duplicating `server.CategoryAcceptanceReopened`: `auditcomplete` cannot import package `server` (the server depends on it), the same reason `"acceptance_outcome_recorded"` is a literal in the same function. The duplication is unguarded by the compiler; `server.TestFixupStrandedMerge_DerivedDetailReachesForgeAndOperator` drives the entry through the server's OWN reopen path, so a rename on either side reddens it.

Why the check does not clear itself: the acceptance stage genuinely re-opened, so the pending is CORRECT. This change makes the block legible and self-clearing — once acceptance is re-dispatched and settles, the run recomputes to pass through the product's own path. Auto-re-dispatching a re-opened acceptance stage would decide on the operator's behalf that a re-opened validation need not re-run, which changes #1682's semantics; that is tracked separately, not decided here.

## Rule 2b (#3092): decomposition trace resolution is NEVER an exemption

A decomposed run's parent implement stage is the fan-out stage — it parks `awaiting_children`, spawns no agent, and by construction can never carry a trace — so rule 2 was unsatisfiable for every decomposed run and the required check could never go green.

`ComputeResult` now RESOLVES that evidence through the fan-out instead of exempting the stage: when the parent implement stage has a trace gap and the run has decomposition children (`run.ListRunsFilter.DecomposedFrom`), it reads each child run's own implement-stage `trace_uploaded` entries and satisfies the parent's requirement only when every executed child is trace-complete. A child genuinely missing a trace still FAILS, naming the child run id and the child stage id. Only the implement stage is eligible — the misses are partitioned by owning stage id through a parallel slice returned by `missingTraces` (never a `Detail` string match, and never a new field on the wire-serialized `MissingItem`), so a plan/acceptance trace gap passes through verbatim.

The five fail-closed branches:

- **Non-terminal child implement stage** (`pending`, `running`, `awaiting_*`) → the parent's opaque `trace_missing` items are replaced by a single pending-flavored `children_pending` item. State `pending`: the requirement stays UNSATISFIED and the merge stays blocked exactly as `review_pending` blocks it today — it is not-yet, never a pass.
- **Zero contributors** (every child cancelled, or carrying no implement stage) → NO `Resolution`. A `Resolution` is a positive claim naming the child runs that actually supplied the evidence, so it is CONSTRUCTED from a non-empty contributor set rather than checked after the fact; the parent's own items stand.
- **Child audit chain does not verify** → the child contributes NOTHING, however trace-complete it looks, and the parent's items are replaced by a `chain_invalid` (hash mismatch) / `chain_unrecoverable` (read or re-hash failure) item naming that child. State `fail`; neither kind is pending-flavored. Rule 4 verifies the PARENT chain only and a child may never be chain-verified anywhere else, so without this a tampered child's entries could carry a decomposed parent's required merge check green — an audit-integrity hole unique to decomposed runs. The verification runs BEFORE the child's `trace_uploaded` read, because those rows come out of the same chain.
- **Page-ceiling overflow** — the child query is paginated to exhaustion (`childPageSize` 100, `childPageCeiling` 100 pages) because the plan schema declares no `maxItems` on `decomposition.sub_plans`; reaching the ceiling without a short page means the child set was read only in PART, so no resolution is emitted and the parent's items stand. A truncated read can never become a pass.
- **No children** (a flat run) → unchanged, byte-identically.

Every read that decides WHICH evidence to look at (child query, child stages, child traces) surfaces a **transient error** so the caller retries — never a silent pass. The child-chain read is the deliberate exception, mirroring rule 4 on the parent's own chain: a failed read or re-hash becomes a `chain_unrecoverable` missing item, so an unreadable child chain gates the merge rather than looking like a retryable blip.

Child PROVENANCE is not separately checked: a run row carrying `decomposed_from == parent` is taken as a legitimate child. `decomposed_from` is not caller-settable (`server.createRunRequest` has no such field), and the only writers are the fan-out paths, which already hold the parent run — so forging one requires the direct database access that would equally let you forge the parent's own evidence.

`Resolution` is surfaced as evidence: the checks endpoint carries it as `resolved[]` on the audit-complete row and the published Check Run's pass summary names the satisfying child runs.
