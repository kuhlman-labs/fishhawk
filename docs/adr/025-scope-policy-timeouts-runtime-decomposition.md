---
id: ADR-025
title: "Scope-policy: workflow-spec timeouts, predicted runtime, and plan-driven decomposition"
status: accepted
date: 2026-05-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/451
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-025: Scope-policy: workflow-spec timeouts, predicted runtime, and plan-driven decomposition

## Decision (2026-05-21)

Approved as drafted. Summary:

- **D1 (timeout config):** Move to spec. Precedence `stage.executor.timeout > workflow.policy.max_stage_runtime > backend default (15m)`. Three levels preserve per-stage signal (plan vs implement runtime profiles differ).
- **D2 (plan schema):** Add `predicted_runtime_minutes` + `predicted_runtime_confidence` + optional `decomposition.sub_plans`. Skip token estimates (too noisy).
- **D3 (decomposition triggers):** Precedence order — agent-proposed (free), reviewer-rejected (planning round), backend-detected (approval-receipt backstop).
- **D4 (branch model):** Single epic PR with child commits to shared branch. Per-child PRs deferred.

This supersedes #449 (tactical timeout configurability) — the spec-field framing is the right level.

Impl tracked at follow-up issues (to be filed).

---

## Context

Two consecutive local-loop runs surfaced a structural problem with how Fishhawk handles agent execution budgets:

| Run | Issue | Implement outcome |
|---|---|---|
| `03bbc610…` | #446 (~45 lines) | Edit complete + correct, but agent timed out at 15m on the `-count 100 -race` verification |
| `a6007305…` | #422 (~520 lines est, 14 files) | 15-min timeout, **325KB bundle of events, tree empty** — agent spent the budget exploring without landing visible work |

The agent timeout (`runner/cmd/fishhawk-runner/flags.go:78`, `--timeout` default 15m) is **a runner-binary flag today, not a spec field**. Both timeouts produced the same category-A failure. Same exit code, very different signals: the first ran out of time on verification (mostly OK, work done); the second was a real scope mismatch (no edits, blown budget).

[#449] proposed making the timeout configurable. The retro on #422 surfaced a deeper question: a scope-vs-budget mismatch should be a **first-class workflow policy concern**, not a runtime knob. Operators want timeout to mean "your scope must fit this" — a deliberate signal that the work was too large for one agent run.

This ADR settles four decisions that turn the runtime knob into a workflow policy + the orchestration that handles over-budget cases gracefully.

## Decisions

### D1: Timeout config moves into the workflow spec, with three-level precedence

Resolution order:
1. **`stage.executor.timeout`** — explicit per-stage override.
2. **`workflow.policy.max_stage_runtime`** — workflow-level default for any stage without an override.
3. **Backend default (`15m`)** — applies when neither is set.

Concrete spec shape:

```yaml
version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: 30m   # workflow-level default; new field
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          timeout: 10m         # override — plan should be tight
      - id: implement
        type: implement
        executor:
          agent: claude-code
          # inherits 30m
```

**Why three levels** (not just workflow-level): plan stages and implement stages have structurally different runtime profiles. A 30m plan is a red flag; a 30m implement is normal. A purely workflow-level number forces operators to pick the loosest budget across stages and loses the per-stage signal. With overrides, the workflow level sets the spirit ("this workflow has bigger tasks") and the stage override captures the exception.

**Backend fallback** ensures new workflows need zero config to get sane behavior. The spec schema treats both fields as optional additions to v0.3.

### D2: Plan schema gains `predicted_runtime_minutes` and `decomposition`

Two additive fields on `standard_v1`:

```json
{
  "predicted_runtime_minutes": { "type": "integer", "minimum": 1 },
  "predicted_runtime_confidence": { "enum": ["low", "medium", "high"] },
  "decomposition": {
    "type": "object",
    "properties": {
      "rationale": { "type": "string" },
      "sub_plans": {
        "type": "array",
        "items": { "$ref": "#/definitions/SubPlanSummary" }
      }
    }
  }
}
```

The plan agent **always** estimates runtime + confidence. **Optionally** proposes a decomposition when the estimate exceeds the stage's timeout (per D1). Token estimates are not added — token-usage prediction is too noisy across model versions to be load-bearing, and runtime is the metric operators actually care about.

The backend correlates `predicted_runtime_minutes` against `actual_runtime_minutes` over time so calibration drift is observable; out-of-scope for v0 enforcement.

### D3: Decomposition triggers fire in precedence order

Three paths from "plan exceeds budget" to "scope gets split," in order of how-they-fire:

1. **Agent-proposed** (best case): the plan agent's runtime estimate exceeds the stage's timeout, so the plan it produces includes `decomposition.sub_plans`. The reviewer approves the decomposed plan; the orchestrator fans out child runs (per D4). No re-planning round-trip needed.

2. **Reviewer-rejected** (mid case): the agent produces a single-path plan, but the reviewer notices `predicted_runtime_minutes > timeout` (surfaced in the approval UI) and rejects with "decompose this." The agent re-plans with the explicit instruction; the second plan should include `decomposition`.

3. **Backend-detected** (last resort): the agent submits a single-path plan that violates budget AND the reviewer approves it anyway. The orchestrator at approval-receipt time checks `plan.predicted_runtime_minutes > stage.executor.timeout`; if so, the implement stage doesn't fire — instead a structured `plan_violates_budget` audit entry surfaces back to the reviewer who must either override explicitly (logged) or re-plan.

The precedence matters because each layer is a different cost: (1) is free, (2) costs a planning round, (3) costs a planning round AND embarrasses the reviewer mid-flow. We want the agent to self-decompose first and last-mile checking only as a backstop.

### D4: Single epic PR, child commits to a shared branch

When a plan with `decomposition` is approved:
- The orchestrator creates a parent run's branch (`fishhawk/run-<short>` — same naming as today's single-run flow).
- Each sub-plan mints a **child run** with `parent_run_id = parent.id`, an inherited workflow_id, and the sub-plan's scope inlined as the issue_context.
- Each child's implement stage commits to the **parent's branch** (not a fresh per-child branch).
- The parent's implement stage transitions to `succeeded` only when every child's implement terminates green.
- The parent's review stage (or auto-PR via #422's mechanism) fires last, producing **one PR** with N child commits.

