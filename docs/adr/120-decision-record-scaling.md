---
id: ADR-120
title: "The decision record scales: lean index lines, a topic map beside the record, a partitioned index"
status: accepted
date: 2026-10-23
issue: https://github.com/kuhlman-labs/fishhawk/issues/4709
supersedes: []
superseded_by: []
amends: ["ADR-084", "ADR-113"]
applies_to: ["backend/internal/decisionrecord/**", "scripts/check-adr", "docs/adr/index.json", "docs/adr/README.md", "docs/adr/topics.json"]
---

# ADR-120: The decision record scales: lean index lines, a topic map beside the record, a partitioned index

## Status

**ACCEPTED (v2, ratified by the Captain 2026-10-23).** Drafted by the first officer on 2026-10-23 at the Captain's direction.
- **Review history:** v1 was reviewed by claude-fable-5-1 (reject; direction endorsed) and claude-opus-5-5 (approve_with_concerns). Their fixes converge, and v2 folds in every finding.
- **Freeze:** tracker-only until ratified; the in-repo record follows the upkeep rule. Every step lands under ADR-118's gate.

## Context

The architect persona (ADR-084) sees the decision record in two ways (`backend/internal/decisionrecord`):
- **The whole index**, `RenderIndex`: one compact JSON line per record, injected into every review and capped at 32,768 bytes (`repodoc.DefaultMaxBytes`).
- **Selected record bodies**, `Select`: records whose `applies_to` matches a change path, plus every record that supersedes or amends a selected one (followed forward only). They are injected within a separate 32,768-byte record budget: whole records first, then excerpts, then dropped.

Other records are reachable only through read-only grounding, which is:
- per round, all or nothing (`allInvocationsGrounded`);
- off by default (`FISHHAWKD_REVIEW_GROUNDING`);
- absent on `github_actions`;
- read at the change's HEAD.

Findings from grounded reads are `low` and unquotable (`docs/review/architect-remit.md`).

**Three limits bind.**

1. **The index's size.** After #4699 it renders 24,200 bytes at 113 records, 74% of the cap.
   - Each line repeats its `path` (6,812 bytes in total), even though the path follows the fixed `docs/adr/NNN-slug.md` convention.
   - The verify floor (10 lines at the largest line) fires at about 28.7 KB, roughly 20–40 records from now.
   - A customer repository with hundreds of ADRs exceeds the cap on day one, so this is a product limit (ADR-110).
2. **Selection is nearly blind.**
   - Only 19 of 113 records declare `applies_to`.
   - The other 94 are empty by the README's deliberate rule ("empty unless the Decision or Consequences names a path as the decision's SUBJECT … when in doubt, `[]`"). ADR-110 is the one crowding-motivated case.
   - Most decisions reach the architect only as an index line.
   - **The recorded escape:** ADR-042's index line was shown, but its body was not selected, and its status read `unknown`. #4583 shipped what it declined (ADR-119).
3. **Governing chains are long.**
   - ADR-090's text is spread across ADR-092, ADR-093, ADR-096 and ADR-098.
   - ADR-093 has three amendments: ADR-109, ADR-112 and ADR-115.

**Constraints:**
- ADR-113 D3 forbids new front-matter keys and index fields on records, and fixes the record-metadata line.
- `ParseIndex` refuses unknown fields.
- The architect eval arm runs ungrounded through the API adapter (ADR-118 D1), so it cannot observe a persona choosing to read.

## Options

- **A. Raise the caps.** Rejected as the strategy: the cost scales with record count, and the limit only moves. (A separate 64 KB index cap is about 6k tokens per review. It is kept as an emergency bridge only.)
- **B. A leaner rendering, a topic map beside the record, a topic-partitioned index, and consolidation (recommended).**
- **C. Model-chosen retrieval: grounding, embeddings, or the persona asking for records.** Rejected as the primary channel:
  - it is ungated by the eval arm;
  - its findings are `low` and unquotable;
  - embeddings conflict with ADR-084 rule 4 ("no model preselection") and add a product dependency.

  A server-mediated lookup is left to a later record.
- **D. A single living architecture document.** Rejected: it loses per-decision provenance, status and supersession, on which ADR-113 depends.

## Recommendation

**Option B.**

**D1. A leaner index line.**
- The rendered line drops `path`. The framing states the `docs/adr/<NNN>-<slug>.md` convention, and each record's path stays in `index.json` and is fetched as today.
- Titles of records NOT selected for the change are capped at 60 characters. Selected records keep the #4699 cap of 100.
- The rendering version is bumped. The measured effect is about 13.5 KB at 113 records, room for roughly 270 records.
- Status is unchanged: every `accepted`, `proposed` and `unknown` record keeps a full line. With PR #4707's rulings, no `unknown` records remain.

**D2. A topic map beside the record, not in it.**
- **The file.** A strictly parsed, repository-declared file `docs/adr/topics.json` (schema `adr-topics-v1`) maps each topic name to path globs and record ids, plus a declared `standing` list.
- **Location.** It is discovered next to the declared index, so no workflow-schema change is needed. It is fetched at the admission commit through repodoc and attributed in the same injection set. A missing file degrades by name (ADR-110 rule 4), and the product default is no topics.
- **Validation.** `scripts/check-adr` validates it:
  - the topic vocabulary is closed;
  - every id exists;
  - the globs pass `spec.Predicate`;
  - every record from ADR-121 onward is either listed under a topic or declares `applies_to`.
