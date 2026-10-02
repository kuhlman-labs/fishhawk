---
id: ADR-036
title: "Gate plan approval on agent-review completion (block-on-completion, unblock-on-terminal)"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/874
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-036: Gate plan approval on agent-review completion (block-on-completion, unblock-on-terminal)

**Status: Decided (2026-06-08).** Refines the approval gate for stages that configure an advisory agent reviewer (ADR-027). Same *enforce-don't-rely-on-convention* philosophy as ADR-035 (#857).

## Context

The `feature_change` plan stage configures an advisory agent reviewer (`reviewers {agent:1, human:1}`, ADR-027). The agent review runs **detached** and lands its `plan_reviewed` audit entry ~2–3 min AFTER the plan stage completes. Nothing today prevents the human from approving the plan gate during that window — `fishhawk_get_plan` shows `plan_review_status: pending`, and the operator can approve before the review lands.

This is not hypothetical: on the ADR-035 arc (2026-06-08) the operator approved the plan in **4 of 5 runs** (#797, #858, #862, #867) BEFORE the agent `plan_reviewed` entry landed — the operator's own "read the agent review before approving" convention failed under a human racing the async review. The operator configured the agent reviewer precisely to get that signal before the gate resolves; approving before it lands defeats the declared intent. Convention is insufficient — the same lesson ADR-035 drew about branch-ownership namespacing.

## Options

1. **Status quo (convention only).** Rejected — demonstrably skipped 4/5 times.
2. **Make the agent review GATING (a reject fails the stage).** Rejected — that is a different semantic (verdict-binding). It removes the human's authority over an *advisory* verdict and conflates "must have waited for it" with "must obey it." Gating reviewers (`human==0`) already fail the stage synchronously and are out of scope here.
3. **Block approval while a configured agent review is IN-FLIGHT; unblock on any terminal outcome.** Chosen. Enforces "the human must wait for / have seen the review" WITHOUT making its verdict binding.

## Decision

When a stage has a configured agent reviewer (`reviewers.agent > 0`) that has been DISPATCHED (`*_review_started` emitted), the approval gate refuses approval **while the review is in-flight**:

1. **Block on COMPLETION, not VERDICT.** The human retains authority to approve OVER advisory concerns. The enforcement is "you must have waited for it," not "you must obey it." `POST /v0/stages/{id}/approvals` and `fishhawk_approve_plan` return a typed `agent_review_pending` error while in-flight.
2. **In-flight** = `*_review_started` present AND fewer than the configured count of TERMINAL review entries present.
3. **Unblock on ANY terminal outcome** — `*_reviewed`, `*_review_failed` (#664), `*_review_skipped`, or timed-out (#747). A failed / skipped / timed-out reviewer MUST NOT strand the gate.
4. **HARD BACKSTOP (non-negotiable).** A max-wait bound after `*_review_started`, after which approval is allowed with a logged degrade — so a reviewer that dies without emitting any terminal entry can NEVER brick the gate forever. No #856-class stranding. The block is a *wait*, never a deadlock.
5. **No agent reviewer configured / not dispatched → no block** (byte-for-byte today's behavior). Gating reviewers are unaffected (they already fail the stage synchronously).
6. **No silent bypass.** An explicit, audited operator override is deferred (likely unnecessary given the terminal-unblock + backstop); if added later it must be audited.

## Consequences

- **Positive:** the operator can no longer skip the configured advisory signal by racing the async review; ADR-027's declared intent is enforced structurally. Directly fixes the demonstrated 4/5 lapse.
- **Cost:** ~2–3 min added latency at the gate (wait for the review). In the MCP loop, `fishhawk_approve_plan` returns `agent_review_pending` and the operator polls/retries. In the UI, approve is disabled with "agent review in progress."
- **Risk:** mis-implementing the unblock = a stranded gate (#856 class). Mitigated by treating ALL terminal outcomes as unblocking + the hard max-wait backstop. This is the load-bearing part of the decision.
- **Scope:** plan-approval gate first; the implement-review / merge gate (ADR-031, merge-driven) gets the same completion-gate as a follow-up, not bundled.
- **Cross-refs:** ADR-027 (#545, review-agent primitive), ADR-031 (#710, review gate), #664 (`plan_review_failed`), #747 (review timeout), ADR-035 (#857, enforce-not-convention). Motivated by the operator-lapse observed across #797/#858/#862/#867.

Parent epic: #389
