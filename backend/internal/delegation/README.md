# backend/internal/delegation

Operator-agent delegation conditions (ADR-040 / #1026): evaluates each `operator_agent` knob's named v0 condition against current run state.

## Evaluator

The `Evaluator` answers each condition over narrow interfaces the server already holds (`run.Repository.ListStagesForRun`, `concern.Repository.ListOpenByRun`, `audit.Repository.ListForRunByCategory`):

- `clean_dual_approval` — every configured reviewer verdict for the pending gate's stage is `approve`, counted within the LATEST `*_review_started`-delimited round per the drive settlement rule, AND zero open concerns.
- `convergent_concerns` — implement round settled, no gating-authority reject, ≥1 open concern. Severity/verdict-aware (#1964): when every verdict is approve-class (no reject), an open concern must rank at or above the `route_fixup_min_severity` threshold (default `medium`; set `low` to restore route-on-any-concern) or the gate PARKS for the operator instead of auto-routing a full fix-up pass. A reject verdict BYPASSES the threshold — advisory arbitration (and the gating-reject page) are unchanged. An unrecognized concern severity ranks below `low` and parks (fail-closed).
- `solo_low` — exactly one open concern, severity low.
- `infra_flake` — latest failed stage is category-A with an infrastructure signature in its `FailureReason`, which embeds the verify output verbatim: the literal `verify_infra_flake_retry` marker, the #972 testcontainers start-flake signature, or (widened alongside the runner's matcher by #2718) testcontainers-go's `port "9000/tcp" not found` rendering (matched on `port` as a whole WORD — a leading `\b`, so a suffix like `airport "9000/tcp" not found` in untrusted verify output cannot turn a category-B failure retryable) and the Docker daemon-unreachable markers. The set MIRRORS `isVerifyInfraFailure` in `runner/cmd/fishhawk-runner/main.go`; the two live in different Go modules and cannot share a fixture, so each is pinned against the same corpus of verbatim observed outputs — including the accepted `backend/internal/pgtest` fixture-dump residual, pinned expecting-MET on both sides.
- `gates_resolved_ci_green` — latest `run_auto_advanced` rule is `checks_green_awaiting_merge` + PR open + no pending gate + zero open concerns; evaluated/surfaced only — v0 has no backend merge endpoint to enforce it on.

## workflow-v2 autonomy matrix (ADR-066 / E52.10 / #2222)

A workflow-v2 document declares delegation as an `autonomy` tier plus an `actions` matrix, which `spec.ParseBytes` resolves and projects back onto the same `*spec.OperatorAgent` block this package already reads — so every condition above is evaluated by unchanged code. What `Result` gains is legibility:

- `Result.Tier` — the declared tier, empty when only `actions` was declared, under a campaign override, and for every v0/v1 knob block.
- `Result.Matrix` — the RESOLVED matrix: every class with its mode (`report | gated | auto`), condition, `min_severity` and provenance `Source` (`explicit | tier | default`). `Decision.Mode` / `Decision.Source` carry the same provenance onto each evaluated action.
- `Result.Reports` — the evaluation of `mode: report` classes that declared a `when`. Kept OUT of the decision set because report delegates nothing; a `when` naming ANOTHER class's condition produces no entry at all (fail-closed — this package would otherwise answer a predicate the author did not name). Parse-time validation now REJECTS a foreign-`when` report entry outright (`backend/internal/spec` `validateAutonomy`), so this evaluator branch is defense-in-depth: a validated v2 spec never reaches it.

**The DECISION SET is deliberately unchanged**: `Actions` still carries one entry per class the effective block actually delegates, so an all-`gated` matrix yields ZERO decisions exactly as a knob-absent v0 block does. The matrix changed; what is delegated did not.

Resolution runs on the same ladder as the effective block. A campaign-level override is PROJECTED onto a matrix (set knob → `auto`, unset → `gated`, no tier, every `Source=explicit`) — it is a delegation input, not a grammar version, which is why the wire payload gates the new fields on the run's spec routing to major >= 2 (`runs.go::delegationPayloadFrom`) and a v0/v1 response stays byte-identical, override or not. A run parked at `awaiting_input` resolves NO matrix: it delegates nothing and proposes nothing.

## Resolution and surfacing

- The effective block resolves via `spec.Workflow.EffectiveOperatorAgent` (pending approval gate's block wins wholesale, else workflow level, else nil = fail-closed), and `Configured` short-circuits unconfigured specs before any repository read.
- Surfaced as the `delegation` block on `GET /v0/runs/{run_id}` (`runs.go::buildDelegationPayload` — single-run read ONLY, omitted on terminal runs / legacy spec-less rows / evaluation failure, the Concerns degradation posture).
- Every unmet decision names the exact failed predicate.
- Action-time enforcement (`delegated: true` on approve/fixup/retry/waive) is the #1026 enforcement slice; audit-payload rule attribution rides it.

### `may_approve` decline on a human-quorum gate (#2381)

A met `may_approve` (`clean_dual_approval`) is necessary but NOT sufficient for the auto-driver to submit an approve: the gate's real quorum state independently decides whether a delegated vote could ever advance it. `server/autodrive.go`'s `may_approve` arm calls `delegatedApproveWouldAdvance` (`server/quorum.go`) FIRST and DECLINES — `decision_required` / `decision_state: human_quorum_required`, no approval submitted — on a gate declaring an `approvals` block (a delegated submission is unconditionally uncounted, so it can never reach the human count), on an unreadable block, and on a firing or unevaluable escalation over a block-less gate (fail-closed, the #2374 posture). A delegated approve returning a DUPLICATE reports `decision_state: delegated_approval_no_progress`. Only a genuine legacy no-approvals, no-escalation gate still auto-approves. This keeps a delegated vote — which #1709 records but never counts — from wedging a human-quorum gate the auto-driver can never clear on its own.

## Escalation autonomy ceiling (E53.4 / #2227)

A workflow's `escalations` block may declare a `max_autonomy` CEILING for a change matching its predicate. This package applies it **LAST** — after `spec.ResolveOperatorAgent` and `resolveMatrix` have produced the fully resolved block, i.e. after the workflow tier AND after every explicit `actions` override AND after a campaign override has been projected into the matrix. Clamping the tier or the declared entries instead would let an explicit `actions: {merge: {mode: auto}}` re-widen the class afterwards.

Final resolution order: **workflow tier → explicit `actions` overrides → campaign override → escalation ceiling.**

`Evaluate` then RE-DERIVES the knob block (`spec.DerivedOperatorAgent`) from the clamped matrix. Every enforcement site reads the derived `*OperatorAgent`, not the surfaced `ResolvedMatrix`, so clamping only the matrix would show `gated` on the run read while leaving the agent authorized to act.

### The resolver is REQUIRED at construction

`Evaluator`'s four dependencies are **unexported** and `NewEvaluator(stages, concerns, audit, escalations)` is the only constructor. Go forbids a composite literal from setting a non-exported field of a struct in another package, so `&delegation.Evaluator{…}` is a **compile error** everywhere outside this package. An Evaluator that cannot clamp is therefore *unconstructible* rather than merely discouraged — the enforcement site someone adds later cannot compile without supplying a resolver.

There is deliberately **no** optional / nil-means-inert resolver and **no** exported no-op resolver: the no-escalations case is answered INSIDE the server resolver, which returns the zero `spec.ComposedRequirements` when the workflow declares none, so "inert" is reached by a code path that exists. Honestly stated: same-package construction is still possible, which is why this package's own tests go through `NewEvaluator` too; the guarantee binds every OTHER package, and all three enforcement sites live in `backend/internal/server`.

Production construction sites, all converted:

| Site | Fail-closed degradation on a constructor / resolver error |
|---|---|
| `server/runs.go::buildDelegationPayload` | warn-log, omit the delegation block |
| `server/approvals.go::checkDelegation` | 500, the delegated action is refused |
| `server/autodrive.go::evaluateRunDelegation` | observe-only — the auto-drive gate acts with no operator in the loop, so this is the site an unclamped evaluator would have hurt most |

A resolver ERROR returns an error from `Evaluate`; each caller's existing degradation delegates nothing, so that mode is already fail-closed.

## Shadow evaluation (ADR-085 rule 4 / E82.1 / #3778)

`Evaluator.Shadow(ctx, runRow, wf, action)` answers what delegation WOULD have done for one action class on the run's current state. It is the input the server's blind delegation shadow stamp records at a human's action; this package only answers, the server capture and append live in `backend/internal/server`.

- **Evaluated REGARDLESS of mode.** A `gated` (or `report`) class is evaluated exactly as if it were delegated. That is the counterfactual. `Shadow.Mode` / `Shadow.Source` record the class's actual resolved mode next to the answer.
- **Same resolution as `checkDelegation`.** The campaign override is nil, as `checkDelegation` passes it. `checkDelegation` takes only `(run, action)` and no request-specific input such as selected concern ids or a target stage, so `Shadow` takes the same inputs and answers what the action-time check would. The escalation ceiling is applied LAST through the same resolver and `spec.ClampResolvedMatrix`, and the knob block is re-derived from the clamped matrix (so the fixup threshold still reads `min_severity`). A resolver error is RETURNED.
- **One condition evaluator.** `Evaluate` and `Shadow` both answer a class through `evalClassCondition`. On the same state, `Evaluate` with the class `auto` and `Shadow` with the class `gated` agree on condition, met and the verbatim unmet reason (`TestShadow_ParityWithEvaluate`, all five classes, met + unmet each).
- **`not_delegable` without evaluating:** an action with no delegable class (answered before any repository read), and a run parked at `awaiting_input` (where `Evaluate` delegates nothing). Deferral is one such action, but it is INTENTIONALLY UNSTAMPED in this change. #3778's proposal item 3 stamps non-delegable classes `not_delegable`; the operator accepted narrowing that for deferral, which is not a delegable action class, so the server hooks no deferral path at all. `Shadow("defer")` still answers `not_delegable` should a caller ask.
- **Provenance with no matrix.** A v0/v1 knob block is projected the way a campaign override is (set knob → `auto`, unset → `gated`, both `explicit`), so a human acting on a class the knob block delegates is not stamped as gated. With no block at all, the class reads `gated` / `default`. `MatrixResolved`, `Tier` and `Matrix` are set only when a workflow-v2 matrix governs the run.
- **Never folded into `Result`.** `Shadow` writes nothing and returns its own value; `Evaluate` never calls it. An all-gated matrix still yields zero `Actions` and zero `Reports` (`TestShadow_GatedClassNotInResult`).

**Verdict vocabulary.** `ShadowVerdict` is ONE closed set, declared together here: `met`, `unmet`, `not_delegable`, `unevaluable` (`ShadowVerdicts()`, `Valid()`). `Shadow` returns only the first three. `unevaluable` is produced by the server capture when it cannot reach an answer (spec unreadable, repository failure, timeout), because `Shadow` returns an error rather than a verdict in those cases.

**`Shadow` alone does NOT apply the `page_human_on` override.** Whether an active page event (a requirement-category concern, an unruled crew escalation, …) would have paged the human needs server-side state this package does not read. The server capture applies that override over `Shadow.MustPageHuman` and stamps `not_delegable` with the page event. A caller that uses `Shadow` directly and skips that step would record a met verdict for a run delegation would actually have paged on.

The resolved-matrix hash a stamp carries is `delegationview.HashMatrix(Shadow.Matrix)`, the E76.1 wire mirror's digest (see `backend/internal/delegationview/README.md`).
