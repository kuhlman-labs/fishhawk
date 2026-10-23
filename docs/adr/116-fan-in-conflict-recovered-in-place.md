---
id: ADR-116
title: "A slice conflict at fan-in parks and is recovered in place (conflict probe, mechanical union, then a hunk-confined resolution pass, then a scoped parent fix-up, with a verified operator resolution always available), never by restarting the whole run"
status: accepted
date: 2026-10-23
issue: https://github.com/kuhlman-labs/fishhawk/issues/4680
supersedes: []
superseded_by: []
amends: ["ADR-041", "ADR-115"]
applies_to: []
---

# ADR-116: A slice conflict at fan-in parks and is recovered in place (conflict probe, mechanical union, then a hunk-confined resolution pass, then a scoped parent fix-up, with a verified operator resolution always available), never by restarting the whole run

## Status

**ACCEPTED (v2, ratified by the Captain 2026-10-23).**
- **Origin:** filed by the first officer on 2026-10-23 at the Captain's direction, from #4668 gap 2.
- **Review history:** v1 was reviewed by claude-fable-5-1 and claude-opus-5-5. Both returned approve_with_concerns, with 5 and 4 HIGH findings respectively, which converge. v2 folds in every finding.
- **Freeze:** tracker-only until the 2026-11-01 catch-rate re-pin; the in-repo record follows the upkeep rule.
- **Prevention** (#4668 gaps 1 and 3) is #4679 and needs no ADR.

## Context

A decomposed run fans its slice branches into one consolidated branch, `fishhawk/run-<parent>` (ADR-041, E24.2). `IntegrateSlices` merges the slices one at a time through the forge merges API and records each merge commit (`integration_commit_recorded`). It STOPS at the first conflicting slice:
- lower-index slices are already on the consolidated branch;
- higher-index slices have not been attempted.

The conflict surfaces at two sites:
- **All children terminal** (`consolidate.go`, and the child-completion sweeper's all-terminal arm). The parent implement FAILS category B. The parent's resume is refused (`resume_unsupported_decomposed`), and the child's redrive is refused (`recovery_not_eligible`, because the child succeeded). `fanInRemedySlice` in the gate view names only a full restart: re-plan, plus every slice again.
- **Between waves** (the sweeper's between-wave arm, `/integrate-wave`). The parent stays parked in `awaiting_children` behind the same audit row, the dependent wave is blocked, and there is no resolution path.

On #4626's first run (917c89f9), that cost about 3 hours of agent time to reconcile one README bullet. Both slices' work was correct; only the textual merge failed.

What fishhawkd knows at conflict time is limited:
- GitHub answers the merge with a 409 that lists no paths.
- The base attribution compares a slice against the BASE only, so a slice-vs-slice conflict records `no_base_overlap` with no conflicting paths.
- Computing the conflict set needs a local three-way merge, which belongs in a RUNNER pass (ADR-063), never in fishhawkd.

ADR-041 decision 4 promised that a fan-in conflict would be "recoverable … reuse the category-B in-place recovery shape". That was never delivered. ADR-115 D1 concerns capacity waits, not textual conflicts. ADR-115 D4 REMOVES the mid-stage rebase-conflict arm rather than rerouting it. The nearest precedents are:
- ADR-096 D4 (#4343): `base_fold_conflict` degrades to the merge-time #3202 conflict-resolution pass;
- #4079: the `SliceHeadMissing` park in `awaiting_children`.

## Options

- **A. Status quo.** Restart on any fan-in conflict. Rejected: the cost grows with slice count and is unrelated to the size of the conflict.
- **B. In-place recovery ladder with a new park state.** Rejected: a new stage state costs a CHECK migration, transitions, settled checks, the `children_status` / `next_actions` / `await_children` classifiers, the restart blockers and the cancel cascade. It also collides with the gate view's existing `fan_in_conflict` block name.
- **B′. In-place recovery ladder, parked in `awaiting_children` with a durable conflict marker (decided).**
- **C. Operator resolution only.** Rejected as the sole path: it needs a human for every conflict.

## Recommendation (decided)

**Option B′.**

**D1. Park, don't fail (amends ADR-041 decision 4).**
- At either conflict site, the parent implement stays in `awaiting_children`.
- A durable `fan_in_conflict_parked` row records the conflict:
  - the slice index and child run;
  - `consolidated_tip_sha` (the tip at conflict);
  - `slice_head_sha`;
  - the conflict sequence.
- Correct slice work is kept: no rung re-plans, and no rung re-runs a slice that merged.
- While a conflict is parked, the sweeper, `/consolidate` and `/integrate-wave` MUST NOT integrate past it. An idempotent re-pass must not absorb an unvalidated push. Integration resumes only through D3's rungs or D3.4's verb.
- The park emits a page-class event and names the next rung in `next_actions`. A campaign item is paged, never left silently "running".

**D2. Conflict probe (runner).**
- A runner pass computes `git merge-tree --write-tree <consolidated_tip_sha> <slice_head_sha>`.
- It records on the park row:
  - the conflicted paths and hunks;
  - the BASELINE tree, conflict markers in place.
- **The resolution bound** for every rung is:
  - the conflicting slices' plan `scope.files`;
  - ∪ their APPROVED child scope amendments (the #4338 union);
  - ∪ the probed conflict paths.
- `forbidden_paths` and `agent_instruction_paths` apply as at the post-implement gate.

**D3. The ladder.** A rung ADVANCES to the next only when it produces NO commit:
- it does not qualify;
- its gate refuses;
- its budget is exhausted;
- or it fails before pushing.

A rung that pushes a commit ends the ladder and goes to D4.

1. **Mechanical union.**
   - It runs in a runner pass.
   - Qualifying paths: insert-only conflicts on paths the repository declares as union, and regeneration of paths it declares as generated. These are ADR-115 D5's declarations, read from the run's PINNED BASE and never from a slice head.
   - A repository with no declarations SKIPS this rung, and the skip is recorded (ADR-110 rule 4). It is never a refusal.
   - ADR-115 D5's approval-transfer clauses do not apply: no approval exists before the PR.
2. **Resolution pass.**
   - It reuses the RUNNER's conflict-resolution gate unchanged (`runConflictResolutionStage` / `conflictresolve`), with HEAD = `consolidated_tip_sha` and MERGE_HEAD = `slice_head_sha`.
   - The agent edits only inside git's conflicted hunks. Any other working-tree change refuses the pass (the existing hunk-confinement contract).
   - New on the backend:
     - a fan-in trigger site;
     - an admission arm for the parked parent;
     - its OWN counter category, `stage_fan_in_resolution_triggered`, with a ceiling of 1 per (parent, slice index, conflict sequence).
   - It never spends the implement stage's #3202 merge-time ceiling.
3. **Scoped parent fix-up (replaces a child re-run).**
   - The fan-in first SKIPS the conflicting slice and integrates every remaining slice: skip-and-continue, a stated change to stop-at-first-conflict for this rung only.
   - A single parent-level, scoped fix-up implement then re-applies the conflicting slice's approved change onto the consolidated tip.
   - That slice's approved plan slice, conditions and amended criteria are re-served, and the fix-up is bounded by D2's bound.
   - The succeeded child is NOT re-opened, and its slice branch is not rewritten.
4. **Operator resolution (always available at a parked conflict).** The operator pushes a resolution commit and calls the resume arm of `fishhawk_consolidate_slices`. The arm:
   - is operator-token-only and refuses a run-bound token;
   - takes the full 40-hex commit SHA and the expected prior tip;
   - requires the commit's parents to be EXACTLY (`consolidated_tip_sha`, `slice_head_sha`) as recorded on the park row;
   - checks in a runner pass that every non-conflicted path equals the three-way merge result, refusing any edit outside the conflict;
   - enforces D2's bound;
   - continues only if the consolidated tip EQUALS the verified SHA at continue time (compare-and-swap). A moved tip refuses `fan_in_head_moved`.

   It is refused while an automatic rung is live. Authority comes from the audited verb: no signature is required, and the forge commit's author and committer are recorded. The operator becomes a change author for ADR-112 R1(b) and `not: [author]`, a single-maintainer residual stated in Consequences.

**D4. Re-gating is the same as for a clean fan-in.**
- **Ledger.** Every pushed resolution is recorded in a row that BOTH reported-head ledger readers union: ADR-035 lineage and auditcomplete rule 5 (the #3415 two-reader trap). The generic `vouch_commit` is not sufficient, because it verifies nothing.
- **Same integration record.** Every rung ends in the ordinary `slices_integrated` row covering EVERY child, so acceptance admission (#4080) and the fan-in verify see the same shape as a clean fan-in.
- **Same downstream gates.** The fan-in verify, consolidated reviews and acceptance run exactly as for a clean fan-in. A red fan-in verify routes to ADR-090 D6's fix-up, never back to the ladder.
- **Resolution diff.** It is diff(D2 baseline, resolved tree) restricted to the conflicted paths. Outside those paths the diff is ASSERTED empty, not merely displayed.
  - Until the 2026-11-01 re-pin, it is shown in the gate view and on the issue/PR thread only.
  - Rendering it into the review prompt is a later step: it touches a catch-rate-gate trigger path, so it needs eval evidence, a quarantine envelope and an injection-corpus containment class (E60.2).

**D5. Audit and visibility.**
- Categories, registered in `categories.go`, plus `activityCategories` and `docs/issue-comment-surfaces.md` where posted to the thread:
  - `fan_in_conflict_parked`;
  - `fan_in_conflict_probed`;
  - `fan_in_resolution_attempted` (rung, outcome);
  - `fan_in_resolution_recorded` (rung, SHA, and for D3.4 the operator identity and SHA).
- `fanInRemedySlice` and the failure-signature playbook name the ladder instead of a restart.

**D6. Drivers and durability.**
- Each automatic rung is a system-triggered stage named in `next_actions`. On the local runner, that is a host dispatch.
- Rung state lives in audit rows, never in in-memory sweeper state, so it survives a restart.
- The restart blockers count a live rung, and the cancel cascade reaches a live probe, pass or fix-up.

**D7. Portability (ADR-110).**
- The fan-in is GitHub-only today. A forge without slice integration answers `provider_unsupported`.
- The D3.2 budget is a product default with a workflow-v2 override.
- Evidence: a non-Fishhawk fixture drives a slice-vs-slice conflict through consolidate and the ladder (rule 3(a)).

**D8. Restart is the last resort.** If every automatic rung produces no commit and the operator declines D3.4, the operator may still cancel and restart.

**Not amended:** ADR-093 and ADR-112. The ladder completes before the consolidated PR opens and before any forge approval exists, so ADR-112 R1's chain is unaffected.

## Decision

**Accepted 2026-10-23** by the Captain (repository maintainer): **Option B′**, i.e. D1–D8 as stated in the Recommendation (v2). It amends ADR-041 decision 4 (a fan-in conflict parks and is recovered in place) and ADR-115 D5 (its declarations also apply at the fan-in). Prevention is #4679.

## Consequences

- **Gains.**
  - A fan-in conflict costs, in order: a probe and a hunk-confined agent pass (minutes); then one scoped parent fix-up; or an operator merge. It never costs a re-plan plus every slice.
  - The #4626 incident (a bullet rewrite, not an insertion) would have been resolved by D3.2 or D3.4. D3.1 would not have qualified: this repository does not declare structured READMEs as union, and D3.1 lands last.
- **Costs:**
  - the park marker and the integration hold at both conflict sites;
  - the runner conflict probe;
  - the D3.4 verification pass and its ledger row unioned by both readers;
  - the D3.2 backend trigger, admission arm and counter;
  - skip-and-continue integration plus the scoped parent fix-up (D3.3);
  - the audit categories, the gate view and thread rendering, and the playbook update;
  - campaign paging;
  - the restart-blocker and cancel-cascade coverage;
  - extending ADR-115 D5's declarations to the fan-in.
- **Residuals.**
  - A textually clean resolution can still be semantically wrong. That is what D4's verify, reviews and resolution diff exist to catch.
  - The operator resolver counts as a change author, so a single-maintainer repository relies on the ADR-112 voucher exception.
  - D3.1 waits for ADR-115 D5 and its measurement gate.
- **Implementation order:**
  0. #4679 (prevention), first or in parallel.
  1. D1 park and hold at both sites, D2 probe, D3.4 operator resume with its ledger row, D5 audit and gate-view/thread diff, and D6. This is the escape hatch.
  2. D3.2 resolution pass.
  3. D3.3 skip-and-continue plus the scoped parent fix-up.
  4. D3.1, once ADR-115 D5 lands.
  5. The review-prompt rendering of the resolution diff, after the 2026-11-01 re-pin, with eval evidence.
- **Record metadata.** amends: ["ADR-041", "ADR-115"]; supersedes: []; applies_to: []
- **Related:** ADR-035, ADR-041, ADR-063, ADR-090 (D5, D6), ADR-096 D4, ADR-110, ADR-112, ADR-115 (D4, D5), #3202, #3415, #4079, #4080, #4338, #4343, #4626, #4668, #4679.
