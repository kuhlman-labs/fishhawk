---
id: ADR-093
title: "A Fishhawk-owned merge queue: extend ADR-092's per-base serializer so the queue advances, verifies and MERGES the head of line, provider-agnostic, batch size 1"
status: accepted
date: 2026-10-10
issue: https://github.com/kuhlman-labs/fishhawk/issues/4224
supersedes: ["ADR-090", "ADR-092"]
superseded_by: []
applies_to: []
---

# ADR-093: A Fishhawk-owned merge queue: extend ADR-092's per-base serializer so the queue advances, verifies and MERGES the head of line, provider-agnostic, batch size 1

## Context

ADR-090 and ADR-092 together give Fishhawk most of a merge queue, but they leave its most important property to the operator.
- **ADR-090:** a Fishhawk-dispatched merge requires an up-to-date head (D1), and a head produced by a base advance, a conflict resolution or a fan-in must pass a runner verify-only pass bound to that exact head SHA (D2/D3).
- **ADR-092:** at most one live pass per `(repository, base ref)`, the others held FIFO, re-anchored and auto-advanced at admission, and run through a `local-verify` group. Its implementation is #4200.
- **What's left to the operator:** the MERGE itself. After a pass, a human (or the driving session) must re-approve and call `fishhawk_merge_run`. Between the pass and that call, the base can move again (ADR-090 D8 / Q4, the gate-to-merge window).

In campaign efc57a94 (2026-10-09/10), the driving session ran this as a hand-operated queue across ~15 merges:
1. rebase the next PR;
2. dispatch its pass;
3. wait about 15–40 minutes;
4. re-approve;
5. merge;
6. repeat.

Every merge made every other open PR behind. Correctness depended on the driver keeping exactly one live pass and merging a passed PR immediately. Throughput was bounded by the driver's attention as well as by the verify time: an idle turn stalled the line for 5.5 hours.

