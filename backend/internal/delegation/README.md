# backend/internal/delegation

Operator-agent delegation conditions (ADR-040 / #1026): evaluates each `operator_agent` knob's named v0 condition against current run state.

## Evaluator

The `Evaluator` answers each condition over narrow interfaces the server already holds (`run.Repository.ListStagesForRun`, `concern.Repository.ListOpenByRun`, `audit.Repository.ListForRunByCategory`):

- `clean_dual_approval` — every configured reviewer verdict for the pending gate's stage is `approve`, counted within the LATEST `*_review_started`-delimited round per the drive settlement rule, AND zero open concerns.
- `convergent_concerns` — implement round settled, no gating-authority reject, ≥1 open concern. Severity/verdict-aware (#1964): when every verdict is approve-class (no reject), an open concern must rank at or above the `route_fixup_min_severity` threshold (default `medium`; set `low` to restore route-on-any-concern) or the gate PARKS for the operator instead of auto-routing a full fix-up pass. A reject verdict BYPASSES the threshold — advisory arbitration (and the gating-reject page) are unchanged. An unrecognized concern severity ranks below `low` and parks (fail-closed).
- `solo_low` — exactly one open concern, severity low.
- `infra_flake` — latest failed stage is category-A with the #972 testcontainers start-flake signature — or the literal `verify_infra_flake_retry` marker — in its `FailureReason`, which embeds the verify output verbatim.
- `gates_resolved_ci_green` — latest `run_auto_advanced` rule is `checks_green_awaiting_merge` + PR open + no pending gate + zero open concerns; evaluated/surfaced only — v0 has no backend merge endpoint to enforce it on.

## workflow-v2 autonomy matrix (ADR-066 / E52.10 / #2222)

A workflow-v2 document declares delegation as an `autonomy` tier plus an `actions` matrix, which `spec.ParseBytes` resolves and projects back onto the same `*spec.OperatorAgent` block this package already reads — so every condition above is evaluated by unchanged code. What `Result` gains is legibility:

- `Result.Tier` — the declared tier, empty when only `actions` was declared, under a campaign override, and for every v0/v1 knob block.
- `Result.Matrix` — the RESOLVED matrix: every class with its mode (`report | gated | auto`), condition, `min_severity` and provenance `Source` (`explicit | tier | default`). `Decision.Mode` / `Decision.Source` carry the same provenance onto each evaluated action.
- `Result.Reports` — the evaluation of `mode: report` classes that declared a `when`. Kept OUT of the decision set because report delegates nothing; a `when` naming ANOTHER class's condition produces no entry at all (fail-closed — this package would otherwise answer a predicate the author did not name).

**The DECISION SET is deliberately unchanged**: `Actions` still carries one entry per class the effective block actually delegates, so an all-`gated` matrix yields ZERO decisions exactly as a knob-absent v0 block does. The matrix changed; what is delegated did not.

Resolution runs on the same ladder as the effective block. A campaign-level override is PROJECTED onto a matrix (set knob → `auto`, unset → `gated`, no tier, every `Source=explicit`) — it is a delegation input, not a grammar version, which is why the wire payload gates the new fields on the run's spec routing to major >= 2 (`runs.go::delegationPayloadFrom`) and a v0/v1 response stays byte-identical, override or not. A run parked at `awaiting_input` resolves NO matrix: it delegates nothing and proposes nothing.

## Resolution and surfacing

- The effective block resolves via `spec.Workflow.EffectiveOperatorAgent` (pending approval gate's block wins wholesale, else workflow level, else nil = fail-closed), and `Configured` short-circuits unconfigured specs before any repository read.
- Surfaced as the `delegation` block on `GET /v0/runs/{run_id}` (`runs.go::buildDelegationPayload` — single-run read ONLY, omitted on terminal runs / legacy spec-less rows / evaluation failure, the Concerns degradation posture).
- Every unmet decision names the exact failed predicate.
- Action-time enforcement (`delegated: true` on approve/fixup/retry/waive) is the #1026 enforcement slice; audit-payload rule attribution rides it.
