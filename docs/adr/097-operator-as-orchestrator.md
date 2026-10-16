---
id: ADR-097
title: "Operator as orchestrator: the driver delegates evidence work to stateless specialist jobs; the first officer keeps a standing trigger-driven watch the operator cannot silence (amends ADR-089, ADR-040)"
status: accepted
date: 2026-10-11
issue: https://github.com/kuhlman-labs/fishhawk/issues/4299
supersedes: []
superseded_by: []
amends: ["ADR-040", "ADR-089"]
amended_by: ["ADR-101"]
applies_to: []
---

# ADR-097: Operator as orchestrator: the driver delegates evidence work to stateless specialist jobs; the first officer keeps a standing trigger-driven watch the operator cannot silence (amends ADR-089, ADR-040)

## Context

ADR-089 (#4149) made the first officer a structurally read-only crew seat attached to a campaign. Its D1 defines attachment and lifecycle but not **what makes the seat act**. E85.6 (#4148) implements the seat as an audit-stream subscriber that "may run scratch verification", which leaves open whether the seat acts on its own or only when the driving operator asks.

ADR-040 (#997) made the operator role spec a product artifact. It says nothing about which work the driver should do itself and which it should hand off.

Campaign 9275cbb2 (2026-10-10 to 11) ran the improvised bridge/away-team model again, with 22 items. Two observations shape this ADR.

**1. Most of what the overseer caught was never requested by the driver.** The driver did not know to ask for these:
- Scope amendment 4a4f8636 would have "fixed" a main that already passed. Checking in the pinned gate image showed the change would have turned CI red (#4245).
- The red passes on #4233 and #4028 were flakes; re-running confirmed it. The alternative was a fix-up pass each.
- A fix-up prompt truncated the routed concern list (#4284). The first #4200 fix-up addressed one concern of three.
- Something was pruning `.git/worktrees` admin directories (#4281).
- The Go build cache grew to 112 GB between trims.
- Cross-run file overlaps dictated merge order.

The counterfactual batteries the overseer ran on its own initiative found a race (#4260) and a security gap (#4273). Both were fixed before merge.

If the checked party decides when the checker runs, these are the checks that get skipped under throughput pressure.

**2. Inline evidence work exhausts the driver's context.** The overseer session itself compacted from carrying batteries, tree reads and gate state together. The driver's job (read the gate, decide, dispatch, merge) needs a lean context, while evidence work produces bulky intermediate output that only its conclusion is needed from.

## Options

- **A. Status quo (ADR-089 as written).** The seat exists, and the trigger model is left to the implementation. Risk: an implementation that makes the seat on-call only, losing its independence.
- **B. On-call first officer.** The operator orchestrates and calls the first officer for specific tasks. This gives a lean driver and clear ownership, but the checker's scope is set by the checked. The improvised model's best catches (observation 1) would not have happened.
- **C. Orchestrating operator plus a standing-watch first officer (RECOMMENDED).**
  - The operator orchestrates and delegates evidence work to stateless specialist jobs.
  - The first officer keeps a standing, trigger-driven watch that the operator cannot silence.
  - It also accepts operator requests.
- **D. Fold the first officer into the operator as a sub-agent.** Cheapest, but it removes the structural read-only boundary and the independent record. ADR-089 rejected this as Option A. Rejected.

## Recommendation

**Option C.** The proposed decision points follow.

**D1 Operator remit: orchestration.**
- The base operator role spec (ADR-040) gains an orchestration remit. The driver:
  - reads gates;
  - decides and dispositions;
  - dispatches, merges and pages;
  - does NOT perform evidence work inline.
- Evidence work is delegated to specialist jobs (D2). It covers:
  - counterfactual batteries;
  - consolidated fan-in builds;
  - cross-run overlap analysis;
  - inventories;
  - plan cross-checks against the tree;
  - ADR and issue drafting.
- This is playbook procedure in the base spec. Per ADR-040's thinness rule, a repository overlay can neither add to it nor remove it.
- It is a remit, not a capability fence: the operator keeps its read scopes. Inline evidence work is a playbook deviation, not a permission error.

**D2 Specialist jobs: stateless, single-remit and typed.**
- A specialist job is a one-shot invocation with one remit drawn from a closed kind enum, v0:
  - `counterfactual_battery`
  - `fanin_build`
  - `overlap_scan`
  - `plan_crosscheck`
  - `inventory`
  - `draft`
- It returns a structured result and keeps no state between invocations.
- Every kind except `draft` is an ADR-089 D3 verification job: scratch clone, never a lineage worktree; no push, commit, run or stage state change, or tracker write; the result is runner-observed and recorded on the audit chain with the requesting actor.
- `draft` produces text only (an ADR or issue body) and writes nothing. Filing stays with the operator or the captain.
- Specialists are not reviewer personas (ADR-084). They cast no vote and do not count toward quorum.
- Either seat may request a job: the operator directly, the first officer per D3 and D4.

**D3 First officer standing watch (amends ADR-089 D1).**
- The seat acts on a declared trigger set read from the audit chain, without waiting for a request. Closed trigger enum, v0:

  | Trigger | Job |
  |---|---|
  | `plan_submitted` | `plan_crosscheck` against the tree and other in-flight runs' scopes |
  | `scope_amendment_pending` | `plan_crosscheck` of the amendment's premise |
  | `fixup_pushed` on a run with routed concerns | `counterfactual_battery` over the routed concerns, checking that every routed concern was addressed |
  | `review_gate_approved` | `overlap_scan` against other in-flight runs before merge |
  | `campaign_item_admitted` | `plan_crosscheck` of the issue premise against the current tree |
  | `ops_monitor_alert` (E85.1/E85.2) | an advisory finding recommending the remedy (ADR-089 D9 keeps remediation human) |

- The set is declared in the workflow spec's `operator_agent` block as `first_officer.triggers`. **Default: all enabled.**

**D4 On-request channel.**
- The operator may ask the first officer for any specialist job. It does so with an ADR-081 crew message addressed to `first_officer` on its campaign or run.
- The seat runs the job and answers with a `finding` (or a `notice` when there is nothing to report).
- A requested job is held to the same audit, budget and blocking rules as a triggered one.

**D5 Independence guards.**
- Only the captain can change the trigger set mid-campaign; a delegated operator agent cannot. The change is an audited act.
- The operator cannot cancel a job the first officer started.
- The operator cannot disposition a finding except through ADR-089 D5 (accept, waive with a non-empty reason, or defer to issue).
- A trigger that fires but does not run its job (budget, a refused job, a runner error) records a `notice` naming the trigger and the reason. A skipped check is visible, never silent.

**D6 Budget.**
- Specialist jobs are charged to the requesting seat.
- First-officer jobs, triggered or requested, count against ADR-089 D6's per-campaign cap.
- Verification jobs run in the `local-verify` admission group (ADR-089 D6, generalized by ADR-094). They never take an implement slot.
- When the cap is exhausted, triggers degrade to D5 skip notices. The campaign does not fail.

**D7 Unchanged.**
- ADR-089 D2: seat identity and withheld scopes.
- ADR-089 D4: blocking requires product-produced evidence.
- ADR-089 D9: destructive remediation, tier, identity and approver rulings, and merges stay human or driver acts.

**Ratification questions:**
- **Q1** Should the first officer keep a standing, trigger-driven watch that the operator cannot silence (D3, D5), rather than act only on request?
- **Q2** Should the operator's orchestration remit be playbook procedure in the base role spec (D1): non-overridable, but a remit rather than a permission fence?
- **Q3** Are the v0 specialist kinds and trigger enum (D2, D3) the right closed sets, with additions by ADR amendment?
- **Q4** Does this amend ADR-089 D1 (adding the trigger model) and ADR-040 (adding the orchestration remit) without superseding either?

## Decision

**Accepted (2026-10-11). Ratified by the Captain as recommended, on the instruction "yes draft the ADR then ratify and file".** Option C is adopted with D1–D7 as written.

- **Q1: yes.** The standing watch cannot be silenced by the operator. Only the captain changes the trigger set.
- **Q2: yes.** The orchestration remit is base-spec procedure, not a capability fence.
- **Q3: yes.** v0 kinds and triggers are as listed. The enums grow by ADR amendment.
- **Q4: yes.** This amends ADR-089 D1 and ADR-040. Neither is superseded.

## Consequences

**Independence.** The first officer's checks no longer depend on the driver knowing what to ask. A skipped check leaves a notice.

**Driver context.** The operator carries gate state and decisions. Evidence work arrives as a structured conclusion with its audit reference.

**Cost.**
- Standing triggers spend tokens on every plan, fix-up and approval, including uneventful ones.
- They are bounded by ADR-089 D6's cap, with visible degradation.
- Campaign 9275cbb2's catch rate (observation 1) is the baseline for judging whether the spend pays.
- **Re-measure trigger:** if a campaign's triggered jobs produce no accepted finding over 20 consecutive items, revisit the default trigger set.

**Risk.**
- Over-reporting stalls gates. The mitigations are unchanged from ADR-089:
  - blocking requires product evidence;
  - every finding can be dispositioned with a reason;
  - spend is capped.
- New risk: trigger storms on large campaigns. The `local-verify` admission group serializes them, so they queue rather than oversubscribe the host.

**Implementation.**
- E85.6 (#4148): gains the trigger model, the on-request channel and the D5 guards.
- New E85 children:
  - specialist job kinds and their result schema;
  - the operator role spec orchestration remit.
- E85.8 (#4152): the capstone walk exercises a trigger-caught finding the driver did not request.

## Relations

Amends ADR-089 (#4149) D1 and ADR-040 (#997). Related:
- ADR-081 (crew messages)
- ADR-084 (reviewer personas, explicitly distinct)
- ADR-094 (#4239, admission groups)
- E85 (#4141)
- #4148, #4152, #4245, #4284, #4281
