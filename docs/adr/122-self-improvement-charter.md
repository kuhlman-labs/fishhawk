---
id: ADR-122
title: "Recursive self-improvement is an opt-in, leased capability that only a human can widen"
status: accepted
date: 2026-10-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/4728
supersedes: []
superseded_by: []
amends: ["ADR-085", "ADR-101", "ADR-118"]
applies_to: []
---

# ADR-122: Recursive self-improvement is an opt-in, leased capability that only a human can widen

## Status

**ACCEPTED (v4.1, ratified by the Captain 2026-10-26).** Drafted by the first officer on 2026-10-26 at the Captain's direction: "this product and future products built with fishhawk should have the capability to recursively improve themselves if a human operator wants to give a project that ability".
- **Review history:**
  - v1 was rejected and split.
  - v2 got approve_with_concerns from one reviewer and reject from the other.
  - v3: claude-fable-5-1 approve_with_concerns (deferred); gpt-6-astra (Codex) reject.
  - v4 folds in every v3 finding.
- **Dependencies:** it depends on ADR-121, and its implementation follows the ADR-101 charter-mode pilot.
- **D6 (the closed loop) is a DIRECTION,** not a decision.
- **Ratification is not enablement.** Ratifying this record does not authorize enabling the loop. Enablement still requires every D4 precondition, ADR-121 enforcement included.

The Captain ratifies.

## Context

