---
id: ADR-092
title: "Serialize merge-candidate verify passes per repository, re-anchor a queued pass at admission, and admit passes through a local-verify group (amends ADR-090)"
status: accepted
date: 2026-10-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/4202
supersedes: ["ADR-090"]
superseded_by: ["ADR-093"]
applies_to: []
---

# ADR-092: Serialize merge-candidate verify passes per repository, re-anchor a queued pass at admission, and admit passes through a local-verify group (amends ADR-090)

## Context

ADR-090 (#4170, ratified 2026-10-09) made a Fishhawk-dispatched merge require two things: an up-to-date head (D1), and, for a head produced by a base advance, a conflict resolution or a fan-in, a passing runner verify-only pass bound to that exact head SHA (D2/D3). Two of its choices proved wrong on the first live campaign that ran under it (efc57a94, 2026-10-09):

- **Q4:** "No per-repo merge serializer is required now … revisited only if the window proves material in practice".
- **Consequences:** the pass "re-opens the implement stage, so on a local runner it is admitted through the implement concurrency group (ADR-087)".

**Observed (#4200, and the comment there of 2026-10-09):**

1. **Passes wait behind full implements.** A verify-only pass runs no agent and makes no commit or push, yet it took an implement slot and queued FIFO behind full 40–65-minute agent implements. The pass for #4183 (PR #4192) queued at position 7, #4178's (PR #4196) at 6, #4163's (PR #4197) after them, and #4177's (PR #4198), the fix for a flake that had just discarded a 65-minute implement (#4190), at 6. Merge-ready PRs waited hours.
2. **Every merge invalidates every queued pass: O(N²) passes.**
   - A pass anchors `expected_head_sha` when it is triggered. Each merge makes every other open PR behind.
   - Re-advancing a PR whose pass is still queued is refused ("stage is in state pending") and records no new trigger, so the queued trigger still names the old head.
   - When it is admitted, the runner's tip check settles it `not_executed` / `head_moved`. The next up-to-date rebase then triggers a fresh pass at the BACK of the queue.
   - With N mergeable PRs, each merge invalidates the other N−1 pending passes.
   - The driver fell back to holding exactly one live pass at a time by hand.
3. **A false concurrent-push warning on every rebase (#4199, 3 of 3).** It is a read-after-write symptom on the same path, and is tracked separately.

The ADR-089 D6 first-officer design had already planned a `local-verify:<host>` concurrency group, limit 1, so that verification jobs "never consume an implement slot or preempt one".

## Options

- **A. Keep ADR-090 as is and serialize by operator discipline.** No build cost. Throughput collapses on multi-PR waves, and correctness of order depends on the driver.
- **B. Re-anchor or replace the trigger only.** Removes the wasted `head_moved` cycles, but a merge still invalidates every in-flight pass. Passes still wait behind implements.
- **C. Serialize merge-candidate passes per repository base, re-anchor at admission, and admit passes through `local-verify` (RECOMMENDED).** At most one live pass per base, so a merge can only invalidate passes that have not started. Each admitted pass verifies the PR's current head, and passes never queue behind agent work.
- **D. Full merge queue, with Fishhawk owning merge order and atomically merging the verified head** (ADR-043 Rev 1's shape). This closes the D1 gate-to-merge window entirely, but it is a larger ownership change. It is deferred.

## Recommendation

**Option C**, with the decision points below. It keeps ADR-090's guarantees and changes only how passes are scheduled and anchored.

## Decision

**Accepted (2026-10-09). Ratified by Brett (repository maintainer) on the explicit instruction "ratify as recommended".** The agent records the maintainer's decision; it is not self-approval. The decision points were drafted by the overseer session from the live ADR-090 walk in campaign efc57a94. The maintainer's rulings on the ratification questions are recorded at the end of this section and bind the implementation.

**This ADR partially supersedes ADR-090:** its Q4 ruling (no serializer now) and its Consequences bullet that admits passes through the implement concurrency group. ADR-090 D1–D8 otherwise stand.

**D1 One live pass per repository base.**
- At most one merge-candidate verify pass is live (admitted or running) per `(repository, base ref)`.
- Others are held in a per-base FIFO, ordered by the time they became eligible: their PR is at review approval and needs a pass.
- A held pass consumes no runner slot.

**D2 Re-anchor at admission.**
- When a held pass is admitted, it reads the PR's current head and the current base tip.
- If the head is behind, it first advances the branch through the existing `rebase_run_branch` machinery, then verifies the resulting head.
- `expected_head_sha` is bound at admission, not at trigger.
- A rebase on a PR whose pass is held re-anchors that same queue entry. It does not refuse, and it does not re-queue at the tail.

**D3 Own concurrency group.**
- Admitted passes run through a `local-verify:<host>` concurrency group (ADR-087 machinery), default limit 1. This mirrors ADR-089 D6 and is shared with its verification jobs.
- They never take or wait for a `local-implement` slot. The per-repository verify lock still serialises the actual verify run against implement-stage verifies under the existing #3315 rules.

**D4 Merge-to-pass hand-off.**
- When a PR merges, the base's next held pass is admitted under D2.
- A held pass whose PR is closed, merged externally or whose run is cancelled is dropped, with an audit row.

**D5 Visibility.**
- Read surfaces show a held pass's per-base queue position and live/held state: `get_run_status`, `fishhawk_merge_run`'s immediate status, and gate_view.
- A pass's anchoring head is shown alongside the PR's current head.

**D6 Unchanged.**
- ADR-090 D1 (the up-to-date requirement) still applies at merge dispatch.
- D2 (verify bound to the exact head) is now satisfied by construction, because the admitted pass verifies the head it anchored.
- D6's routing of a red result and D7's fail posture are unchanged.
- The remaining gate-to-merge window, a merge dispatched outside Fishhawk between pass and merge, stays ADR-090 D8's stated residual. Option D is deferred.

**Ratification rulings (maintainer, 2026-10-09).**
- **Q1 — scope of "base":** per `(repository, base ref)`.
- **Q2 — `local-verify` limit:** default 1. The verify lock already serialises the verify itself.
- **Q3 — auto-advance at admission:** yes. An admitted pass whose PR is behind advances it through the existing `rebase_run_branch` machinery, then verifies the resulting head.
- **Q4 — ordering:** by eligibility time. The operator promote verb (#4191) may move a held pass ahead, audited.

## Consequences

- **Throughput.** A wave of N merge-ready PRs costs N passes run back to back, not O(N²). Passes never wait for agent implements.
- **New state.** There is a per-base held-pass queue, persisted (likely beside `stage_concurrency_slots`), and audit rows for hold, admit, re-anchor and drop.
- **Operator change.** The driver no longer hand-serializes passes, and `fishhawk_rebase_run_branch` on a held PR re-anchors instead of refusing.
- **Residuals.**
  - The D1 window for merges outside Fishhawk remains (ADR-090 D8).
  - A full merge queue (Option D) is deferred.
  - #4199's read-after-write false warning is tracked separately.
- **Implementation.** #4200 (re-scoped to this ADR) and a follow-up for the held-pass queue. #4191's promote verb can act on the held queue (Q4).
