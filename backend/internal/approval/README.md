# backend/internal/approval

Approval state management behind `POST /v0/stages/{id}/approvals` (with `backend/internal/server/approvals.go`).

## Core semantics

- approve → succeeded; reject → failed-D.
- Idempotent on (stage_id, approver_subject).
- SLA timeout via the `backend/internal/sla` ticker.
- Role-based authorization via `RoleResolver` (E4.4): the subject must be in the gate's `approvers` after expanding `@org/team` refs from the spec's `roles:` map. Falls back to "any authenticated subject" when no resolver is wired.

## Plan-approval budget gate (#994)

Plan-stage approvals are additionally gated on a budget check: when `predicted_runtime_minutes` exceeds the resolved (p95-aware) implement-stage budget, approval is blocked (422 `plan_violates_budget`) unless the plan includes `decomposition.sub_plans` or the comment contains `--override-budget`.

- The resolved budget is `resolvePlanGateBudget`: max(spec-resolved implement timeout, calibration p95 × 1.5) clamped to spec × 2 — the same base the dynamic implement kill cap (`resolveImplementTimeout`) builds on, deliberately excluding the plan's own predicted×2 term so the gate cannot self-satisfy (#994).
- Outcomes emit `plan_violates_budget` or `plan_budget_override_acknowledged` audit entries carrying the resolved `budget_minutes`, `budget_source` (`spec`|`p95`|`ceiling`), and the raw `spec_budget_minutes`.
- The plan-review prompt's gate-evidence Budget check cites the same resolved number and evaluates the decomposition branch with the same predicate as the gate (#1029): an over-budget plan with a non-nil `decomposition` renders "gate satisfied without override" with the sub-plan count and per-slice minutes (flagging any slice that itself exceeds the budget), while the "will be refused" wording appears only on the genuinely refusable over+undecomposed branch.
- Reject submissions containing `--decompose` add `reject_reason=decompose_required` to the `approval_submitted` payload, which the next plan-stage prompt reads to inject a binding decompose instruction.

## Plan-approval completion gate (ADR-036 / #875)

`checkPlanReviewSettled` refuses a plan-stage approve (`409 agent_review_pending`) while a configured agent plan review is in-flight (`reviewers.agent>0` + a `plan_review_started` entry + fewer than the configured count of TERMINAL `plan_reviewed`/`plan_review_failed`/`plan_review_skipped` verdicts landed); placed before `Submit` so a poll-then-retry is idempotent.

A hard backstop (`ReviewBudget.Cap × agentCount` from the earliest `plan_review_started`) allows the approval once elapsed even with no terminal verdict, recording a `plan_review_backstop_elapsed` audit entry.

No-agent-reviewer and not-dispatched stages are unchanged; all reads fail open. See `docs/ARCHITECTURE.md` §4.2.

## Deploy pre-execution gate (ADR-038 / #1384)

A `deploy`-stage approve is gated PRE-`Submit` by `checkDeployPreflight`, which resolves the deploy stage's pre-flight constraints from the run's cached spec and refuses `422` on violation, advancing the stage `awaiting_deploy_approval → dispatched` (NOT `succeeded` — the delegating executor still has to fire) on a pass.

**Deploy-gate authorization (ADR-038 / #1390)**: a deploy-stage `approve` additionally requires the `write:deploy` scope on top of the handler's baseline `write:approvals` — an operator bearer holding `write:approvals` but missing `write:deploy` is refused `403 insufficient_scope` (`required_scope: write:deploy`) before `checkDeployPreflight` runs.

- Cookie sessions (no token scope list) are exempt, and the requirement is approve-only (a deploy `reject` and every plan/implement/review approval are unchanged).
- `write:deploy` is in `operatorDefaultScopes`, so freshly-issued and `token migrate --apply`'d operator tokens carry it.
- Deploy `all_of` approvers are enforced through the same `checkApproverAuthorization → RoleResolver.CanApprove` path every gated stage uses — the approving subject must belong to EVERY named role (else `403 approver_not_authorized`).

Two operator-facing **approval-comment flags** drive the pre-flight:

- **`--environment=<env>`** names the requested target environment checked against the stage's `allowed_environments` (a disallowed/missing env → `422 deploy_environment_not_allowed`).
- **`--override-freeze`** acknowledges an active `change_freeze` (its absence under a declared freeze → `422 deploy_change_freeze_active`).

`required_upstream` (`ci_green`/`review_merged`) unmet → `422 deploy_upstream_not_satisfied`.

Unlike the plan gates, this gate **fails CLOSED**: a can't-evaluate condition (nil repos, run-read failure, absent/unparseable spec, deploy stage not found) refuses `422 deploy_preflight_unevaluable` rather than passing — an unverifiable deploy is denied. Every refusal emits a `deploy_preflight_refused` audit.

A deploy stage that parses but declares no pre-flight constraints passes (nothing to enforce). The live change-freeze / upstream runtime signals and the delegating executor are downstream (E23.5/E23.6/E23.10).
