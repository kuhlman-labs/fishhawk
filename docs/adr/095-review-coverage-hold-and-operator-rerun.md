---
id: ADR-095
title: "Review-coverage hold for decomposed parents, plus an operator re-run verb with its own same-head dedup bypass marker, distinct from ADR-091 D6's boot-only marker (amends ADR-091)"
status: accepted
date: 2026-10-10
issue: https://github.com/kuhlman-labs/fishhawk/issues/4287
supersedes: []
superseded_by: []
amends: ["ADR-091"]
applies_to: []
---

# ADR-095: Review-coverage hold for decomposed parents, plus an operator re-run verb with its own same-head dedup bypass marker, distinct from ADR-091 D6's boot-only marker (amends ADR-091)

## Context

ADR-091 D6 made the #797 same-head dedup bypass (and the synchronous diff-secret concern skip) **internal-only**: only the boot-sweep re-dispatcher sets the re-dispatch marker, carried in an unexported context key. "Any future change that exposes the marker to a request path violates this ADR."

ADR-091 also noted that an operator path needing a second review of the same head would be "a new design". #4219 (campaign 9275cbb2, 2026-10-10/11) needed exactly that.

**What happened:** on decomposed parent d3b1d915 (#4165), the consolidated implement review FAILED on infrastructure. The Claude reviewer was OOM-killed and Codex was quota-exhausted. Slice 1 had no review of its own. The gate still reported `awaiting_merge`, so a slice could reach a merge with zero agent review. The operator's only options were to merge it unreviewed or to review it out of band:
- the boot sweep re-dispatches only ORPHANED advisory rounds;
- `fishhawk_reconcile_reviews` does not re-run a round that settled `failed`.

**What #4219 adds:**
- **A fail-closed review-coverage hold.** `review_coverage_incomplete` applies to decomposed parents.
- **An operator re-run verb.** `fishhawk_rerun_review` re-runs a settled, verdictless implement-review round against the same head. That verb needs a same-head dedup bypass reachable from a request path, which D6 forbids for its own marker.

The Captain ruled on 2026-10-10 (#4219 plan, option C): approve, keep the D6 marker untouched, and record this amendment after merge.

## Options

- **A. Expose the D6 boot marker to the new verb.** This violates D6 directly and widens an internal-only primitive. Rejected.
- **B. A separate, operator-authenticated, self-verifying marker that is distinct from D6.** The D6 marker and guard stay byte-identical. (Chosen.)
- **C. No re-run verb, and add a merge-time override of the coverage hold.** An override lets unreviewed slice code merge on a single human act, which is the hole #4219 closes. Rejected.

## Recommendation

Option B.

## Decision

**Accepted (2026-10-10), ratified by the Captain (option C on the #4219 plan), recorded after #4219 merged (PR #4279, 7a3bf993).** This amends ADR-091 D6 and leaves D6 itself unchanged.

**D1 Review-coverage hold.**
- A decomposed parent is merge-eligible only when every slice's code is covered by a successful agent review, meaning either:
  - the parent's latest implement-review round has ≥1 `implement_reviewed` verdict; or
  - every succeeded slice child's own latest round has one.
- A round that settled `failed` or `skipped` covers nothing.
- On a miss, `review_coverage_incomplete` names the uncovered slices and the recovery.
- It is enforced at:
  - the merge endpoint (409 before any write; fail-closed on a read error);
  - the gate view's merge_readiness;
  - the drive observer (replacing `awaiting_merge`);
  - the delegated may_merge arm (observe-only);
  - MCP next_actions (no merge offered).
- **No merge-time override ships.** The re-run verb is the recovery.

**D2 A second, distinct dedup bypass for an operator re-run.**
- `POST /v0/runs/{run_id}/reviews/rerun` and `fishhawk_rerun_review` re-run the run's latest implement-review round only when ALL hold:
  - the round has settled with **zero** verdicts;
  - it is not gating;
  - it is not terminal, advanced or superseded;
  - no round is in flight, including a pending boot re-dispatch.
- Access requirements:
  - **adminWrite** tier;
  - human or operator only: run-bound `mcp:run:*` and operator-agent tokens are refused;
  - `write:runs` scope.
- Concurrent requests start exactly one round.

**D3 Marker constraints (the conditions under which D2 is compatible with D6).**
- The bypass rides its **own** unexported context key, separate from D6's.
- The key is set only by the re-run handler. Its contents come from audit state, never from request input.
- It is honoured only when the dispatch's stage AND head match, and the newest `implement_review_started` for that pair is the named round. This is re-verified inside `reviewDispatchMu` at dispatch.
- A reachability guard test fails if the setter, an alias, or a direct key construction appears anywhere else.
- **D6's marker, its key and its guard (`d6Allow`) stay byte-identical.** The two D6 consumer guards each gain one `&& !rerunBypassApplies(...)` term, and no other change.
- D6's rule ("no request path can set the boot re-dispatch marker") stands unchanged for that marker.

**D4 Audit.**
- `review_round_rerun_requested` is appended before any dispatch, and an append failure dispatches nothing.
- The new round's `implement_review_started` carries `rerun_of`.
- Both are registered in `backend/internal/audit/categories.go`.

**D5 Residuals, stated.**
- `claimRerun` and `reviewDispatchMu` are process-local, the same limit as D6's boot path. Multi-replica deployments need a DB lock.
- Re-runs have no depth cap. Each is a deliberate operator act, but each spends reviewer budget. Against a quota-exhausted provider a re-run fails again until ADR-094 D6 (#4242) parks the provider.
- If the parent has no `implement_review_started` at all, the verb refuses `no_review_round`, and only a fix-up clears the hold. This occurs when the consolidated dispatch never started (compare failed, or the forge was unwired) and a slice has no verdict. Tracked in #4282.

## Consequences

- An infra-failed review can no longer let unreviewed slice code reach a merge silently.
- The operator has a one-verb recovery for a failed consolidated round, with no merge override.
- Two same-head bypass paths now exist: D6 boot (internal) and D2 re-run (operator). Each has its own marker, guard and audit. Any future change that lets either marker be set from an unintended path violates this ADR.
- Implementation: #4219 (PR #4279). Follow-ups: #4282 (dead-end recovery, test gaps) and #4242 (quota parking).

## Amendment 1 (2026-10-12): first consolidated round via the re-run verb (#4282)

**Accepted (2026-10-12), ratified by the Captain.** Amends D5 residual 3; D1–D4 and the other D5 residuals are unchanged.

**Context.** D5's third residual reads: "If the parent has no `implement_review_started` at all, the verb refuses `no_review_round`, and only a fix-up clears the hold … Tracked in #4282."

#4282 (PR #4365) closes that residual. The fix-up-only path burned a fix-up slot and a full implement re-run just to start a review round that had never begun.

**Decision.** The D2 re-run verb gains a **first-round path**. It applies to a decomposed parent that has NO `implement_review_started` row and an OPEN consolidated PR. On that path the verb starts the parent's FIRST consolidated round. It reuses `DispatchConsolidatedReview`'s round body (extracted as `runConsolidatedReviewRound`, byte-preserving), so the round behaves exactly like one the fan-in would have started. That includes the ADR-090 D5 fan-in verify hold.

- **Refusals happen before any write (409).** They cover:
  - a child run, or a parent with no children;
  - reviewers unwired, or no agent reviewers;
  - gating authority, where the message names `fishhawk_fixup_stage` and the next step stays `await_review`;
  - the consolidated PR not open;
  - no implement stage.
- **A failed start returns 502, with nothing dispatched.** The reasons are:
  - `compare_failed`;
  - `round_not_started`;
  - `fanin_verify_held`, which carries `details.merge_candidate_state`. Its hint is to await when the merge-candidate pass is in flight, or to route a fix-up on the parent implement stage when the pass FAILED.
- **Audit.** `review_round_rerun_requested` gains an additive `first_round: true` field. No new category.
- **Unchanged.** D2 (same-head bypass marker), D3 (marker separation from ADR-091 D6), D4 (audit order) and the other D5 residuals are unchanged.

**D5 residual 3, as amended.** "If the parent has no `implement_review_started` and its consolidated PR is open, the re-run verb starts the first consolidated round (Amendment 1). If the PR is closed, or the parent is not decomposed, the verb refuses, and only a fix-up clears the hold."

**Consequences.**
- The common dead end (the consolidated dispatch never started: compare failed, or the forge was unwired) now has a one-verb recovery and costs no fix-up slot.
- Re-running the permission-drift check on this path can re-append `permission_narrowing_noticed`, and can re-raise addressed or unkeyed drift findings. That matches the existing fan-in re-entry semantics, and is documented in `backend/internal/server/README.md`.
- Implementation: #4282 (PR #4365).

## Relations

Amends ADR-091 (#4171) D6. Related: ADR-094 (#4239), ADR-041, #4219, #4165, #797.
