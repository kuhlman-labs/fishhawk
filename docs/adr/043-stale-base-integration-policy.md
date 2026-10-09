---
id: ADR-043
title: "Stale-base integration policy for concurrent agent branches (runner merges base before PR; clean verdict-transfer vs conflict re-invoke+re-review; merge not rebase)"
status: accepted
date: 2026-06-22
issue: https://github.com/kuhlman-labs/fishhawk/issues/1293
supersedes: []
superseded_by: ["ADR-090"]
applies_to: []
---

# ADR-043: Stale-base integration policy for concurrent agent branches (runner merges base before PR; clean verdict-transfer vs conflict re-invoke+re-review; merge not rebase)

## Context

With a fleet of agents completing issues concurrently, run branches will routinely fall behind the default/merge base between run-start and PR-open/merge — stale base becomes the common case, not the exception. Today a stale base is handled poorly:

- **Conflict-on-push only:** the runner has `reinvokeOnBaseRebaseConflict` (#989, runner/cmd/fishhawk-runner/main.go ~1604) — a bounded stash-reapply that re-invokes the implement agent when the PR push FAILS due to a base-rebase conflict. #1218 (audit) + #1250 (supplemental re-review of the honored exemption delta) cover the gate visibility on that re-invoke path.
- **Gap — clean-but-stale:** when the branch is merely behind base with NO push conflict (main added orthogonal files), nothing integrates the moved base. The run ships a stale-base committed tree (`verified_tree`), and the runner's tree-vs-current-main scope check counts main's newly-merged files as PHANTOM DELETIONS — e.g. #1290/PR #1292 showed 9 deletions of the just-merged #820 files (16 staged vs 7 declared), which the implement review correctly rejected as a regression. GitHub's 3-dot PR diff is actually clean (merge-base predates the sibling), so it looks fine on GitHub but the run artifact is genuinely stale. Recovery today = cancel + fresh run, which does not scale to a fleet.

This policy must respect two invariants: **forward-gating (ADR-031)** — the implement review must be anchored to the tree that actually ships — and **sole-writer/vouch (ADR-035)** — writes to a run branch are attributable to the run.

## Options

- **(a) Status quo — restart per stale base.** Cancel the run, start fresh on current main. Correct but O(restarts) under concurrency; does not scale.
- **(b) Rebase the agent branch onto the new base.** Linear history, clean diff — but requires force-push, which rewrites the agent's vouched commit SHA (ADR-035 lineage churn) and complicates the vouch chain.
- **(c) Runner merges base into the agent branch before PR-open (RECOMMENDED).** No force-push; the agent's vouched commit SHA is preserved; the integration is a mechanical, runner-authored merge commit. Generalizes the existing #989 re-invoke from the conflict-only path to the clean path.
- **(d) GitHub merge queue only.** Serializes + re-tests at merge time — solves final-merge ordering, but NOT the review/verify-saw-a-stale-base gate problem (the review still ran against the stale tree). Complementary, not a substitute.

The load-bearing nuance for (b)/(c): the merge outcome determines whether the existing review verdict still holds.
- **Clean merge + agent's effective diff unchanged + `verify` still green on the merged tree** → the review verdict TRANSFERS; just open the PR. No restart, no re-review. (This is the #1290 case and the common case.)
- **Conflict, OR a clean merge that breaks the build (main changed a symbol the agent depends on in another file — no git conflict, but the merged tree won't compile)** → re-invoke the implement agent to integrate + re-verify, and RE-REVIEW the resolved delta (the #1218/#1250 supplemental-re-review mechanism generalizes here). A silent auto-merge here would ship an unreviewed/unbuilt integration, violating forward-gating.

## Options considered → Recommendation

Adopt **(c) + (d)**:
1. Runner detects "behind base" before PR-open (it already diffs against current main) and runs `git merge origin/<base>` into the agent branch — merge, NOT rebase (preserve the vouched SHA; no force-push).
2. ALWAYS re-run `verify` on the merged tree. Transfer the existing verdict ONLY when the merge is clean AND the agent's effective diff is unchanged AND verify stays green; otherwise re-invoke + re-review the delta.
3. Attribute/vouch the mechanical merge commit as a runner-authored integration, distinct from agent work, so ADR-035 lineage stays honest.
4. Add a GitHub **merge queue** (human-led; branch-protection/repo-settings) so the FINAL merge is serialized and re-tested against the then-current base — covering the tail where the base moves again between PR-open and merge.

## Decision

### REVISION 2 — 2026-06-22 (final): provider-agnostic partial fix, NO merge ownership

Planning the merge gate (#1294) surfaced a second foundational fact: **Fishhawk does not perform merges.** The merge is external (operator/forge auto-merge); the backend's merge reconciler only OBSERVES the merged PR (webhook/60s poll) and resolves the run (`mergereconciler.go` → `ResolveReviewFromPollState`). There is no backend merge step, no provider `Merge` abstraction (only `GitHubAPI.MergeBranch` for fan-in), and no merge serialization. A true "re-integrate AT merge time" gate would therefore require Fishhawk to TAKE OVER performing the merge (a new provider-agnostic `Merger` + serialization + merge-correctness ownership) — a major capability the operator declined for now.

**Final decision (operator, 2026-06-22): the provider-agnostic PARTIAL fix, without merge ownership.**
1. **3-dot (merge-base) comparison** at the pre-merge gate that produced #1290's phantom deletions — compare the run's CONTRIBUTION against the merge-base, not current origin/main (2-dot), so a moved-but-orthogonal base never reads as deletions. The runner already integrates the current base at push time via `FreshFetchBase` (commit.go:505-545), and the real GitHub merge is already 3-dot, so this aligns the gate with reality.
2. **Tighten the push-time fresh-base reapply window** (best-effort) so the base is as current as possible at push.
3. **Accept the residual post-push semantic-staleness race as a documented known limitation** (the base advancing AFTER push such that the run's changes don't compose with new main) — rare, and a resulting main-break is detectable. Revisit "Fishhawk owns the merge" only if this proves material in practice.

Merge ownership (the full gate) and a forge merge queue are both explicitly OUT. Implementation: #1294 (re-scoped to the partial fix). This supersedes Revision 1's Fishhawk-native-merge-gate decision below.


**REVISED 2026-06-22 — supersedes the earlier same-day ratification.** The original ratified decision (runner `git merge --no-ff origin/<base>` before PR-open, "merge not rebase to preserve the agent's vouched commit SHA", plus a GitHub merge queue) was **wrong-premised**, surfaced by the #1294 implement agent reading primary sources and correctly declining to ship it (great dogfood outcome):

- The runner has **no agent commit with a vouched SHA to merge into** — the agent's edits are uncommitted working-tree changes, committed once at push time.
- `CommitAndPush` builds a **single scope-staged commit** and cannot push a two-parent merge without restructuring the #960/#980/#1151/ADR-035 gates.
- `FreshFetchBase` (runner/internal/gitops/commit.go:505-545, #861/ADR-035) **already integrates a moved base at push time** via a fresh-base stash→fetch→checkout→pop reapply — so "merge not rebase / preserve the agent SHA" both contradicts and duplicates existing behavior.
- **Platform merge queues are out** — Fishhawk is provider-agnostic (may not be GitHub; a queue may not be enabled), so option (d) is rejected.

**Adopted: a provider-agnostic, Fishhawk-owned merge gate.** The residual real gap is the **post-push race** (the base advances between a run's push and its merge) plus **semantic staleness** (the run's changes may not compose with the new base); a plain forge merge can land a stale/broken tree even on GitHub without a queue. Fishhawk owns the fix end to end:

1. **Serialize merges per repo in the backend** — Fishhawk's own merge queue/lock, NOT a forge feature.
2. **Re-integrate at merge time** against the freshly-fetched current base using the runner's EXISTING fresh-base reapply (a single fresh commit on current base — NOT a two-parent merge commit), and **re-verify** the integrated tree.
3. **Verdict transfers** when the reapply is clean and verify is green; a reapply **conflict OR a merged-tree build/test failure → re-invoke + re-review** the delta (generalized #989 / #1218 / #1250). Forward-gating (ADR-031) preserved: the tree that ships is the one that verified.
4. **Compare scope / verified-tree against the merge-base (3-dot), not current main (2-dot)**, so a moved base never reads as phantom deletions (the #1290 symptom).

merge-vs-rebase is moot: there is no agent commit history to preserve; the runner reapplies a single scope-staged commit. ADR-035 sole-writer stays honest because the reapplied commit is the run's own scope-staged commit, re-verified at merge time.

## Consequences

- The fix is **provider-agnostic** — works on any forge and with no platform merge-serialization, the hard requirement that killed the original merge-queue plan.
- Closes the #1290 post-push-race / phantom-deletions class without restart-per-stale-base and without push-path two-parent-merge surgery.
- New surface: a backend per-repo merge serializer + a merge-time re-integrate-and-re-verify step; the 3-dot comparison is a smaller, independently-shippable first slice.
- Re-review fires only when the re-integration changes the effective tree (conflict/build-break), reusing the #989/#1218/#1250 substrate; the common clean case transfers the verdict.
- Implementation: **#1294 repurposed** to the Fishhawk-native merge gate (+ 3-dot comparison); **#1295 (GitHub merge queue) closed as non-viable** under the provider-agnostic constraint. Cross-references ADR-031 (forward-gating), ADR-035 (sole-writer), #861 (FreshFetchBase), #989/#1218/#1250 (re-invoke/re-review).
