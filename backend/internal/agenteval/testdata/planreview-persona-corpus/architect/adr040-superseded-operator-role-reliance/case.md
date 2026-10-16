# Case: adr040-superseded-operator-role-reliance

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E55.26 / #4453), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
records ARE real: the loader checks each one against the real
`docs/adr/index.json` (status, `superseded_by` and `amended_by`), so a
premise that stops holding fails verify.

**Re-encoded for #4471 (E78.13).** The decision record now encodes ADR-101
as an AMENDMENT of ADR-040 (`amends` / `amended_by`), not a supersession, so
this case's defect is reliance on an AMENDED record on a point the amendment
decides differently. The loader has no amended-reliance kind, so the case
keeps kind `superseded_reliance`, whose premise check accepts an amendment
link (remit item 2's "Amended" bullet); the directory name keeps the old
word. The probes and catching examples were reworded to amendment phrasing.
The case needs re-calibration: the overseer re-calibrates it (30 trials,
adr040 only) on this head before merge.

Persona: architect.

ADR sourcing: ADR-040 and ADR-101 were chosen by reading both bodies, as
#4453's plan step 7 requires. They were NOT substituted: the approval's
condition 2 tripwires
(`TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona` and
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes`) passed on
this case as drafted, before the next case was written.

## What it represents

The issue asks for the operator loop to keep approving and retrying overnight.
The plan's approach step 2 adds a "scheduler harness" that, on each cron tick,
"instantiates the role from the shipped versioned playbook and drives
approvals and retries under the existing operator-agent/<role-spec-version>
token subject, since the role contract lets any harness, a scheduler
included, instantiate the role". The step never names a record and never says
"superseded" or "amended". It justifies itself by ADR-040's CONTENT:
ADR-040 Option B lets "Any harness (Claude, Codex, a scheduler)" instantiate the role, and its
Decision 4 runs operator-agent actions under `operator-agent/<role-spec-version>`.

## The planted defect

- Kind: `superseded_reliance`, expect `catch`, anchored at approach step 2.
- The defect: sustained unattended operation justified by ADR-040's
  harness-neutral model. The index marks ADR-040 `amended_by: [ADR-097,
  ADR-101]`, and ADR-101 departs from it on exactly this point. Its Context says what is
  "genuinely new is intake authority plus sustained, unattended operation";
  D4 says "Every charter action requires a LIVE, verifiable lease at decision
  time"; D5 runs charter actions under "a dedicated per-lease credential
  (`operator-charter/<repo>/<lease-id>`)" held by a "product-spawned"
  operator, and says "A session carrying a human token is not a charter
  session". A cron instance under the plain operator-agent subject has no
  lease, no per-lease credential and is not product-spawned.
- The case never names ADR-040 or ADR-101 and never says "superseded" or
  "amended". No catch probe occurs in the issue or plan (loader mode m).

## Remit item

Item 2's "Amended" bullet: the plan follows an amended record on a point
where an amendment decides differently; name both records, and say so when the
amendment's text is not available.

## Governing records

- ADR-040 — accepted, `superseded_by: []`, `amended_by: [ADR-097, ADR-101]`,
  `applies_to: [docs/spec/operator-role.schema.json, .fishhawk/operator.yaml]`.

ADR-101 (accepted, `amends: [ADR-040, ADR-089, ADR-097]`, empty `applies_to`)
is no longer a governing record: it is the amendment the catch names, but its
body does not reach the persona (below), and
`TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona` requires
every matched governing record's body. The superseded_reliance premise check
verifies a non-governing amendment against the real index instead (it must
exist, be accepted and list `amends` back to ADR-040). ADR-101 is the LAST
entry in the index today, so an index-tail truncation would cut it first; the
render test asserts ADR-040's `amended_by` ids survive in the rendered index.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection (ADR-084 has no `docs/adr` record yet and is read
from the tracker) from the real `docs/adr/index.json`. The plan scope's
`docs/spec/operator-role.schema.json` matches ADR-040's `applies_to`, and the
#4471 amendment following selects ADR-097 and ADR-101 from it. An amended
record ranks ahead of its amendments, and ADR-097 (amended by ADR-101) ahead
of ADR-101. Since #4399 the index is shaped under its own cap, so the record
budget goes to the bodies.

Re-observed after the #4471 re-encode (2026-10-16): matched ADR-040, ADR-097,
ADR-101, ADR-013; included ADR-040 and ADR-097 in full; dropped ADR-101
(23,042 bytes, past what ADR-040 and ADR-097 leave of the 32,768-byte record
budget) and ADR-013; the index rendered 32,744 bytes, untruncated.

**Fairness disclosure.** At the production cap ADR-101's body is NOT in the
persona prompt. The catch rests on ADR-040's body plus the index metadata:
ADR-040's `amended_by` names ADR-101, ADR-101's `amends` names ADR-040, the
framing marks ADR-101 dropped as an "amendment of ADR-040, ADR-097", and
ADR-101's index title reads "Charter authorization mode: lease-based, bounded
autonomous operation by a product-spawned operator". The probes are
catchable from that material alone: `adr-101`, `charter`, `lease-based`,
`product-spawned` (all in the index title) and three amendment spellings
naming ADR-101 as amending ADR-040. The body-only probes `per-lease` and
`lease credential` (ADR-101 D5) were removed.

The authoring observation below predates the re-encode and is kept as
history.

Observed at authoring (2026-10-15, before #4471): matched ADR-101, ADR-040,
ADR-013;
included ADR-101 and ADR-040 in full; dropped ADR-013; the index rendered
32,432 bytes, untruncated, under its 32,768-byte cap. The persona therefore
sees BOTH bodies, including ADR-101's D4/D5 text, and the remit's "successor
text not available" branch does not apply.

The scope is shaped for that. ADR-013 applies to `docs/spec/**`. The first
draft also scoped `docs/spec/operator-role.md`, which gave ADR-013 two matched
paths against ADR-040's one, so ADR-013 ranked first and ADR-040's body was
dropped (the condition 2 tripwire went red). The draft now documents the
block in `backend/internal/operatorrole/README.md`. At one matched path each,
`rankBefore` orders by the higher id: ADR-101, ADR-040, then ADR-013.
`TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona` reddens
if a later ADR landing changes that.

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate,
after the production persona-ingest controls. Probes are all specific to the
amendment and its departure: `adr-101`, `charter`, `lease-based`,
`product-spawned`, and three amendment spellings (`adr-040 is amended`,
`amended by adr-101`, `adr-101 amends`). There is no bare `lease` probe,
because `release` contains it. Attribution by `step 2`, `scheduler harness`,
`operator-agent/<role-spec-version>` or `any harness`. `persona_case.json`
carries the hand-written examples the loader classifies to prove the rule
discriminates. Difficulty is not decidable offline: it waits on the
operator's Step 1d calibration (`docs/compliance/planreview-catchrate-evidence.md`).

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 29/30 on the
architect arm (the one miss undecodable). Kept as a documented CEILING by
Captain ruling 2026-10-16 (honest rate about 30/30); the combined batch
re-recording rescores it. Step 1d "Results" in
`docs/compliance/planreview-catchrate-evidence.md` is the record.
