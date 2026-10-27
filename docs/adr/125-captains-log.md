---
id: ADR-125
title: "The captain's log: evidence-bound lessons, confirmed by a human, read back to the driver"
status: accepted
date: 2026-10-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/4756
supersedes: []
superseded_by: []
amends: ["ADR-082"]
applies_to: []
---

# ADR-125: The captain's log: evidence-bound lessons, confirmed by a human, read back to the driver

## Status

**Accepted (v3.1).** Ratified by the Captain on 2026-10-27. Drafted by the first officer on 2026-10-27 at the Captain's direction ("a captain's log at the beginning and end of each episode … to reflect back on what was happening and what they learned").

**Review history.** v1 was reviewed by claude-fable-5-1 (revise) and gpt-6-astra (revise). Both called the kernel worth building and the rest duplicated, theatre, or unsafe. v2 is the minimal version they converged on:
- operator-facing only;
- human confirmation;
- decision-bearing evidence, with scope derived from it;
- routing through existing verbs;
- recurrence only as a declared predicate, never an escape;
- a usage measure that can retire the feature.

v2 was confirmed by claude-fable-5-1 as revise, with four text blockers: scope derivation, the proposing rule, the ADR-123 reference, and the consult carve-out. v3 folds those in, along with its notes (`log_reviewed`, who sets `watch`, and the usage-measure denominators). The gpt-6-astra v2 round failed on quota. Its v3 review (revise) found two contract gaps: generation-bounded scope, and non-agent governance of `folded`. v3.1 folds both, plus its scorer-specification note.

Ratified by the Captain.

## Context

