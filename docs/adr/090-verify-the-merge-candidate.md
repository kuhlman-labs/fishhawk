---
id: ADR-090
title: "Verify the merge candidate: a Fishhawk-dispatched merge requires an up-to-date head, and a base-advanced head must pass the project's declared verify command in a runner verify-only pass"
status: accepted
date: 2026-10-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/4170
supersedes: ["ADR-043"]
superseded_by: ["ADR-092", "ADR-093", "ADR-096"]
amends: ["ADR-124"]
amended_by: ["ADR-142"]
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

## Amendment 1 (2026-10-14): an operator base merge is a base advance (#4368)

**Ratified by the Captain (2026-10-15).** Amends D8's third bullet ("operator-vouched heads") and the matching Consequences residual. D1–D7 are unchanged.

**Context.** D8 exempted every operator-vouched head from the merge-candidate pass. Rules 1–2 of the head classifier recognise only base advances the PRODUCT wrote (a performed `branch_rebased` merge, a `conflict_resolution_pushed` head). So an operator who merges the base into the run branch by hand and vouches the result takes the exemption, and the combined tree is never product-verified.

Recorded instance: #4293, run e16e6c42. The operator pushed a merge of `main` (head 4ce8fa15) and vouched it. `rebase_run_branch` answered `already_up_to_date` with merge-candidate state `not_required`, and `merge_run` queued the merge with no pass. The tree that landed was never verified.

**Decision.** Rule 4 (an operator-vouched head) no longer exempts unconditionally. The vouch exempts the head H only when a forge probe proves H brings in no base commit that the run's newest VERIFIED head lacks.

- **Reference head V.** The newest head on the run's chain (by chain sequence) whose tree the product gated or verified, never H itself:
  - a runner-gated head report (`pull_request_opened`, `fixup_pushed`, `child_pushed`);
  - a `merge_candidate_verified` row with result `passed`;
  - in ADR-096 `enforce` mode only, the `to_head` of a `merge_candidate_verdict_transferred` row (the same rollback contract as the verdict read).
