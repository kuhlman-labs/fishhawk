---
id: ADR-070
title: "Cost forecasting as a planning input: interval forecasts at the plan gate, an in-flight circuit-breaker, and budget-aware grooming"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2276
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-070: Cost forecasting as a planning input: interval forecasts at the plan gate, an in-flight circuit-breaker, and budget-aware grooming

## Context

Fishhawk records cost thoroughly and forecasts it nowhere. `runs.cost_usd_total` (migration 0028) rolls per-run spend control-plane-side from the signed bundle manifest; `cost_recorded` audit entries carry model, token split, USD and a per-stage `source` bucket; `cost.AggregateRunCost` / `AggregateCacheEfficiency` and `issuecomment/economics.go` render it after the fact. Budgets exist (ADR-030 / #688): periodic per-workflow ceilings, `SumWorkflowCostInRange`, and `CheckBlockingBudget` refusing a new run at admission.

Every one of those surfaces reports spend that has **already happened**. No decision in the product is informed by what a change is *going to* cost.

**The template already exists — for time.** The runtime loop is complete: `predicted_runtime_minutes` + `predicted_runtime_confidence` are REQUIRED plan-artifact fields; `prompt.CalibrationHint` feeds aggregate actuals into the plan prompt so the agent self-corrects; `DecomposeRequired` forces `decomposition.sub_plans` when the prediction exceeds the implement-stage budget. Cost has the ledger and the admission gate but no prediction and no feedback — the two halves the runtime loop has.

### What the data says (queried 2026-07-26: 894 costed runs across 575 issues, $7,553.68 total)

Per-issue: **median $6.76, IQR $3.43–$14.69, max $104.04**. All 894 are `feature_change`; no other workflow has cost history.

**Filing-time attributes do not predict cost.** Grouping by `type:` / `area:` / `autonomy:` produces between-bucket differences smaller than within-bucket spread (`type:feature` median $8.79 vs `type:bug` $5.40, while each has an IQR spanning ~$15). A per-issue estimate from labels would be the population distribution wearing a number — it would look like data and carry none.

**`scope.files` does predict, once bucketed.** Linear correlation is only r=0.30 because the relationship saturates, but the buckets separate monotonically:

| plan `scope.files` | n | median | IQR |
|---|---|---|---|
| 1–3 | 142 | $2.98 | $1.86–$4.55 |
| 4–7 | 227 | $6.34 | $3.68–$11.50 |
| 8–14 | 195 | $8.53 | $5.00–$13.46 |
| 15+ | 174 | $9.92 | $3.17–$18.47 |

A 3.3x median spread, on a value known at exactly the moment a planning agent needs it. (`predicted_runtime_minutes` alone is weaker, r=0.24.)

**Rework dominates the variance, and is unknowable at plan time.** Stage count is a fix-up proxy: **≤3 stages → median $4.21; 4–5 stages → median $12.18** (2.9x, max $70.91). This is why residual IQR stays ~3x *within* every `scope.files` bucket, and it is the single most important fact for the design: no plan-time model can predict it, because it has not happened yet.

**Waste is measurable.** $566 of $7,553 (~7.5%) went to runs that never merged (failed $359.70, cancelled $205.84).

**The shipped budget calibration is badly stale.** `.fishhawk/workflows.yaml` justifies its weekly $1,000 advisory ceiling as "calibrated from recorded feature_change run costs (avg ~$3.4, max ~$8.4 over the first 29 costed runs)." Actual across 894 runs is **avg $8.45, max $70.91** — 2.5x and 8.4x off.

## Options

1. **Status quo.** Cost stays a post-hoc report. Rejected: the two moments spend is actually decided — the plan gate and the fix-up loop — both happen blind, and grooming/campaign work now being designed (E54, E25) has no economic input available to it.

2. **Point estimates at plan time.** Attach a single predicted dollar figure to a plan. Rejected: with residual IQR ~3x inside every predictor bucket, a point estimate asserts precision the data does not support, and would anchor operator and agent judgment on a number that is wrong most of the time.

3. **Two-phase interval forecasting, rolled out in tiers.** A bucketed *interval* at plan time from `scope.files` plus the planner's own judgment, revised **in flight** as stages accumulate — because the dominant variance driver only becomes observable once rework starts. Delivered in tiers so trust is earned before the forecast feeds automated judgment.

4. **A full statistical cost model** (regression / learned estimator over plan features). Rejected for now: the dominant driver is unobservable at plan time, so a sophisticated model over plan-time inputs cannot beat the bucket by much — it would add opacity and maintenance for marginal accuracy. Revisit if in-flight signals prove rich enough to warrant it.

5. **Estimate at filing time, on the issue.** Rejected on the evidence above: no filing-time attribute predicts cost. Actuals stamped on closed issues are worth having; estimates on open ones are not.

## Recommendation

**Option 3**, mirroring the runtime loop's proven shape: a required prediction field, a calibration hint feeding actuals back to the planner, and a budget-derived trigger. Delivered in four tiers, sequenced so the forecast is *observed* before it is *trusted*.

### Tier 1 — put a number where a human already decides

- **Plan-gate forecast.** Surface the interval and the rework base-rate for the plan's `scope.files` bucket ("8–14 files → typically $5–$13, median $8.53; ~29% of runs this shape needed fix-up"), alongside remaining periodic budget (`SumWorkflowCostInRange` already computes it). An operator approving a plan is approving spend; today that is invisible.
- **In-flight circuit-breaker.** Pause for operator confirmation when a run crosses N× its forecast. Today per-stage budgets are `enforcement: advisory` and periodic budgets are checked only at admission — **nothing stops a run spending 5x its forecast mid-flight**, and the data locates exactly where that happens.
- **Recalibrate the shipped budget** against the 894-run reality.

### Tier 2 — feed the planning agents (E54 / E25)

- **Cost as a grooming rubric dimension** (E54 #2232), never the sort key — one line among value / risk / unblocking / staleness, with #2235's verbatim rubric citation keeping a cost-informed ranking auditable.
- **Budget-enveloped proposals**: the groomer proposes *"the next $200 of work, ordered"* rather than an unbounded ranking — a qualitatively different artifact that answers what-next and what-cost in one gated decision.
- **Budget-bounded campaigns**: a campaign carries a spend ceiling and stops dispatching when projected spend exhausts it, turning the periodic budget from an alert into a planning constraint.
- **Forecast-triggered decomposition**, copying `DecomposeRequired`.

### Tier 3 — aim levers we already have

- **Model selection** informed by scope shape (`executor.model` #1013, `model_policy` #1421; `cost_recorded` already carries the model).
- **Make `max_files_changed` explicit as a cost governor.** E50 enforces it as a planning constraint and `scope.files` is our best cost predictor — the 45-file cap is already a de-facto spend cap, currently by coincidence rather than design.
- **Cache-investment targeting** from `AggregateCacheEfficiency` (the #1725 prefix ordering was already a cost optimization).

### Tier 4 — commercial

- **Margin input for pricing (ADR-011 #75).** That ADR is parked pending "market signal that does not exist yet" — but the *cost* half of a pricing decision is our own data, and we have 894 runs of it. Per-account cost is already tracked.
- **Epic-level forecasts** (E57 #2255 beta scope), where averaging makes the mean meaningful even though individual estimates are not.

### Design constraints

**Forecasts are intervals with a stated base rate, never point estimates.** Every surface must show the spread.

**Aggregate forecasts are sound; individual ones are weak.** An epic of 10 children at median $6.76 is a usable number; one issue is not. Surfaces must not imply otherwise.

## Decision

**Accepted (2026-07-26).** Adopt **Option 3** — two-phase interval forecasting, delivered in tiers. Three forks were settled; where they refine the Recommendation, **these govern**.

### 1. Scope: Tier 1 built, Tier 2 declared, Tiers 3–4 directions

**In the epic:** the plan-gate forecast, the in-flight circuit-breaker, a cost `CalibrationHint` mirroring the runtime one, and a `predicted_cost_usd` plan field.

**Declared, not scoped:** Tier 2 lands with E54 (#2232), which is deferred post-alpha. It is already cross-referenced there so grooming is *designed with* a cost dimension rather than retrofitted — the charter rubric, budget-enveloped proposals, and budget-bounded campaigns.

**Directions only:** Tier 3 (model selection, `max_files_changed` as an explicit cost governor, cache targeting) and Tier 4 (pricing margin) are recorded but unscoped. Tier 3 touches three live surfaces with their own owners (#1421, E50 #2052, #1725); Tier 4 depends on ADR-011 (#75), which is parked.

### 2. The plan-gate forecast is ADVISORY — it informs, never blocks

**The principle: enforce on actuals, inform on forecasts.** Hard enforcement stays exactly where it is — `CheckBlockingBudget` at admission, which evaluates real period spend. The plan gate displays the interval, the rework base rate, and remaining budget, and the operator decides.

Gating on a forecast was rejected because it re-introduces the point estimate this ADR rejects in option 2: residual IQR is ~3x *inside* every predictor bucket, so blocking on a midpoint blocks work that would have come in under budget.

### 3. The circuit-breaker parks at a stage boundary, keyed to the bucket's p90

**Trigger:** cumulative run spend exceeds the p90 for the plan's `scope.files` bucket.
**When:** at the next stage boundary — never mid-agent.
**Action:** park for operator confirmation instead of auto-advancing.

Measured thresholds (894 costed runs, 2026-07-26):

| `scope.files` | n | median | **p90 (trigger)** | max |
|---|---|---|---|---|
| 1–3 | 142 | $2.98 | **$6.75** | $23.38 |
| 4–7 | 227 | $6.34 | **$17.32** | $46.35 |
| 8–14 | 195 | $8.53 | **$20.57** | $39.23 |
| 15+ | 174 | $9.92 | **$30.95** | $70.91 |

A fixed 3x multiple was rejected on the data: **entering a fix-up round is itself ~3x** (≤3 stages median $4.21 → 4–5 stages $12.18), and 261 of 894 runs reach 4–5 stages. A 3x trigger would fire on ~29% of runs — routine behaviour, not an anomaly — and an alarm that common trains dismissal. The p90 keying fires on roughly the worst 10%: the genuine tail.

**Thresholds are derived from recorded data, not hard-coded.** They must recompute as the corpus grows, exactly as the runtime `CalibrationHint` does — the numbers above are today's values, not constants.

### 4. Budget recalibration routed separately

The stale calibration in `.fishhawk/workflows.yaml` (avg ~$3.4 / max ~$8.4 from 29 runs, versus avg $8.45 / max $70.91 across 894) is a **live defect independent of this ADR** and is filed on its own rather than gated on this epic.

Named approver: repository maintainer (human).

## Consequences

The two moments spend is decided — the plan gate and the fix-up loop — gain a number, and the ~7.5% of spend on runs that never merge becomes visible where it is incurred rather than only in retrospect. Grooming and campaigns gain an economic input, which is what makes "what should we work next" answerable as a budgeted question rather than a purely qualitative one. Pricing (ADR-011) gains its cost half from data rather than guesswork.

Risks to design against:

- **Goodhart on value-per-dollar.** A groomer optimising cost-efficiency systematically deprioritises expensive work. The data *inverts* the obvious fear — `autonomy:low` work is cheaper (median $4.17 vs $7.33) so security and crypto would not be starved — but cost must stay one rubric line among several, never the sort key.
- **Anchoring the planner.** A planner shown "similar plans cost $8.50" may pad scope toward it. The runtime `CalibrationHint` has the same exposure and appears not to have caused drift, but this should be *checked* against `predicted_runtime_minutes` history rather than assumed.
- **False precision.** The single largest failure mode: any surface that renders a point estimate re-creates the problem option 2 was rejected for.
- **Single-workflow sample.** All 894 costed runs are `feature_change`. Forecasts for any other workflow are extrapolation until it has history, and must say so rather than silently reusing the wrong base rate.