**What exists:**
- **The ship's log.** The audit chain records every decision.
- **ADR-082 (#3728), the historian.** It derives a decision index from the chain, ranks precedent deterministically at the gate, and asks the divergence question ("one-off or doctrine change?"). Its rule 6 keeps precedent CAPTAIN-FACING in alpha: none of it reaches planner or reviewer prompts. The single carve-out is ADR-081's structured-field consult.
- **E76.** The handover brief.
- **ADR-117.** Concern outcomes and escapes.

**What no reader of the chain records** is what an episode taught: which judgement call should go differently next time, and why.

**Where that lives today:** in the operator's private memory (about 150 entries, about 42k tokens loaded on every turn), in hand-written retros (#4501), and in scratch handover briefs. It is unportable, unverifiable, invisible to a second operator (E76) or a spawned operator (ADR-101), and loaded whether relevant or not.

**#4745** inventories that memory. Its own finding is that most entries are product UX workarounds, whose correct home is a fix: `next_actions`, error text, docs or a guard. A log that merely read those back would preserve the workaround. This record is for the RESIDUE: judgement calls that a product change cannot remove. It is sequenced after #4745's inventory.

## Options

- **A. Status quo.** Rejected: private, unverifiable, unbounded.
- **B. v1:** per-episode opening and closing entries, predictions, agent-prompt delivery, recurrence-as-escape. Rejected by both reviewers:
  - a persistent cross-episode injection channel that reverses ADR-082 rule 6;
  - vacuous evidence;
  - undecidable recurrence;
  - duplicated routing;
  - prediction theatre.
- **C. Evidence-bound, human-confirmed lessons, with a bounded read-back to the driver only, through the historian's surfaces (recommended).**

## Recommendation

**Option C.** It amends ADR-082, extending the historian's read path with one written record type.

**D1. One record type: `lesson_recorded`, appended to the audit chain.**

Fields:
- **`statement`:** bounded length.
- **`class`:** `defect | guidance | guard | judgement | doctrine_question`.
- **`evidence`:** one or more audit entry ids. Each must be a DECISION-BEARING entry: a decision-index class, a concern, a verify red, a fix-up, an amendment, a waiver, a rejection or an escape. It must be on the episode's run, or on a member run of its campaign. Evidence is stored with each entry's hash.
- **`scope`:** DERIVED, never typed by the author, per cited entry:
  - `Store.GateContext` over the entry's run and stage (workflow, stage kind, touched paths, escalation keys);
  - plus the decision class and normalized concern category, when the entry is a decision-index row.

  The lesson's scope is the union. An entry with no run is refused.

  **Scope is bounded to each cited entry's episode generation and recorded context cutoff.** The derivation reuses `GateContext`'s extraction helpers, but not its latest-plan query: it resolves the plan revision in force at the cited entry's sequence. The resolved scope, the source plan id and hash, and the derivation version are persisted with the lesson. Later amendments or generations never change it. If the historical context cannot be established, scope is `unavailable`, and the lesson is refused. The current run context is never substituted.
- **`author`:** derived by the server from the authenticated identity, never asserted.
- **`status`:** `proposed | confirmed | refuted | superseded | folded | retired`.

Status changes are new entries that cite the prior one, and the record is append-only. Writes are bounded: a per-episode count and byte cap, both declared.

**D2. Who writes, who confirms.**
- **Proposing** requires the identity that authenticated the request to hold one of:
  - plan-approval authority on the run's repository (ADR-040, ADR-113);
  - an agent identity acting under a charter delegation, with `delegated` recorded (ADR-101).

  A first-officer seat may propose only by citing its own finding (ADR-089).
- **Run-scoped tokens:** implement, review and specialist run tokens cannot write lessons.
- **Derived fields:** the server derives `author` and `actor_kind` from the authenticated identity on each write.
- **Status transitions.** Creation always produces `proposed`. EVERY later status transition (`confirmed`, `refuted`, `superseded`, `retired` and `folded`) is a NON-AGENT act by an identity with repository plan-approval authority, recorded with actor kind, under the credential floor in force for stage approval gates (ADR-113 as amended by ADR-123). `folded` additionally requires a server-verified link to the routed item's merge. Agents may PROPOSE a transition, but not make it.
- **What confirmation means:** "worth remembering". It confers NO policy authority. A lesson never grants authority, satisfies an approval, edits a spec, standing order, delegation, charter or decision record, or becomes a judge input (ADR-121).

**D3. No new router: lessons route through existing verbs.**
- `defect`, `guidance`, `guard`: the driver files them with `fishhawk_file_issue` or `fishhawk_defer_concern`, citing the lesson id. A `guidance` item that lands in an agent instruction path, a review convention or the decision record is tier I (ADR-121) and lands through that classification.
- `doctrine_question`: opens ADR-082's divergence question from the lesson. A doctrine change stays the governed standing-order change.
- `judgement`: stays in the log. It is read back (D4) and never enforced.
- **Folding.** When an item routed from a lesson merges, the lesson may be marked `folded` by a non-agent (D2), citing the merge. A folded lesson is no longer read back, because the product now carries it.

**D4. Read-back: the driver's surfaces only (ADR-082 rule 6 holds).**
- **When it runs:** at plan approval and at implement dispatch, the points where scope and touched paths exist. It does not run at run creation.
- **What it selects:** only `confirmed` lessons, never `proposed`, `refuted`, `superseded`, `folded` or `retired`.
- **Hard filters:** same account and repository; a derived-scope match; and compatibility with the policy version (workflow major and decision class).
- **The scorer** is a lesson scorer separate from `precedent.Rank`. The implementation is obliged to specify its weights, missing-field behaviour and scope-match semantics in the owning package README, and to version them. It has:
  - named weights;
  - Jaccard over path prefixes, reusing `precedent`'s helper;
  - concern-category and decision-class equality;
  - a total-order tie-break (newest confirmation, then id);
  - `NaN` refused;
  - a scorer version.
- **Bounds:** K lessons and B bytes, both declared.
- **What it records:** the selected ids, their hashes, the scorer version and the chain snapshot sequence. It reports `truncated`, `retrieval_unavailable` and `no_match` as distinct states.
- **Where it renders,** as DATA with cited ids: `get_run_status`, the gate view, the digest and the handover brief.
- **Not a historian row.** A lesson is never a `decision_index` row and never a historian-responder class. ADR-082 rule 6 is amended only to add `lesson_recorded` to its captain-facing surfaces. Decision (e)'s structured-field consult carve-out is unchanged, and the consult responder cannot return a lesson.
- **What it never reaches:** no planner, implement or reviewer prompt receives a lesson. Agent-facing delivery requires its own ADR-081/082 amendment, made after the live injection arm (#3187) has measured behavioural resistance. Structural enclosure alone is not enough.

**D5. Recurrence: only as a declared predicate.**
- **The predicate.** A lesson may carry a `watch` predicate over a closed observable vocabulary: fix-up rounds, amendment count, concern category, verify reds, merge outcome, acceptance state. The vocabulary is versioned. The proposer sets it at write time. The confirmer may edit it at confirmation, and it is frozen after that. Only a `confirmed` lesson's predicate is evaluated.
- **What fires.** When the predicate holds on a later episode whose derived scope matches, the product records `lesson_recurred`, a digest line for the driver.
- **What it is not.** It is not an ADR-117 escape, and it is not proof the lesson failed: the lesson may not have been delivered, or may have been deliberately not applied. A lesson without a predicate computes no recurrence.

**D6. Disclosure and storage.**
- **Access.** Lessons are access-controlled internal records. They are NOT issue comments and NOT site content.
- **Public export.** Any export to a public surface needs an operator-reviewed, allow-listed projection.
- **Secrets.** They are redacted before append.
- **Security lessons.** In a public repository, a lesson about a security defect carries the class and the fix contract only (#4516). A reproduction stays in private storage, and a lesson may cite it by id only.

**D7. Episodes, generations and closure.**
- **Episode kinds.** The built-in kinds are a run and a campaign.
- **Generations.** A revived or reopened run starts a new episode generation. Lessons cite the generation they were written in, and a late observation appends to it.
- **Closure.** The driver closes an episode's log with either at least one lesson or a `log_reviewed` marker, an audit entry with an optional one-line note meaning "reviewed, nothing to record". The digest lists a terminal episode with neither as `log_unreviewed`. This is visible, never blocking: absence of reflection stays distinguishable from a review that found nothing.

**D8. A usage measure and a retirement criterion.**
- **The measure.** Over the same reason kinds (approve, waive and reject reasons), the digest reports the rate per reason of citing a lesson id, beside the rate of citing a precedent entry by hand, plus confirmed lessons per episode. ADR-082's hand-citation finding (64 of 1,726 waive reasons) is the historical reference. The baseline is the hand-citation rate measured over the same reason kinds in the declared window before launch.
- **The retirement criterion.** If, after a declared window (default 60 days of use), the lesson-citation rate does not exceed that baseline, the Captain is asked whether to retire the feature.

**D9. Portability (ADR-110).**
- **Product mechanisms:** the record type, the derivation, the scorer and the surfaces.
- **Repository declarations, with conservative defaults:** the caps, K/B, the windows, and the predicate vocabulary extensions.

## Decision

**Accepted 2026-10-27:** ratified by the Captain (v3.1). Adopt option C, as stated in the Recommendation (D1–D9).

## Consequences

- **Gains:**
  - judgement calls become portable, evidence-bound, human-confirmed, and read back by relevance to whoever drives next, including a second operator or a spawned operator;
  - a lesson that names a product gap is routed to fix it, and folded away when fixed;
  - the feature measures its own use.
- **Costs:**
  - `lesson_recorded` and `lesson_recurred` (category registry; operator-only, no issue-comment surface);
  - the lesson validator and scope derivation;
  - the scorer;
  - render lines on four existing surfaces;
  - the predicate evaluator;
  - the digest counts.
- **Residuals:**
  - a lesson can be confirmed and still wrong; status changes and folding bound this;
  - scope derivation is structural, so it misses semantic matches;
  - a driver can record nothing. `log_unreviewed` shows absence, not quality.
- **Operator memory (not a decision).** The product cannot control what a harness loads at session start. Once lessons exist, an operator MAY move judgement-class memory entries into the log, per entry, keeping a rollback, and verifying retrieval before changing what loads. #4745 owns the migration.
- **Sequencing:**
  1. #4745's inventory.
  2. This record.
  3. Implementation.

  Nothing here touches the frozen prompt package, so the catch-rate freeze does not gate it.
- **Deferred, each needing its own decision:**
  - opening/closing snapshots and predictions;
  - custom episode kinds beyond run and campaign;
  - agent-facing delivery;
  - automatic routing;
  - per-driver calibration.
- **Record metadata.** amends: ["ADR-082"]; supersedes: []; applies_to: []
- **Related:** ADR-081, ADR-089, ADR-101, ADR-110, ADR-117, ADR-121, ADR-122, ADR-123, E75 (#3696), E76 (#3697), E77 (#3698), #3187, #3738, #4501, #4516, #4745.
