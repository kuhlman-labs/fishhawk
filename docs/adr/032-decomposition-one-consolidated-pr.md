---
id: ADR-032
title: "Decomposition delivery model: one consolidated PR per decomposition"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/719
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-032: Decomposition delivery model: one consolidated PR per decomposition

**Status: Decided (2026-06-03).** Amends the decomposition fan-out design (the per-child PR behaviour that emerged from ADR-031 Phase 1). Rides on ADR-031's `github_merge` resolution unchanged. Re-scopes #714 as its implementation.

## Context

Decomposition splits a change too large for one implement run into budget-sized `decomposition.sub_plans[]`, executed as fan-out **child runs** (each an implement-only run; the parent's plan is the contract). It exists to bound **execution size** — how much an agent does in one run (budget / context).

ADR-031 Phase 1 made `push_and_open_pr=true` the local-loop default (#702), and #713 made it actually work for MCP/local runs. A side effect no one chose: every implement stage opens a PR — **including each child implement stage** — so a decomposition now fans out into **N separate child PRs**, while the **parent run has no PR of its own**. This is an *emergent accident of the default*, not a design decision.

Two problems result:

1. **ADR-031 breaks for decomposed parents (#714).** The merge reconciler resolves review stages via the run's own PR; a decomposed parent has none, so it parks at `review awaiting_approval` forever. Resolving it on `children_settled{succeeded}` is wrong because children are implement-only and reach `succeeded` at **PR-open, not PR-merge** — so the parent would reach `succeeded` with N child PRs still unmerged, violating ADR-031's invariant ("`succeeded` = verifiably landed, never an assertion").
2. **N child PRs fights the operator and the work.** Operators assume **1 issue = 1 PR**; N PRs for one logical change multiplies review clicks and CI runs. Worse, sub-plans run in **dependency order** (child 2's code builds on child 1's), so as separate PRs child 2 cannot go CI-green until child 1 merges — forcing a manual merge ordering.

Underneath: two distinct axes were conflated. **Execution size** (decomposition's job) and **review size** (how much a human reviews in one PR — the operator-control goal in METHODOLOGY). They need not be solved the same way. Review size is signalled by the **plan** — the operator sees the decomposition + `scope.files` at the plan gate and accepts or rejects the size *before any code exists*; PR-splitting is the wrong lever.

## Options

1. **N child PRs + child merge-reconciliation.** Give children a review stage (or reconcile child PRs) so a child reaches `succeeded` only on its PR merge; the parent resolves on all-merged. Uniform (every PR resolves via merge) but the most machinery, leaves the parent a ghost, and still imposes N review gates + merge ordering on the operator.
2. **Stacked branches** (child PRs → a parent branch → a parent PR → default). Most flexible — small child PRs *and* a final integration PR; reviewer picks granularity. But for agent-driven work it collapses: auto child→parent merges ⇒ the only real review is the parent→default PR (= option 3 with extra orchestration); human-reviewed child merges ⇒ N gates + ordering. High complexity; its value (granularity choice) matters most with multiple reviewers.
3. **One consolidated PR.** Children push commits to a **shared parent branch** (one commit per sub-plan, individually reviewable); child implement stages do **not** open PRs; the **parent opens one PR** against the base branch after `children_settled`; the parent's review reconciles on that PR's merge via the existing ADR-031 `github_merge` path. "1 issue = 1 PR"; ADR-031 falls out free; aligns with dependency-ordered execution (child N sees child N−1's commits on the shared branch).
4. **Separate issues for separable work.** When work is genuinely N independently-shippable changes, the plan proposes filing N issues (operator approves the split — operator-owns-filing preserved), each driven as its own 1-issue-1-PR run. The honest form of "I want N PRs."

## Decision

**A decomposition delivers exactly one PR (option 3), and genuinely-separable work is split into issues at plan time (option 4).**

1. **One consolidated PR per decomposition.** Child implement stages push their commits to a **shared parent branch** and do **not** open their own PRs. After `children_settled{succeeded}`, the **parent** opens a single PR (one commit per sub-plan) against the base branch. The parent's review stage reconciles on that PR's merge through the existing ADR-031 `github_merge` reconciler — no child-merge tracking, no force-succeed, no PR-less resolution.
2. **Review size is governed by the plan, not by PR-splitting.** The operator bounds how large a change an agent may make at the **plan gate** (the decomposition + `scope.files` are visible before any code), and reviews the resulting PR **commit-by-commit** (one reviewable commit per sub-plan). "A large change is multiple commits in one PR" is the human-SDLC norm. An optional per-workflow **max-files/commits-per-PR** hard cap may be added later as belt-and-suspenders (not required now).
3. **Separable vs. coupled is a plan-time choice.** The plan agent proposes either "decompose into commits (one coupled change, one PR)" **or** "file separate issues X/Y/Z (N independent changes)"; the operator approves the split. Decomposition is for coupled work; truly separable work becomes separate 1-issue-1-PR runs.
4. **Stacked branches (option 2) and child merge-reconciliation (option 1) are explicitly not built.** Revisit stacked branches only if commit-level review in one PR proves too coarse, or when multiple reviewers make per-child granularity valuable.

## Consequences

- **#714 is re-scoped** from "resolve the decomposed parent's review without a PR on `children_settled`" to **"the parent opens the consolidated PR on `children_settled{succeeded}`; its review reconciles on that PR's merge."** The child-merge-tracking machinery (option 1) is not built; the merge reconciler is unchanged.
- **Fan-out change (`backend/internal/orchestrator` + runner):** child implement stages run with PR-opening suppressed (push commits to the shared parent branch, no PR); the orchestrator opens the parent PR after children settle. Children sharing one branch also fixes dependency-ordered execution — child N naturally sees child N−1's commits.
- **Partial failure:** if a child fails, the shared branch holds the prior children's commits but **no PR is opened** (the parent fails via the existing child-failed path) — no half-merged PR, no orphan to reconcile. Branch cleanup / redrive semantics are an implementation detail for #714.
- **ADR-031 unchanged.** This rides on `github_merge`-by-merge; the parent simply now *has* a PR to reconcile. The local loop stays production-faithful (review-on-PR → merge → resolve).
- **Operator experience:** 1 issue = 1 PR uniformly; review size is predictable from the approved plan; no N-way merge ordering.

## Open implementation questions (for #714)

- **Who opens the parent PR** — the backend/orchestrator after `children_settled`, or a final parent "assemble + open PR" step on the runner. (Leaning orchestrator-side, since `children_settled` is where the signal already lives.)
- **Shared-branch naming + base** (parent run's branch; base = the run's target branch).
- **Failed-child branch cleanup / redrive** interaction with the existing `/redrive` path (#698).

## Related

ADR-031 (#710, `github_merge` resolution — unchanged), #702 / #713 (`push_and_open_pr` default + installation attribution that surfaced this), **#714** (re-scoped implementation), decomposition fan-out (`backend/internal/orchestrator` child-run creation, `children_settled`), METHODOLOGY (autonomy / operator-control of change size).

Parent epic: #389
