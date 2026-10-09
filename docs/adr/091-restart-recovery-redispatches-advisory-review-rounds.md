---
id: ADR-091
title: "Restart recovery re-dispatches orphaned advisory review rounds from a durable round-source descriptor instead of terminating them as failed"
status: accepted
date: 2026-10-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/4171
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-091: Restart recovery re-dispatches orphaned advisory review rounds from a durable round-source descriptor instead of terminating them as failed

## Context

Plan and implement reviews run as fishhawkd-owned work: `claude --print` and `codex exec` are children of the daemon. A daemon restart kills them, and no terminal audit entry ever lands for the round.

**The settled contract (#1781 → PR #1834; #2712 → PR #2727).**
- `ReconcileOrphanedReviews` (`backend/internal/server/review_reconcile.go`) runs once at boot.
- For the latest `*_review_started` of each review-bearing stage that predates the process boot marker and has fewer landed terminals than `configured_agents`, it synthesizes the missing `*_review_failed` entries. The reason is "reviewer orphaned by daemon restart; no terminal review entry from the prior process".
- With the round closed, `await_review` resolves and "the operator can re-trigger".
- #2712 fixed the sweep to cover `pending` runs. Every plan review is dispatched while its run is still `pending`, so plan-review strands could not heal before that fix.
- #2712 also added the on-demand terminate-only verb `POST /v0/runs/{id}/reviews/reconcile` (`fishhawk_reconcile_reviews`).
- #2712's proposal preferred re-dispatching only the unlanded reviewers, so that landed verdicts are not paid for twice. What shipped was terminate-and-re-trigger.

**#3974 made the restart refuse instead.** `scripts/dev reload` and `post-merge` refuse while `GET /v0/restart-blockers` reports a review in flight or a decomposed parent with undispatched children. `--force` overrides. That prevents silent loss. It does not make a reload possible during a busy campaign.

**The cost (#4077).**
- In campaign a2c12763 on 2026-10-07, the daemon ran several merges behind `main` because no quiet window arrived.
- Daemon-compiled knowledge drifted with it, for example the audit registry the plan-gate sweep reads (#4073).
- A reload needed a manually coordinated quiet window between the operator and the campaign driver.
- After a forced restart, "there is no verb to re-dispatch the round" (#4077): `/reviews/reconcile` only terminates.
- A terminated advisory round also silently degrades the gate's review topology. In #2712, one of two configured reviewers landed, and nothing surfaced the missing one.

**Constraints from prior ADRs.**
- **ADR-027 authority.**
  - With `human >= 1`, agent reviewers are **advisory**. Their verdict informs a human gate.
  - With `human == 0` and `agent >= 1`, they are **gating**. The verdict drives a synchronous stage transition owned by the upload request, or the trace handler's `FailStage` branch, that dispatched the round.
- **ADR-036.** Plan approval is refused while a configured, dispatched agent review is in flight. Any terminal outcome unblocks it, and a max-wait backstop prevents a dead reviewer from bricking the gate.
- **The #797 (stage_id, head_sha) same-head dedup** is the second line of defence against a double implement-review dispatch for one head.

## Options

- **A. Keep terminate-and-re-trigger (#1781 / #2712) plus the #3974 refusal.**
  - It is correct and simple.
  - A restart costs a manual re-trigger or a wait for quiet, so in practice the daemon runs stale through campaigns.
- **B. Re-dispatch every orphaned round, gating included.**
  - A gating round's verdict is consumed synchronously by a request handler that died with the process.
  - Re-dispatching it would need a second, asynchronous path to apply the gating transition. That reopens the forward-gating surface ADR-027 / ADR-031 keep narrow.
- **C. Re-dispatch only the reviewers missing from a partially landed round (the #2712 preference).**
  - It does not pay twice for landed verdicts.
  - It mixes two dispatch epochs in one round: different prompt build times, and grounding at a different HEAD. It also needs per-reviewer round accounting that `reviewStatusFor` does not have. `reviewStatusFor` reads the latest round as a unit.
- **D. Re-dispatch whole ADVISORY rounds at boot from a durable round-source descriptor (RECOMMENDED).**
  - Gating rounds and every round that cannot be rebuilt faithfully keep today's terminate closure, with a named reason.
  - The restart refusal narrows to the rounds a restart would actually lose.

## Recommendation

**Option D**, with the decision points below. It changes the #1781 / #2712 contract only for advisory rounds, where the verdict informs a human and no dead request handler owned a transition. It keeps the terminate closure as the universal fallback, so no new strand class is introduced.

This ADR also records the descriptor fields as a **durable contract**: later recovery code depends on them, so they are not an implementation detail of one change.

## Decision

**Accepted (2026-10-09). Ratified by Brett (repository maintainer) on the explicit instruction "ratify as recommended".** The agent records the maintainer's decision; it is not self-approval. The decision points were drafted by the overseer session from #4077's approved plan; the maintainer's rulings on the ratification questions are recorded at the end of this section and are binding on the implementation.

This ADR amends the restart-recovery contract settled by issues #1781 and #2712 (terminate orphaned review rounds as failed, then re-trigger) for advisory rounds only. It supersedes no ADR; ADR-027 and ADR-036 constrain it and stand unchanged.

**D1 Scope.**
- Only the **boot sweep** re-dispatches.
- Only **advisory-authority** rounds (ADR-027) are re-dispatched.
- Gating-authority rounds keep the #1781 / #2712 synthesize-failed closure and remain restart blockers.
- `POST /v0/runs/{id}/reviews/reconcile` stays **terminate-only**.
- Otherwise, the #1781 / #2712 closure is amended for advisory rounds and not withdrawn.

**D2 One eligibility predicate, shared by boot recovery and restart blockers.** A round is re-dispatched only when ALL of these hold:
1. Authority is advisory.
2. A reviewer backend is wired.
3. `redispatch_depth` < the cap (D5).
4. No `review_round_redispatched` entry already names this round.
5. For a plan round:
   - the plan stage is still `awaiting_approval`;
   - the artifact store is wired;
   - no `plan_generated` is sequenced after the round.
6. For an implement round:
   - `round_origin` is a known value;
   - its input source is wired (the trace store for `trace`; the forge compare for `fixup_push` / `consolidated`);
   - no `stage_fixup_triggered` is sequenced after the round.

Any other round is closed by today's synthesis, with the reason extended by `; not re-dispatched: <slug>`. Slugs:
- `gating_authority`
- `reviewer_unwired`
- `already_redispatched`
- `depth_cap_reached`
- `unknown_round_source`
- `plan_stage_not_awaiting_approval`
- `superseded_by_new_plan`
- `superseded_by_fixup`

A re-dispatch that fails to start a new round is also closed by synthesis, with the reason `re-dispatch did not start a round: <slug>`. Slugs:
- `plan_artifact_unavailable`
- `trace_bundle_unavailable`
- `forge_compare_unavailable`
- `compare_failed`
- `round_not_started`

No path leaves a round pending forever.

**D3 Round-source descriptor (durable contract).**
- Every `*_review_started` payload (`planreview.ReviewStartedPayload`) gains four additive `omitempty` fields:

  | Field | Meaning |
  |---|---|
  | `round_origin` | `trace` \| `fixup_push` \| `consolidated`; empty on plan rounds |
  | `round_base_sha` | the base the round's diff was computed against |
  | `redispatch_of` | the audit sequence of the orphaned round's started entry |
  | `redispatch_depth` | the re-dispatch lineage depth |

- Contract rules:
  - **Additive only.** Payloads without the fields stay byte-identical.
  - **The meaning of an existing value never changes.** A new origin value enters the eligible set only by an explicit change. An unrecognized or absent origin is `unknown_round_source` and falls to terminate.
  - **Later recovery code may rely on these fields.** Any change to them is a backward-compatibility change under the ADR-026 discipline.
- Implement rounds written before this change carry no `round_origin`. So the **first** reload after deploy still terminates any in-flight implement round. Plan rounds re-dispatch regardless.
- Inferring origin from surrounding audit context was rejected as fragile.

**D4 Faithful inputs, rebuilt prompt.** A re-dispatch reviews the **same plan artifact, or the same head**, as the orphaned round:
- **Plan rounds** use the artifact whose content hash matches the latest `plan_generated` before the round, plus the persisted gate payloads between that `plan_generated` and the round.
- **Implement rounds** use:
  - `trace`: the stored **redacted** trace bundle whose head matches (ADR-029);
  - `fixup_push` / `consolidated`: `ComparePatch(round_base_sha, head)`.

The prompt itself is rebuilt at boot. Its current-state sections reflect boot time, not the original dispatch time:
- settled and prior concerns;
- crew messages;
- approval conditions;
- plan-review grounding at the working directory's current HEAD.

This is the same view any later round would get.

**D5 Bounds.**
- **One re-dispatch per round per boot.** This holds structurally: the new round's started entry post-dates the boot marker.
- **Crash-loop guard.** A round already named by a `review_round_redispatched` entry is never re-dispatched again. This covers a crash between the audit append and the new round.
- **Depth cap.** `redispatch_depth` is capped at a named constant, proposed as 3. Past the cap the round terminates and stays a restart blocker.
- **Non-blocking boot.** Re-dispatch runs in the background-review goroutine set, so boot and `/healthz` readiness are not delayed. Shutdown already drains that set.
- **Handoff guard.** A process-local pending set keeps the on-demand reconcile verb from terminating a round mid-handoff. Such a round reports the existing `review_dispatched_by_this_process` skip reason.

**D6 Security invariant: the #797 dedup bypass is internal-only.**
- A re-dispatch deliberately reviews the same head. It therefore bypasses two things:
  - the #797 same-head dedup;
  - the synchronous diff-secret concern raise, which already ran for the orphaned round.
- Both bypasses happen **only** under a re-dispatch marker carried in an **unexported context key**, set solely by the boot-sweep re-dispatcher.
- **No HTTP handler, MCP verb, request body field, header or token scope can set it.** Without the marker, a same-head dispatch still dedups exactly as today.
- Any future change that exposes the marker to a request path violates this ADR.

**D7 Audit.**
- New category `review_round_redispatched` {`stage`, `stage_id`, `orphaned_round_sequence`, `configured_agents`, `landed_before`, `authority`, `round_origin`, `head_sha`, `redispatch_depth`}.
- It is appended synchronously **before** the new round is dispatched, and registered in `backend/internal/audit/categories.go`.
- It is not an issue-comment surface: the synthesized `*_review_failed` it replaces is not one either.

**D8 Restart safety, scoped.**
- `GET /v0/restart-blockers` reports `review_in_flight` only in two cases, using the D2 predicate so the two surfaces cannot disagree:
  - a current-process round that a restart would **not** re-dispatch;
  - a pending re-dispatch.
- The wire shape and reason enum are unchanged.
- An eligibility read error still reports `check_failed`, which refuses.
- `scripts/dev reload` stops refusing on `undispatched_child`, because that hazard comes from `post-merge`'s `git pull` (#3974 item 1). `post-merge` keeps refusing on it.
- `reload --when-quiet` waits, bounded, until no runner is live and no reload-relevant blocker is reported. It then falls through to the normal guards.
- The claim this ADR makes is **"a reload is safe whenever no runner is live and no gating round or capped round is in flight"**. It does not claim that every restart is lossless.

**D9 Acceptance upload (premise correction).**
- #4077 states that the runner's acceptance verdict upload already rides the ~90s terminal-egress budget (#2897). **It does not.** `ShipAcceptance` uses the blip retry policy.
- This decision moves `ShipAcceptance` onto the settling terminal-egress budget, alongside `ShipTrace`, `ShipPlan` and `ReportRunnerFailure`.
- `ShipAcceptanceTranscript` stays on the blip budget, because it is best-effort.

**Ratification rulings (maintainer, 2026-10-09).**
- **Q1 — whole-round re-dispatch is accepted.** D4/D5 re-run all configured reviewers. Verdicts the orphaned round had already landed are superseded and paid for again. This knowingly departs from #2712's preference for preserving landed verdicts; missing-reviewer re-dispatch (Option C) is not required.
- **Q2 — D6 must be pinned by a test.** In addition to the bypass-narrowness tests, a test must assert that no request path can set the re-dispatch marker (for example, that the context key's setter has exactly one caller, the boot-sweep re-dispatcher). #4077 is not complete without it.
- **Q3 — depth cap is 3.** The crash-loop guard in D5 already prevents repeated re-dispatch of the same round.
- **Q4 — no `supersedes` entry.** #1781 and #2712 are issues, not ADRs; the amendment is stated in prose above.

## Consequences

**For operators.**
- A daemon restart with no live runner no longer costs a manual re-trigger for advisory rounds. The round completes on its own after boot, and `await_review` follows it.
- `scripts/dev reload` (and `reload --when-quiet`) becomes usable mid-campaign, so the daemon no longer runs stale behind `main` for a whole campaign.
- `post-merge` no longer orphans an advisory review round. It still refuses on an undispatched child, and a re-dispatched plan round is grounded at the post-pull HEAD.
- ADR-036's completion wait applies to the re-dispatched round as to any round, so plan approval waits for it.

**Costs and residuals.**
- A re-dispatched round costs reviewer spend again (Q1).
- Gating-authority rounds still terminate and still block a reload.
- The first reload after deploy still terminates in-flight implement rounds (legacy rows have no `round_origin`).
- A round that the restart-blocker check judged re-dispatchable can still fail input rebuild at boot (for example, a deleted artifact) and close failed. That is a TOCTOU gap between cheap predicate checks and the full rebuild.
- `--when-quiet` narrows, but does not close, the scan-to-teardown window documented by #2897 and #3974.

**Contract obligations.**
- The D3 descriptor must stay backward compatible.
- The D6 marker must stay unexported.
- The D2 predicate is the single source of truth for both recovery and restart blockers.
- `auditcomplete.ReviewPresent` anchoring on the earliest `*_review_started` across rounds is pre-existing and unchanged.

**Follow-ups owned by the operator.**
- `.agents/skills/sync-main/SKILL.md` cites the #3974 refusal set. Implement stages cannot write `.agents/**`.

**Implementation.** #4077 (run 582a47d2), in four slices:
- 0: runner `ShipAcceptance` budget;
- 1: round-source plumbing;
- 2: boot re-dispatch;
- 3: restart-blockers narrowing, `scripts/dev`, and docs.
