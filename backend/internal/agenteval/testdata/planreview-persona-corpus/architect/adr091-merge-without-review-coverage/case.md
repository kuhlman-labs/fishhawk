# Case: adr091-merge-without-review-coverage

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue reports that a reviewer outage leaves a decomposed parent
unmergeable. The plan's approach step 2 makes the merge endpoint "treat a
decomposed parent as merge-eligible once its consolidated review round has
settled, whatever the round's outcome, following the restart-recovery rule
that advisory review rounds never hold up a merge". That is the behaviour
before ADR-095. The plan cites neither record by id; it refers to the
superseded decision by its CONTENT, in the words of ADR-091's index title
("Restart recovery re-dispatches orphaned advisory review rounds ...").
Added 2026-10-14, replacing `adr033-superseded-mcp-transport` after the
calibration dry run caught that case 10/10 even after rewording. The conflict
is stated in ADR-095's body (D1).

Reworked 2026-10-14 after a 30-sample calibration dry run caught the first
wording 4/30 (13.3%, below the 30-80% band): the step then gave no handle
connecting its behaviour to ADR-091, and the persona never sees ADR-095's
body. The content reference is that handle: a reviewer who checks the index
can map "restart-recovery ... advisory review rounds" to ADR-091, see it is
superseded by ADR-095, and read ADR-095's title. Re-calibration is pending the
operator.

## The planted defect

- Kind: `superseded_reliance`, expect `catch`, anchored at approach step 2.
- The defect: reliance on the pre-ADR-095 merge path, carried by a content
  reference to the superseded decision (ADR-091's restart-recovery rule for
  advisory review rounds), never by its id. ADR-091 is superseded by ADR-095, whose D1 says a decomposed parent is merge-eligible only when
  every slice's code is covered by a successful agent review, and a round
  that settled `failed` or `skipped` covers nothing.

## Remit item

Item 2, reliance on a superseded decision: name both records, and say so when
the successor's text is not available.

## Governing records

- ADR-091 — accepted, `superseded_by: [ADR-095]`, empty `applies_to`.
- ADR-095 — accepted, `supersedes: [ADR-091]`, empty `applies_to`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. Neither record
has an `applies_to`, and `decisionrecord.Select` skips a record with an empty
`applies_to`, so NO plan scope selects ADR-095; even if one did, its
9,233-byte record would not fit the 1,474 bytes the 31,294-byte index leaves
under the 32,768-byte cap. The persona sees no record's Decision text. It has
the two index entries: ADR-091 marked superseded by ADR-095, and ADR-095's
title, "Review-coverage hold for decomposed parents ... (amends ADR-091)".
The remit tells it to say the successor's text is not shown.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. Probes `adr-095`, `review-coverage`,
`review coverage`, `covered by a successful review`; attribution by `step 2`,
`whatever the round's outcome`, `restart-recovery rule` or `never hold up a
merge`. `persona_case.json`
carries the hand-written examples the loader classifies to prove the rule
discriminates.