**Why single PR** (not per-child PRs): the operator analog is "epic branch + child PRs" because every operator is a human reviewer who needs incremental checkpoints. Agent children are deterministic and per-run audited in the run rows + audit chain, so child PRs add review fatigue without proportionate value. The exception is `autonomy: low` workflows where a human IS expected to review per-child; those can opt in to per-child PRs as a follow-up if real demand surfaces.

The single-PR model matches what was done manually on #422 today: four scoped sub-task runs accumulated commits on `422-auto-open-pr`, one PR (#450) closes the umbrella.

## Consequences

- **Spec schema bump to v0.3** for the new `policy.max_stage_runtime` + `executor.timeout` fields and the plan-schema additions. Both fields are optional/additive — existing v0.2 specs parse cleanly.
- **Plan-stage prompt** extends to require runtime estimate + optional decomposition. Updates to `backend/internal/prompt/buildPlan` and the plan-schema doc.
- **Approval-stage UI** surfaces predicted-vs-budget at approve-time so reviewers see the signal. SPA work; out-of-scope for the first impl pass (text in the audit log is sufficient as a v0.5).
- **Orchestrator gains a fanout path:** when an approved plan has `decomposition`, mint child runs instead of dispatching the parent's implement stage. New audit category: `plan_decomposed`.
- **Audit threading:** today's `parent_run_id` is used for CI-retry chains (#216). Decomposition runs use the same field but a different semantic — needs a `relation` qualifier on `parent_run_id` or a separate `decomposed_from` field to distinguish at query time.
- **Pre-flight calibration data:** predicted-vs-actual runtime correlation accumulates immediately; provides empirical input for tuning workflow-level defaults over time.
- **Operator workflow:** "scope is too large" becomes a structured signal at plan-approval time, not a runtime surprise. Operators can author tighter `max_stage_runtime` policies as a quality lever ("our plans must fit in 15 min or decompose").

## Out of scope

- Per-child PRs (deferred; revisit if `autonomy: low` workflows ask).
- Auto-tuning workflow-level timeouts based on observed runtime data (calibration is observability v0.5; enforcement automation is out).
- A "merge ordering" constraint across child runs (today's single-branch commit accumulation works; sub-plans that depend on each other are surfaced in the decomposition rationale and handled by ordering).
- Token-usage estimation (per D2 rationale — too noisy).
- Re-planning UI in the SPA (out of scope; CLI/MCP rejection + agent re-plan covers v0).

## Related

- #422 / PR #450 — the manual sub-task split that demonstrated the single-PR pattern works.
- #449 — tactical timeout-configurability proposal; this ADR supersedes it with the spec-field framing.
- #447 — plan citation-or-test rule; complementary (the runtime estimate is itself a non-obvious claim that benefits from the citation-or-test discipline).
- #441 — in-band test gate; the test gate is the kind of stage-runtime contributor the budget needs to account for.
- ADR-007 — audit chain; the new `plan_decomposed` audit category extends but doesn't alter the chain shape.
- ADR-024 — MCP runner-subprocess tool; orthogonal but the decomposition orchestration plays nicely with the MCP-driven local loop.
- [[feedback-dogfood-local-loop]] — the methodology that surfaced this.
