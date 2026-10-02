---
id: ADR-027
title: "Review-agent stage primitive: per-stage agent + human reviewer counts"
status: accepted
date: 2026-05-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/545
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-027: Review-agent stage primitive: per-stage agent + human reviewer counts

## Context

Today's dogfood loop (2026-05-26) shipped ~11 PRs. During those loops, the operator (human reviewer) caught several issues by reading agent-generated plans and implementations before approving:

- **#527 implement-stage regression** — agent's fix added a plan-artifact check at trace-upload time, which would have marked every healthy plan stage as failed in production (runner uploads trace BEFORE plan). Rejected at review; patched in-loop with correct architecture.
- **#534 first plan scope gap** — agent's plan covered only the backend-side validator. The runner has its own independent validator (`runner/internal/plan/plan.go:115`) that would have rejected coerced plans locally. Patched in-loop after the agent skipped the approval-note instruction.
- **#538 same scope gap pattern** — agent re-emitted backend-only coverage on a fresh plan after rejection (see #539 about feedback propagation). Approved and patched.
- **#533 cross-branch rejection test** — agent's plan covered the happy-path validation but missed the cross-branch case. Approval-note ask was respected this time.
- **Calibration confidence-band nudges** — agent confidently predicts "high" but high-confidence implements are 1/11 within 1.5x of actual. Repeated approval-note nudge to use "medium" instead.

These catches were valuable but operator-dependent: they happened because the operator was paying attention in that specific moment. The dogfood loop's value proposition assumes the operator is the reviewer, but the operator's attention is finite and review quality degrades with cadence (more PRs per day → less depth per review).

**Proposal**: formalize what the operator is doing manually as a workflow-spec primitive. Each stage can declare how many agent reviewers and how many human reviewers are required. The audit log captures each review verdict and concern list.

## Options

1. **Status quo: operator does manual reviews.** Pros: zero new code, zero new spec surface. Cons: review quality bounded by operator attention; agent-caught issues are inconsistent; compliance can't claim "this plan was reviewed by N parties."

2. **Plan-review agent only.** Add `reviewers: { agent: N, human: M }` to plan-stage executor. Agent reviewer runs after plan-stage agent produces standard_v1, emits a structured verdict, then human gates on the verdict + their own read. Implement-stage stays operator-reviewed. Pros: smallest scope, addresses the most expensive failure mode (bad plans drive bad implementations). Cons: doesn't catch implement-stage regressions like #527.

3. **Plan-review + implement-review agents.** Both stages get review primitives. Implement reviewer reads the diff against the plan's declared `scope.files` and flags out-of-scope edits, missing test coverage, regression risks. Pros: catches both classes; symmetric model. Cons: bigger scope; implement-review is fundamentally a diff-reading problem with its own prompt-template + tooling requirements.

4. **Full multi-reviewer pipeline with model selection.** Each `reviewers.agent` entry can specify model (`agent: [{model: "claude-opus-4-7"}, {model: "claude-sonnet-4-6"}]`). Different model perspectives. Pros: maximum coverage; explicit cross-model fresh eyes. Cons: complexity; tokens scale with reviewer count; tie-breaking semantics unclear.

## Recommendation (initial — subject to ADR review)

**Option 3 with phased rollout**: plan-review first, then implement-review.

### Spec shape (workflow-v0.x additive)

```yaml
stages:
  - id: plan
    type: plan
    executor:
      agent: claude-code
    reviewers:
      agent: 1   # 1 advisory agent review
      human: 1   # 1 human gate (today's default; backward-compatible)
  - id: implement
    type: implement
    executor:
      agent: claude-code
    reviewers:
      agent: 1   # diff vs scope.files cross-check
      human: 0   # gateless once review-agent approves (or set to 1 for double-gate)
```

Defaults preserve today's behavior:

```yaml
reviewers:
  agent: 0
  human: 1  # the existing approval gate
```

Per ADR-026's schema-evolution discipline (#472), this is an additive optional field within `workflow-v0.x` — no major version bump.

### Sequencing and verdict propagation

- **Sequential**, not parallel. Agent reviewers run first; the human reviewer sees the agent verdict(s) + concern list before deciding. This sidesteps the "two parties make independent gating decisions" failure mode where they disagree and operators have to mediate.
- **Verdicts are advisory, not authoritative**. A human can approve over an agent's "reject" verdict, or reject over an agent's "approve" verdict. The audit log captures both verdicts and the override.
- **Structured verdict shape** (proposed):
  ```json
  {
    "verdict": "approve | approve_with_concerns | reject",
    "concerns": [
      {"severity": "high|medium|low", "category": "scope|architecture|tests|security|other", "note": "..."}
    ],
    "free_form": "..."
  }
  ```

