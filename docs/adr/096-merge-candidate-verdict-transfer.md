---
id: ADR-096
title: "Merge-candidate verdict transfer across an independent base delta, plus a single verification point for decomposed parents (amends ADR-090)"
status: accepted
date: 2026-10-11
issue: https://github.com/kuhlman-labs/fishhawk/issues/4290
supersedes: ["ADR-090"]
superseded_by: []
applies_to: []
---

# ADR-096: Merge-candidate verdict transfer across an independent base delta, plus a single verification point for decomposed parents (amends ADR-090)

## Context

ADR-090 D2 requires a passing merge-candidate verify for exactly the live head SHA whenever Fishhawk produced that head by a base advance, a conflict-resolution push or a fan-in integration. ADR-092 serializes those passes, one live per base, and ADR-093 turns the serializer into a merge queue. All three assume that **every base advance invalidates the candidate's verdict**.

Campaign 9275cbb2 (2026-10-10 to 11) measured the cost of that assumption:

| Measure | Value |
|---|---|
| Per-PR merge path | implement (own full verify) → acceptance (one shared preview, serialized) → reviews → fan-in pass (decomposed only) → rebase → merge-candidate pass → re-approve → merge |
| One `scripts/test verify` pass | ~15–20 min |
| Throughput | ~1.2 merges/hour |

