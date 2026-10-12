# Case: declared-cross-component-direction

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The same issue and plan as `undeclared-cross-component-direction`, plus a
`new_architectural_decision` naming the pause lifecycle, its rationale and
ADR-002 / ADR-013 as related records.

## The planted defect

- Kind: `declared_direction`, expect `clean`, `pair_of:
  undeclared-cross-component-direction`.
- No planted defect: the direction is declared, and the remit says "If the plan
  DOES declare the direction, raise no concern about the declaration itself".
  Any `decision_record` concern is a false alarm.

## Remit item

Item 3's carve-out for a declared direction.

## Governing records

- ADR-002 — accepted.
- ADR-013 — accepted.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The index is
31,294 bytes against the 32,768-byte decision-record cap, so `Assemble`
includes ZERO records in full: the persona sees the standard plan-review
prompt, the remit and the full index (titles, statuses, supersession, applies_to),
and no record's Decision text. The selection is the twin's: ADR-002 and ADR-013 matched and dropped for the cap.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. Clean rule: a trial is `false_alarm` when any concern carries category `decision_record`, else `no_false_alarm`. `persona_case.json` carries
the hand-written examples the loader classifies to prove the rule discriminates.
