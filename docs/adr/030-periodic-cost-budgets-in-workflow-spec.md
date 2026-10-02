---
id: ADR-030
title: "Periodic cost budgets in the workflow spec (advisory + admission-time blocking)"
status: accepted
date: 2026-06-02
issue: https://github.com/kuhlman-labs/fishhawk/issues/687
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-030: Periodic cost budgets in the workflow spec (advisory + admission-time blocking)

## Context

#649 and its follow-ups (#680 double-count, #681 reviewer cost, #682 known_usage honesty, #684 resolved_model pin) shipped complete per-run cost accounting: a `cost_recorded` audit ledger (timestamped, model, token split, usd, `source`, `known_usage`) and a `runs.cost_usd_total` rollup — correct across stage agents + reviewers, all runner/agent/reviewer backends, and honest about unreported usage.

Operators now need a **cost-control surface**: a periodic spend cap they configure in the workflow spec, with a choice to WARN or BLOCK when hit. This is the agentic-SDLC report's "the system can stop itself" governance control, in its absolute-budget form.

**Existing scaffolding & constraints:**
- The per-stage `budget` block already has `enforcement: advisory | blocking` (`workflow-v0.schema.json`). But `workflow-v0.md` defers per-stage *blocking* to v0.x "once Fishhawk issues ephemeral agent keys (so the proxy can hard-cap)" — because hard-capping spend **mid-run** needs a spend proxy.
- Complementary controls already exist/planned: **#649 `spend_alert`** (rate anomaly — runaway-loop detector) and **#653** (per-run cost/loop tripwire, the folded-in G3).

**Key realization:** a *periodic* budget is enforced at **run admission**, not mid-call — sum `cost_recorded` over the period and compare to the cap *before* dispatching the next run. That's a deterministic gate on already-known data, so **periodic-budget blocking does NOT need the ephemeral-key proxy** the per-stage blocking was waiting on. It is the more tractable blocking to ship first.

## Options

- **Scope** of one limit: per-workflow / repo-wide / both.
- **Period** boundary: calendar (resets 1st-of-month / ISO-week, timezone-aware) / rolling trailing window.
- **Enforcement**: warn-only first / warn + opt-in blocking now.
- **Block enforcement point**: run-admission (deterministic, no proxy) / mid-run hard-cap (needs the deferred ephemeral-key proxy).

## Recommendation / Decision

**Decided (2026-06-02):**
- **Per-workflow** budgets, defined in the workflow spec. One budget sums `cost_recorded` for that workflow's runs over the period. (Repo-wide aggregate can layer on later.)
- **Calendar** period (`weekly` | `monthly`), timezone-configurable, with a defined reset boundary.
- **Warn (advisory) default + opt-in blocking**, reusing the existing `enforcement: advisory | blocking` enum:
  - **advisory (WARN)**: emit a new `budget_alert` audit entry + issue comment at `warn_at` (e.g. 80%) and at 100%; runs proceed. (Reuses the #649 `spend_alert` surfacing pattern.)
  - **blocking (BLOCK)**: at run admission (`fishhawk_start_run` / dispatch), refuse to start a new run once the period spend ≥ limit, with a clear reason; **in-flight runs finish**; an **operator override** can force past.
- Spec shape (additive, `workflow-v0.x`):
  ```yaml
  budgets:
    - period: monthly        # weekly | monthly
      limit_usd: 500
      enforcement: advisory  # advisory | blocking
      warn_at: 0.8           # optional early warning fraction
  ```

## Consequences

- **Spec schema change**: additive `budgets` to `workflow-v0.x` (no major bump); update `docs/spec/workflow-v0.{schema.json,md}` + embedded copies (sync-schemas) + the plan validator's recognized set.
- **New admission gate** in the run-create/dispatch path — deterministic, reads the `cost_recorded` ledger; distinct from (and unblocking relative to) the deferred per-stage mid-run blocking.
- **New `budget_alert` audit kind** + issue-comment surface (`docs/issue-comment-surfaces.md`).
- **Override mechanism** for blocking (operator force-past), since budgets enforce on **estimated** cost (pricing table is point-in-time, and `known_usage=false` runs undercount) — a hard block on an estimate must be escapable.
- **Three-axis budget story** now coherent: per-run tripwire (#653) + rate anomaly (#649 `spend_alert`) + periodic cap (this ADR).
- Implementation tracked separately (follow-up; may decompose like #649 — spec+schema, admission gate+block/override, warn surfacing).

## Related
- #649/#680/#681/#682/#684 (the cost ledger this builds on), #653 (per-run tripwire), #649 spend_alert, ADR-025 (#451, stage budget framing), `workflow-v0` spec. 

Parent epic: #389
