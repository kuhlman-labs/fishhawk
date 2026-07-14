# backend/internal/delegation

Operator-agent delegation conditions (ADR-040 / #1026): evaluates each `operator_agent` knob's named v0 condition against current run state.

## Evaluator

The `Evaluator` answers each condition over narrow interfaces the server already holds (`run.Repository.ListStagesForRun`, `concern.Repository.ListOpenByRun`, `audit.Repository.ListForRunByCategory`):

- `clean_dual_approval` — every configured reviewer verdict for the pending gate's stage is `approve`, counted within the LATEST `*_review_started`-delimited round per the drive settlement rule, AND zero open concerns.
- `convergent_concerns` — implement round settled, no reject, ≥1 open concern.
- `solo_low` — exactly one open concern, severity low.
- `infra_flake` — latest failed stage is category-A with the #972 testcontainers start-flake signature — or the literal `verify_infra_flake_retry` marker — in its `FailureReason`, which embeds the verify output verbatim.
- `gates_resolved_ci_green` — latest `run_auto_advanced` rule is `checks_green_awaiting_merge` + PR open + no pending gate + zero open concerns; evaluated/surfaced only — v0 has no backend merge endpoint to enforce it on.

## Resolution and surfacing

- The effective block resolves via `spec.Workflow.EffectiveOperatorAgent` (pending approval gate's block wins wholesale, else workflow level, else nil = fail-closed), and `Configured` short-circuits unconfigured specs before any repository read.
- Surfaced as the `delegation` block on `GET /v0/runs/{run_id}` (`runs.go::buildDelegationPayload` — single-run read ONLY, omitted on terminal runs / legacy spec-less rows / evaluation failure, the Concerns degradation posture).
- Every unmet decision names the exact failed predicate.
- Action-time enforcement (`delegated: true` on approve/fixup/retry/waive) is the #1026 enforcement slice; audit-payload rule attribution rides it.