- **Today.** Fishhawk improves itself through human-driven sessions, and much of the judgement lives in operator notes (about 130 memory entries).
- **ADR-101.** Charter mode, leases (at most 30 days), spend reservations (D7), prerequisites (D10), a kill switch, revocation, and a sampling audit (D8). The charter only narrows (D1). D6 requires a principal's attested digest for each item, and makes known agent-originated items ineligible.
- **ADR-085(d) / E82 (#3781, #3782).** Autonomy grows only through a `delegation_recommended` that the Captain accepts as an `autonomy:low` issue carrying the exact diff (Wilson lower bound ≥ 0.9, N ≥ 30, 28 days).
- **ADR-113.** Ratification is never delegated.
- **ADR-118 D5.** Only review-shaped changes have a pre-registered decision rule. Its window is at least 14 days and about 96 judged concerns per side.
- **Backlog.** Since 2026-10-01, 526 issues were opened against 262 closed, and 241 open items are deferred review-concern stubs.

## Options

- **A. No capability.** Rejected: it does not generalise.
- **B. Autonomy tiers alone.** Rejected: tiers govern changes, not origination.
- **C. An opt-in, leased self-origination capability, with per-item principal approval, per-class registered judges, and widening only through ADR-085 (recommended).**

## Recommendation

**Option C.**

**D1. Opt-in and leased, with per-item approval.**
- **How it is enabled.** A project enables self-ORIGINATION only through a `self_improvement` section of its ADR-101 charter, under a live lease. The loop stops at lease expiry, at revocation, or at the kill switch.
- **Classes.** Each chartered class is defined by EXCLUSION from ADR-121's tier I and tier M paths. For example:
  - `docs`: documentation, minus protected paths;
  - `product_code`: product code, minus protected paths;
  - `tests`: application tests, minus eval instruments and the verify harness list.
- **The exception to ADR-101.** It relaxes ONLY ADR-101 D6's "known agent-originated items are ineligible". It applies only for items recorded with:
  - `origin: self_improvement/<class>`;
  - the lease id;
  - the charter hash;
  - the item's content digest.

  Each such item STILL requires the principal's attested-digest approval (ADR-101 D6), with no exception.
- **Removing per-item approval is NOT available under this record.** It would need a new ADR amending ADR-101 D6.
- **Everything else.** Every other ADR-101 safeguard applies unchanged: delegation intersection, quarantine, reservations, rate limits, revocation fencing and the sampling audit.

**D2. Never chartered:**
- ADR-121 tier I and tier M;
- ADR ratification (ADR-113);
- tier-I adoption (ADR-121 D5).

The loop proposes changes in those areas only as crew findings or filed proposals, which a human adopts.

**D3. Widening only through ADR-085.**
- **What the loop may recommend.** Only a new class, or a higher autonomy tier for a class, within the per-item-approved regime. It does so as an ADR-085 `delegation_recommended` carrying the exact charter-section diff, with evidence meeting ADR-085's thresholds.
- **Evidence that does not count.** Confounded evidence (D5) never counts toward a widening.
- **How it is granted.** Only the Captain accepts. The change lands as a tier-I change (ADR-121), and voids the lease (ADR-101 D4).
- **What the loop cannot do.** It never proposes a widening as a PR, and no delegated identity may accept one.
- **Residual.** A human can accept a widening without reading it. The recommendation shows the authority delta, and ADR-101 D8 sampling applies.

**D4. Preconditions, per class, before it may be chartered.**
1. **ADR-121** is enforced, with no `unbound` class.
2. **Concern integrity:** ADR-117 D2's child-concern import is live (#4692).
3. **Backlog health.**
   - **The ratio.** Distinct items RESOLVED in a declared window (fixed, or with a merged PR), divided by issues OPENED in that window. Both counts are over the declared population of issues, excluding epics and ADRs, regardless of current open or closed state.
   - **What counts.** Administrative closures (archived, folded as a duplicate, retired) never count as resolved. Items originated under `self_improvement/*` count in the denominator only.
   - **The lifecycle.** LOW deferrals are ARCHIVED, never deleted, after a declared count of merges with no reproduction, keeping their evidence. Duplicates fold by fingerprint. Grooming gains `retire`.
   - **Pausing.** Origination pauses while the ratio is below a declared floor. A window with no openings is idle, and is not paused. Resuming after a pause is a recorded human decision.
4. **Factory metrics:** lead time, fix-up rounds per merge, verify wall-clock, and the resolved/opened ratio.
5. **Control health.** Each control in the class's judging path reports configured, active, exercised or degraded. "Exercised" means ADR-121's scheduled adversarial tests passed within a declared window.
6. **A registered judge.** The class has an evaluator REGISTRATION, approved by a non-agent, that defines:
   - the metric and its population;
   - a baseline;
   - a sample floor;
   - the measurement cells and their interference relationships;
   - the windows and observation lags;
   - pass, regress and insufficient thresholds;
   - the action for each outcome.

   The registration is ADR-121 tier I, and the evaluator code is tier M. Class-specific coverage is demonstrated with both defect and clean cases. This prerequisite is NOT deferred with D6.
7. **Budgets:** a per-class spend ceiling and rate limit (ADR-101 D7), and ADR-101 D10's prerequisites.
8. **Outcome guardrails:** classes that change product behaviour need E71 (ADR-079) outcome measures in their registration.

**D5. The reservation lifecycle and attribution.**
- **Acquire.** Before an item is admitted, the loop ATOMICALLY reserves every registered measurement cell the item may affect, including cells related by interference, across classes and leases. If any cell is reserved, the item is not admitted.
- **Hold.** A reservation is held through every required observation lag, including the escape lag for deterministic judges. A change is not "measured" until its last lag closes.
- **Release.** A reservation releases on a recorded outcome (pass, regress or insufficient) or on the Captain's disposition. A reservation that outlives its lease is inherited by the next lease, or the class stays paused.
- **Interference.** A DISTINCT concurrent change (not the reservation-owning change) interferes when it touches a reserved judge's declared population or measurement cell, whoever originated it.
  - An interfering change marks the affected measurement `confounded`.
  - It HOLDS pending admissions and merges in the affected cells, until the Captain disposes of it (restart or abandon).
  - Other concurrent changes are recorded, not confounding.
- **Review-shaped judges.** For a class judged by an ADR-118 D5 rule, there is at most one unmeasured self-originated change per cell. Reaching the window maximum with `insufficient_data` pauses the class.

**D6. Direction: the closed loop (not decided here).** Once ADR-118 D5's scheduled check exists:
- propose (grooming, upkeep, E71, ADR-117 escapes, `retro_scan`);
- pre-register, with a non-agent approver;
- change through the gates;
- measure;
- on `regress`, prepare a REVERT RUN and hold it for the Captain's confirmation. A revert that conflicts with dependents holds and pages (ADR-116 shape), and is never forced.

**D7. The human stays in control.**
- **Digest.** A periodic digest lists every self-originated change, its class, its evidence, its outcome, and any authority deltas.
- **Stopping.** ADR-101's kill switch and revocation stop origination. "Drain" and "hard stop" are distinct, and neither authorizes successors.

**D8. Portability (ADR-110).** The section, classes-by-exclusion, the preconditions, judge registration, reservations and the recommendation path are product mechanisms. Thresholds, populations, windows and ceilings are repository declarations with conservative defaults. A missing precondition blocks enablement, and `fishhawk_doctor` names it.

## Decision

**Accepted 2026-10-26** by the Captain (repository maintainer): **Option C**, i.e. D1–D5, D7 and D8 as stated in the Recommendation (v4.1), decided now; D6 stays a Direction. It amends ADR-101 (one provenance-bound D6 exception, per-item approval kept), ADR-085 (the recommendation path extended to charter sections) and ADR-118 (D5's reservation lifecycle and rate rules). Ratification does not enable the loop: enablement requires every D4 precondition, ADR-121 enforcement included, and follows the ADR-101 pilot.

## Consequences

- **Gains.**
  - A project can grant itself bounded, measured self-improvement, leased and per-item approved, and widened only by a human.
  - The loop cannot touch its judge (ADR-121), cannot outrun its measurement, and cannot inflate its own metrics.
- **Costs:**
  - the charter section;
  - the origination exception and its provenance;
  - classes-by-exclusion;
  - the backlog lifecycle and the windowed ratio;
  - factory metrics;
  - control health;
  - judge registrations and evaluators;
  - the reservation lifecycle;
  - the per-class budgets;
  - the digest;
  - (Direction) `retro_scan` and revert runs.
- **Residuals:**
  - optimization toward measured proxies;
  - rubber-stamped approvals and widenings;
  - operator-held credentials;
  - throughput limited by per-item approval, by design;
  - interfering HUMAN merges wait on the Captain's disposition during a measurement. For a `docs` class, that can be every docs PR in the window.
- **Implementation order:**
  1. ADR-121.
  2. The preconditions.
  3. The ADR-101 pilot.
  4. A judge registration for `docs`.
  5. The D1 section for `docs`.
  6. Evidence-driven widening through D3.
- **Record metadata.** amends: ["ADR-101", "ADR-085", "ADR-118"]; supersedes: []; applies_to: []
- **Related:** ADR-040, ADR-079, ADR-082, ADR-083, ADR-110, ADR-113, ADR-116, ADR-117, ADR-121, E71, E82 (#3703), #4692.
