# backend/internal/auditcomplete

Audit-complete derivation (#229, #282, #947): derives the `fishhawk_audit_complete` blocking-check state on demand via `Compute(ctx, runID, deps)`.

## Sub-topics (full detail in docs/architecture/audit-complete.md)

- [The six rules](../../../docs/architecture/audit-complete.md#the-six-rules)
- [Normalization for rule 4](../../../docs/architecture/audit-complete.md#normalization-rule-4-specifics) (#302/#308)
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
