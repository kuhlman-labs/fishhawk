---
id: ADR-118
title: "Review changes are judged, not re-pinned: a per-arm eval gate (standing, experiment and monitor arms; model changes as paired experiments), reviewer configuration as a gate trigger, and one pre-registered offline+live decision rule"
status: accepted
date: 2026-10-23
issue: https://github.com/kuhlman-labs/fishhawk/issues/4698
supersedes: []
superseded_by: []
amends: ["ADR-068", "ADR-084", "ADR-117"]
amended_by: ["ADR-121", "ADR-122"]
applies_to: ["backend/internal/agenteval/**", "scripts/check-review-prompt-eval"]
---

# ADR-118: Review changes are judged, not re-pinned: a per-arm eval gate (standing, experiment and monitor arms; model changes as paired experiments), reviewer configuration as a gate trigger, and one pre-registered offline+live decision rule

## Status

**ACCEPTED (v2, ratified by the Captain 2026-10-23).** Drafted by the first officer on 2026-10-23 at the Captain's direction. It is the follow-up record that ADR-117 (#4690) deferred (#4697).
- **Review history:** v1 was reviewed by claude-fable-5-1 and claude-opus-5-5. Both returned approve_with_concerns (Fable 4 HIGH, Opus 9 HIGH, converging). v2 folds in every finding.
- **Timing:** this departs from #4697's plan, which was "drafted after the re-pin". It is drafted now so that, if ratified first, the ADR-112–118 records join the pre-re-pin record batch (#4600 or a sibling PR) and merge immediately before the 2026-11-01 recording. The architect arm's prompt embeds `docs/adr/index.json`, so every record that lands afterwards stales that arm. If it is not ratified in time, its record rides D1's first recording instead.
- **Implementation:** everything lands at or after the 2026-11-01 re-pin (see the implementation order). Nothing here changes the gate before then.

## Context

ADR-068 Decision 3 made the review-prompt eval gate HARD and standing: "An advisory number gets waved through; a gate does not." What exists today:

- **The trigger set.** `scripts/check-review-prompt-eval` triggers on the review-prompt paths, the agenteval catch harness, `catchrategate/` and the corpora. It is run by hand: its CI job is operator-installed and is NOT installed, and no `.github/workflows` file references it.
- **The record is stale.** The committed record (`planreview-catchrate-evidence-v2`, pinned 2026-10-15) is stale on main today. The gate reports the architect, security, with_conventions and without_conventions arms stale, pending the 2026-11-01 re-pin.
- **One generator model.** The record has ONE record-level `generator_model`, claude-sonnet-5-5, part of the record fingerprint. The persona arms already run under their production model: the personas are declared on sonnet-5-5 to match. The conventions arms stand in for the standard plan reviewers, which in production are claude-fable-5-1 and claude-opus-5-5 (gpt-6-astra when available).
- **Comparability.** A baseline is comparable only under the same generator model, the same case set and `input_digest`, and the same catch rule. Any recording re-measures EVERY arm, and a subset recording is a dry run only.
- **Power floor.** It is 136 trials per arm per expectation class, with differences decided at a 0.10 tolerance.
- **The adapter.** The harness generates through the Anthropic Messages API, with no tools and no checkout. Production standard reviewers run through the `claudecode` CLI with a checkout, and astra through `codex exec`. No codex sender exists in the harness.
- **The corpus** is synthetic: some cases are modelled on real misses, but there is no real-derived intake.
- **Existing arms.** The security arms render the IMPLEMENT-review prompt. The architect persona is attached in production to plan review only.