- **Every merge puts every other open PR behind.** Each advance costs that PR a full pass, so N PRs cost at least N serialized passes. Every operator or docs merge in between costs one more cycle; for example, merging ADR record #4244 cost #4235 a full pass.
- **Most re-passes verified a combination that could not interact.** The base delta was docs-only, or confined to packages outside the PR's dependency closure.
- **Decomposed parents paid twice.** Each ran a fan-in pass on the consolidated head and then, because main had moved by merge time, a second full base-advance pass. Examples: #4200, #4190, #4028, #4086.
- **Two flaky red passes** (#4233, #4028) each cost a fix-up pass and a re-pass. #4229 already tracks a single-test retry.

## Options

- **A. Status quo.** Re-verify after every advance. Simple and maximally safe, but throughput is bounded by verify time × merges, and any unrelated merge invalidates everything.
- **B. Verdict transfer across an independent base delta, plus a single verification point (RECOMMENDED).**
  - A passing verdict for head H on base B carries to head H' = H rebased onto B'. This applies only when the delta B..B' is provably independent of the PR's change set: no interaction surface, defined conservatively below.
  - Decomposed parents integrate onto the current base tip, so the fan-in pass doubles as the merge-candidate pass.
- **C. Speculative batching** (ADR-093 Option D). This raises throughput when passes are slow, but adds combined-tree construction and bisection. It is orthogonal to B, and ADR-093's Q4 trigger still governs it.
- **D. Trust CI only after merge.** Merge on a clean rebase and rely on post-merge CI. This reverses ADR-090's core finding (#4018, red main), so it is rejected.

## Recommendation

**Option B.** It removes the re-passes that cannot find anything while keeping a full pass wherever the delta could interact, and it composes with ADR-092 and ADR-093 unchanged. Proposed decision points:

**D1 Verdict transfer on an independent base delta.** A head H' produced by a CLEAN base advance of a passed head H inherits H's `passed` verdict without a new pass when ALL of the following hold, computed by the backend at advance time:
- **(a) Clean advance.** The advance was a conflict-free merge or rebase, with no conflict-resolution push.
- **(b) No global surface in the delta.** No file in the base delta B..B' matches the global-surface set (D3).
- **(c) Disjoint Go reach.** In the full workspace import graph at B', the reverse-dependency closure of every Go package the delta touches does not intersect the reverse-dependency closure of every Go package the PR's diff touches.
  - Test packages are included, and so is every package in any module whose go.mod or go.work changed.
  - Any non-Go file the delta touches that lies under a Go package directory counts as touching that package. This covers testdata, embedded assets and fixtures.
- **(d) Disjoint non-Go paths.** The PR's own diff and the base delta touch no common path prefix among the non-Go "shared artifact" directories in D3.

When any condition fails or cannot be computed, ADR-090 D2 applies unchanged: a full pass. Fail closed means "verify", never "transfer".

**D2 Audit and visibility.**
- A transfer appends `merge_candidate_verdict_transferred` {`from_head`, `to_head`, `from_base`, `to_base`, `delta_paths_count`, `reason`}, registered in `categories.go`.
- The merge gate (D2 of ADR-090) treats a transferred `passed` exactly like a recorded one.
- The gate view shows which verdict a head carries and where it came from.
- A refused transfer records the failing condition as `reason`, e.g. `global_surface:docs/spec/workflow-v2.schema.json` or `reach_overlap:backend/internal/server`, so operators can see why a pass was required.

**D3 Global surface and shared artifacts (conservative, enumerated).** Any delta touching these forces a full pass:
- `go.work`, any `go.mod`/`go.sum`;
- `scripts/**`, `.golangci.yml`, `deploy/gate-image/**`;
- `.fishhawk/**`;
- `docs/spec/**` (schemas plus mirrors);
- `docs/api/**`, `site/**`, `docs/adr/**`, `docs/ARCHITECTURE.md`. Global verify legs read these: site IA, site voice, ADR index, generated reference, doc line budget.
- `backend/internal/postgres/migrations/**`, because migration numbering and ordering collide across PRs;
- `backend/internal/audit/categories.go`;
- `testdata/wire/**`.

The set lives in one versioned place, and the verify-gate harness pins that every global verify leg's inputs are covered by it. An unlisted global leg is a test failure, not a silent transfer. The set may only grow by ADR amendment or a reviewed change to that file.

**D4 Single verification point for decomposed parents.**
- Fan-in integration builds the consolidated head on the **current base tip**, not on the original branch point. The fan-in pass then verifies the actual merge candidate.
- Later base advances follow D1: they either transfer or trigger one base-advance pass.
- A parent therefore pays one full pass in the common case instead of two.
- ADR-090 D5's hold semantics are unchanged.

**D5 Interaction with ADR-092 / ADR-093.**
- A transferred verdict counts as `passed` for queue admission and merge-on-pass.
- The head of line whose advance transfers proceeds straight to merge, with no verify slot used.
- The ADR-093 Q4 batching trigger is re-measured after this ships, since transfer should cut queue wait substantially.

**D6 Out of scope here, tracked separately.**
- Single-test flake retry inside a pass: #4229, with #4250 to make the failing test name visible.
- Per-run previews to un-serialize acceptance: #4063, ADR-094 `preview-target`.
- Acceptance continues to run once per content change, and is not re-run on a pure base advance (unchanged).

**Ratification questions:**
- **Q1** Is the D1 rule safe enough to replace ADR-090 D2's re-verify-every-advance for independent deltas, given the residual (D3 / Consequences)?
- **Q2** Should the D3 global-surface set be enumerated as above, and pinned by a harness that fails when a new global verify leg's inputs are uncovered?
- **Q3** Should fan-in integrate onto the current base tip (D4), accepting that a slice integration can now conflict with main at fan-in time rather than at the merge pass?
- **Q4** Does this partially supersede ADR-090 D2 (for independent deltas) and D5 (fan-in base)?

## Decision

**Accepted (2026-10-11), ratified by the Captain as recommended.** Adopt Option B with D1–D6 as written.

- **Q1:** yes. D1 transfer replaces re-verify for independent deltas. The stated residual is accepted, with post-merge CI on main as the backstop.
- **Q2:** yes. The global-surface set is enumerated in one versioned place and pinned by a harness that fails when a global verify leg's inputs are uncovered.
- **Q3:** yes. Fan-in integrates onto the current base tip, accepting earlier conflict surfacing.
- **Q4:** yes. This partially supersedes ADR-090 D2 (independent deltas only) and D5 (fan-in base).

Sequencing: this is the next batch after campaign 9275cbb2 and the reviewer-persona work.

## Consequences

- **Throughput.**
  - Unrelated merges stop costing every open PR a full pass. In campaign 9275cbb2 that covers most re-passes: docs-only deltas, disjoint packages and operator ADR records.
  - Decomposed parents pay one pass instead of two.
  - Queue wait falls by about the number of avoided passes times 15–20 min.
- **Correctness residual.**
  - D1 relies on the Go import graph plus an enumerated global set. Interactions it cannot see could pass undetected:
    - runtime coupling through the database schema (mitigated by migrations being global);
    - shared external services;
    - reflection or `go:linkname`;
    - test-order or global-state coupling across packages that do not import each other.
  - Post-merge CI on main remains the backstop. A red main traced to a transferred verdict is a D3 coverage bug, so add the missed path to the set.
- **Auditability.** Every transfer and every refused transfer is a chained audit row with its reason.
- **Supersession.** Partial supersession of ADR-090 D2 (independent deltas) and D5 (fan-in base). ADR-092 and ADR-093 are unchanged in mechanism.
- **Implementation.** Three slices:
  1. the backend delta classifier and reach computation, with the global-surface set and harness pin;
  2. the transfer at advance time plus audit, gate view and queue integration;
  3. fan-in onto the current base tip.

## Relations

Amends ADR-090 (#4018 lineage). Related: ADR-092, ADR-093, ADR-094 (#4239), ADR-043 (verdict transfer principle), #4229, #4250, #4063.