### Audit shape (new category)

`plan_reviewed { reviewer_kind: agent|human, reviewer_id, verdict, concerns[], free_form }` for plan stages.
`implement_reviewed { reviewer_kind: agent|human, reviewer_id, verdict, concerns[], free_form }` for implement stages.

These are distinct from `approval_submitted` (today's existing category) so compliance can query "show me all plans where an agent flagged 'high severity concern in scope' but the human approved anyway." That query is the load-bearing operability win.

### Review-agent prompt template (sketch)

The review-agent gets:

- The plan artifact (standard_v1 JSON for plan reviews; diff + plan for implement reviews).
- The originating issue body.
- The relevant `docs/` references (architecture, spec, methodology).
- A constrained instruction: emit ONLY the verdict structure; do NOT propose changes; flag concerns the operator should consider; assume the operator is the gate.

The review-agent does NOT see the parent agent's chain of reasoning. Cold fresh eyes.

## Why this matters

Today's loop had 4-5 catches that should have been systematic. Each catch saved one bad-PR iteration (~30-50k tokens + 10-30 min of operator time). At ~10 PRs/day cadence, even a 20% catch rate compounds. Concrete numerator: today the catches saved ~150k tokens and ~2 hours.

The compliance angle is also real. Without review-agent attribution, "this plan was reviewed before approval" answers "by the operator (who is also the requester)." With it: "by the operator AND by N independent agent reviewers, with the following structured concerns recorded."

## Open design questions

1. **Tie-breaking semantics when multi-agent reviewers disagree.** Probably "any reject = surfaced to human as override-required; all approve = human reviews with verdict summary." But: does the human gate become harder to ignore when 2/2 agents approve vs 0/2? Worth a UX nudge in the audit log payload.

2. **Reviewer-agent model selection.** Today the spec is string `agent: 1`. Future: `agent: [{model: "claude-opus-4-7"}, {model: "claude-sonnet-4-6"}]` for cross-model perspectives. Defer to v0.5 unless data says otherwise.

3. **Cost cap per review.** Plan reviews are cheap (~10-20k tokens reading a standard_v1 + relevant code). Implement reviews are more expensive (diff scanning, scope.files cross-check). Cap at e.g. 50k tokens per review with a budget-exhausted verdict fallback?

4. **Self-review prevention.** A review-agent should not be the same model run as the plan-agent (defeats the fresh-eyes premise). Easy when models differ; subtler when both are Sonnet. Maybe require explicit different `executor.agent` or `executor.model` between plan + review.

5. **Skip-review escape hatch for trivial PRs?** Some XS PRs (~30 LoC, no architectural risk) don't benefit from review and the token cost is wasted. Per-PR opt-out via the plan stage's note? Per-workflow `reviewers.skip_under_loc: 100`? Probably defer — v0 ships with the cost; tighten later if it bites.

6. **Implement-review's `scope.files` cross-check** depends on the implement agent producing diffs that match the plan's declared `scope.files`. Today agents drift from declared scope (per the in-loop patching pattern). The review-agent could flag this systematically — but enforcement (refuse to ship if drift) is a separate decision.

## Phasing

1. **Plan-review-agent first.** Smallest surface (text-only review). Spec field + audit category + review-agent prompt template + handler that runs review-agent after plan-stage agent completes. Roughly an XL ticket but parallel-safe with other work.

2. **Implement-review-agent second.** Once plan-review's machinery is settled, implement-review reuses the verdict structure + audit shape. New prompt template focused on diff reading. Bigger ticket.

3. **Model selection + cost caps third.** Once we have real data on review quality vs. cost, tune the knobs. Don't pre-optimize.

## Decision

**Adopt Option 3 (plan-review + implement-review, phased), with the `reviewers: { agent: N, human: M }` spec shape.** Resolved 2026-05-27. The six open design questions settle as follows:

### Reviewer authority — tied to the spec's `human` count

Authority is determined by the operator's own spec, which preserves ADR-021's "agent doesn't make gating decisions" principle wherever a human is in the loop:

| `reviewers` config | Behavior |
|---|---|
| `human >= 1` | Agent reviewers are **advisory**. The human is the gate; agent verdicts + concerns are surfaced before the human decides. An agent `reject` is a strong signal but does not block. |
| `human == 0` AND `agent >= 1` | Agent reviewers **are the gate**. An agent `reject` blocks the stage (no advance); all-approve advances. The unattended/batch path — the operator explicitly removed the human, so the agent review carries gating authority. |
| `human == 0` AND `agent == 0` | No review (today's gateless behavior). |
| default `{agent: 0, human: 1}` | Today's behavior — single human gate, no agent review. Backward-compatible. |

Rationale (Q1): agents make a gating decision only when the operator has explicitly removed the human. With a human present, the ADR-021 principle holds.

### Self-review prevention — warn-only (Q4)

If a review-agent's `executor`/model matches the stage agent's, log a warning but allow. Operators configure distinct models for true fresh eyes; same-model/fresh-context retains some value, so no hard block, no schema rejection.

### Implement-review scope drift — flag-only (Q6)

The implement-review agent reports diff-vs-`scope.files` drift as a **concern**, not a block — agents legitimately discover scope mid-implementation. (Under `human == 0`, the agent's *overall* verdict can still block per the authority table, but scope-drift alone is a concern, not an automatic reject.)

### Cost — no dedicated cap (Q3)

Bounded by the existing plan/implement-stage timeout (ADR-025 D1), not a separate token cap. No new knob in v0; revisit if review cost proves material.

### Deferred (confirmed)

- **Reviewer model selection** (`agent: [{model: ...}]`, Q2) — v0 ships scalar `agent: N`; revisit v0.5.
- **Skip-review escape hatch** (Q5) — v0 ships with the cost; add later if it bites.

### Verdict + audit shape

Structured verdict `{verdict: approve|approve_with_concerns|reject, concerns: [{severity, category, note}], free_form}`. New audit categories `plan_reviewed` / `implement_reviewed`, distinct from `approval_submitted`, carrying `reviewer_kind` (agent|human) so the "agent flagged a concern but the human approved anyway" query is first-class.

### Children (implementation issues)

- #560 — Plan-review-agent primitive (impl 1/2): `reviewers` spec field, review invocation, authority resolution, `plan_reviewed` audit, verdict surfacing. The prerequisite machinery.
- #561 — Implement-review-agent (impl 2/2): diff review against the approved plan, `scope.files` drift as a flag-only concern, `implement_reviewed` audit. Depends on #560.

## Consequences

- **Spec gains `reviewers` block** on every stage. Additive, backward-compatible. Operators who don't set it get today's behavior (`agent: 0, human: 1`).
- **New audit categories** (`plan_reviewed`, `implement_reviewed`) — distinct from existing `approval_submitted`. Compliance queries gain a new dimension.
- **Token cost increases** for opted-in workflows. Plan review ~10-20k tokens; implement review larger. Operators control via the spec — they pay for what they opt into.
- **Latency increases** for opted-in workflows — agent reviews are sequential. Roughly +1 agent invocation per stage with reviewers configured.
- **The dogfood loop's "operator-as-reviewer" tightening becomes "operator-as-final-reviewer."** The first-pass reading happens via an agent; the operator's attention budget is spent on tie-breaks and overrides.

## Related

- ADR-021 / #322 (MCP server v0 read-only) — established the "agent doesn't make gating decisions" principle. Review-agents are advisory, not gating; this stays consistent.
- ADR-026 / #472 (schema-evolution discipline) — new `reviewers` field follows the additive-within-major pattern.
- #539 — reject-feedback propagation. Adjacent: if a review-agent rejects, propagating the rejection reason to the next plan attempt is the same problem.
- Sibling open methodology issues from today's loop (#541 D4 scope-hint, #542 D4 parent advance, #544 dev auto-migrate) — none block this ADR; all share the "we observed operability gaps while dogfooding" pattern.

## Out of scope

- Multi-tenant review-agent assignment (which agent reviews which plan based on org/team). v0 is single-tenant.
- Cross-PR review state (e.g., "this reviewer's verdict on related PRs"). Each PR review is independent.
- Reviewer-agent self-tuning (review-agent learns from human overrides). Future research; not v0.

## Notes (today, 2026-05-26)

Today's review catches that motivated this ADR:

- #527 implement: prevented a production regression that would have broken every healthy plan stage. Would-have-cost = full PR rollback + re-investigation, ~1 hour.
- #534 plan: caught missing runner-side coverage. Would-have-cost = #537 (coercion) coming back to fix this too, ~30 min.
- #538 plan: same pattern re-emerged (see #539). Would-have-cost = third PR to add runner coverage, ~30 min.
- #533 cross-branch rejection test: caught at approval, agent honored it. Cost saved by test-the-contract discipline.
- Multiple calibration "use medium not high" nudges. Cumulative effect: keeps the agent's confidence calibration honest over time.

Total estimated savings today: ~2 hours and ~150k tokens that would have been spent on recovery iterations.
