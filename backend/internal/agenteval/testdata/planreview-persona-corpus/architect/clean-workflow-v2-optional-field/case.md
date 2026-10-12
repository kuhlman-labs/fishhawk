# Case: clean-workflow-v2-optional-field

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue asks for an optional `display_name` on workflow-v2 stages. The plan
adds it as an optional schema property (additive within the current major per
AGENTS.md's schema checklist), mirrors the schema with `scripts/sync-schemas`,
validates it in `backend/internal/spec/parse.go` and documents it.

## The planted defect

- Kind: `clean_area`, expect `clean`, `min_matched_records: 3`.
- No planted defect: an additive optional field inside the existing workflow-v2
  grammar.

## Remit item

"A new field, route, option or test inside an existing mechanism is not direction."

## Governing records

- ADR-013 — accepted; selected by `docs/spec/**` and `cli/**`.
- ADR-046 — accepted; selected by `backend/internal/spec/**`.
- ADR-067 — accepted; selected by `docs/spec/workflow-v2.schema.json`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The index is
31,294 bytes against the 32,768-byte decision-record cap, so `Assemble`
includes ZERO records in full: the persona sees the standard plan-review
prompt, the remit and the full index (titles, statuses, supersession, applies_to),
and no record's Decision text. All three records are matched and dropped for the cap.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. Clean rule: a trial is `false_alarm` when any concern carries category `decision_record`, else `no_false_alarm`. `persona_case.json` carries
the hand-written examples the loader classifies to prove the rule discriminates.
