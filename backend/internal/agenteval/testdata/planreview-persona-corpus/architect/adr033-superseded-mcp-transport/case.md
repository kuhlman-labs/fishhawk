# Case: adr033-superseded-mcp-transport

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E78.11 / #4313), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status and `superseded_by`), so a premise that stops
holding fails verify.

## What it represents

The issue asks for `fishhawk_export_audit`, a new MCP tool. The plan's approach
step 2 registers it "in backend/cmd/fishhawk-mcp's stdio tool table beside the
other export verbs (transport per ADR-033)". The reliance is incidental: the
step states no rationale and no prohibition, so a reviewer has to check
ADR-033's status in the index to see it (reworked 2026-10-13 after the
calibration dry run caught the original wording 10/10: it named ADR-033 as the
reason and forbade every other transport).

## The planted defect

- Kind: `superseded_reliance`, expect `catch`, anchored at approach step 2.
- The defect: reliance on ADR-033, which the index lists as superseded by
  ADR-076 ("MCP over HTTP served by fishhawkd ... supersedes ADR-033's loopback
  gate"). The tool is registered in the stdio binary's tool table only, so it
  is reachable over stdio alone: the plan follows the superseded record where
  the successor decides differently.

## Remit item

Item 2, reliance on a superseded decision: name both records, and say so when the successor's text is not available.

## Governing records

- ADR-033 — accepted, `superseded_by: [ADR-076]`, empty `applies_to`.
- ADR-076 — accepted, `supersedes: [ADR-033]`, empty `applies_to`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. The index is
31,294 bytes against the 32,768-byte decision-record cap, so `Assemble`
includes ZERO records in full: the persona sees the standard plan-review
prompt, the remit and the full index (titles, statuses, supersession, applies_to),
and no record's Decision text. Neither record has an `applies_to`, so neither is selected; the persona has the two index entries and their supersession metadata, not ADR-076's Decision text. The remit tells it to say so in the note.
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes` logs the
selection per case and asserts each governing record's index entry survives
in the rendered index (approval condition C1).

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate, after
the production persona-ingest controls. `supersed`, `ADR-076`, `successor`, `no longer governs`; attribution by `step 2`, `stdio tool table` or `ADR-033`. `persona_case.json` carries
the hand-written examples the loader classifies to prove the rule discriminates.
