---
id: ADR-085
title: "Earned autonomy: recommendations only, from server-attested evidence and a blind shadow record, with asymmetric thresholds (lower fast, raise slowly) and captain ratification through a human-authored spec change"
status: accepted
date: 2026-09-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/3777
supersedes: []
superseded_by: []
amended_by: ["ADR-101", "ADR-122"]
applies_to: []
---

# ADR-085: Earned autonomy: recommendations only, from server-attested evidence and a blind shadow record, with asymmetric thresholds (lower fast, raise slowly) and captain ratification through a human-authored spec change

## Context

ADR-080 #3693: earned autonomy (the crew's record recommending changes to its own delegation, with the captain always ratifying) is **designed in alpha and may land after it**, because delegation is the most safety-relevant surface in the product. E82 #3703 carries it (`milestone:beta`). `docs/METHODOLOGY.md` (~l.393) already promises that the tiers "will be revised based on what the audit log shows". Today that revision is done by hand from memory (#1964 tuned a delegation condition from one observed run).

**How delegation works (verified 2026-09-27).** Tiers expand in `spec/v2autonomy.go` `expandTier`:
- `low`: everything gated.
- `medium`: approve (`clean_dual_approval`), fixup (`convergent_concerns`) and retry (`infra_flake`) auto; waive and merge gated.
- `high`: adds waive (`solo_low`) and merge (`gates_resolved_ci_green`).

Each action class has **exactly one legal condition**, so the only moves are the mode ladder `gated → report → auto`, fixup's `min_severity`, and the tier itself. Resolution in `internal/delegation` `Evaluate` runs tier, then explicit `actions`, then campaign override, then the **escalation `max_autonomy` ceiling applied last** (`spec/escalation.go` `ClampResolvedMatrix`). At action time `page_human_on` wins over everything (`autodrive.go` `activePageEvent`), and every action re-checks its condition. Grooming's ordering, dedup, scoping and milestone classes are hard non-delegable.

**Evidence in this repository's dev chain** (1,845 runs, 103.7k audit rows):

