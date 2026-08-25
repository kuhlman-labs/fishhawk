---
title: Tuning autonomy
description: How the autonomy tier delegates decisions, what changes when you move a tier, and the two ways to adjust one class or one path without moving the whole tier.
---

Your [first run](/fishhawk/start/first-run/) chose a preset. By now you have
evidence about where you want to sit — which decisions you are happy to delegate
and which you want to keep. This page is how you move, and how to read what
actually changes when you do.

## The mechanism: a tier is shorthand over an action matrix

`autonomy:` on a workflow is a one-word tier — `low`, `medium`, or `high` — that
expands to an **action matrix**: one entry per action class, each resolving to a
`mode`. The three modes are the whole grammar:

| Mode | Who acts |
|---|---|
| `gated` | The human acts. This is the fail-closed default — an unlisted class, an absent matrix, and an absent `autonomy` block all resolve to it. |
| `auto` | The operator agent may act — and **only** `auto` requires a `when` naming that class's single backend-evaluable condition. |
| `report` | The agent surfaces a proposal and does **not** act. |

A delegated action is **condition-gated, not trust-gated**: the backend
re-evaluates the named condition against current run state at action time and
refuses with the exact failed predicate otherwise. Delegation never widens what
the action may do. The canonical reference is
[`docs/METHODOLOGY.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/METHODOLOGY.md)
and the v2 schema
[`docs/spec/workflow-v2.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-v2.md).

## What each tier expands to

| Tier | Expands to |
|---|---|
| **low** | Every class `gated`. Nothing is delegated; every judgment — approval, fix-up routing, waiver, retry, merge — pages the human. This is also what a workflow declaring no autonomy block resolves to. |
| **medium** | `approve: auto (clean_dual_approval)`, `fixup: auto (convergent_concerns)`, `retry: auto (infra_flake)`; `waive` and `merge` stay `gated`. |
| **high** | Medium plus `waive: auto (solo_low)` and `merge: auto (gates_resolved_ci_green)`. |

## What actually changes when you move a tier

Moving `low` → `medium` stops routine mechanical judgments from reaching you: a
clean dual-approved plan is approved, a convergent review concern is routed to a
fix-up, an infra-flake failure is retried — each only when its named condition
holds against live state. Moving `medium` → `high` additionally lets the agent
waive a solo low-severity concern and merge a change whose gates are resolved and
CI is green.

What does **not** change is what pages you. Both `medium` and `high` carry the
**same** non-delegable `page_human_on` list — the same seven events:
`gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`,
`policy_override`, `exception_request`, `requirement_arbitration`. So **raising
the tier delegates the clean-path verbs and never disagreement arbitration** — a
reviewer reject, a rejection, a scope amendment or an override still reaches a
person at every tier. A `page_human_on` event wins over a `report` proposal too:
an event that must page is not a suggestion.

The audit log records the difference: each class resolution carries its
**provenance** — whether an explicit entry, the tier expansion, or the
fail-closed default decided it — surfaced on `GET /v0/runs/{id}`'s
`delegation.matrix`, so an operator reading `approve: gated (tier)` sees *why* an
action was or was not taken.

## `report` is the underused middle step

Between "the human always does it" and "the agent may do it" there is `report`:
the agent evaluates the class and surfaces the proposal it *would* have acted on,
without acting. It records a proposal row at a live gate for a person to act on.
The honest way to evaluate delegating a class is to run it in `report` mode first
and read what the agent would have done, before you hand it the `auto` authority.

## Moving without moving the whole tier

Two mechanisms adjust the matrix without changing the tier for everything:

- **A per-class `actions:` entry overrides the tier for that class only.** On an
  `autonomy: medium` workflow you can set `actions: {merge: {mode: report}}` to
  preview merges while leaving approve, fixup and retry at their medium values.
  (Note the wholesale rule: a *gate* that declares its own `autonomy` or
  `actions` supplies the entire block for that gate and inherits nothing — every
  class it does not name falls to the fail-closed default, not to the workflow's
  tier.)
- **A per-path `escalations` block can clamp `max_autonomy`** for changes
  touching sensitive paths. An escalation may only ever **raise the bar, never
  lower it** — a change reaching into an escalated path cannot be carried at a
  higher autonomy than the escalation permits.

## The caution this repository learned on itself

An escalation is a control only if someone can clear it. An escalation that
raises an approval count **beyond the number of eligible approvers** is not a
stricter control — it is a gate nobody can clear, and the run wedges. When you
tighten a gate, check that the tightened bar is still reachable by the people
`not: [author, agent]` leaves eligible.
