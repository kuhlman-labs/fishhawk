# Case: undeclared-cross-component-direction

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue asks for run pause and resume that survive a fishhawkd restart. The
plan's approach step 2 adds a `run_pause_state` table with four states, written
by fishhawkd and polled by fishhawk-runner, which drives the drain transition:
a new persisted state machine shared by two components. The plan carries no
`new_architectural_decision`.

## The planted defect

- Kind: `undeclared_direction`, expect `catch`, anchored at approach step 2.
- The defect: new architectural direction (a new persisted cross-component
  state machine) while `new_architectural_decision` is absent.
- Its clean twin is `declared-cross-component-direction`: the same plan plus
  the declaration (the loader's `pair_of` check pins that the two plans differ
  in nothing else).

## Remit item

Item 3, undeclared direction: "a new persisted state machine" is named as direction.

## Governing records

- ADR-002 — accepted (Postgres access layer); selected by `backend/internal/run/queries.sql` and `run/db/**`.
- ADR-013 — accepted; selected by `runner/**`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The index is
31,294 bytes against the 32,768-byte decision-record cap, so `Assemble`
includes ZERO records in full: the persona sees the standard plan-review
prompt, the remit and the full index (titles, statuses, supersession, applies_to),
and no record's Decision text. ADR-002 and ADR-013 are selected by the scope but both are DROPPED for the cap, so the framing lists them as not shown.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. `undeclared`, `not declared`, `new_architectural_decision`, `new architectural decision`, `architectural direction`, `new direction`; attribution by `step 2` or `run_pause_state`. `persona_case.json` carries
the hand-written examples the loader classifies to prove the rule discriminates.