| Class | Delegated exercise | Available outcome signal |
|---|---|---|
| fixup | 91 delegated `stage_fixup_triggered` (`convergent_concerns`) | next review verdicts; rounds per run (308×1, 85×2, 41×3, 1×4); `fixup_concern_unattempted` / `fixup_no_changes` |
| approve | 30 delegated (06-12 → 07-31), now inert here (the plan gate declares `approvals`, #2381) | plan-review miss rate; acceptance outcomes (448 accepted / 221 not validated / 20 rejected / 14 undecidable) |
| waive, retry, merge | **0** (1,728 waives and 552 merge verdicts, all human) | none from delegation |

`mode: report` (`autodrive.go` `reportMatrixProposals`) is the existing shadow primitive. It has **0 rows** for run classes, and it is shown to the operator, so it anchors the human's decision.

**Missing:** revert detection (1 revert in 1,616 commits, never detected), CI status on merge commits, incident-to-merge attribution (E35 #1585, all open), and any record of what delegation *would* have done at a human-decided gate.

**Precedents for "the agent recommends, a human ratifies":**
- Grooming refuses agent-proposed `autonomy:*` labels without a per-entry gate approval (rule 8, #2855).
- ADR-079 binds a decision to exact content plus its predecessor, and delegated credentials never count.
- ADR-082 rule 5: a doctrine change files an `autonomy:low` issue and never edits `.fishhawk/**`.
- ADR-083 / E76.5 #3768: confirm or lower on handover, and nothing raises.

agenteval Tier A/B (`scorer.go`, `judge.go`) is offline, and the judge is uncalibrated against human labels.

**Risks specific to raising:**
- **Goodhart and self-influence:** condition inputs are agent outputs, such as reviewer severity and prose.
- **Automation bias:** report-mode proposals are shown before the human decides.
- **Survivorship:** a delegated merge gets no downstream review.
- **Confounding:** `clean_dual_approval` selects easy work.
- **Small samples:** only fixup has N > 30.
- **Long incident latency.**

## Options

**(a) Unit of evidence.** 1: workflow × class. 2: **workflow × class × escalation stratum × doctrine version** (resolved-matrix hash from E76.1 #3747, plus `workflow_sha`), with per-role input-quality evidence attached. 3: per-role scorecards alone.

**(b) Counterfactual and "clean".** 1: outcome-only lookback. 2: **a blind shadow stamp**: at every human action on a gated class, record the evaluator's would-be verdict (condition, met or unmet, reason, matrix hash). It is never shown before the decision. 3: the agenteval judge.

**(c) Thresholds.** 1: a fixed N and rate. 2: **asymmetric statistical thresholds**: raise only on a Wilson lower bound over matured outcomes; lower immediately on a single R2 event. 3: captain judgment with no threshold.

**(d) Surface and ratification.** 1: file an issue directly. 2: **record `delegation_recommended`** (bound to the matrix hash and `workflow_sha`, stale when either changes). The captain accepts, which files an `autonomy:low` issue carrying the exact `workflows.yaml` diff, or declines, which suppresses the recommendation until the evidence materially changes. 3: a runtime lower-only overlay.

**(e) Alpha vs beta.** Design only in alpha, or start the record-only instruments early so history accumulates.

## Recommendation

**(a) 2, (b) 2 for raising and 1 for lowering with 3 advisory only, (c) 2, (d) 2 with 3 deferred to E58.3 #2272, (e) design in alpha, instruments early.** Binding rules:

1. **Recommendations only.** Nothing in E82 changes delegation. Every change is a human-authored `.fishhawk/workflows.yaml` change the captain makes after accepting a recommendation. Agent and run tokens are refused on every E82 write.
2. **Hard ceilings a recommendation can never cross:** an escalation's `max_autonomy` clamp; any `page_human_on` event; non-delegable classes; and the E76.5 confirm path, which stays raise-free (recommendations appear there only as read-only context).
3. **Evidence is server-attested only:** forge merge SHA, CI status on the merge commit, revert detection, human-actor decision rows, and incident attribution. **Agent-written prose and severities never count as outcome evidence.** They may appear as context, labelled.
4. **Blind shadow.** The shadow stamp is recorded at decision time and never rendered to the deciding human, so automation bias cannot inflate agreement. `mode: report` rows are kept, but labelled as anchored.
5. **Asymmetry.** **Lowering**: one R2 event (an attributed incident, a revert of a delegated action, a waived concern later traced to a defect) produces an immediate recommendation plus a page. **Raising**: one step at a time (`gated → report → auto`, or one tier), only when the Wilson 95% lower bound of shadow agreement with clean matured outcomes is ≥ 0.9, with N ≥ 30 condition-met occurrences over ≥ 28 days, then a 30-day cooldown. It resets on any doctrine change. These numbers are the starting point and are tuned by backtesting on this repository's history before anything is enabled. The honest alpha output is mostly "insufficient evidence", and the surfaces must say so plainly.
6. **Maturity windows.** A delegated outcome counts as clean only after a class-specific window (merge: no revert, green CI on the merge commit, no attributed incident within W).
7. **Stratify, don't pool.** Evidence is kept per escalation stratum and doctrine version. Path strata can only drive *lowering* (by proposing a new escalation entry), because the grammar cannot raise per path, and that asymmetry is kept.
8. **Built on the historian.** The record is a rebuildable projection over ADR-082's decision index (E75.1 #3729, E75.2 #3730), and every figure cites its chain entries.

## Decision

**Accepted (2026-09-27). Ratified by Brett (repository maintainer) on the explicit instruction "ratify ADR-085" after reviewing this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt the Recommendation as binding: (a) evidence per workflow × class × escalation stratum × doctrine version; (b) a blind shadow stamp as the raising counterfactual, outcome lookback for lowering, the agenteval judge advisory only; (c) asymmetric thresholds — lower on one R2 event, raise one step at a time only on a Wilson 95% lower bound ≥ 0.9 over ≥ 30 condition-met matured occurrences across ≥ 28 days with a 30-day cooldown, reset on doctrine change, all tuned by backtest before enabling; (d) `delegation_recommended` bound to the matrix hash and `workflow_sha`, accepted by the captain into an `autonomy:low` issue carrying the exact diff, or declined; the runtime lower-only overlay deferred to E58.3 #2272; (e) design ratified in alpha, with the two record-only instruments (blind shadow stamp, post-merge outcome attribution) permitted to start in alpha. Rules 1–8 bind the implementation: recommendations only, never across an escalation clamp, `page_human_on` or a non-delegable class, server-attested evidence only.

**Scope of this ratification:** the design. No delegation changes as a result of it.

## Consequences

- **Children once ratified.** Most are beta builds; two record-only instruments may start in alpha because the thresholds need history:
  - Instruments (alpha): the blind shadow stamp, and post-merge outcome attribution (revert detection, CI on the merge commit, incident-to-suspect-run by deployed SHA range, the last depending on E35.2 #1599 / E35.4 #1601).
  - Beta builds: the delegation record projection, the recommendation engine, the captain accept/decline verb, the surfaces (handover brief, digest, bridge, REST/MCP), and a backtest-and-walk capstone (one lowering from an injected incident, one report-to-auto raise ratified by a human PR, and a METHODOLOGY update).
- **The runtime lower-only incident clamp** is deferred to E58.3 #2272.
- **Out of scope:** automatic changes to delegation, raising per path, and using the agenteval judge as a threshold input.
- **Reversal:** stop emitting recommendations. The shadow and outcome records remain as history.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- #3703 [E82] Earned autonomy: the crew's record recommends delegation changes the captain ratifies — low confidence (0.24); shared: autonomy, captain, earned, record

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"epic_suggestion":{"number":3703,"url":"https://github.com/kuhlman-labs/fishhawk/issues/3703","title":"[E82] Earned autonomy: the crew's record recommends delegation changes the captain ratifies","score":0.23793103448275862,"confidence":"low","basis":"autonomy, captain, earned, record"},"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2560} -->
