---
id: ADR-090
title: "Verify the merge candidate: a Fishhawk-dispatched merge requires an up-to-date head, and a base-advanced head must pass the project's declared verify command in a runner verify-only pass"
status: accepted
date: 2026-10-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/4170
supersedes: ["ADR-043"]
superseded_by: []
applies_to: []
---

# ADR-090: Verify the merge candidate: a Fishhawk-dispatched merge requires an up-to-date head, and a base-advanced head must pass the project's declared verify command in a runner verify-only pass

## Context

Concurrent runs collide on files their changes both touch. Each branch is gated in isolation, so each passes. The tree that actually lands is `head ⊕ base`, and that tree has not been gated. Fishhawk cannot know a project's semantics, but the project's own tests catch these collisions when they run against the tree that will land.

Recorded instances:
- **Sequence-number clash.** #3964 planned migration `0096_*` while a parallel run (#3771) landed its own `0096_*` first. A duplicate-migration-prefix test caught it, but only once someone ran it on the combined tree (#4018).
- **Fan-in registry collisions.** Fan-in branches collided on shared registries (`audit/categories.go`, `docs/api`) (#4018).
- **Ungated consolidated fan-in tree.** A consolidated fan-in tree reached a PR without ever being verified as a whole (#3983). Every decomposed parent drew an "unverified consolidated fan-in tree" review concern. The operator closed it by hand with a scratch-worktree build plus race tests, as on #2135 and #3964 (#4018, folded section).
- **Hand-built merge candidates.** On 2026-10-08 an overseer session built and tested the merged tree by hand before each merge: #4067 after #3939 merged, then #3939 and #3792 (#4120). ADR-089 records the same work ("consolidated fan-in builds") as one of the things the overseer seat caught that the loop did not.

The pattern recurs across campaigns and today depends on a second operator session doing `head ⊕ main` checks by hand.

**Prior decision this revisits: ADR-043 (#1293), Revision 2.**
- Revision 2 rests on the premise that **"Fishhawk does not perform merges."** On that premise it adopted:
  - the 3-dot comparison;
  - the push-time `FreshFetchBase` reapply;
  - item 3: "accept the residual post-push semantic-staleness race as a documented known limitation … Revisit 'Fishhawk owns the merge' only if this proves material in practice."
- Revision 2 also explicitly superseded Revision 1, which proposed a Fishhawk-native merge gate: a per-repo serializer plus re-integrate and re-verify at merge time.

**The premise no longer holds on `main` (verified 2026-10-09).**
- `POST /v0/runs/{id}/merge` (`backend/internal/server/merge_run.go`, E48.7 / #1954, MCP `fishhawk_merge_run`) records a `merge_verdict_recorded` row and queues a squash merge.
- The delegated `may_merge` arm (`dispatchAcceptanceGatedMerge`, `autodrive.go`) dispatches through the same seam.
- That seam is `ForgeMerger` (`forge_merger.go`, E45.47 / #3464). It routes GitHub and GitLab runs.
- The orchestrator's `auto_merge` stage (`dispatchAutoMergeStage` → `enableAutoMerge`) is a third dispatch path. It defers to forge branch protection, as ADR-017 decided for `routine_change`.
- `fishhawk_rebase_run_branch` (`rebase_branch.go`, E64.23 / #3125) advances a run branch with a forge-side merge of the base into the branch. It decides "already contains base" with a behind-probe, `CompareCommits(base=<PR head>, head=<base ref>)`, which fails closed on error.
- On a conflict, a bounded agent conflict-resolution pass re-opens the implement stage (E64.62 / #3202).
- The only pre-merge forge guard in `merge_run` is the #3109 conflict guard (`prMergeConflicting`). It is best-effort and fail-open: it refuses only on an explicit `dirty` / `mergeable=false` and proceeds on every read error.
- Nothing refuses a PR that is merely behind its base.
- Nothing re-verifies a head produced by a base advance, a conflict resolution or a fan-in integration.
- No per-repo merge serializer exists.

So Fishhawk now owns merge dispatch, base advance and conflict resolution, but not merge-candidate correctness. The residual ADR-043 accepted has proved material.

Invariants this must respect:
- **ADR-031:** the review gate succeeds only on verified landing.
- **ADR-035:** declared base and sole-writer lineage.
- **ADR-063:** untrusted gate commands run in the isolated gate.
- **ADR-032 / ADR-041:** one consolidated PR, produced by fan-in onto `fishhawk/run-<parent>`.

## Options

- **A. Status quo.** Keep the ADR-043 residual and leave the combined-tree check to operator discipline, or to an ADR-089 first-officer verification job.
  - No build cost.
  - Correctness depends on a human or a second session remembering to test `head ⊕ main` before each merge, which is the recurring manual cost described above.
- **B. Defer to the forge.** Rely on GitHub's "require branches to be up to date" branch-protection rule plus required CI (the ADR-017 posture).
  - It closes the up-to-date half for any merge path, including merges clicked outside Fishhawk.
  - It is GitHub-specific, which is the provider-agnostic objection under which ADR-043 Rev 1 rejected forge merge queues.
  - A red result does not route back into the run as a fix-up. A required check simply blocks.
  - It cannot gate a consolidated fan-in tree before the parent's PR exists.
  - It depends on how each customer configures branch protection.
- **C. Revive ADR-043 Rev 1 in full:** a backend per-repo merge serializer, plus merge-time re-integrate and re-verify, plus re-review.
  - It closes the post-push race completely.
  - It adds a new serialization primitive and merge-correctness ownership that the operator declined in Rev 2.
  - The re-review leg duplicates review cost on every clean base advance.
- **D. Gate the merge candidate at the dispatch paths Fishhawk owns (RECOMMENDED):**
  - an up-to-date requirement, using the existing behind-probe;
  - a targeted re-verify of heads produced by Fishhawk's own base-advance, conflict-resolution and fan-in writes, using the project's declared verify command in a runner pass with no agent;
  - a red result routed to the existing bounded fix-up.

  It relies only on git ancestry and the project's own verify command, so it is language-agnostic. It reuses the #3125 / #3202 base-advance machinery and the ADR-063 gate. It does not take a merge lock.

## Recommendation

**Option D**, with the decision points below. B is complementary, not a substitute: on GitHub, enabling strict up-to-date branch protection narrows the gate-to-merge window that D leaves open (see Consequences). It does so under ADR-017's existing deferral, and Fishhawk does not need to require it.

Relationship to ADR-043: this ADR **partially supersedes ADR-043 Revision 2, item 3** (the accepted post-push semantic-staleness residual), for merges Fishhawk dispatches.
- Revision 2 items 1–2 (3-dot comparison; push-time fresh-base reapply) stand unchanged.
- Revision 1's per-repo merge serializer is **not** revived.
- ADR-043 would stay `accepted` and gain `superseded_by` for this ADR, following the ADR-061/ADR-058 and ADR-076/ADR-033 partial-supersession precedent in `docs/adr/README.md`.

The opt-in `sequences:` allocator that #4018 also proposes is **recorded outside this ADR** (Q2):
- It is an additive, opt-in workflow-v2 field, already governed by the schema-change checklist and ADR-026's additive-within-major discipline.
- It is advisory input to the planner and prevents nothing by itself.
- This ADR's correctness does not depend on it: the merge-candidate gate is what catches a sequence collision.

## Decision

**Accepted (2026-10-09). Ratified by Brett (repository maintainer) on the explicit instruction "ratify as recommended".** The agent records the maintainer's decision; it is not self-approval. The decision points were drafted by the overseer session from #4018's approved plan; the maintainer's rulings on the ratification questions are recorded at the end of this section and are binding on the implementation.

**This ADR partially supersedes ADR-043** (Revision 2, item 3 only: the accepted post-push semantic-staleness residual), for merges Fishhawk dispatches. ADR-043 Revision 2 items 1–2 stand; its Revision 1 per-repo merge serializer is not revived.

**D1 Up-to-date requirement.**
- Before Fishhawk dispatches a merge, the PR head must contain the current base tip.
- The probe is the one `rebase_branch.go` already uses: `CompareCommits(base=<live PR head>, head=<base ref>)`. A non-empty result means behind. Only `len > 0` matters, so the compare API's commit cap is irrelevant.
- A behind PR is refused with 409 `merge_base_behind` {`head_sha`, `base_ref`, `behind_by`}, naming `fishhawk_rebase_run_branch` as the next step.
- The check runs on both Fishhawk-owned dispatch paths: `POST /v0/runs/{id}/merge` and the delegated `may_merge` arm.
  - On the merge endpoint it runs after the #3109 conflict guard and before the `merge_verdict_recorded` append, so a merge that cannot land records no verdict.
  - The delegated arm reports an observe-only outcome naming the rebase step. It does **not** auto-rebase.
- A PR the forge reports as already merged skips the gate, so the #3622 already-merged resume still answers 200.
- The merge reconciler only observes merges the forge already performed, so it has nothing to refuse. The "(and the merge reconciler)" wording in #4018 is realized on the dispatch paths.

**D2 Targeted verify requirement.**
- A live head needs a passing merge-candidate verify for **exactly that head SHA** when the head was produced by any of these Fishhawk writes:
  - a base advance: `branch_rebased` where a merge was performed;
  - a conflict-resolution push: `conflict_resolution_pushed`;
  - a fan-in integration: `integration_commit_recorded` / `slices_integrated` on a parent with decomposition children.
- Implement and fix-up heads are already gated on the committed tree by the runner and need nothing more.
- A universal "every live head must be gate-verified" rule is rejected. It would wedge existing vouch, park and held-commit flows.
- Refusal outcomes:
  - unverified or in flight → 409 `merge_candidate_unverified`;
  - failed → 409 `merge_candidate_verify_failed`.
- A chain read error is returned as an error, never read as "no requirement".

**D3 Runner verify-only merge-candidate pass.**
- **Trigger.** A durable `stage_merge_candidate_verify_triggered` entry re-opens the implement stage, mirroring the #3202 conflict-resolution pass. The pass keeps its **own** counter and **never reads or spends the fix-up budget**.
- **Execution.**
  - The runner fetches the run-branch tip without touching any checkout.
  - It requires the tip to equal the expected head. Otherwise it reports `not_executed` / `head_moved`.
  - It runs **only** the project's declared `executor.verify` command, in full form (no scoped packages), in the existing isolated committed-tree gate (ADR-063: container path, or the clone fallback).
- **No agent, no commit, no push.** It reports one `merge_candidate_verified` outcome: `passed` | `failed` | `not_executed`, with a bounded, pre-redacted output tail.
- **Idempotent per head.**
  - An unconsumed trigger for the same head returns that pass.
  - A recorded `passed` or `failed` result starts nothing new.
  - A `not_executed` result may be re-triggered.
- **Producers.**
  - `fishhawk_rebase_run_branch` triggers the pass after it performs a base merge.
  - On its already-up-to-date arm it re-triggers when the live head is an unverified base-advance head, for example after a conflict-resolution push.
  - The decomposed-parent hold (D5) also triggers it.
- ADR-035 is unaffected: the pass writes nothing to the run branch. The base advance it verifies is the existing App-installation write from #3125.

**D4 Opt-in by declaration.**
- The verify requirement (D2/D3) applies only when the workflow declares a verify command. With none declared, the verify state is `not_required`.
- D1's up-to-date requirement applies regardless of a declared verify command.

**D5 Decomposed parents.**
- When a parent has decomposition children and a declared verify command, the orchestrator holds the parent's review gate. The hold sits after `maybeOpenConsolidatedPR` and before review dispatch, and lasts until the consolidated fan-in head has a `passed` merge-candidate verify (cause `fan_in`).
- The consolidated implement review renders that result as the authoritative parent-level verify evidence, under the existing prompt rule for a PARENT-LEVEL verify run.
- This verifies the consolidated head as integrated. Combining it with any base movement after integration is then covered at merge time by D1 + D2.

**D6 Routing a red result.**
- A `failed` pass routes one bounded fix-up through the existing `fixupStageAs` path **only when the run's workflow delegates fix-up routing** (`actions.fixup` in workflow-v2, `may_route_fixup` in v0–v1, ADR-066). The routed fix-up runs under an in-process system identity carrying `write:fixups` (the auto-driver's identity construction), and the existing fix-up ceiling bounds it.
- When fix-up routing is not delegated, no fix-up is routed: the merge stays refused as `merge_candidate_verify_failed`, naming `fishhawk_fixup_stage` for the operator.
- The routed concern names **only trusted fields**: head SHA, cause and the declared command. **The verify output is untrusted** and is never embedded in the concern. The fix-up pass re-runs verify and sees that output through its existing gate channel.
- If the route is refused (for example, budget spent), the merge stays refused and names `fishhawk_fixup_stage`.

**D7 Fail posture.**
- On a GitHub-wired run with every anchor resolvable (client, installation, parseable repo, PR number), a forge read error or audit-chain read error **fails closed** with 502 `merge_candidate_check_failed`. Nothing is recorded, and the call is retryable. This is deliberately stricter than the #3109 conflict guard, which fails open on read errors.
- When the deployment cannot probe at all (no GitHub client, no installation, no PR number, or a GitLab-family run), the gate **proceeds with a WARN log**. That follows the same determinability ladder as `prMergeConflicting`.
- **GitLab merge requests therefore keep today's behaviour.** This is a stated residual, not a decision that GitLab is exempt.

**D8 Scope boundary.**
- Not gated by this decision:
  - the orchestrator `auto_merge` stage, which stays deferred to forge branch protection per ADR-017;
  - merges performed outside Fishhawk. The `fishhawk_audit_complete` Check Run does not go pending on an unverified candidate.
  - operator-vouched heads.
- Recovery of a verify pass whose runner crashes before reporting uses the existing reap path.

**Ratification rulings (maintainer, 2026-10-09).**
- **Q1 — ADR-043 relationship: partial supersession.** This ADR supersedes ADR-043 Revision 2 item 3 only. ADR-043 stays `accepted` and records this ADR in `superseded_by`; this ADR records ADR-043 in `supersedes`. ADR-043 Revision 2's factual premise ("Fishhawk does not perform merges") is stale on `main` independently of this ADR.
- **Q2 — `sequences:` is out of this ADR.** It is a minor additive workflow-v2 spec decision under the schema-change checklist and ADR-026. It still ships under #4018, since that run's slice 1 depends on slice 0.
- **Q3 — red-result routing follows delegation (D6 as written above).** A fix-up is routed automatically only when the workflow delegates fix-up routing; otherwise the merge is refused naming `fishhawk_fixup_stage`. #4018's implementation must follow D6 as ratified, not the plan's unconditional auto-route.
- **Q4 — the gate-to-merge window is an accepted, narrower residual.** No per-repo merge serializer is required now. GitHub strict up-to-date branch protection is the optional operator-side close under ADR-017. A serializer is revisited only if the window proves material in practice.
- **Q5 — universal behind refusal.** D1 refuses on any behind. #4120's overlap-filtered refusal, with its `allow_stale_base` override, is not adopted now; it may return as a relaxation layered on D1 if the throughput cost proves material.
- **Q6 — one fan-in verify mechanism.** E85.4 (#4147) consumes this ADR's merge-candidate pass (the declared verify command) and contributes only its main-advance re-trigger and debounce. It does not ship a second, Go-shaped job kind.

## Consequences

- **Correctness.** For merges Fishhawk dispatches on GitHub, the tree that lands is either the tree that verified, or a tree whose only change since verification is the base movement inside the Q4 window.
  - Collisions the project's own tests detect (duplicate sequence numbers, registry clashes, semantic breaks across files) are caught in the loop and routed to a fix-up. They no longer depend on an operator or overseer building `head ⊕ main` by hand.
  - The "unverified consolidated fan-in tree" review concern on every decomposed parent is answered by product evidence.
- **Throughput cost.** Every merge into an advanced base costs one base advance plus one full verify run. In this repository that is a full `scripts/test verify`, with a 40m timeout per `.fishhawk/workflows.yaml`.
  - The pass re-opens the implement stage, so on a local runner it is admitted through the implement concurrency group (ADR-087) and runs under the per-repository verify lock.
  - Parallel runs therefore serialize at merge time. This serialization is emergent, not a lock: each merge makes every other open PR behind.
  - Campaign merge walks get longer. That is the intended trade.
- **New surface.**
  - Audit categories `stage_merge_candidate_verify_triggered` and `merge_candidate_verified`, registered in `backend/internal/audit/categories.go`. They are not issue-comment surfaces.
  - A new runner-to-backend report outcome, `merge_candidate_verified`, on the cross-module wire contracts guarded by `TestCrossModuleWireParity`.
  - New 409/502 codes on `POST /v0/runs/{id}/merge`.
  - New `fishhawk_merge_run` statuses: `behind_base`, `merge_candidate_unverified` and `merge_candidate_verify_failed` (immediate, not polled), plus `merge_candidate_check_failed` as a tool error.
  - New `rebase_run_branch` response fields.
  - An orchestrator hold on decomposed parents' review gates.
- **Residuals, stated.**
  - The Q4 gate-to-merge window.
  - The GitLab family proceeds ungated (D7).
  - The `auto_merge` stage and external merges are ungated (D8).
  - Operator-vouched heads are not re-verified.
  - A pass that cannot start (no re-openable implement stage) leaves the merge refused as `merge_candidate_unverified` with the refusal named.
- **Rollback hazard.** An unconsumed `stage_merge_candidate_verify_triggered` at revert time would be served by the old backend as a plain implement re-dispatch. The rollback plan cancels or reaps such stages first.
- **Related work.**
  - #4119 (refuse a merge while an overlapping run's implement is live) and #4118 (cross-run scope-overlap signal) are complementary signals. They are not substitutes.
  - #3973 (base pinned per stage) means base movement is handled once, at merge time.
  - ADR-089's first-officer fan-in verification jobs remain useful for counterfactuals. The merge-candidate check itself becomes product behaviour rather than a judgement-seat task.
- **Implementation.** #4018 (campaign run a9cb95c3) is decomposed into five slices:
  - 0: `sequences:`;
  - 1: trigger, state predicate and recorder;
  - 2: rebase producer plus runner pass;
  - 3: merge gate;
  - 4: decomposed-parent hold.