- **Probe.** Two GitHub three-dot compares: `V...H` (commits H has and V lacks) and `<PR base ref>...H` (H's branch-only commits). A commit in the first and not in the second is one the base already contains, so H brought it in → `base_advance`. None → `not_required`, which is D8's operator-edit case.
- **Undecidable on a probeable run fails closed to `base_advance`:** no verified reference head on the chain, an empty PR base ref, or a compare at the un-paginated 250-commit cap (a truncated list cannot prove absence).
- **A forge or audit read error is returned.** Every reader already fails closed on it (D7): the merge gate answers 502 `merge_candidate_check_failed`.
- **A deployment that cannot probe** (no GitHub client, no installation, an unparseable repo, no PR number) keeps the pre-amendment exemption with a WARN log, mirroring D7's determinability ladder.
- **No new surface.** No new cause, state, wire field or audit category. The existing readers do the rest: `merge_run` refuses 409 `merge_candidate_unverified` (cause `base_advance`) until a pass passes on exactly that head, and `rebase_run_branch`'s already-up-to-date arm starts the verify-only pass.

**D8, as amended.** "Operator-vouched heads are not gated ONLY when they bring in no base commit the newest verified head lacks. An operator merge of the base (or an operator conflict-resolution merge) is a base advance and needs a passing merge-candidate verify."

**Consequences.**
- An operator base merge or conflict-resolution merge now costs one verify-only pass, started by `rebase_run_branch`'s already-up-to-date arm and serialized through the ADR-092 queue.
- A vouched edit on top of a head whose tree was never verified (a product rebase whose pass never reported, a decomposed parent before its fan-in pass) now reads `base_advance`, or fails closed when nothing on the chain was ever verified. The tree under the vouch was never verified either.
- Each classification of a vouched head not caught by rules 1–3 costs one pull-request read and up to two compares, on every reader (merge gate, rebase up-to-date arm, fan-in hold, queue admission, gate view, verdict transfer, review re-run hint).

**Residuals, stated.**
- A base change carried in by a NON-merge commit (a cherry-pick, a hand copy) has a fresh SHA the base does not contain, so it is not detected. Option (b), a scope-bounded tree diff, was not adopted: it would strip D8's canonical case, an operator remediation commit outside the run's scope.
- A deployment that cannot probe keeps the exemption (D7).
- The 250-commit compare cap fails closed, so a very long branch pays a pass it might not need.

**Implementation.** #4368.

**Partially superseded (clarified by the Captain 2026-10-23):** Partially superseded by ADR-092 (Q4; pass admission group), ADR-093 (D8 for queue-dispatched merges) and ADR-096 (D2 for independent deltas; D5 fan-in base). D1, D3, D4, D6, D7 and the rest of D2/D5 still govern.

## Amendment A2: fishhawk_audit_complete goes pending on an unverified merge candidate (Captain decision 2026-11-19 on #5409; text ratified at merge)

Ratification: Captain, 2026-11-19 (#5409); final text confirmed by the Captain at merge.

**Context.** This is the #5226 gap 2 (PR #5405). D8 left merges performed outside Fishhawk ungated, and the `fishhawk_audit_complete` Check Run did not go pending on an unverified candidate. So a PR could be merged from the forge UI while its merge candidate was unverified, with the check reading green. The Captain decided the check alone changes (CHECK ONLY): no new refusal and no forge binding. This amendment does not conflict with ADR-093, which partially supersedes D8 for queue-dispatched merges: the queue still merges only a head that verified, and this amendment changes only what the check reports for merges the queue does not dispatch.

**Decision.**

- **A2.1.** The check reads the D2 predicate the merge gate reads (`mergeCandidateVerifyState`, D2 as amended by Amendment 1 and ADR-142), for the live PR head the foreign-commit rule reads.
- **A2.2.** The mapping:
  - `not_required` or `passed` clears;
  - `unverified` or `in_flight` adds a pending-flavored `merge_candidate_unverified` item;
  - `failed` adds a `merge_candidate_verify_failed` item;
  - a predicate or run-row read error adds a pending-flavored `merge_candidate_check_failed` item, whose detail is a fixed message plus the head (the raw error goes to the server log only);
  - any other answer is held pending as `merge_candidate_unverified`.

  The binding behaviour is that the check never reads green on a failed or unreadable candidate, and that a pass for a different head does not clear it. Mapping a failed verify to a hard `fail` (`merge_candidate_verify_failed`, mirroring the merge gate's 409 code) rather than to pending is a planner choice, for the Captain to confirm at merge. Its precondition: when a delegated fix-up (D6) or the #4391 re-run re-opens the implement stage, the check's mid-flight early return answers pending with `stage_not_terminal` items, so a failed head that is being repaired reads pending, not failed.
- **A2.3.** `passed` means a verdict for exactly that head: a recorded pass, or in ADR-096 `enforce` mode a transferred verdict for that head. That is the same answer the merge gate admits on.
- **A2.4.** A deployment or run that cannot read the live head (no GitHub client, no installation or PR, the GitLab family) skips the rule, as D7 does.
- **A2.5.** This amendment makes nothing required on the forge. Branch protection applies to every PR, so binding the check as required is a separate, later, human-led step, gated on the server posting a passing status on PRs that are not Fishhawk runs. Admin merges are unaffected.

**D8, as amended.** Fishhawk still does not refuse merges performed outside it. But the `fishhawk_audit_complete` state Fishhawk computes no longer reads green on an unverified, in-flight or failed merge candidate. Whether that blocks an outside merge is the repository's branch protection (ADR-017).

**What holds, and where.**

- The rule makes the COMPUTED state pending at the run's live head wherever it is computed: `GET /v0/stages/{id}/checks`, the forced at-head publishes (the rebase verb, the vouch), and the queue and inline pre-merge republishes.
- Unforced publishes (the synchronize webhook, the `GET /checks` heal, the reconciler's heal sweep) still target the RECORDED head (the #4397 residual).
- The conflict-resolution report publishes nothing at the merge head. So the live head can carry NO `fishhawk_audit_complete` check until an at-head publish runs.
- On the forge, "pending at the PR head" therefore holds only where an at-head publish has reached. A production at-head publish after a Fishhawk-written head (a conflict resolution or a base advance) is the candidate follow-up; this amendment adds none.

**Consequences.**

- Every audit-complete recompute pays the predicate's audit reads, and for a vouched head up to one pull-request read and two compares (the Amendment 1 cost). That applies on the read endpoint, the synchronize republish, the reconciler heal sweep and every forced republish.
- An outside merge of an unverified candidate leaves the computed check pending on the merged head, which records that the landed tree was never verified.
- There are three new missing kinds: `merge_candidate_unverified`, `merge_candidate_verify_failed` and `merge_candidate_check_failed`. There is no new audit category, state, cause or runner wire field. The verifier mirror stays rules 1 to 4.

**Residuals, stated.**

- The Q4 gate-to-merge window.
- GitLab-family runs (D7).
- The `auto_merge` stage, still deferred to forge protection.
- Unforced publishes, which still target the recorded head (#4397 residual), and the absence of a publish at a conflict-resolution merge head.
- A failed head, whose published check keeps its last published non-green state, because the verdict republish stays passed-only.

**Implementation.** #5409.

## Amendment A3: a decomposition child's runner gate is a scoped slice gate, and the fan-in pass is its full verify (Captain decision 2026-11-21 on #5498; text ratified at merge)

Ratification: Captain, 2026-11-21 (#5498); final text confirmed before the #5498 pull request (#5520) merges, as the ratification's condition of merge requires.

**Context.** A decomposed run paid about five full verify passes, against about one for a flat run (#5498, 2026-11-20 study). Every child paid the #3315 scoped pre-pass plus a full re-verify, and then the parent paid the fan-in merge-candidate pass, base-advance passes and a queue pass. D2 exempts implement heads because "Implement and fix-up heads are already gated on the committed tree by the runner and need nothing more." A decomposition child's head is never a merge candidate, though. It is integrated into the parent's consolidated head, and that head already needs its own exact-head pass: D2's fan-in producer and D5's hold. So the child's full verify repeats work that the fan-in pass does on the tree that lands.

**Decision.**

- **A3.1 The slice gate.** A decomposition child's non-fix-up implement stage runs ONE verify per gate decision, scoped to the slice's reverse-import reach. This replaces the #3315 scoped pre-pass plus full re-verify, in the verify-fix loop's verifies and in the single-shot gate's verify.
  - The reach is the Go packages the slice scope touches, plus their reverse dependencies in the child clone's import graph, tests included.
  - A red slice gate fails the slice, as a red full gate did.
  - The legs the scoped form keeps (lint, the record and doc checks, schema sync, the gate harnesses, the site gates) still run. Patch coverage does not.
- **A3.2 What stays full.** The slice gate changes no other verify:
  - every child fix-up;
  - the #960 strict re-verify before push, and its in-place re-runs;
  - every merge-candidate pass, which D3 fixes at full form. Its executed `merge_candidate_verified` rows record `gate_shape: "full"` with `gate_shape_source: "contracted"`.

  The #4229 outside-only absorb re-run keeps its own outside-package scope, unchanged.
- **A3.3 Widen, never narrow.** Whatever the gate cannot decide runs the full form, with a fixed reason token. That includes:
  - the gate not being offered;
  - a refused or unreadable verdict-transfer declaration;
  - an empty, non-concrete or undecodable scope entry;
  - a `go.work`, `go.work.sum`, `go.mod` or `go.sum` at any depth;
  - a scope path matching the declaration's `global_surface`, `doc_check_inputs` or `opaque_paths`, or the workflow spec path;
  - a non-host gate path (A3.7);
  - any reach-load, mapping or size failure.
- **A3.4 D2, as amended.** A child head reported as `child_pushed` is still a runner-gated head report. Its tree was certified by the slice gate, not by the full form. That is safe only because a child head is never merge evidence:
  - `child_pushed` is appended to the CHILD's chain, never to the parent's.
  - A decomposed parent's head therefore classifies `fan_in` and needs a passed pass at exactly that head.
  - The same holds for Amendment 1's verified reference and for ADR-142 D2's runner-gated terminal. Neither reads a child's chain when it classifies a parent head.

  A change that lets a slice-gated head serve as merge evidence, or as a verified reference on a chain that merges, must amend this record first. D2's exemption is unchanged for implement and fix-up heads of runs that are not decomposition children, and for child fix-up heads.
- **A3.5 The fan-in pass is the full verify.** For a decomposed run, the fan-in merge-candidate pass is the only full verify of the integrated slices, and the merge precondition. That is D3 with cause `fan_in`, held by D5, and it is ADR-096 D4's single verification point. D3, D5 and ADR-096 D4 are unchanged: when the pass runs and what it gates do not change. A slice break the slice gate misses fails that pass and routes under D6.
- **A3.6 A second, weaker consumer of the verdict-transfer declaration.** The slice gate reads the repository's `verdict_transfer` declaration (ADR-124), resolved from the child run's cached admission spec. It is a second consumer of that declaration, explicitly weaker than verdict transfer, and the fan-in full pass is its backstop. This amends ADR-124 for this consumer only:
  - **ADR-124 D3 (the inert rule).** A scope path seeds every ancestor directory that is a package, so graph reach is kept. A path under no package directory that matches `inert_paths` contributes nothing. ADR-124 D3's sentence "An inert declaration never removes a path from graph reach, data-read edges or the shared-artifact rule" does not govern the slice gate.
  - **ADR-098 D2 (data-read edges) and ADR-096 D1(d) (shared-artifact dirs)** do not govern it. The server does not serve `shared_artifact_dirs` to the slice gate.
  - **ADR-098 D3 (shadow first) and ADR-124 D4's enforce-mode completeness check** do not govern it. A slice-gate result is never merge evidence, so the gate acts in every transfer mode, on any usable declaration.
  - **ADR-124 D5 (tier I) extends to this consumer.** The declaration now also decides how much of a child's gate runs, so a change to it stays a tier-I change. Shrinking `global_surface`, `doc_check_inputs` or `opaque_paths`, or widening `inert_paths`, narrows child gates as well as transfer. D5's self-coverage holds: the workflow spec path is always a full-gate path. D5's interim controls (the implement-stage `.fishhawk/**` forbidden path and the operator-authored rule) suffice for this consumer, as they do for shadow mode. D5's enforce-mode precondition does not apply, because a slice-gate result never decides a merge.

  ADR-124 D1 to D7 govern verdict transfer unchanged.
- **A3.7 The ADR-063 boundary.** The reach load runs `go list -e -json` and an imports-only parse on the runner host, over the materialized clone. It is a runner metadata load by the runner's own `go` binary, like ADR-096's backend classifier, not a repository-supplied gate command. It therefore does not route through the shared gate-exec helper.
  - **Where it runs.** Only where ADR-063's selection already runs the gate on the host: `clone`, which includes the unconfigured host exec, or `clone-sandbox`, and only under a non-hosted profile. It reads the same memoized selection the gate exec reads, before any load.
  - **Where it does not.** A container path, a hosted profile, a refused selection or any other value widens to full with `gate_isolation_not_host:<selection>`, and no host `go list` runs.
  - **Before any metadata read,** the load refuses a symlinked or non-regular `go.work`, `go.work.sum`, `go.mod` or `go.sum` at the root, at each `use` directory and at each filesystem `replace` target. That is the Lstat predicate the container module seed uses. It also refuses any `use` or `replace` that resolves outside the clone.
  - **Env pins.** After ADR-029 sanitization it appends `GOWORK`, `GOPROXY=off`, `GOTOOLCHAIN=local`, `GOFLAGS=`, `CGO_ENABLED=0`, `GONOPROXY=`, `GOPRIVATE=`, `GONOSUMDB=`, `GONOSUMCHECK=`, `GOINSECURE=`, `GOENV=off`, `GOCACHEPROG=` and `GOSUMDB=off`. A module that is not already cached is never fetched; the load widens instead.
  - **Time bound.** One 2-minute bound starts before the load touches the clone and covers every read, the walk and the `go list`. It is not charged to the gate's own timeout.

  ADR-063's selection rules are unchanged, and so is ADR-149 D3's refusal of host paths for charter work. A charter-driven stage never reaches the load.
- **A3.8 Scheduling.** ADR-092 D1 to D3, as amended by ADR-115 D3, and ADR-093 D3, with ADR-115 D7, are unchanged.
  - The slice gate is a gate verify inside the child's implement stage. It runs under that stage's slots and the per-repository verify lock, and takes no verify-group slot and no queue entry.
  - The fan-in pass and every base-advance and queue pass are admitted and ordered as today.
  - The saving is in each child's gate time, not in the number of merge-candidate passes.
- **A3.9 Evidence.**
  - Each hooked gate decision appends a `verify_slice_gate` trace event. It carries the shape (`slice_scoped` or `full`), the reason, the package count, at most 50 package directories and the iteration.
  - The `/prompt` response gains an additive `slice_gate` key, served only to a non-fix-up implement stage of a decomposition child.
  - No audit category, state, cause or permission is added.

**Residuals, stated.** The fan-in full pass backstops each of these. Each one costs a later failure, routed as a parent fix-up, never a merge escape.

- Tests that read other packages' source as data are invisible to the import graph. Examples are the wire-contract parity scans and the audit-category emission scan.
- Declared-inert documents that Go tests read as data contribute no reach. ADR-098's Context names four such tests. A repository that wants such documents to force the full gate declares them in `global_surface` or `doc_check_inputs` instead.
- The reach is computed on the host. A directory whose only files carry a build constraint the host never sets is walked but not emitted. The walk closes the import-graph half of the host-versus-container GOOS and tag gap, not the execution half.
- `vendor/` trees, nested-module metadata and non-`.go` source files outside the Lstat predicate are bounded only by the load timeout.
- Patch coverage is enforced only at fan-in.
- The saving needs a host gate path and a warm module cache. A cold cache widens through `package_error`. Where a gate image is configured, ADR-115 D2 puts the gate on the container path, where the slice gate always widens.

**Consequences.**

- A decomposed run pays one scoped verify per child gate decision plus one full fan-in pass, in place of a scoped and a full verify per child. Child fix-ups still pay the full form.
- A break the slice gate misses surfaces at the fan-in pass as a parent-level failure. That is later, and harder to attribute to a slice, than a child failure.
- A slice review sees a scoped gate only through the scoped-shape detail sentence. The consolidated implement review keeps D5's parent-level verify evidence.
- The post-merge split of `slice_scoped` from `full` by reason measures whether the gate pays for itself. A high share of `gate_isolation_not_host` or `package_error` says it does not.

**Relation to ADR-124.** This amendment amends ADR-124 (D3 and D5) for the slice-gate consumer only. It supersedes nothing.

**Implementation.** #5498 (PR #5520).
