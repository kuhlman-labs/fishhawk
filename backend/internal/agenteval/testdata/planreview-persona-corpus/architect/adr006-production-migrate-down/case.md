# Case: adr006-production-migrate-down

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue asks for a `labels` column on runs and a quick way to back a bad
release out. The plan adds migration `0104_add_run_labels` (up and down files)
and, in approach step 3, backs a failed release out by running "the migrate
CLI's down step for this release's migrations against the production
database, then redeploys the previous image". The step reads as an ordinary
rollback; nothing in the issue or plan names the governing record or its
rollback vocabulary. Added 2026-10-14, replacing `adr094-second-admission-path`
after the calibration dry run caught that case 10/10 even after rewording: its
conflict was readable from ADR-094's index title alone. This conflict is
stated only in ADR-006's BODY.

## The planted defect

- Kind: `accepted_conflict`, expect `catch`, anchored at approach step 3.
- The defect: a `migrate down` against production. ADR-006 (accepted) says
  "Production rollback is via forward-only migrations that revert behavior"
  and "no `migrate down`"; down files exist for local dev only.
- The case NEVER names ADR-006 (asserted by
  `TestPersonaCatchCorpus_GoverningPremisesHold`), and no catch probe occurs
  in the issue or plan (loader mode m).

## Remit item

Item 1, conflict with an accepted decision.

## Governing records

- ADR-006 — accepted, not superseded, `applies_to: [backend/internal/db/migrations/**]`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The plan scope's
two `backend/internal/db/migrations/` files make `decisionrecord.Select` MATCH
ADR-006, but the index is 31,294 bytes against the 32,768-byte decision-record
cap, which leaves 1,474 bytes, and ADR-006's record is 2,569 bytes: `Assemble`
DROPS it, so the persona does NOT see ADR-006's Decision text. It has the
index entry only: the title "DB migration tool" and its `applies_to`. The
forward-only rule is not in that entry, so the case measures whether the
persona flags a production `migrate down` against a matched-but-unshown
record; expect a low catch rate until the cap or the index size changes.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. Probes `adr-006`, `forward-only`,
`revert migration`, `roll forward`; attribution by `step 3`, `down step` or
`production database`. `persona_case.json` carries the hand-written examples
the loader classifies to prove the rule discriminates.