- **Nothing else changes.** No record, no front-matter key and no index field changes (ADR-113 D3 holds). "Records transcribe, never correct" also holds, because a topic is classification, not transcription.
- **Selection.** `Select` matches a record by its own `applies_to`, OR by a topic whose globs match a change path.
- **Ranking:**
  - the order is match kind (path, then topic), then status, then matched paths, then newer;
  - a followed successor or amendment takes its root's match kind;
  - topic matches are ordered by topic specificity, the fewest matched paths first;
  - at most a declared number of topic-matched records (default 4) are shown whole, and the rest are excerpted.

  An unknown or proposed record matched by path therefore outranks an accepted record matched by topic.
- **The standing list** (for example ADR-099, ADR-110 and ADR-113) is shown as excerpts from the record budget, within a declared sub-budget (default 6 KB).
- **The backfill.** The 94 untagged records are assigned topics in one reviewed batch, and the Captain approves the vocabulary and the assignment.

**D3. A topic-partitioned index (replaces model-driven retrieval).**
- **When it applies.** Once the D1 rendering exceeds 85% of the cap (declared), the injected index becomes:
  - full lines for every record in a topic the change touches;
  - full lines for every selected record, the standing list, and every record a committed corpus case names;
  - a one-line count per untouched topic.
- **What does not change.** Everything stays injected, verifiable and pinned to the admission commit, and ADR-084 rule 4 is untouched.
- **Drop order.** On a round that still overflows, the rendering drops by a declared order (superseded and rejected records first, then oldest), never from the tail. The truncation is reported.

**D4. Consolidation by verbatim derivation.**
- **What triggers it.** When a record accumulates three or more amendments, or its governing text spans three or more successors, an upkeep item proposes a consolidated ADR.
- **How it is built.** The ADR is built ONLY from verbatim quoted Decision clauses, each tagged with its source id and section. `check-adr` verifies that each quoted block occurs byte-for-byte in the cited record, and connective text is limited to headings.
- **Supersession.** It `supersedes` only records whose every decision clause it carries. Otherwise it `amends` them.
- **Ratification** is under ADR-113, as an operator-led change.
- **Due first:** ADR-090/092/093/096/098 with ADR-109/112/115. The consolidated Decision must be self-contained, because a body over the record budget shows only as its Decision excerpt.

**D5. Each step is measured, not assumed.**
- Each step adds at least one architect corpus case that fails without it:
  - D2: an ADR-042-shaped, topic-only catch;
  - D3: a governing record reachable only through a touched partition;
  - D4: a catch that depends on a consolidated chain.
- This moves the instrument, so it is a new baseline under ADR-118.
- D1, D2 and D3's rendering land in ONE post-re-pin recording, and D4 follows separately.
- `backend/internal/decisionrecord/` and `docs/adr/index.json` become eval triggers (#4700).

## Decision

**Accepted 2026-10-23** by the Captain (repository maintainer): **Option B**, i.e. D1–D5 as stated in the Recommendation (v2), decided now. It amends ADR-084 (D4 and rule 4's whole-index injection) and ADR-113 (D4's live index-size checks and frozen-tree contents).

## Consequences

- **Gains.**
  - The index fits about 270 records at once, and scales by partition beyond that.
  - Most decisions become selectable without editing them.
  - Selection stays deterministic and eval-gated.
  - Chains become readable without changing any decision.
  - Other repositories get a decision record that scales.
- **Costs:**
  - the rendering change and its version bump;
  - the `adr-topics-v1` file, its parser, the check-adr validation and the `Select` ranking;
  - a 94-record topic batch;
  - the partitioned rendering;
  - the derivation check;
  - corpus cases;
  - one architect recording for D1–D3 and another for D4.
- **Residuals:**
  - a wrong topic hides a record, which ADR-117 escapes measure;
  - more selected bodies raise the architect's false-alarm exposure, which the arm measures;
  - consolidation's connective text is human-reviewed only.
- **Implementation order:**
  1. D1, D2 (the file, the parser, the check-adr validation and the ranking, with the backfill) and D3's rendering behind its trigger, bundled into one post-re-pin recording with their corpus cases.
  2. A D4 consolidation of the ADR-090/093 chain.

  The headroom floor test (#4699) and the 64 KB bridge cover the interval.
- **Record metadata.** amends: ["ADR-084", "ADR-113"]; supersedes: []; applies_to: ["backend/internal/decisionrecord/**", "scripts/check-adr", "docs/adr/index.json", "docs/adr/README.md", "docs/adr/topics.json"]
  - ADR-084: D4 and rule 4's whole-index injection become partitioned.
  - ADR-113: D4's live index-size checks and frozen-tree contents gain the topic map.
- **Related:** ADR-042, ADR-068, ADR-084, ADR-110, ADR-113, ADR-117, ADR-118, ADR-119, #4699, #4700, #4707.
