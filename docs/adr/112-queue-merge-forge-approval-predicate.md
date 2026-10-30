---
id: ADR-112
title: "Amend ADR-093 D6: a transferred approval needs a fresh, trusted forge approval at the verified head; an undeterminable or missing one HOLDS rather than ejects on a non-blocked PR"
status: accepted
date: 2026-10-22
issue: https://github.com/kuhlman-labs/fishhawk/issues/4622
supersedes: []
superseded_by: []
amends: ["ADR-093"]
amended_by: ["ADR-115", "ADR-121"]
applies_to: []
---

# ADR-112: Amend ADR-093 D6: a transferred approval needs a fresh, trusted forge approval at the verified head; an undeterminable or missing one HOLDS rather than ejects on a non-blocked PR

**Status: accepted (v4.1, ratified by the Captain 2026-10-22). Amended by A1 (R3 clarifications), ratified by the Captain 2026-10-23; by A2 (re-probe due time and round-robin), ratified 2026-10-28; and by A3 (retryable merge-gate legs wait or yield, never drop intent before dispatch), ratified 2026-10-30.**
- **Review history:**
  - v1: Codex (reject, 10 findings) and fable (approve_with_concerns, 12 findings).
  - v2: Opus, substituting for Codex during its quota outage (reject, 14 findings), and fable (approve_with_concerns, 6 new findings).
  - v3: fable (approve_with_concerns) and Opus (approve_with_concerns); both cleared the v2 blockers and asked for text fixes.
  - v4 folds in all of those fixes, including one existing advance-integrity weakness (R1(a) parent check). An Opus final check on v4 returned "ratify with named notes".
  - v4.1 applies those notes as text, with no design change: the wait anchor, the ledger and row 4, the entry-site scope, the full CLEAN predicate, the voucher exception, the OAuth scope and the store op.
- **Freeze:** tracker-only until the 2026-11-01 catch-rate re-pin; the in-repo record follows the upkeep rule.
- **Relations:** it **amends ADR-093** (D4, D6, D7, and the mechanics of Q1(a); Q1(a)'s ruling is KEPT), and realizes ADR-055 Phase 3 ("artifact-anchored approvals") for queue merges. It does not supersede either.

## Context

ADR-093 D6 lets a run's Fishhawk review-gate approval TRANSFER across a clean base advance plus a green verify-only pass, "so the queue merges with no human re-approval" (Q1(a)). D4 ejects on a dismissed forge approval. Implementing merge-on-pass (#4296, PR #4609) exposed four problems in how the queue judges FORGE approvals.

1. **A weaker rule than the gate.** The LIVE basis (`backend/internal/server/merge_queue_eligibility.go`, main 52e06dad) accepts any approving reviewer when the review gate declares no `members`. It never checks the review's commit, `not: [author, agent]`, `min_permission` or `member_of`. ADR-055 defines the predicate, and `quorum.go` `resolvePredicates` already evaluates it via `identity.Provider.PermissionLevel`. The security class is tracked as #4623, with details in a private draft advisory.
2. **Untrusted influence.** Any account's CHANGES_REQUESTED vetoes, and review volume can truncate the REST listing.
3. **Liveness.** A held head of line blocks its base: the store admits nothing while a live row exists, inline `merge_run` refuses while the queue owns the merge (ADR-109), and no exit verb exists. Approval-leg failures at admission and at merge-on-pass step 4 DROP the entry, so its intent is lost.
4. **Evidence after the fact.** Authorization is visible only on `merge_queue_merged`.

## Options

- **A. Keep the current rules,** adding an association check on the transferred basis. Rejected: it stays weaker than the gate.
- **B. One forge-approval predicate for every basis, with a clean-advance chain that preserves Q1(a), a yield, and evidence before dispatch (recommended).**
- **C. Repeal Q1(a):** every advanced head needs a fresh human approval. Rejected: it reverses the Captain's ADR-093 Q1 ruling and starves busy queues.

## Recommendation

**Option B.**

**R1 — Which forge approval counts (one rule for every basis).** A forge review counts as a qualifying APPROVAL only if all four hold:
- **(a) Chain.** It is the reviewer's latest opinionated review (keyed by the forge's immutable user id, ordered by `submitted_at` then id; COMMENTED and PENDING ignored), its state is APPROVED, and its `commit_id` is either:
  - the verified head; or
  - a head from which the verified head is reached ONLY through a chain of Fishhawk-recorded CLEAN advances.

  **How the chain is computed:**
  - The walk is SEEDED at the review's `commit_id`. It follows `branch_rebased` rows by `prior_head_sha → new_head_sha` adjacency, ordered by audit sequence only, with no comparison of forge time to audit time. It is NOT seeded from the intent's anchored head.
  - Both queue advances and the operator's `rebase_run_branch` write `branch_rebased`, and both count.
  - A fix-up, a vouch, a conflict-resolution push, or any push not recorded as a clean `branch_rebased` row breaks the chain.
  - **CLEAN advance (full predicate).** A `branch_rebased` row is CLEAN only if ALL of these hold:
    - **(i) The existing predicate:** `!already_up_to_date && merge_commit_sha != "" && new_head_sha == merge_commit_sha` (`merge_queue_eligibility.go`).
    - **(ii) Parent check (advance integrity):** the merge commit's parents are exactly (`prior_head_sha`, `base_tip_sha`).
      - `base_tip_sha` is the base tip read immediately before `POST /merges` and recorded on the row.
      - The check is computed SYNCHRONOUSLY in `advanceRunBranch`, after the merge call and before `writeBranchRebasedAudit`, and carried on the advance outcome. The ADR-096 verify-verdict transfer uses it too.
      - A failed commit read makes the row NOT clean (fail closed).
      - Rows written before this check carry no `clean_verified` mark and are NOT clean, so in-flight entries need one re-approval.
    - **Why (ii):** the forge merges API has no compare-and-swap, so a push landing between the head re-check and `POST /merges` would otherwise be absorbed as a clean advance and carry an approval to unapproved content. That weakness exists today; (ii) closes it for every transfer. A push landing after the merge (on top of M) is excluded by (i), because `resolvePostMergeHead` records it as `new_head`.

  This preserves Q1(a): an approval kept across clean advances still counts, so the queue merges with no re-approval.
