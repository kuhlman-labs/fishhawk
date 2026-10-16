# Case: adr046-required-field-in-frozen-major

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E55.26 / #4453), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
record IS real: the loader checks it against the real `docs/adr/index.json`
(status and `superseded_by`), so a premise that stops holding fails verify.

Persona: architect.

ADR sourcing: ADR-046 was chosen by reading its body, as #4453's plan step 8
requires. The plan asked to confirm that the body carries the rule the case
anchors on and to substitute another record if it does not. It does, so there
was no substitution: Decision 6 reads "New optional fields continue
additively within each major line (`v0.x`, `v1.x`) per the existing soak
convention", Decision 1 makes each major file "independently frozen", and the
recorded precedent is that a major bump is "a new `workflow-vN.schema.json`
... coexisting until a separate ADR sunsets an old major". The approval's
condition 2 tripwires
(`TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona` and
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes`) passed on
this case as drafted.

## What it represents

The issue asks that a workflow-v1 deploy stage can name its target
environment, carry it on the deployment artifact, and refuse an unregistered
environment. It does not ask that a stage without an environment be refused.
The plan's approach step 1 adds `environment` "to the deploy stage's
properties and to its required list, editing the v1 file in place so a v1
spec whose deploy stage omits it is refused at parse". The other steps are
ordinary, and the two criteria match the issue.

## The planted defect

- Kind: `accepted_conflict`, expect `catch`, anchored at approach step 1.
- The defect: a REQUIRED property added inside an existing major. ADR-046
  keeps v0 and v1 validating side by side "indefinitely" and adds only
  optional fields within a major line, so every existing v1 spec whose deploy
  stage has no `environment` stops validating. The conforming shapes are an
  optional field (the repository's `x-intended-required` soak) or a new
  major.
- Why it is a BODY-level conflict: the index title is "Workflow schema
  major-version coexistence model — how workflow-v1 (deploy stage) coexists
  with and routes against workflow-v0". It says nothing about what may change
  within a major; that rule is in Decision 6.
- The case never names ADR-046 (asserted by
  `TestPersonaCatchCorpus_GoverningPremisesHold`), and no catch probe occurs
  in the issue or plan (loader mode m). The plan avoids the words
  "additive", "breaking" and "major".

## Remit item

Item 1, conflict with an accepted decision (weakening a stated invariant).

## Governing records

- ADR-046 — accepted, not superseded, `applies_to` the three
  `workflow-v1.schema.json` copies and `backend/internal/spec/**`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. Observed at
authoring (2026-10-15): matched ADR-046 and ADR-013 (`docs/spec/**`,
`cli/**`); both included in full, none dropped; the index rendered 32,432
bytes, untruncated, under its 32,768-byte cap. The persona can quote
Decision 6.

The clean twin in spirit is `clean-workflow-v2-optional-field`, whose plan
adds an OPTIONAL field within a major and leaves the required list unchanged.

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate,
after the production persona-ingest controls. Probes name this defect or its
fix: `adr-046`, `non-additive`, `not additive`, the Decision 6 phrasings
`additively within`, `within a major line` and `within each major line`,
`x-intended-required`, `is a breaking change`, `breaks existing`,
`breaks every existing`, the `stop(s) validating` / `no longer validate`
consequences, and `needs a new major`, `requires a new major` and
`bump the major`. There is no bare `breaking change` or `new major` probe,
because "not a breaking change" or "no new major needed" would match.
Attribution by `step 1`, `required list` or `v1 file in place`.
`persona_case.json` carries the hand-written examples the loader classifies
to prove the rule discriminates. Difficulty is not decidable offline: it
waits on the operator's Step 1d calibration
(`docs/compliance/planreview-catchrate-evidence.md`).
