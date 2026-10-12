# Case: adr094-second-admission-path

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue asks for `fishhawk_rerun_acceptance`, a new MCP verb that spawns a
runner for just the acceptance stage. The plan's approach step 3 spawns that
runner "behind a per-verb semaphore of capacity 2 held in the verb handler, so
a re-run starts at once instead of waiting behind queued implement stages".

## The planted defect

- Kind: `accepted_conflict`, expect `catch`, anchored at approach step 3.
- The defect: a second admission path for a spawning verb. ADR-094 (accepted)
  decides "one admission path for every spawning verb"; the per-verb semaphore
  lets this verb's runner start outside it.
- The case NEVER names ADR-094 (asserted by
  `TestPersonaCatchCorpus_GoverningPremisesHold`): the persona has to connect
  the step to the index entry itself.

## Remit item

Item 1, conflict with an accepted decision: "adding a second path beside a mechanism the ADR makes the single path, such as a queue, an admission path".

## Governing records

- ADR-094 — accepted, not superseded, empty `applies_to`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The index is
31,294 bytes against the 32,768-byte decision-record cap, so `Assemble`
includes ZERO records in full: the persona sees the standard plan-review
prompt, the remit and the full index (titles, statuses, supersession, applies_to),
and no record's Decision text. ADR-094 has an empty `applies_to`, so no scope path selects it: the only text the persona has is its index title, "Resource-keyed admission: one admission path for every spawning verb, ...". The case is catchable from that title alone.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. `admission`, `ADR-094`, `resource-keyed`, `resource group`, `spawning verb`; attribution by `step 3`, `per-verb semaphore` or `acceptance-only runner`. `persona_case.json` carries
the hand-written examples the loader classifies to prove the rule discriminates.
