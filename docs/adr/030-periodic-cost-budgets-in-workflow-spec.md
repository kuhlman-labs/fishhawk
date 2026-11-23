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

## Amendment A1 (accepted 2026-11-22) — blocking budgets are also enforced when the host dispatcher spawns a stage

This amendment extends where an `enforcement: blocking` periodic budget blocks (#5473). The Captain ratified it on 2026-11-22 with rulings C1 (the verdict reads the live budget) and C2 (the override exemption inherits through `DecomposedFrom` only) and approval conditions D1 to D5. It also extends ADR-104's host-dispatcher contract: the dispatcher's pre-spawn checks gain a budget verdict. ADR-104 itself is not edited.

1. **The blocking point extends to spawn time.** Blocking still refuses a new run at admission. It now also refuses the host dispatcher's spawn of any LATER stage of an admitted run while the period spend is over the limit (`budget_blocked`). In-flight stages finish; nothing running is stopped. "In-flight runs finish" now reads "in-flight stages finish".
2. **The verdict reads the current budgets (C1).** The dispatcher reads `GET /v0/runs/{run_id}?include=dispatch_budget` before every spawn. The server evaluates the repository's CURRENT default-branch `.fishhawk/workflows.yaml` budgets, every budget rather than the first, at the effective limit (`FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD` when set). A `limit_usd` raise, a switch to `enforcement: advisory` or a period rollover therefore unblocks an in-flight run on the next dispatcher tick. Lowering a limit, or making a budget blocking, parks in-flight runs' later stages on the next tick.
3. **Everything else keeps the admission spec.** Only the budgets come from the live spec. Stages, verify, slot routing, prompts and alerts keep reading the run's admission spec (`runs.workflow_spec`).
4. **Sources and read failure (D2).** A forge with no server-side spec read (GitLab today, or a deployment with no GitHub client) evaluates the run's admission spec. When the live read fails (no installation, spec not found, an unparseable spec, the workflow absent from the live spec, or a forge error or rate limit), the server falls back to the run's admission spec. It fails closed (`503 budget_check_failed`, and the dispatcher refuses `budget_check_failed` and retries on the next tick) ONLY when that admission spec declares a blocking budget. An admission spec with no blocking budget clears, so an advisory-only repository never stalls on a read error: advisory never blocks. Every clear or blocked verdict names its source in `budget_source`: `live_spec`, `admission_spec_fallback` or `admission_spec`. The 503 body carries only an enumerated reason; the raw cause is logged server-side with the run id.
5. **No period-level override.** There is no verb that lifts a budget for everyone for the rest of the period. The remedies are the per-run override, the rollover, and a default-branch edit to `limit_usd` or `enforcement`.
6. **The override exemption is per run, inherited through decomposition only (C2).** A run admitted with `budget_override` is exempt for its lifetime. The grant is bound to the created run by an operator-only `run_budget_override_bound` run-chain row naming the repo and workflow. Its `DecomposedFrom` descendants with the same repo and `workflow_id` are exempt too. A recovery, CI-retry or follow-up run (`ParentRunID`) is not, and needs its own override; a webhook CI retry cannot carry one. A plain create cannot nominate a parent, and every non-decomposition create path leaves `DecomposedFrom` nil. If the binding row cannot be appended, the run stays admitted but is not exempt, and the create or recover response says so with a remedy.
7. **Merge-candidate verify-only passes are exempt.** A verify-only pass re-verifies an already-built head and spawns under an exhausted budget. The server decides it from its own `stage_merge_candidate_verify_triggered` row; an unreadable trigger is not exempt.
8. **Exemptions first, then fail closed (D1).** The server decides both exemptions BEFORE the live budget read, so an exempt run or a verify-only pass spawns even when that read fails. Any other undecided verdict defers fail-closed, unlike admission, which is fail-open on a sum error.
9. **Only the host dispatcher is gated.** Operator verbs are not: a hand `fishhawk_dispatch_stage` spawns the stage and spends past the limit. The stdio status overlay (`host_dispatch_budget_blocked`), `fishhawk_doctor` (`host_dispatch_budget`) and `fishhawk-mcp host-dispatch --status` name the budget, its period, its spend, its source and the remedies that source allows: a default-branch edit only for a `live_spec` verdict, the rollover or a new run with `budget_override` for an admission-spec verdict.
10. **The display agrees with the gate.** `GET /v0/runs/{run_id}/budget` reads the same source at the same effective limit and reports the governing budget: the blocking budget the dispatcher blocks on, else the first budget. On a live-read failure the display falls back to the admission spec, because it is never a gate. A run-bound (`mcp:run:`) caller is served the admission spec with no live read, so an in-runner agent cannot spend the installation's forge rate limit through the display.

**Rollback (D3).** Reverting #5473 removes the include value and the dispatcher check; there is no migration. The revert must KEEP the `run_budget_override_bound` entry in `withholdScopeOverlapFromRunBound` (`backend/internal/server/reads.go`) and the category registration, because rows persisted before the revert outlive it and carry spend figures. A revert that drops them must re-add the entry by hand. Both are interim under ADR-106. To stop only the dispatcher behaviour without a revert, set every budget to `enforcement: advisory` on the default branch; the next tick clears every live-spec verdict.

Recorded with #5473; final text confirmed at merge by the first officer.
