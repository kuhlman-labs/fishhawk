---
id: ADR-117
title: "Live review effectiveness is measured: concern outcomes and human judgements, child-slice concerns imported into the parent, an escape record attributed to the rounds that saw the code, and a presence-conditioned per-reviewer scorecard"
status: accepted
date: 2026-10-23
issue: https://github.com/kuhlman-labs/fishhawk/issues/4690
supersedes: []
superseded_by: []
amended_by: ["ADR-118"]
applies_to: []
---

# ADR-117: Live review effectiveness is measured: concern outcomes and human judgements, child-slice concerns imported into the parent, an escape record attributed to the rounds that saw the code, and a presence-conditioned per-reviewer scorecard

## Status

**ACCEPTED (v2, ratified by the Captain 2026-10-23).** Drafted by the first officer on 2026-10-23 at the Captain's direction.
- **Review history:** v1 was reviewed by claude-fable-5-1 and claude-opus-5-5. Both returned approve_with_concerns, with 5 HIGH findings each, which converge.
- **v2 changes:** it applies every finding, and SPLITS the framework.
  - This ADR covers the live, product-side measurement (D1–D4), which can ship now.
  - The offline-harness extensions and the decision rule for review changes go to a follow-up record after the 2026-11-01 re-pin (see Consequences).
- **Freeze:** tracker-only until the 2026-11-01 catch-rate re-pin; the in-repo record follows the upkeep rule.

## Context

