---
id: ADR-041
title: "Per-child branch isolation + fan-in integration for parallel decomposition"
status: accepted
date: 2026-06-18
issue: https://github.com/kuhlman-labs/fishhawk/issues/1140
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-041: Per-child branch isolation + fan-in integration for parallel decomposition

## Context

Decomposition (ADR-025) mints one child run per slice (`fanoutIfDecomposed`, `backend/internal/orchestrator/orchestrator.go:574`), but children run **serially**: every child pushes to a single shared branch `fishhawk/run-<parent>` with `RebaseFromRemote` + `ForceWithLease` (`runner/cmd/fishhawk-runner/main.go:3447`, `:3822`), and on the local-runner backend the operator drives them one at a time through the synchronous `fishhawk_run_stage` MCP tool. To scale implement agents dynamically — running slices concurrently — the shared-branch serialization (ADR-032) and the absence of a parallel dispatch/await primitive must be resolved. The run/stage data model and the child-completion sweeper (`backend/internal/childcompletion`) already treat each child as an independent run consolidated in any terminal order, so the core lifecycle is not the blocker; the git integration model and dispatch path are.

## Options

**A. Per-child slice branches + fan-in merge.** Each child is the sole writer of `fishhawk/run-<parent>/slice-<n>`; a new orchestrator fan-in step sequentially integrates succeeded slices onto the consolidated branch `fishhawk/run-<parent>`. True parallelism; new fan-in + conflict logic. Satisfies ADR-035 sole-writer lineage cleanly.

**B. Shared branch + serialized push-retry.** Keep one branch; children run agents in parallel but the runner retries on `--force-with-lease` rejection (fetch/rebase/re-push). Minimal new code; fragile for overlapping slices and mid-run rebases of uncommitted agent work; integration races remain.

**C. Status quo (serial children).** No parallelism; rejected against the goal.

## Recommendation

**Option A — per-child slice branches + fan-in merge.** It is the only option that delivers genuine parallel agent execution while preserving ADR-035's sole-writer invariant (each child owns exactly one branch). The fan-in step localizes integration risk to one deterministic, auditable place rather than scattering it across concurrent rebases.

## Decision

**Ratified 2026-06-18: adopt Option A** (per-child slice branches + fan-in merge). Operator-ratified for epic [E24] #1139. The pinned points:

1. **Slice-branch naming:** each child is the sole writer of `fishhawk/run-<parent>/slice-<n>`, where `n` is the child's sub_plan index (threaded from the orchestrator's sub_plan ordering into the child runner config).
2. **Fan-in ordering:** succeeded slice branches are integrated in **ascending sub_plan index** order (deterministic).
3. **Integration mechanism:** **sequential `git merge`** of each succeeded slice branch onto the consolidated branch `fishhawk/run-<parent>` (NOT cherry-pick) — robust for multi-commit slices, clean per-slice provenance, conflicts surface as merge conflicts.
4. **Conflict semantics:** a fan-in merge conflict fails the parent implement stage **recoverable** with a dedicated audit kind + `next_action` — **never a silent drop**. Reuse the category-B in-place recovery shape (#1081) where possible.
5. **Consolidated-PR head:** produced by the fan-in onto `fishhawk/run-<parent>`; `maybeOpenConsolidatedPR` is unchanged downstream and opens the single consolidated PR from the integrated branch.
6. **Concurrency cap:** max parallel children configured per-run **and** global (backend config), with an optional additive `decomposition.max_parallel` workflow-spec knob (workflow-v0.x, declared soak). Consumed by E24.3/E24.4/E24.5; defined in E24.6 (#1146).
7. **Budget/cost aggregation:** budget/cost accounting sums across concurrently in-flight children so a wide fan-out cannot bypass the spend alert; defined in E24.6 (#1146).


## Consequences

Revises **ADR-032** (#714): the consolidated PR head is now produced by a fan-in integration of per-slice branches, not by children rebasing onto a shared branch. Cross-reference this ADR in ADR-032's Consequences. Extends **ADR-035** (#857): the sole-writer invariant now applies per slice branch; the parent owns the consolidated branch only via the fan-in step. New failure mode: fan-in conflict (recoverable). Cost/budget exposure rises with concurrency, mitigated by the cap (guardrails issue). Implementation tracked under epic **[E24] #1139**.