**This repository has already decided against forge-native queues twice:**
- **ADR-043 Revision 2** adopted "Serialize merges per repo in the backend — Fishhawk's own merge queue/lock, NOT a forge feature", and closed the GitHub merge queue (#1295) as non-viable because "Fishhawk is provider-agnostic (may not be GitHub; a queue may not be enabled)".
- **ADR-090 (Option B)** rejected leaning on forge branch protection for the same reason.

Neither built the Fishhawk-owned queue. ADR-090 Q4 deferred the serializer, and ADR-092 built the serializer for passes but not for merges.

## Options

- **A. Status quo plus ADR-092.** Passes serialize automatically (#4200); an operator still merges each PR.
  - The cheapest option.
  - The gate-to-merge window and the operator-attention bottleneck remain.
  - A merge line of N PRs still needs N human merge acts at the right moments.
- **B. Forge-native queues (GitHub merge queue, GitLab merge trains), with Fishhawk as the required verifier check.**
  - The forge owns ordering, batching and the atomic merge.
  - It is provider-specific, the objection ADR-043 Rev 2 and ADR-090 already ratified.
  - It depends on operator enablement: branch-protection or ruleset configuration, plan tier, and on GitHub an organization-owned repository. This repository is user-owned, so it could not dogfood it.
  - Candidate commits are created by the forge on its own refs, so the ADR-035 reported-head ledger would read them as FOREIGN. The verify pass would need new lineage attribution.
  - A local runner would have to react to forge queue events.
- **C. A Fishhawk-owned merge queue, batch size 1, extending ADR-092 (RECOMMENDED).**
  - ADR-092's per-base held-pass queue becomes the merge queue: the head of the line is advanced, verified and then MERGED by Fishhawk, with no operator act between pass and merge.
  - The behaviour is the same on every forge and needs no forge feature.
  - It closes ADR-090 D8's window for queue-dispatched merges.
- **D. C plus speculative batching** (test several queued PRs combined, bisect on failure).
  - Higher throughput when verify is slow.
  - Much more machinery: combined-tree construction, bisection, attribution of a red to a member, and local-runner capacity for parallel speculative passes.
  - Deferred, not rejected.

## Recommendation

**Option C.** It realises the per-repo backend serializer ADR-043 Rev 2 adopted and ADR-090 Q4 deferred, on ADR-092's machinery, and it keeps Fishhawk provider-agnostic. Batching (D) is a later, measured decision.

## Decision

**Accepted (2026-10-10). Ratified by Brett (repository maintainer) on the explicit instruction "ratify as recommended".** The agent records the maintainer's decision; it is not self-approval. The decision points were drafted by the overseer session from the hand-operated merge walk in campaign efc57a94. The maintainer's rulings on the ratification questions are recorded at the end of this section and bind the implementation.

**This ADR partially supersedes ADR-092** (D4, the merge-to-pass hand-off, which becomes merge-on-pass) **and ADR-090** (D8, for queue-dispatched merges). ADR-092 D1–D3 and D5–D6 and ADR-090 D1–D7 otherwise stand. It realises ADR-043 Revision 2 item 1.

Decision points:

**D1 Enqueue.**
- `fishhawk_merge_run` on a run whose merge is otherwise eligible ENQUEUES it, per `(repository, base ref)`. It does not merge inline when the PR is behind or its head lacks a verified pass. Otherwise eligible means: review gate approved, acceptance merge-eligible, required checks green at head, and no open blocking concerns.
- An up-to-date PR with a valid pass for its exact head and base may merge inline, as today. The queue is the path for everything else.
- Enqueue is the operator's merge intent and is audited (`merge_queue_entered`).

**D2 Order.**
- FIFO by enqueue time.
- The operator promote verb (#4191) may move an entry, audited.
- At most one entry per base is in flight (advancing, verifying or merging).

**D3 Head-of-line cycle.**
1. Advance the head-of-line branch to the current base tip through the `rebase_run_branch` machinery. A no-op when it is already up to date.
2. Run the ADR-090 verify-only pass bound to the resulting head SHA, through the ADR-092 `local-verify` group.
3. On a pass, immediately re-probe the base. If the base tip is still the tip the pass verified against, Fishhawk merges through the forge merge API with the head pinned to the verified SHA (GitHub `sha`, GitLab `sha`). If the base moved, re-advance and re-verify, keeping the entry at the head of the line.

Base-tip pinning is not atomic on every forge: GitHub's merge API pins the head, not the base. The window shrinks from "operator latency" to "probe-to-merge latency", which is seconds. Strict up-to-date branch protection closes it completely where an operator enables it (ADR-017 posture); Fishhawk does not require it.

**D4 Failure.**
- **Red pass:** eject from the queue and route per ADR-090 D6: an automatic fix-up when delegated, otherwise refuse naming `fishhawk_fixup_stage`.
- **Advance conflict:** eject to the existing conflict-resolution path.
- **Re-entry:** an ejected entry re-enqueues on the operator's next `merge_run`, or automatically after a delegated fix-up passes. It joins at the TAIL (Q2).
- **Forge merge refusal** (approval dismissed, required check missing): eject naming the forge's reason. It never retries in a loop.

**D5 External merges.**
- A merge to the base outside the queue (UI, another tool) is observed through the existing base-advance and merge-observation paths. It re-anchors the head of the line under ADR-092 D2.
- The queue does not block external merges. That is ADR-090 D8 / ADR-017's boundary, unchanged.

**D6 Approval survival (Q1).**
- An advance push can dismiss forge review approvals under "dismiss stale approvals". Fishhawk cannot re-approve, because self-approval is prohibited and approver identity is `github:<login>`.
- The queue therefore treats the run's Fishhawk review-gate approval as transferring across a CLEAN advance plus a green pass (ADR-043's verdict-transfer principle).
- A dismissed FORGE approval is reported as a named eject reason (D4) with the operator remedy.

**D7 Visibility.**
- A queue read surface (MCP and HTTP) per base: entries, positions, head-of-line state, the anchored head vs the current head, and an ETA from recent pass durations.
- Gate view and `get_run_status` show a run's queue position.
- Audit rows: `merge_queue_entered`, `merge_queue_advanced`, `merge_queue_verified`, `merge_queue_merged`, `merge_queue_ejected`, `merge_queue_promoted`, each registered in `backend/internal/audit/categories.go`.

**D8 Durability.**
- Queue state is persisted, beside `stage_concurrency_slots` or ADR-092's held-pass rows.
- A fishhawkd restart resumes the head-of-line cycle from its last recorded step. An in-flight pass is recovered by the existing reap and re-dispatch paths, and the restart-blocker guard reports a live head-of-line merge.

**D9 Scope.**
- Batch size 1.
- Speculative batching (Option D) is revisited when measured queue wait exceeds an agreed threshold (Q4).
- GitLab follows when ADR-090's behind-probe covers merge requests; until then GitLab runs keep ADR-090's stated residual.

**Supersession (Q3):** this ADR would partially supersede ADR-092 D4 (merge-to-pass hand-off becomes merge-on-pass) and ADR-090 D8 for queue-dispatched merges. It would realise ADR-043 Revision 2 item 1.

**Ratification rulings (maintainer, 2026-10-10).**
- **Q1 — approval transfer: (a), approval transfers.** A run's Fishhawk review-gate approval carries across a CLEAN base advance plus a green verify-only pass bound to the resulting head, so the queue merges with no human re-approval. A conflict-resolution advance does not transfer; it ejects to the existing re-review path. A dismissed FORGE approval is a named eject reason (D4/D6).
- **Q2 — re-entry at the TAIL.** An ejected-and-fixed entry rejoins at the tail. The operator promote verb (#4191) may move it, audited.
- **Q3 — partial supersession as stated:** ADR-092 D4 and ADR-090 D8 (for queue-dispatched merges). Both stay `accepted` and record this ADR in `superseded_by`.
- **Q4 — batching trigger:** Option D (speculative batching) is reopened when the median queue wait across a campaign exceeds 2 hours.

**Ratification questions (as drafted):**
- **Q1 — approval transfer.** Is a run's Fishhawk review-gate approval allowed to carry across a clean base advance plus a green pass, so the queue can merge with no human re-approval? Or must every advanced head be re-approved by a human, which would keep a human act per merge and limit the queue to ordering plus verification?
- **Q2 — re-entry position.** Does an ejected-and-fixed entry rejoin at the tail, or keep its original position?
- **Q3 — supersession.** Partial supersession of ADR-092 D4 and ADR-090 D8 as stated?
- **Q4 — batching trigger.** What measured condition reopens Option D (e.g. median queue wait over 2h across a campaign)?

## Consequences

- **Throughput and attention.**
  - A merge line no longer needs an operator act per PR, only one `merge_run` (enqueue) per PR at approval time.
  - The line advances back to back at the rate of one verify pass per PR.
  - An idle driver session no longer stalls it.
- **Correctness.**
  - For queue-dispatched merges, what lands is the head that verified, against the base it verified on, up to the D3 probe-to-merge window of seconds. ADR-090 D8's operator-latency window closes.
  - External merges remain ungated (unchanged boundary).
- **New ownership.**
  - Fishhawk now performs merges as a queue, not on an operator call.
  - Failures in ordering, ejection and recovery become product bugs. They used to be operator errors.
  - It needs the D7 visibility and D8 durability to be trustworthy.
- **Provider-agnostic.** No forge feature, plan tier or repository setting is required. Strict branch protection remains an optional hardening where available.
- **Residuals.**
  - The D3 probe-to-merge window on forges without base pinning.
  - External merges.
  - Forge approval dismissal surfaces as an eject, not a silent stall.
  - Batch size 1 keeps throughput bound by verify time until Option D.
- **Implementation.** It follows #4200, ADR-092's serializer, which is a prerequisite and already scheduled. Likely slices:
  1. the persisted queue plus enqueue/read surfaces;
  2. the head-of-line cycle with merge-on-pass;
  3. eject, re-entry and the promote integration (#4191);
  4. durability and restart-blocker integration.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2345} -->