Fishhawk runs many reviews:
- plan review;
- implement review per slice and consolidated (#677, #1060);
- reviewer personas (ADR-084: architect and security);
- acceptance;
- the merge-candidate and fan-in verify;
- operator counterfactual batteries.

Their configuration changes often (a model substitution, #4624; persona attachment, #4684; routing, #4687), yet live effectiveness is barely measured.

**Measured or recorded today:**
- `backend/internal/agenteval`'s plan-review catch-rate gate (E55.4, ADR-068): planted defects, per-arm catch and false-alarm rates, a power floor and a pinned baseline. The committed record is refused on main until the 11-01 re-pin.
- An implement-review catch arm for the security persona (E80.17).
- Severity calibration, and the live halves of the injection and envelope corpora, exist as harnesses but have NOT yet been measured (#3187).
- Three live feeds exist without a common record:
  - acceptance-triage class-3 writes `plan_review_miss`, reported by `GET /v0/acceptance-triage/stats` (ADR-049);
  - the severity-calibration distiller joins concerns to their waive, defer and condition dispositions (`corpusdistill`);
  - a GREEN plan-declared counterfactual raises a `control_unpinned` server_check concern (`counterfactual_observed`, E85.3).

**Not measured:**
1. **Live precision.** A concern closes `addressed`, `addressed_by_condition`, `deferred` or `waived` (`backend/internal/concern`). `superseded` is non-terminal, and re-enters only through an operator fix-up.
   - Waive, bulk waive and resolve take free text only (`waive.go`, `bulk_waive.go`, `resolve_concerns.go`).
   - `addressed` has system writers: a reviewer `confirmed`, and the clean-re-review-round auto-close.
   - `addressed_by_condition` is server-written (`condition_claims.go`).
   - None of these records whether the concern was a defect.
2. **Concerns that never close.** In #4687 step 1 (15 decomposed parents, 2026-10-16 to 10-23), 230 of 367 concerns were still `raised` after merge.
3. **Slice concerns are dropped.** The merge-eligibility concern leg counts only the PARENT run's open concerns (`merge_queue_eligibility.go`, `ListOpenByRun`), and the fan-in reads no child verdict or concern. In step 1:
   - no slice was fixed up;
   - 102 slice-only defects (95 LOW, 4 MEDIUM, 3 HIGH) were never dispositioned;
   - slice `reject` verdicts did not block integration;
   - only the slice defects a consolidated round re-raised were acted on.

   One dropped HIGH was a live defect on main (#4688).
4. **Escapes.** No record links a defect found after review to the rounds that reviewed the code that introduced it.
5. **Per-reviewer value and cost.** Nothing measures what a reviewer, persona or round type contributes beyond the others, or what a real finding costs.

## Options

- **A. Status quo.** Rejected: slice findings are silently dropped, and no review change can be judged on live evidence.
- **B. Structured outcomes and judgements, a child-concern import, an escape record and a scorecard (recommended).**
- **C. A merge gate requiring every child concern to be dispositioned.** Rejected: at about 14 child concerns per parent, 93% of them LOW, it buys toil that degrades the labels (operators already bulk-waive LOWs with one shared reason), and it gains no control the import does not give.

## Recommendation

**Option B.**

**D1. Outcome and judgement on every concern.**
- **Two axes.** Every concern carries two fields.
  - **`outcome`**, written by whoever closes it, system or human, from a closed set:
    - `fixed`;
    - `deferred` (to an issue);
    - `waived`;
    - `duplicate_of:<id>`;
    - `carried` (rolled up, D2);
    - `open_at_merge`.
  - **`judgement`**, written ONLY by a human-initiated verb, from a closed set:
    - `defect`;
    - `defect_pre_existing` (real, but not introduced by this change);
    - `no_defect`;
    - `tradeoff` (a real cost, knowingly accepted; the reason must state the cost);
    - `preference`;
    - `stale` (not applicable at the head);
    - `insufficient_evidence`;
    - `unlabelled`.
- **Who writes what.**
  - Human verbs REQUIRE a judgement: waive, bulk waive, resolve-with-evidence, defer, and a new `fishhawk_label_concern` for relabelling.
  - System writers record their outcome with judgement `unlabelled`: reviewer `confirmed`, clean-round auto-close, and condition claims. A system can know that a fix landed, never whether the concern was a defect.
  - Every judgement records the actor kind, and whether it was delegated or bulk.
- **`open_at_merge`.** At merge, every still-open or `superseded` concern of the run, and of its children per D2, is stamped `open_at_merge`. Never-closed concerns become a measured outcome rather than a silent drop.
- **Duplicates.** A duplicate names the earliest matching concern, at the same or an earlier sequence, and inherits its judgement. A chain that ends without a judgement reads `unlabelled`.
- **Legacy rows** read `unlabelled`. #4687 step 1's hand labels may be imported through `fishhawk_label_concern`, tagged `backfill:true`.
- **Visibility.** Outcomes and judgements are audited, shown in the gate view and exported. They are NOT rendered into review prompts. Waive and defer reasons already are, and changing that would touch the frozen review-prompt paths and shift recall.
- **The interim convention** (Captain's standing order, 2026-10-23) maps onto this vocabulary: a waive reason prefixed `false_positive:`, `accepted_tradeoff:` or `preference:` becomes judgement `no_defect`, `tradeoff` or `preference`.

**D2. Child-slice concerns are imported into the parent.**
- **When.** At `slices_integrated`, every OPEN child concern of severity HIGH or MEDIUM is copied into the parent run's ledger as `raised`. The copy keeps:
  - its origin, reviewer model and `reviewer_role`;
  - a `rolled_up_from` link to the child concern id.

  The child concern's outcome becomes `carried`.
- **After import,** the copies flow through the EXISTING machinery:
  - parent fix-up routing;
  - the consolidated round's confirm and auto-close;
  - bulk waive with a judgement;
  - the merge-eligibility concern leg, which already blocks on an open HIGH.

  No new merge gate is introduced.
- **LOW child concerns** are not imported. They are stamped `open_at_merge` at merge, which keeps them visible to the scorecard without cluttering the parent's re-review context.
- **Audit.** A new audit category, `concern_rolled_up`, is registered in `categories.go` and in `activityCategories`.
- The import does not change ADR-112's forge-approval predicate.

**D3. The escape record.**
- **What a record holds:**
  - the defect, its severity and category;
  - the merged PR, or the run and head;
  - the source;
  - its INTRODUCING commit(s), computed by blame at the merged head, never chosen by the operator.
- **Sources:**
  - **automatic:**
    - acceptance class-3 `plan_review_miss`;
    - a GREEN `counterfactual_observed`, for a test-vacuity miss;
    - red CI on main after merge;
  - **operator-recorded:**
    - a later reviewer's finding;
    - a follow-up bug traced to a PR;
    - the operator's hand battery;
    - a human finding.
- **A finding from a review source counts as an escape only after it carries judgement `defect`.** Otherwise one reviewer's false positive would become another reviewer's escape.
- **Attribution.** A review round SAW the defect only if BOTH hold:
  - every introducing commit is an ancestor of the round's recorded head;
  - the file was inside that round's RENDERED diff (a slice round: its slice diff; a consolidated round: the merged diff). A truncated diff counts as not rendered.

  Other cases:
  - **Plan rounds** saw a plan defect only for a criterion or scope defect at that plan revision.
  - **Unseen code.** A defect introduced only by commits no round saw (a fix-up after the last round, a rebase, or a fan-in resolution) is attributed to the `unreviewed_code` bucket and to the merge-candidate verify. No review round is blamed for it.
  - **Matching.** A round that raised a matching concern is a CATCH: it is not a miss, even if that concern was then waived or judged `no_defect`. That case is a `caught_dismissed` disposition error, fed to the label audit. Matching uses the #4687 step-1 rule, accepting only `exact` or `probable` grades, so a vague concern cannot claim an escape.
- **Prerequisite.** Every implement and consolidated review round records its reviewed head. A round that does not is reported `head_unknown`.
- **Security escapes** follow the two-part disclosure rule (#4516): the class is public, and the reproduction stays in a draft advisory.

**D4. The scorecard.**
- **What it is.** Computed from the audit trail, per repository and per window, and never hand-maintained.
- **Dimensions:**
  - surface (plan, slice implement, consolidated implement, acceptance, merge-candidate verify, CI);
  - reviewer model;
  - persona;
  - round type.
- **Metrics:**
  - **Defect precision:** `defect` + `defect_pre_existing`, divided by judged concerns.
  - **Actionable precision:** the same, plus `tradeoff`.
  - **`unlabelled` share and `open_at_merge` share:** always shown beside precision, never dropped from view.
  - **Unique real catches,** counted per DEFECT, not per concern, within a round and across rounds and surfaces. Presence-conditioned: a reviewer or persona absent from a round, or a persona not attached, is excluded from that round's denominator, and never scored as a miss.
  - **Escape rate** per merged PR, severity-weighted and censored for detection lag. Reported separately per surface, and for `unreviewed_code`.
  - **Severity mix.**
  - **Output tokens per real finding,** priced through `pricing`. Input tokens are cache-dominated and not comparable.
  - **Round wall-clock.**
  - **Free signals consumed:** reopen rate, `concern_resolution_vetoed`, `concern_relitigation_suppressed`, and the acceptance-triage stats.
- **Exclusions.** Concerns of `server_check` or `first_officer` provenance are excluded from reviewer metrics.
- **Statistics.**
  - Every figure carries a Wilson interval.
  - A cell below its minimum (default 20 judged concerns for precision, 10 attributed PRs for an escape rate) reports `insufficient_data`, never a value.
- **Label audit.**
  - Each window, a stratified sample is re-adjudicated blind (blind to reviewer model, role and the original judgement): at least 30 concerns, plus every HIGH, oversampling `no_defect`/`preference` on HIGH and MEDIUM, `tradeoff`, bulk and delegated judgements, and `caught_dismissed`.
  - The default second labeller is a human. An agent labeller must be a model NOT on the reviewer panel, and is reported separately.
  - Agreement is reported as Cohen's kappa beside every precision figure.

**D5. Portability (ADR-110).**
- **Product mechanisms:** the outcome and judgement vocabulary, the import, the escape record and attribution, and the scorecard.
- **Per-repository declarations, with product defaults:** cell minimums, the window, and the label-audit sample size, in a declared block of `.fishhawk/workflows.yaml`.
- **Evidence:** a non-Fishhawk fixture drives a decomposed run's import and an escape attribution through the production code (rule 3(a)).
- **No history:** a repository without enough history reports `insufficient_data`.

## Decision

**Accepted 2026-10-23** by the Captain (repository maintainer): **Option B**, i.e. D1–D5 as stated in the Recommendation (v2), decided now. The offline-harness extensions and the decision rule for review changes are deferred to the follow-up record named in Consequences, after the 2026-11-01 re-pin. Until D2 lands, the Captain's 2026-10-23 standing order (disposition slice MEDIUM+ concerns at the parent gate) covers the gap by hand.

## Consequences

- **Gains.**
  - Slice findings can no longer be silently dropped: a HIGH blocks through the existing leg.
  - Every concern ends with a measured outcome, and with a human judgement where a human closed it.
  - Real misses are attributed to the rounds that saw the code.
  - Every review surface gets presence-conditioned precision, unique value, escape rate and cost, with intervals.
- **Costs:**
  - the outcome and judgement fields (a concern schema migration);
  - the required judgement on the waive, bulk-waive, resolve and defer verbs, their MCP tools, the OpenAPI file and `v0.md`;
  - `fishhawk_label_concern`;
  - the `open_at_merge` stamp;
  - the child import and its audit category;
  - reviewed-head recording on every review round;
  - the escape verb, blame attribution and the automatic sources;
  - the scorecard computation and its surface;
  - the label-audit sampler;
  - operator time: one judgement per human close, about 30 or more blind re-adjudications per window, and escape adjudication.
- **Residuals.**
  - Judgements remain operator opinions. The label audit measures their reliability but cannot remove the bias toward progress.
  - Undetected defects are never counted.
  - Small repositories may stay `insufficient_data` for a long time.
- **Implementation order:**
  1. D2 child-concern import (HIGH/MEDIUM), the `carried` outcome and the `open_at_merge` stamp. This closes the live gap; until it lands, the Captain's standing order covers it by hand.
  2. D1 outcomes, judgements and `fishhawk_label_concern`.
  3. A minimal D3 escape verb, before 2026-10-27 18:42 ET if possible, so astra's re-reviews of the merge-path work are recorded as escapes or confirmations.
  4. D4 scorecard and label audit.
  5. D3 automatic sources and blame attribution.
- **Follow-up record (after the 2026-11-01 re-pin).** It covers:
  - the offline-harness extensions: standard-reviewer, architect, acceptance and slice-vs-consolidated arms under the production reviewer models; real-derived cases entering a non-gating monitor arm first, batch-promoted at scheduled re-pins, embargoed for security, human-reviewed before commit; leave-one-out as a diagnostic only, since it cannot resolve contributions below the gate's 0.10 tolerance;
  - the decision rule for review changes: committed pre-registration under `docs/compliance/`; a scheduled scorecard check that writes an audit entry; an interval-based regression test; a power-derived window plus a lagged escape window; an emergency path for forced substitutions; reviewer-block changes in `.fishhawk/workflows.yaml` added as a review-prompt-eval trigger; and any reviewer-floor rule measured per attached round, severity-weighted, exempting a lens that is the sole coverage of its remit, and decided by the Captain, never automatically.

  It amends ADR-068 D3 if adopted as an ADR.
- **Record metadata.** amends: []; supersedes: []; applies_to: []
- **Related:** ADR-049, ADR-068, ADR-084, ADR-110, ADR-112, ADR-116, E55.4 (#2245), E80.17 (#4314), E85.3, #4516, #4624, #4684, #4687, #4688.