**What is not covered:**
- **Reviewer configuration.** `.fishhawk/workflows.yaml` is not a trigger: #4624 substituted four reviewer entries with no evidence.
- **Standard reviewers on implement diffs.** They have no arm.
- **Slice vs consolidated routing** (#4687) has no arm.
- **Model changes cannot be judged.** A reviewer-model change makes the record non-comparable, so it can only re-pin, never be judged against its predecessor.

ADR-117 (ratified 2026-10-23) supplies the live half: outcomes and judgements, child-concern import, escapes and a scorecard. This record decides the offline half, and the rule that ties both halves to every review change.

## Options

- **A. Status quo.** Rejected: reviewer configuration bypasses the gate, and model changes cannot be judged.
- **B. A per-arm gate with standing arms, decision experiments and monitor arms, a reviewer-configuration trigger, and one pre-registered rule (recommended).**
- **C. ADR-117's live scorecard alone.** Rejected: it is slow and post-hoc, and cannot test a change before it ships.

## Recommendation

**Option B.**

**D1. A per-arm gate (evidence schema v3).**
- **Per-arm model and pin.** `generator_model` moves PER ARM, and each arm has its own pin and baseline. The record fingerprint becomes per arm.
- **Subset recording.** A recording may measure a SUBSET of arms. An arm that is not re-measured is carried forward, with its fingerprint and `input_digest` re-verified at gate time, and the gate judges carried arms as-is. An ADR landing then re-records only the architect arm, never the whole table.
- **Arm kinds:**
  - **`standing`.** A gating arm, re-measured at re-pins and judged against its own pinned baseline under the existing within-run rule. Today's arms become `standing`.
  - **`experiment`.** A one-off decision experiment. It is recorded once, with its pre-registered rule, under `docs/compliance/`, and is never re-measured or gating.
  - **`monitor`.** Non-gating. Its corpus is outside every trigger path, and it is recorded to a separate evidence file excluded from the record fingerprint, the comparability rule, the regression rule and the power floor.
- **A model change is a treatment, judged by a paired experiment.** A reviewer-model change is recorded as a paired `experiment`: incumbent and challenger in ONE recording, on identical cases and trial indices, under the arm's prompt, corpus and catch rule. It is judged per class by the within-run rule (catch drop and false-alarm rise ≤ 0.10). Only on a pass does the arm's pin move to the challenger, with an explicit `PIN_REASON`.
- **Declared models.** The arm table declares each arm's model. A reviewer-model change in `.fishhawk/workflows.yaml` obliges the arm-table change in the same PR (D4).
- **The adapter, stated.** Arms generate through the API adapter: no checkout, no tools, the model ID only. That differs from production's checkout-bearing CLI adapters, and every result carries that fidelity note.
- **Non-Anthropic models.** Until a codex sender exists, a non-Anthropic model is recorded `not_measured`, with a stated residual, and the gate fails closed only on measured arms. Recordings with codex would draw on the same quota whose exhaustion caused #4624.
- **New arms:**
  - **`standing`:** the standard implement reviewers on implement diffs, one arm per declared production model.
  - **`experiment`:** slice vs consolidated routing (#4687 step 3). This is a new multi-prompt `panel` kind: N slice prompts plus a consolidated prompt per case, with cross-round dedupe, and a NEW cross-slice defect class (contract mismatches between slices).
  - **`experiment`, optional:** the architect on implement diffs, as pre-enablement evidence under ADR-084 rule 7. It is not attached in production.
  - **Dropped:** an offline acceptance arm. Acceptance is an agentic stage against a live preview, so a single-shot arm would measure something else. Acceptance is measured live by ADR-117.
- **Every new arm** gets a calibration dry run (Step 1b) before its first recording, and its first recording pins.

**D2. Real-derived cases (monitor first).**
- **Sources:** ADR-117's adjudicated escapes become miss cases, and `defect`-judged catches become positive cases.
- **Entry.** They enter `monitor` arms only. Promotion into a `standing` arm happens in cohorts of at least 10 cases, only at a scheduled re-pin. The record states that the instrument moved.
- **Before commit,** every case:
  - passes the `redaction` package's patterns;
  - passes a mandatory human review that strips operator paths, emails, unmerged or rejected plan text, and untrusted issue-comment or user-report text;
  - passes the same loader rules as synthetic cases: discriminating probes, attribution, non-catching examples, and the 30–80% calibration band.

  The operator authors cases; that authoring is the real cost.
- **Security cases** are excluded until their advisory is PUBLISHED (#4516). A class-level description alone is not a case.
- **Bias, both ways.** Miss cases accumulate what INCUMBENTS missed, and catch cases what they caught. Both biases are stated in every result, and real-derived results are never used alone to compare an incumbent with a challenger.
- **Contamination.** The repository is public, so models may memorise its cases, and a public set is expected to read optimistic over time. This is stated, not prevented.
- **Not portable.** Real-derived cases are repository-specific and never feed `security_portability` (ADR-110 rule 3(b)).
- **A held-out set,** if kept outside the public tree, is an operator-run diagnostic only. Only its aggregates are committed, and it is never a gate input.

**D3. Leave-one-out is a diagnostic.**
- **How.** For a `panel` arm, every reviewer is sampled on the same case and trial index (paired). The catch rule is the union ("any reviewer caught"), and a false alarm is "any reviewer raised". A per-reviewer sidecar column records each one.
- **Statistics.** Leave-one-out is reported with a paired-proportion interval (Newcombe).
- **Never decisive.** It never alone removes or demotes a reviewer (D6). It cannot resolve contributions below the 0.10 tolerance.

**D4. The standing gate covers reviewer configuration (amends ADR-068 Decision 3).**
- **Reviewer configuration** is this closed list of subtrees of the RESOLVED spec (`defaults`/`extends` applied):
  - `reviewers.{agents (provider, model, reasoning_effort), human, authority, conventions, personas}` per stage;
  - `reviewer_personas` (model, `reasoning_effort`, remit path, `severity_cap`);
  - `review_conventions`;
  - `escalations[].require.reviewers` and `escalations[].match.paths`;
  - a declared review-routing setting, once #4687 makes routing a workflow setting.

  Comment-only and unrelated edits do not count.
- **Detection.** A Go sub-command, in a package OUTSIDE every trigger path, parses the spec at the merge base and at the head through `backend/internal/spec`, and diffs the reviewer projection. `check-review-prompt-eval` calls it.
- **The rule:**
  - The check passes when the head's reviewer projection equals the projection the committed record was measured under.
  - **Restoring a recorded or grandfathered configuration is not a review change.** The configuration in force before #4624 is grandfathered.
  - Otherwise, the check requires a passing record for exactly the arms the change affects (D1 per-arm), or an unexpired emergency stub.
- **The emergency path:**
  - The marker is a committed pre-registration stub (D5) with `emergency: {by: Captain, expires_at, reason}`.
  - `expires_at` is at most 7 days out, and only the Captain may set it.
  - The check passes while the stub is unexpired.
  - At expiry the obligation is REVERT or PRE-REGISTER, never "record evidence", since an outage usually makes evidence impossible. The D5 scheduled check reports `emergency_expired`.
- **Enforcement is honest.**
  - Until an operator installs the check's CI job, with `review-prompt-eval` in `ci-pass.needs`, the gate is run by the operator, not enforced. Installing it is in the implementation order.
  - The whole trigger extension lands WITH the 2026-11-01 re-pin, not before, so the planned revert of #4624 is unaffected.

**D5. One pre-registered decision rule for every review change.**
- **Pre-registration** is a machine-readable document (YAML front matter): the primary metric, the affected arms and cells, the threshold, the window and the author. Its schema lives under `docs/spec/`.
  - Where it lives is DECLARED in ADR-117 D5's `.fishhawk/workflows.yaml` block. This repository declares `docs/compliance/`.
  - It is committed in the change's own PR.
  - It is authored or approved by an operator or the Captain, never by the implement agent making the change.
  - The D4 sub-command checks that a reviewer-diff PR adds it.
  - At merge, a registration audit entry records its path and commit, so no directory listing is needed.
- **Offline.** The affected arms hold, or for a model change, the paired experiment passes (D1).
- **Online.** A scheduled non-diff workflow, declared in `.fishhawk/workflows.yaml` and run by fishhawkd's scheduler like `upkeep_scan`, evaluates the ONE pre-registered primary cell of ADR-117's scorecard. Its verdict is an audit entry on that run, posted through the existing issue-comment surfaces, and `regress` is routed to the first officer and the Captain.
  - **Segmentation.** Rounds are segmented by the reviewer-config projection hash of each round's CACHED spec, never by calendar date.
  - **Pre-window.** The pre-change window is the most recent equal-count window of rounds under the prior projection.
  - **Statistic.** A two-sample interval on the DIFFERENCE between post and pre (Newcombe/Wilson):
    - `regress` only if its upper bound is below −threshold;
    - `pass` only if its lower bound is at or above −threshold;
    - otherwise `insufficient_data`.
  - **Window size.** The power-derived count n is the smallest n at which the difference interval's half-width is at most the threshold, about 96 judged concerns per side at p = 0.5 and threshold 0.10. Below n, or below ADR-117's cell minimum, the result is `insufficient_data`, never `regress`.
  - **Window length.** The minimum is 14 days and the maximum 90 days. At the maximum, the result is `insufficient_data`.
  - **Escapes** are reported in a separate window lagged by 30 days, as a secondary metric.
  - **Short changes.** A temporary change shorter than the window reads `insufficient_data` by construction.
  - **Confounds.** Concurrent review changes are reported as confounded.
- **Outcomes:**
  - `regress` obliges a rollback, or a re-ratification by the Captain that states the cost.
  - `insufficient_data` leaves the change standing on its offline evidence, flagged.

**D6. Removing or re-scoping a reviewer is the Captain's decision, never automatic.**
- **The proposal.** A removal or re-scoping proposal is itself a D4 review change, and needs a D5 pre-registration.
- **What it must report:**
  - the reviewer's ADR-117 metrics, counted per ATTACHED round;
  - its own `standing` arm result, if one exists;
  - D3 leave-one-out, as a diagnostic.

  Metrics are severity-weighted, with a minimum of 30 attached rounds (or as the Captain sets per proposal).
- **The clock.** It restarts at a unique, `defect`-judged catch at the lens's maximum permitted severity. Personas are capped at MEDIUM, so their maximum is MEDIUM.
- **Exemption.** A lens that is the sole coverage of its remit class (for example, security on escalated paths) is exempt from any floor.

**D7. Portability (ADR-110).**
- D1–D4 and the arm tables are this repository's tooling (rule 5): agenteval and `check-review-prompt-eval`.
- D5's scheduled check, its registration entry and the pre-registration schema are product. Their declarations live in `.fishhawk/workflows.yaml` with product defaults. A repository without them degrades by name (rule 4), and a non-Fishhawk fixture exercises them (rule 3(a)).

## Decision

**Accepted 2026-10-23** by the Captain (repository maintainer): **Option B**, i.e. D1–D7 as stated in the Recommendation (v2), decided now. It amends ADR-068 Decision 3, ADR-084 (persona removal and re-scoping) and ADR-117 D4 (the scorecard dimension). Implementation follows the stated order, starting at the 2026-11-01 re-pin.

## Consequences

- **Gains.**
  - Model changes are JUDGED against their predecessor rather than re-pinned blind.
  - Reviewer configuration enters the gate, once its CI job is installed.
  - ADR landings re-record one arm, not the table.
  - Real failures feed monitor arms without moving the baseline.
  - Live verdicts cannot read noise as regression.
- **Cost.**
  - **Today:** about 1,180 trials per full recording, all on sonnet-5-5 ($2/$10 per Mtok).
  - **D1's standing implement-review arms:** about 272 trials per declared model. At production prices (fable-5-1 $10/$50, opus-5-5 $4/$20) that is several hundred to over a thousand dollars per arm-recording.
  - **Experiments** (routing, model challengers) are one-off, and can cost thousands each.
  - **Approval.** Every recording carries a pre-stated estimate, and the Captain approves spend above the advisory weekly budget.
- **Other costs:**
  - evidence schema v3, with per-arm models, pins and subset recording;
  - the arm kinds;
  - the paired-experiment path;
  - the reviewer-diff sub-command;
  - the emergency stub;
  - the pre-registration schema and its registration entry;
  - the scheduled check workflow;
  - case authoring and review for real-derived cases;
  - a codex sender, if astra is to be measured.
- **Residuals:**
  - **Adapter fidelity:** API vs checkout-bearing CLI.
  - **Multiplicity:** about 20 one-sided comparisons per re-pin gives a material chance of a spurious refusal. That is stated, and a family-wise correction is left to a later re-pin decision.
  - **Contamination.**
  - **Astra** is unmeasured until a codex sender exists.
  - **The emergency gap** is at most 7 days.
- **Implementation order:**
  1. At the 2026-11-01 re-pin, in one batch:
     - schema v3 and subset recording;
     - arm kinds;
     - the reviewer-diff sub-command and the trigger extension;
     - the emergency stub;
     - the pre-registration schema.
  2. An operator installs the `review-prompt-eval` CI job (`.github/workflows/**` is operator-authored).
  3. The standing implement-review arms, calibrated and then pinned.
  4. The D5 scheduled check, once ADR-117's scorecard (#4695) exists.
  5. Experiments as needed: #4687 step 3 first.
  6. The D2 monitor intake, once ADR-117 escapes exist.
- **Record metadata.** amends: ["ADR-068", "ADR-084", "ADR-117"]; supersedes: []; applies_to: ["backend/internal/agenteval/**", "scripts/check-review-prompt-eval"]
  - ADR-068 D3's scope widens to reviewer configuration.
  - ADR-084: removing or re-scoping a persona is now governed by D6.
  - ADR-117 D4's scorecard dimensions gain the reviewer-config projection hash.
- **Related:** ADR-049, ADR-068, ADR-084, ADR-110, ADR-117, E55.4 (#2245), #4235, #4516, #4600, #4624, #4687, #4697.
