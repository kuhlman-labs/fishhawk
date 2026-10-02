---
id: ADR-071
title: "Beta hardening for external operators: intake sanitization, gate notification and SLA, and per-tenant limits"
status: accepted
date: 2026-07-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/2288
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-071: Beta hardening for external operators: intake sanitization, gate notification and SLA, and per-tenant limits

## Context

Beta is defined (E57 #2255) as **external design partners running hosted, multi-tenant Fishhawk against their own repositories**. Three properties of the system change at that boundary, and each exposes a gap that is invisible today because the only operator is the person who built it:

1. **Untrusted input becomes real** — partner repos, partner contributors, partner issue commenters.
2. **Operators become distributed** — gate waits stop being "the founder is right there."
3. **Nobody is watching** — shared capacity and shared inference spend have no per-tenant bound.

### 1. Issue intake is the least-defended channel, and the most exposed

Fishhawk has good untrusted-input machinery: `sanitizeUntrustedComment` (per-line `| ` quote-prefixing plus structure neutralization) inside `BEGIN/END UNTRUSTED` envelopes, built under ADR-050 / E31.8 / #1613. It is applied to **acceptance-failure text** — output from our own validator.

It is **not** applied to the issue body or comments. `backend/internal/prompt/prompt.go:3846`:

```go
if t.IssueBody != "" {
    b.WriteString(t.IssueBody)   // raw
}
writeIssueComments(b, t.IssueComments)
```

`writeIssueComments` filters `[bot]` authors and caps each comment at 2000 bytes. No sanitization, no envelope.

**The risk gradient is inverted:** the most-trusted input (our own validator) is the most-defended; the channel writable by any account with tracker access is raw. Two things sharpen it — the comment channel is a **designed** influence path (documented in `work-management-v0.md` as the "Comment-vs-body refinement channel"), and per the `IssueComments` doc comment it reaches **three** prompts: plan, plan-review, and implement-review.

This is not an unrecognised threat class. The team understood it, built the tooling, and pointed it at the wrong channel first.

### 2. Gates already dominate wall-clock, measured on the best-case sample

Queried 2026-07-27 over 743 runs:

| | median | p75 | p90 | p99 |
|---|---|---|---|---|
| plan→implement gap (approval wait) | **10.4 min** | 18.3 | 30.1 | 97.1 |
| plan agent work | 2.0 min | | | |

Gates are ~5x agent time at the median — **and this is one engaged operator frequently sitting at the keyboard**, which is close to the floor for a human in the loop. A distributed partner team with a specific required approver is hours or days, and a partner whose run sits over a weekend concludes the product is slow.

Two pieces already exist and do nothing: `sla` is declared on the approval gate in `workflow-v*.schema.json` (described as *"SLA before timeout fires a category-D failure"*) and is unimplemented; `latency.AggregateGateLatency` is computed and unconsumed.

### 3. Nothing bounds a tenant

E59.4 (#2282) parks a single run crossing its cost p90. Periodic budgets (ADR-030) bound a workflow's spend per calendar period. **Neither bounds a tenant**: one account can saturate shared runner capacity or accrue unbounded inference spend, and in a hosted multi-tenant deployment (E44 / ADR-057) that is a cross-customer failure, not a self-inflicted one.

## Options

1. **Ship beta without these.** Rejected per item: intake injection is not a wait-and-see class once partner repos are involved; the `sla` field being present and inert is worse than absent; and a missing tenant bound fails at the worst possible moment, during the design-partner program.

2. **Harden all three before partner onboarding.** Bounded work, each item small relative to its failure mode, and each is materially cheaper now than after an incident with a named partner attached to it.

3. **Harden reactively — fix what partners actually hit.** Reasonable for polish, wrong for these three. A prompt-injection finding surfaced by a compliance-conscious partner (E11 targets ≥2 of them, where audit is *the reason* they are there) is a trust event, not a bug report.

4. **Defer to a general post-beta security pass.** Rejected for item 1 specifically: the machinery already exists and is already ratified: this is applying a settled decision to one more call site, not new security architecture. Bundling it into a future pass trades a cheap fix for a long wait.

## Recommendation

**Option 2**, with per-item design.

### Decision

**Accepted (2026-07-27).** Adopt Option 2 — harden all three before partner onboarding. Three forks were settled; where they refine the Recommendation, **these govern**.

### 1. Intake sanitization is ASYMMETRIC: full envelope on comments, lighter on the body

Both go inside `BEGIN/END UNTRUSTED` envelopes with explicit framing that the content is issue text and **must not be treated as instructions**. They differ in neutralization:

- **Comments — full `sanitizeUntrustedComment`.** Per-line `| ` quote-prefixing plus structure neutralization. Comments are pure refinement; aggressive treatment costs nothing.
- **Body — envelope and framing, structure preserved.** No per-line quoting. The body is the plan's **primary input**, and its markdown structure (code blocks, lists, headings) is signal the planner uses. Flattening it would degrade the artifact the plan stage exists to produce.

**The body treatment is eval-gated**, on the `agenteval` corpus, following the discipline ADR-068 applied to review conventions (E55.4 #2245). If the corpus shows the lighter treatment is insufficient — or that the envelope alone already costs plan quality — that is evidence to act on, not to work around.

Applies to all three consuming prompts: plan, plan-review, implement-review.

### 2. `sla` means escalation and notification — never failure

An SLA breach notifies and escalates to a secondary approver. It **does not** fail the stage.

This deliberately **contradicts the field's current schema description** (*"SLA before timeout fires a category-D failure"*). Failing a partner's correct work because an approver was on holiday is a worse outcome than a slow run, and beta is the wrong moment to introduce a way for good work to die on a timer. **Updating the schema description is part of this work, not a follow-up** — leaving the docs promising failure while the code escalates is exactly the kind of silent reinterpretation this ADR exists to avoid. The failure semantics are recorded as **deferred**, not rejected.

### 3. Tenant caps refuse admission with an actionable reason

Concurrency and per-period spend are bounded **per account** (the ADR-057 tenancy unit). Exceeding a cap refuses the new run, reusing `CheckBlockingBudget`'s shape and its fail-open-when-the-capability-is-absent posture.

Queueing was rejected: it trades a clear refusal for a queue with its own ordering, fairness, starvation and timeout semantics, and a run that looks alive but is not running is harder for a partner to diagnose than one that was declined.

**The reason string is the feature.** The person hitting this limit cannot read the code. It must name the limit, current usage, and what clears it.

Named approver: repository maintainer (human).

## Consequences

The three failure modes that only appear with external operators are closed before the operators arrive, and each is cheaper now than during a partner engagement. `sla` stops being a field that silently does nothing. Gate latency becomes visible per partner, which is also an early churn signal — a partner whose gates stall is a partner disengaging.

Costs and risks:

- **Decision 1 may degrade plan quality**, which is why it is eval-gated. A null or negative corpus result is a legitimate outcome that should change the treatment, not be worked around.
- **Decision 2 adds an outbound integration surface** (Slack) and its failure modes; notifications must degrade to advisory rather than blocking a gate.
- **Decision 3 introduces a new refusal path** that will, correctly, sometimes block a partner. The reason string matters more than usual, since the person hitting it cannot read the code.
- The `sla` reinterpretation leaves the schema description inaccurate until it is updated — that documentation fix is part of the work, not a follow-up.