- **(b) Predicate.** Its author satisfies the IMPLEMENT-review gate's `approvals` predicate, including any fired escalations (E53.4, `countEscalatedForgeApprovers`), resolved at decision time through the identity provider using the RUN's installation credential. The predicate covers members, `min_permission`, `member_of` and `not: [author, agent]`. For a forge review:
  - **`author`** is the PR opener (when it is a user) plus every Fishhawk-recorded change author (`operator_commit_vouched` vouchers, `quorum.go` `resolveChangeAuthor`), canonicalised with `identity.CanonicalMember`. This is a SUPERSET of the review gate's rule, which counts only the earliest voucher and never the PR opener.
    - **One exception:** when the gate's effective (escalated) `members` set canonicalises to exactly {that voucher} AND the effective count is 1 (a single-maintainer repository like this one), the voucher's approval is admitted and the exception is recorded on `merge_queue_authorized`.
      - It NEVER applies to a gate without `members` (a `min_permission`-only or `member_of`-only gate), or once an escalation raises the count or adds groups. The residual is stated in Consequences.
  - **`agent`** means a bot or App account; no proof of human control is claimed (ADR-055).
- **(c) Floor.** It always applies, as an AND: the author's effective permission is at least `max(declared min_permission, write)`.
- **(d) Identity.** Its author is not a bot, an App, a `[bot]` login, a ghost user or a null user.
- **Count.** At least ONE qualifying approval is required. The forge check is a head-chain and identity control, not a second quorum; the gate's `count` is satisfied by Fishhawk approval rows as today.

