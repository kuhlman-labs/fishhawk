---
id: ADR-047
title: "Campaign primitive — dependency-ordered multi-run sprint driving, generalizing the decomposition fan-out engine to the epic/issue level"
status: accepted
date: 2026-06-29
issue: https://github.com/kuhlman-labs/fishhawk/issues/1437
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-047: Campaign primitive — dependency-ordered multi-run sprint driving, generalizing the decomposition fan-out engine to the epic/issue level

## Context

Fishhawk captures the **single-run** gated loop (plan → approve → implement → review → merge) and the per-gate **operator contract** (`operator_agent`: delegation conditions, `model_policy`). Decomposition fan-out (`run_children` / `consolidate_slices`) already drives **dependency-ordered sub-plans of one run** — waves keyed on `depends_on`, each wave based on the integrated predecessor branch, a child-completion sweeper — into **one consolidated PR**.

But the **sprint layer** has no product object. Organizing a set of issues into a dependency order, driving each through its **own full run + PR** in sequence, applying the operator contract per run, filing follow-ups as gaps surface, handling cross-run post-merge, and re-sequencing when the landscape changes — all of that lives entirely in the **operator-agent + the driving conversation**, reconstructable only from the transcript. It is not durable, resumable, observable, or reproducible.

This was demonstrated live: a recent sprint drove ~10 issues across two tracks (automatic model selection + the deploy stage) through the loop — including a 4-slice decomposition with transient-child recovery, heterogeneous-review arbitration (defer/waive), a security fixup that inverted a reject to approve, and a deploy-gap discovery chain (#1429 → #1431 → #1432) with follow-up filing — entirely operator-orchestrated, with nothing of the *sprint* persisted.

Crucially, the decomposition fan-out engine is the **same mechanics one level down**. A campaign primitive lifts that engine from *sub-plans of one run* to *issues of an epic/sprint*.

## Options

1. **Status quo — sprint orchestration stays an operator-agent behavior (conversation-only).** Pro: zero product work; maximum operator-agent flexibility. Con: not durable / resumable / observable; not reproducible; "consistent operator operation" cannot reach the sprint layer; no rollup status; the campaign exists only in a transcript.

2. **Campaign primitive (generalize the fan-out engine).** A first-class `campaign` object referencing an epic or explicit issue set + a `depends_on` DAG; the backend drives each issue through its **own full run + PR** in topological order, applies the `operator_agent` contract per run, surfaces rollup status, and handles cross-run advance. Reuses `run_children`'s wave engine (waves, `depends_on`, integrated-predecessor base, completion sweeper). Distinct from decomposition: the unit is an **issue → a separate reviewed/merged PR**, NOT a consolidated slice. Pro: durable, resumable, observable, reproducible; the structural home for sprint behavior. Con: significant new surface (object model, API, driving, status, cross-run failure semantics).

3. **Operator-contract extension only.** Encode sprint-level decision policy (sequencing strategy, fixup-vs-defer-vs-waive thresholds, decompose-above-N, file-follow-up-on-deferral) in the `operator_agent` block, but keep the driving in the operator-agent. Pro: smaller; captures the *policy*; extends the `model_policy` precedent. Con: does not make the sprint a durable object; orchestration still lives in the conversation.

4. **Agent-eval corpus only.** Capture sprint runs as eval cases (the #819 flywheel, `fishhawk-distill-corpus` #1290) to regression-test the operator-agent's judgment, without a campaign object. Pro: cheap; captures decision *quality*. Con: tests behavior, does not make it a product primitive.

## Recommendation

Adopt **Option 2 (campaign primitive)** as the structural answer, complemented by **Option 4 (corpus)** as a parallel capture of decision quality — they are orthogonal: the campaign captures *how the sprint runs* (durable orchestration, reusing the proven fan-out engine); the corpus captures *whether it ran well* (the operator-agent's judgment). **Option 3** is a natural follow-on that configures the campaign's per-run delegation, building on `model_policy`.

Design questions deferred to the impl epic: the campaign object model (epic-ref vs explicit issue set); the `depends_on` DAG source (issue-tracker relations vs an explicit campaign spec); per-run vs per-campaign operator contract; cross-run failure semantics (block dependents vs continue-and-report); rollup status + resumability; how follow-up-issue filing during a campaign feeds back into it.

## Decision

ACCEPTED (2026-06-29). Adopt the campaign primitive (Option 2), reusing the wave engine (`plan.Waves`), the run/stage state machine, and the delegation evaluator. Two design decisions taken at acceptance — the **north-star** posture, not the MVP shortcut:

1. **DAG source = an issue-level `depends_on` TRACKER RELATION** (not a runtime payload or a `campaigns:` spec block). There is no issue-level `depends_on` today (only the sub-plan field, `plan.go:149`); a new work-management relation carries it and a campaign derives its DAG from the tracker (epic children + their `depends_on` edges).
2. **Drive model = the BACKEND AUTO-DRIVES** each issue-run under the `operator_agent` contract — auto-acting when a gate delegates (approve / route-fixup / retry / merge) and **pausing + paging** on `must_page_human`. This moves ADR-040's autonomous cadence from the operator-agent into the product.

The corpus flywheel (Option 4) was the parallel decision-quality capture (PR #1438, three sprint cases landed). The operator-contract sprint-policy extension (Option 3) lands as a late campaign-level `operator_agent` override child.

Implementation: a `[E<n>] Campaign primitive` epic with dependency-ordered children — Track A (the `depends_on` relation), Track B (campaign object + state, making it operator-agent-drivable), Track C (the backend auto-driver, achieving full backend-auto-drive), Track D (MCP/CLI/frontend/docs surfaces), Track E (the campaign-level contract). Foundation A+B+D is immediately useful (operator-agent drives the campaign by consuming its next_action); Track C removes the agent from the loop.

## Consequences

- A new top-level **campaign** object + driving + rollup status, reusing the decomposition fan-out engine — sprint behavior becomes durable / resumable / observable instead of conversation-only.
- The `operator_agent` contract gains a sprint-level extension point (Option 3), so "consistent operator operation" reaches the sprint layer, not just the gate.
- The agent-eval corpus (Option 4) gains operator-orchestration cases (decomposition recovery, heterogeneous-reject arbitration, fixup-inverts-reject, gap-discovery), making the operator-agent's judgment testable.
- On acceptance: stand up an E-epic with impl children (object model + API, the epic-level wave driver, rollup status, the contract extension). The fan-out engine reuse keeps the core driving cost down; the new surface is mostly object model + status + cross-run failure semantics.