**R2 — Who may veto, and how the snapshot is read.**
- **Veto.** A CHANGES_REQUESTED that is the reviewer's latest opinionated review vetoes if its author meets R1(c) and R1(d), the write floor regardless of `members`/`member_of`. `members` says who may approve, not who may object. A veto from a reviewer whose permission cannot be resolved is `undetermined`, never ignored.
- **Snapshot.** Approvals and vetoes come from ONE snapshot, read under the merge-intent lock immediately before the dispatch claim.
- **Read.** On GitHub, `PullRequest.latestOpinionatedReviews(writersOnly: true)` through the existing GraphQL client (`githubclient`, installation token). It is a writers-only, per-user-latest pre-filter, consistent with the floor. Declared `members` membership is checked locally with no forge call. Any declared `min_permission` or `member_of` is still resolved: only the write floor is covered by `writersOnly`. Each reviewer is resolved at most once per decision. A listing that cannot be read completely is `reviews_truncated`, a distinct reason with its own remedy.
- **Credentials.**
  - Permission reads use a NEW installation-scoped read in `githubclient` (`GET /repos/{o}/{r}/collaborators/{login}/permission` with the RUN's installation token, as `doGraphQL` already takes an installation id).
  - The boot-singleton identity provider and its 404→`PermissionNone` contract are NOT changed, so the review gate's quorum path is unaffected.
  - On the new read, a 404, or "none" on a private repository, is `undetermined`, not rejected.
- **Fold result:** `approved`, `vetoed`, `dismissed`, `absent`, `undetermined`, `reviews_truncated` or `provider_unsupported`.
  - Only `approved` proceeds.
  - `approved` requires no reviewer whose undetermined permission could make them a veto.
  - The same GraphQL read fetches `reviewDecision`. A `REVIEW_REQUIRED` decision (for example "require approval of the most recent push") is not `approved`: the entry YIELDS with `next_step: approve_pr`, instead of reaching a forge refusal.
  - Semantics are pinned with recorded fixtures, including whether `latestOpinionatedReviews` returns DISMISSED. Fields are GraphQL names: `author { __typename … on User { databaseId login } }`.
- **Residual race:** an approval withdrawn between the snapshot and the merge call.

**R3 — Yield, with a bounded head-of-line wait (store support named).**
- **Head-of-line wait.** After the verify pass at the head of the line, a fold that is not `approved` first WAITS at the head for a bounded window (`FISHHAWKD_MERGE_QUEUE_APPROVAL_WAIT`, default 30 min).
  - **No re-admissions:** while it holds the line, the queue admits nothing else, so the queue's own merges cannot move the base and a fresh approval at the verified head merges. Non-queue merges (admin merges, operator PRs) can still move the base. A base move readvances the entry as today.
  - **The anchor:** the window is measured from a PERSISTED start (`approval_wait_started_at`, migration 0109) that is set once per yield episode and NOT reset on readvance or re-admission. An externally moving base therefore cannot hold the line indefinitely: at expiry the entry yields regardless.
  - **Never inside the lock:** the wait is a held outcome repeated across pumps, never held inside the merge-intent lock (its fn bound is 90 s). It takes no ledger claim.
  - **Cost, stated:** on a repository whose protection dismisses stale approvals and with no human present, each such entry can hold the base for up to the window. N entries means up to N × the window with nothing merging.
- **Yield.** On window expiry (including for `REVIEW_REQUIRED`, which follows row 3: wait, then yield), or immediately on `vetoed` or `provider_unsupported`, the entry YIELDS:
  - one transaction `Unadmit` plus a marker write under the same `admission_epoch` predicate (no crash window);
  - migration 0109 adds `yield_reason`, `yield_detail`, `yielded_at` and `approval_wait_started_at` to `merge_candidate_queue_entries`;
  - `merge_queue_yielded` is audited and registered in `categories.go`;
  - `AdmitNext` SKIPS marked rows entirely; it does not merely order them behind. `activeForRun` positions and `ListForBase` use the same order.
- **Re-probe.** Clearing a marker is the only way back in. A bounded re-probe runs on the oldest yielded row per base per pump, with backoff on `yielded_at`, AND immediately on a `merge_run` re-invoke or an approval webhook for that run.
  - The forge reads happen OUTSIDE the store transaction.
  - The marker is cleared by a compare-and-swap on (`anchored_head_sha`, `yielded_at`) under the base lock, when the fold would be `approved` and no qualifying veto exists. The row then rejoins FIFO at its original `enqueued_at`. The pump treats a yield like a readvance and CONTINUES to the next held row.
- **Sites:**
  - **Entry (`merge_run`, queue arm only).** On the queue arm, the path taken when the merge-candidate gate refuses as behind, held or unverified, an approval-leg failure with the PR head as the head ENQUEUES the entry as a yielded row with intent recorded, instead of refusing.
    - This uses a new store operation, enqueue-or-reanchor with the marker, because `Unadmit` is live-only; `merge_run` never marks a LIVE row.
    - On the INLINE path, an R1/R2 failure keeps today's "refuse, write nothing", so no ownership transfer happens there.
    - It is a stated change to the queue arm's contract.
  - **Admission.** Approval legs are NOT evaluated (there is no verified head). Admission KEEPS the Fishhawk-side sub-legs (`review_pending`, `review_coverage_incomplete`) and the R2 VETO check, which needs no verified head. The anchor-less ADR-092 rebase path keeps its approval requirement unchanged (ADR-092 D1).
  - **Merge-on-pass.** Step 4 and the in-lock step 8 wait or yield as above; they never drop an entry carrying intent.
- **Legacy check.** `approvalDismissedCheck` is REMOVED from merge-on-pass (all three call sites, and the settle-eject) AND from inline `merge_run` (which adopts R1/R2). The gate view keeps it as an advisory display. The dismissed-approval EJECT in D4 (fourth bullet) and D6 (third bullet) is withdrawn.
- **Precedence table** (normative):

  | Order | Check | On failure |
  |---|---|---|
  | 1 | Non-approval eligibility legs | Existing behaviour |
  | 2 | The R2 veto | Yield |
  | 3 | The R2 approval fold | Wait at head, then yield |
  | 4 | Forge refusal after an `approved` fold that is a typed RELEASE outcome (the claim was released by `settleMergeDispatchClaim`), with a fresh GraphQL re-read showing `reviewDecision` not APPROVED or `mergeStateStatus: BLOCKED` | Yield, `next_step: approve_pr` |
  | 5 | Any other forge refusal | Eject (D4) |

- **Ledger.**
  - A wait, and every yield except row 4's, happens BEFORE any dispatch claim, so it never writes or clears an ADR-109 ledger row.
  - Row 4's yield happens only after the claim was released on a typed refusal. An UNCERTAIN refusal stays `dispatch_failed` and the entry is held (ADR-109). A later qualifying approval never re-arms a `dispatch_failed` claim. ADR-109's release exception is recorded against ADR-109 separately; it is out of scope here.
- **Inline merge.** A yielded entry's intent still blocks inline `merge_run` (ADR-109). The exit is a qualifying approval, or cancelling the run, until #4297's withdraw-intent verb.

**R4 — Evidence before dispatch.**
- Under the merge-intent lock, the fold and the authorization row come BEFORE step 7's `merge_verdict_recorded`, so a yield never leaves a verdict row for an unauthorized head. Append a chained audit row `merge_queue_authorized` (registered in `categories.go`). Its payload reuses the `predicate_snapshot` shape (`quorum.go`), plus:
  - per counted approval: the review id, commit, `submitted_at`, provider, immutable actor id and login, actor type, and resolved permission;
  - the observation time and the chain of clean-advance heads used;
  - the policy version: the cached workflow-spec hash plus the fired escalations;
  - the basis, the original Fishhawk approval and intent, the verify-pass reference, and `auth_method: artifact` with a forge-review channel (ADR-055).
- `merge_queue_merged` carries the authorization row's sequence. Inline `merge_run` writes `merge_queue_authorized` too, before its own claim. There is no claim-table column, so R4 needs no migration.
- Identity fields come from GraphQL (`__typename`, `databaseId`, `login`).

**R5 — Visibility (amends D7).**
- The queue read and `head_of_line` carry `yield_reason`, `yield_detail`, `yielded_at` and `next_step`: `approve_pr`, `dismiss_review` (a write-or-higher veto), `retry_after` (undetermined), or `configure_identity_provider`.
- `get_run_status.next_actions` names the remedy.
- A yield older than one pump tick is posted once to the PR or issue thread. That is a new issue-comment surface, so it updates `docs/issue-comment-surfaces.md` and `activityCategories`.
- Repeated observations are deduplicated within one yield episode; transitions and the resolution are recorded.

**R6 — Scope and portability.**
- **GitLab** is unsupported for merge-on-pass: `provider_unsupported` until approval-to-revision binding exists there (its approvals carry no SHA), per ADR-110 rule 3(c) and ADR-093 D9.
- **OAuth dependency, scoped.** `min_permission` and the write floor use the run's installation token, so they need no OAuth client. `member_of` still resolves through the boot-singleton identity provider (OAuth-gated). On a deployment without an OAuth client, a gate that declares `member_of` yields `provider_unsupported`. A missing installation, or an installation with no repository access, is `undetermined`.
- **No forge approval needed** where a workflow has no review approval gate (unchanged).
- **Merge-settle members check** (`merge_members.go`) keeps its looser rule. It is harmless, because it runs after an already-gated merge, and it is stated here.
- **Inline `merge_run` and the delegated `may_merge` arm** adopt R1/R2 in the same implementation, so the two rules do not diverge.

## Decision

**Accepted 2026-10-22** by the Captain (repository maintainer): **Option B**, i.e. R1–R6 as stated in the Recommendation (v4.1), decided now. It amends ADR-093 (D4, D6, D7 and the mechanics of Q1(a); Q1(a)'s ruling is kept). Implementation: the replanned #4619 (delivering #4623 and #4620), then #4297 and #4298.

## Consequences

- **Security.** One predicate, with a write floor and an attributable head chain, closes #4623 on every basis and every path. Untrusted accounts can neither unlock nor veto. Review volume cannot force a hold through the writers-only pre-filter; a truncated read is its own named reason.
- **Q1(a) kept.** A forge-kept approval survives clean advances; agent pushes break the chain. Where the forge dismisses stale approvals, a fresh qualifying approval at any head on the clean chain is needed.
- **Liveness, conditional.**
  - **Forges that keep approvals across clean advances:** an approval anywhere on the clean chain stays valid, so no re-approval is needed.
  - **Forges whose protection dismisses stale approvals or requires approval of the last push** (this repository): the bounded head-of-line wait gives a human a window to approve the verified head with the base frozen.
  - **After the window:** the entry yields and re-probes, so it never blocks its base.
- **Author residual.** On a single-approver repository, the voucher's own approval is admitted, recorded as an exception. A multi-approver gate with `not: [author]` excludes vouchers on the forge leg exactly as at the review gate.
- **Costs:**
  - migration 0109 (`yield_reason`, `yield_detail`, `yielded_at`);
  - a new installation-scoped permission read;
  - the advance parent check;
  - the head-of-line wait;
  - the entry-site enqueue-as-yielded change to `merge_run`;
  - two audit categories;
  - a GraphQL reviews read;
  - removal of `approvalDismissedCheck` from merge-on-pass;
  - R1/R2 adopted by inline and delegated merges;
  - the PR-thread surface.
- **#4620** collapses into R1(b)/(c) and closes with the implementation.
- **Implementation:** the replanned #4619, delivering #4623 and #4620, under merge-path serialization; #4297 and #4298 follow.
- **Record metadata.** `amends: ["ADR-093"]`, with ADR-093 gaining `amended_by`. `applies_to: []`, following ADR-110's crowding precedent.
- **Related:** ADR-055 (Phase 3 realized for queue merges), ADR-090, ADR-093 (amended), ADR-109, ADR-110, #4296, #4297, #4619, #4620, #4623.

## Amendment A1: R3 clarifications from the #4632 implementation (ratified by the Captain 2026-10-23)

**Context.** Implementing R3 (#4632, PR #4674, merged bbd3ace4) surfaced four cases R3 leaves open or gets wrong. The first officer ruled on each at the gate, and #4632 shipped those rulings, each pinned by a counterfactual (battery b8fa0b4a, 15/15 RED). This amendment makes them part of R3.

**Decision.** R3 is amended as follows. Everything else in R3 stands.

- **A1.1 Same-base head move (re-anchor).** When a yielded row's PR head moves on the SAME base, the row re-anchors by compare-and-swap on (`anchored_head_sha`, `yielded_at`). It KEEPS its marker, `yielded_at` and its approval-wait episode. R3's "not reset on readvance or re-admission" covers this case.
- **A1.2 Base change (new episode).** A change of a yielded row's base ENDS its wait episode. The ADR-092 rebase path re-enqueues the row at the TAIL of the new base's FIFO, unmarked, with a fresh `approval_wait_started_at`. R3's no-reset rule applies within one base. Carrying the marker across bases would key the CAS on a base the row no longer belongs to. A retarget gains no head-of-line position, the new wait is still bounded, and the in-lock fold still refuses an unapproved merge.
- **A1.3 Clearing a row-4 yield (forge refusal after an `approved` fold).** R3's clear rule ("when the fold would be `approved` and no qualifying veto exists"), read literally, re-admits a row-4 yield on the next re-probe. A row-4 yield exists because the fold was already approved, so that reading loops: one forge merge call and three audit rows per minute, indefinitely. Instead:
  - **(a) Forge state must change.** A `forge_review_required` yield clears only when the fold is approved AND a FRESH forge snapshot shows the row-4 entry condition gone: `reviewDecision` not REVIEW_REQUIRED or CHANGES_REQUESTED (a null `reviewDecision` keeps row 4's narrowing), and `mergeStateStatus` not BLOCKED. The snapshot is read even on a run with no approval gate. A failed or undetermined read leaves the row yielded (fail closed). Every other yield reason keeps R3's fold-approved rule.
  - **(b) One predicate.** That rule is the single clear predicate for every AUTOMATIC clear path: the pump's re-probe, the targeted re-probe and the approval webhook. An explicit operator `merge_run` may force one re-attempt per call.
  - **(c) No restarted backoff.** A row that re-yields for the SAME reason on the same base and head continues its episode's `yielded_at`, so its backoff keeps growing and never restarts. This bounds the worst case even when the forge snapshot and the merge call disagree.
- **A1.4 Re-probe due time.** A yielded row's re-probe is due when a backoff schedule point falls in (age − time since this process's previous sweep, age]. After a process start with no previous sweep, the window is (0, age]. A sweep that overruns its interval therefore cannot leave a row permanently not-due. The rule is process-local and needs no migration.

**Consequences.** No new migration or audit category. #4632 already implements A1.1–A1.4. Open residuals, tracked in #4675:
- an age or event bound that ends an episode after a genuine clear followed by a re-block, re-posting the yield notice;
- `next_actions` must not offer `merge_run` for a row-4 yield on an unchanged forge state, so a driver following it cannot poll the loop back.

### Amendment A2: per-entry re-probe due time and round-robin selection (2026-10-28)

**Ratified by the Captain on 2026-10-28.**

**Why.** Two parts of the R3 re-probe text are revised.

**What A2 replaces.** It replaces A1.4's process-wide due window, and R3's "a bounded re-probe runs on the oldest yielded row per base per pump".

**The defects.**
- **Starvation (gpt-6-astra review of #4748, run e8fb6a85; also #4675).** Selecting the oldest-by-`yielded_at` due row, under the 16-interval backoff cap, lets blocked older rows win every sweep once their schedule points align. With 17 or more aligned yielded rows, a younger row that has become eligible is never selected.
- **The process-wide window** advances for every row on each sweep, including rows the sweep never probed.

**A2.1 Per-entry due time.** A yielded row's re-probe is due when a backoff schedule point falls in (age at this row's last service, age]. A row never serviced in this process uses (0, age].
- A row's service mark advances ONLY when that row is actually probed.
- The mark is process-local, so the rule needs no migration. A restart resets every row to never-serviced, which leaves every row due and the selection rule below still bounded.

**A2.2 Round-robin selection.** Among the due yielded rows of a base, the pump re-probes the row with the OLDEST service mark. Never-serviced rows go first, and ties are broken by `yielded_at`, oldest first.
- **The bound:** with N due rows on a base, every due row is probed within N sweeps.
- **Unchanged:** the immediate re-probe on a `merge_run` re-invoke or an approval webhook for that run, and A1.3's single clear predicate and its no-restart backoff.

**Evidence the implementation must carry.**
- A test with N = 17 aligned yielded rows (16 permanently blocked, 1 eligible) asserting the eligible row is probed within N sweeps.
- A counterfactual: reverting to oldest-by-`yielded_at` selection turns that test RED.

### Amendment A3: retryable merge-gate legs wait or yield; they never drop merge intent before dispatch (2026-10-30)

**Ratified by the Captain on 2026-10-30.** Reviewed over six rounds by fable and codex (gpt-6-astra). Final verdicts: approve_with_concerns from both, with every concern folded into this text. Motivated by #4780.

**What A3 amends.**
- **R3**: precedence row 1, for a closed set of legs; the admission bullet, for the same legs.
- **A1.3**: (a)'s last sentence ("every other yield reason keeps R3's fold-approved rule"), narrowed by A3.4(3); (b)'s single clear predicate; and (c)'s same-reason / same-head continuation key, which A3.7 replaces.
- **A1.1 / A1.2**: the anchor's lifetime, added to by A3.7.
- **R3 / A1.1**: the marker compare-and-swap key moves from (`anchored_head_sha`, `yielded_at`) to (`anchored_head_sha`, `yield_revision`) (A3.6).
- **R5**: the `next_step` and `yield_reason` vocabulary, and the matching OpenAPI enums.

**What stands.** Everything else stands:
- R2, R4 and A2;
- ADR-109's dispatch ledger;
- R3's rule that a wait is never held inside the merge-intent lock.

Every A3 path runs before any dispatch claim. After dispatch, nothing changes:
- R3 rows 4 and 5 still apply: a typed release yields as row 4, and anything else ejects;
- an UNCERTAIN claim is never re-armed (ADR-109).

**Why: confirmed mechanisms in today's tree.**
1. **Merge-on-pass drops on a red check or a pending review.** Only an undeterminable result or `checks_not_green` holds (`merge_queue_merge.go:598-603`).
2. **Admission drops on any determined row-1 reason, even beside a veto.** Only an exclusive veto yields; an undeterminable result unadmits (`merge_candidate_queue.go:752-773`).
3. **Any failing suite fails its check name** (`mergeoutcome/ci.go:207-248`). A concurrency-cancelled workflow run concludes its aggregate job (`CI Pass`, `ci.yml:385-399`) `failure`, and that red outweighs a green sibling run.
4. **Zero reported check runs HOLD with no bound.** The result is undeterminable, which becomes `held: not_mergeable_yet`, which has no expiry (`merge_queue_eligibility.go:538-540`, `merge_queue_yield.go:204`).

The motivating incidents (#4780, 2026-10-28/29) were five green-pass entries that lost their intent or stalled. Each needed operator diagnosis and a manual `merge_run`. Nothing unsafe merged. #4803 is an overlap-guard hold, not a drop, and is out of scope.

**A3.1 The retryable merge-gate set (closed).** These legs are RETRYABLE for an entry that carries merge intent. "Retryable" does not mean temporary: a yielded row may stay yielded until someone repairs it, while its base proceeds.

**Check outcome.** Judged in the PR-head merge-gate leg only; the post-merge CI observation (E82.2) is unchanged.
- **Unit of judgement.** For each required check name, each check SUITE contributes its LATEST attempt: its highest check-run id for that name.
- **Pass set.** A latest attempt passes only with a conclusion in the classifier's pass set (`success`, `neutral`, `skipped`).
- **Pass rule.** The check passes only when EVERY suite's latest attempt passes. Cross-suite aggregation is today's conservative rule, unchanged: a suite never hides another suite's failure.
- **What changes inside a suite.** Today the highest COMPLETED attempt is selected (`ci.go:250`), so an older pass stands in for a newer pending attempt. The merge-gate variant ADDS a rule: the latest attempt counts whether or not it has completed, so a newer pending attempt blocks an older pass. This is a variant or parameter of the classifier. E82.2's post-merge observation keeps its current semantics.

**(a) Checks red.** Some suite's latest attempt is COMPLETED and either:
- in the failing set (`failure`, `timed_out`, `action_required`, `startup_failure`); or
- `cancelled`. A completed cancellation is terminal. The remedy is a re-run, which creates a newer attempt in that suite.

**(b) Checks pending or indeterminate.** Applies at merge-on-pass only. Covers any check outcome that is neither pass nor (a):
- a queued or in-progress latest attempt;
- `stale` or any unknown conclusion;
- a required name with no runs, including zero check runs at the head;
- a read that failed, was truncated or came back empty;
- no required-check list while observed checks are not all green (today's `checks_not_green`).

Across suites and across check names, (a) wins over (b).

**(c) Review pending.** A Fishhawk-side review leg fails with `review_pending` or `review_coverage_incomplete`. An intent-bearing entry had a settled, covered round when `merge_run` recorded intent, so this comes from a round re-opened or started after that.

**Everything else keeps existing row-1 behaviour.** That includes:
- merge conflict, unmergeable, closed PR;
- a head not recorded by Fishhawk;
- the overlap guard;
- an open blocking concern;
- undeterminable review, spec, acceptance or concern reads.

**A3.2 Merge-on-pass (step 4, pre-lock, pre-claim).**
- **(a) yields immediately:** `yield_reason: merge_checks_red`, `next_step: retrigger_ci`. The remedy names `fishhawk_retrigger_ci`, then a re-run of `merge_run`.
- **(b) and (c) are HELD.** HELD means an await-kind outcome with an R3 bounded wait, not `mergeOnPassHeld`. The window and its budget follow A3.7. At expiry the yield reuses `mergeOnPassEnforceExpiredWait`'s protections: entry re-read, in-flight registry, dispatch ledger.
  - Its expiry test is the merge-gate window of the CURRENT admission epoch. That window starts at the epoch's FIRST qualifying (b) or (c) hold. The start is persisted in a new column, set atomically when the token is spent, and kept for every later (b) or (c) observation in that epoch. It is never `admitted_at`, so advancement and verification time do not consume it. It is never `approval_wait_started_at`: a (b) or (c) hold neither sets nor reads that column.
  - The window's duration is `FISHHAWKD_MERGE_QUEUE_GATE_WAIT`. It defaults to the approval wait.
  - The yield keeps the step-4 reason. A veto masked at step 4 surfaces at the next re-probe.

  The yield is:
  - **(b):** `merge_checks_pending`, `next_step: retry_after`;
  - **(c):** `merge_review_pending`, `next_step: retry_after`.
- **Bound.** (b) bounds every checks-read failure at merge-on-pass.

**A3.3 Admission.** Evaluation order is unchanged: the review leg returns before the veto read. For an entry carrying merge intent, the FIRST matching row decides:

| # | At admission | Outcome |
|---|---|---|
| 1 | Any undeterminable result | Unadmit, intent kept (unchanged) |
| 2 | Any determined NON-retryable row-1 reason | Drop (unchanged) |
| 3 | An observed veto, alone or with (a) | Veto yield, R3 row 2 |
| 4 | (a) and/or (c), no observed veto | Merge-gate yield, reason by A3.5 tier 2 |

- **The yield** goes through R3's Yield compare-and-swap at the row's admission epoch. The row keeps its FIFO position and episode state.
- **A veto masked by (c)** at admission (the review leg returns first) surfaces at the next re-probe.
- **Unchanged:**
  - An entry WITHOUT recorded intent keeps today's refusal.
  - The `merge_run` entry site still refuses a red check, or zero check runs, with a hint.

**A3.4 One clear predicate, merge-gate aware (extends A1.3(b)).** An automatic clear clears a yielded intent-bearing row only when, at that clear, ALL hold:
1. A1.3's predicate holds: the fold is approved and there is no qualifying veto.
2. If the row-4 flag is set (A3.6), a FRESH snapshot taken at this clear satisfies A1.3(a).
3. Every A3.1 leg PASSES at the anchored head under the MERGE variant's rules: green required, zero runs is not green, and a failed, truncated or empty read does not pass.

The automatic clears are the pump re-probe, the targeted re-probe, the approval webhook, and the A3.8 trigger. The clear is a compare-and-swap on (`anchored_head_sha`, `yield_revision`) (A3.6).

Head discovery on a re-probe does not depend on the approval gate. A gate-less row still reads the PR head, so a repair push re-anchors the row (A1.1), and the row can clear once the new head passes.

**A3.5 Reason precedence for observed blockers (all sites).** The stored `yield_reason` names the highest current tier:
1. a veto;
2. a retryable merge-gate leg, ordered (a) > (b) > (c);
3. the row-4 condition, when the flag is set and a fresh snapshot shows it present (`forge_review_required`);
4. otherwise, any non-approved fold, keeping R2's result and reason mapping (`absent`, `dismissed`, `undetermined`, `reviews_truncated`, `provider_unsupported`).

When both tier 3 and tier 4 hold, tier 3 takes precedence. Re-probes move the reason through these tiers with R3's in-place update, which keeps the episode, `yielded_at` and the anchor. `yield_detail` names any lower tier it has read.

**A3.6 The row-4 flag and the yield revision (columns).**
- **The flag.** When an episode records a row-4 yield, the row's persisted row-4 flag is set. No reason transition clears it. It clears only on a SUCCESSFUL automatic clear (A3.4, with condition 2 met at that clear), or when the episode ends. A forced clear (A3.9) is not automatic, so the flag survives it.
- **The revision.** A monotone `yield_revision` advances on every yield, clear, re-yield, reason change, flag change and re-anchor. EVERY marker write compare-and-swaps on it. A delayed re-probe therefore cannot clear a newer yield, overwrite a newer reason, or bypass a newer obligation.

**A3.7 Episode, windows and the hold cap (replaces A1.3(c)'s continuation key).**
- **Episode.** Keyed by the row's base REF. A head move re-anchors (A1.1). A base-tip advance does not end it. A1.2's base-ref retarget, or the entry settling, ends it.
- **Approval window (tiers 3 and 4).** `approval_wait_started_at` is set by the episode's first approval-fold wait and is RETAINED across a clear or re-admission. A later approval-fold block in the same episode does not get a fresh frozen-base window.
- **Merge-gate window ((b) and (c)).** Each admission epoch can open ONE fresh bounded window. Every re-advance legitimately restarts CI, and while a window is open the base is frozen (R3 head of line). An entry whose CI completes inside one window therefore merges on that hold.
- **Hold budget (a token bucket, persisted per row).** Opening a window spends one token. An epoch whose (b) is followed by (c) spends only one.
  - Capacity: `N` = 3.
  - Refill: one token per `R` = 60 min that the row spends NOT holding (yielded or waiting), capped at `N`.
  - With no token, a (b) or (c) yields at once.
- **What the budget guarantees.**
  - **Bounded occupancy.** Over any interval starting with a full bucket, an entry opens at most `N + floor(T_nonholding / R)` merge-gate windows. Each window is bounded by `FISHHAWKD_MERGE_QUEUE_GATE_WAIT`. One window per `R` is the sustained refill rate, not a rolling cap.
  - **Renewed opportunity.** An entry whose budget is spent always regains a usable window. It merges on a hold in which CI and review complete within that window and approval holds. A3 does not claim eventual merge in general; the residual unbounded holds below still apply.
- **Continuation.** A re-yield continues from the entry's LAST CLEARED row on the same base ref, whatever the reason or head. It keeps that row's `yielded_at` backoff (A1.3(c)), the hold budget, and the approval anchor. Store and audit lookups that key today on head and base SHA, or on the same reason, change to this key.
- **A2 is unchanged:** service marks and round-robin selection.

**A3.8 Check-run trigger, bounded.** A `check_run` completed webhook triggers a targeted re-probe when the run's NAME is in the required set and its head matches the anchored head of an intent-bearing yielded row. The trigger is:
- single-flight per run;
- deduplicated by check-run id;
- subject to the approval webhook's per-run minimum interval;
- coalesced, never dropped. A trigger that arrives while a probe is in flight, or inside the minimum interval, schedules exactly one follow-up probe.

The trigger's name filter is the snapshot's contexts. When the snapshot is nil or empty, the filter is every observed check name. This fallback applies to the trigger filter only. It does not change A3.1's classification: under a nil snapshot, a red optional check is still (b).

Each re-probe adds one check-runs listing to its cost, bounded by that interval and the sweep's backoff. The existing triggers remain: re-running `merge_run`, and the approval webhook.

**A3.9 Operator force.** An explicit `merge_run` still forces one re-attempt per call. As today, it bypasses ONLY A1.3(a)'s fresh-snapshot condition. It never bypasses:
- A3.4(3);
- verify, the fold or the veto;
- any dispatch safety check;
- A1/A2 episode, dedupe and backoff rules.

**Consequences (stated decisions).**
- **One approval window per episode; merge-gate windows from a refilling budget.** The approval wait (tiers 3 and 4) gets R3's frozen-base window once per episode. Merge-gate holds draw on A3.7's token bucket: a burst of `N` = 3, then one window per `R` = 60 min. An entry that keeps getting re-advanced on a busy base costs one CI run per cycle, and it merges once a window lands with its CI finishing inside it.
- **Retryable means repairable, not self-clearing.** A yielded row can wait indefinitely for CI repair or a review, while the base proceeds.

**Residuals.**
- Admission's checks-read uncertainty still unadmits (row 1). That is unchanged. Because unadmission stops the pump (`merge_candidate_queue.go:445`), it can stall the WHOLE base, not just that row. Follow-up.
- Undeterminable review, spec, acceptance or concern reads keep today's unbounded hold (follow-up).
- A re-opened round that posts a blocking concern drops as row 1 after A3 waited for it.
- R5's posted-once yield notice is keyed on `yielded_at`. Under the A3.7 continuation, a genuinely new blocker after a clear is NOT re-posted. This widens A1's #4675 residual; the read surface and `next_actions` stay current.
- A concurrency-cancelled sibling run (#4780's fifth shape) now yields `merge_checks_red` with `retrigger_ci`. It does not merge through. Superseded-run tolerance was deliberately not adopted, because a run that was cancelled does not prove it was superseded.
- Out of scope:
  - the overlap-guard hold on a verify-only pass (#4803);
  - an inline `merge_run` that merges ahead of a live entry (#4832);
  - the runner's fetch race (#4835).

**Costs.**
- One migration adding these columns to `merge_candidate_queue_entries`:
  - `row4_flag`;
  - `yield_revision`;
  - the hold-budget tokens and their refill timestamp;
  - the per-epoch gate-window start.
- A new knob, `FISHHAWKD_MERGE_QUEUE_GATE_WAIT`.
- OpenAPI additions to the `MergeQueueYieldReason` and `MergeQueueNextStep` enums (`v0.openapi.yaml`), plus `v0.md`.
- Each re-probe makes one extra check-runs listing.

**Evidence the implementation must carry.** Each item is a test, and each control has a counterfactual that turns its test RED:
- (a) yields immediately, and (b) and (c) hold and then yield at expiry. None of them ever settles `dropped`.
- Every row of the admission table, including a combined (a)+(c).
- Check judgement:
  - an older pass with a newer pending attempt in the same suite reads pending;
  - an independent red suite blocks;
  - a completed `cancelled` latest attempt reads (a);
  - `stale` reads (b);
  - (a) wins over (b) across suites.
- A3.4: an approved fold with a failing leg does not clear.
- A3.5: each tier transition, including row-4 → merge-gate → (CI green, BLOCKED) → `forge_review_required`.
- A3.6:
  - row-4, then merge-gate, then BLOCKED briefly gone while checks are red, then green with BLOCKED back: stays yielded;
  - a delayed re-probe holding an old `yield_revision` fails its clear compare-and-swap after a newer re-yield.
- A3.7:
  - a merge-gate hold after a re-advance gets a fresh window;
  - with the budget empty, a (b) yields at once;
  - after `R` of non-holding time, one token is restored, and a recovered entry then merges on its next hold (eventual progress);
  - a (b) then (c) in one epoch spends one token;
  - a (b) hold never sets or reads `approval_wait_started_at`;
  - delayed verification followed by CI completing inside the actual hold window merges, because the window starts at the first hold, not at admission;
  - a flap (red, then a green clear, then red) continues the backoff and the hold count;
  - the approval anchor is retained across a clear;
  - a head move keeps the episode;
  - a base retarget resets it;
  - with no approval gate, a yielded red H1 followed by a green repair push H2 is re-anchored and cleared.
- A3.8:
  - required-name filtering, including the nil-snapshot fallback;
  - coalescing;
  - dedupe;
  - a trigger inside the interval schedules one follow-up and is not dropped.
- A3.6: a delayed re-probe cannot overwrite a newer tier-2 reason.
