# Case: seed-synthetic-realmiss-guard-off-upload-route

**Provenance: DERIVED FROM A REAL PLAN-REVIEW MISS.** The source is
Fishhawk run `b00d3249` (issue #3325), criterion
`surface-none-rejects-drivable-criterion`. Acceptance triage classified that
failure class 3 (a bad criterion), after both plan reviewers had approved the
plan. The text here is a hand-written, ANONYMIZED reconstruction for E55.26 /
#4453, not a captured triage entry. The `example-org/flags-service` issue,
the run and artifact ids and triage sequence 120 are placeholders, so
`miss.json` carries `"synthetic": true`. The real run's vocabulary is kept out
of `review_input.json` on purpose. It overlaps the plan-review prompt's own
wording, so it would trip the prompt-leak and mode (m) checks and hand the
reviewer the answer.

## What the real run did

The issue asked that a plan declaring a certain field value be refused when
it also carried a criterion that contradicts it. The plan put the new check in
the plan package's parse-time semantic pass, beside an existing semantic
check. The criterion said the HTTP upload route refuses such a plan, and its
verify hint named that route. The upload handler validates the body against
the JSON Schema only and stores it. The semantic pass runs later, when the
artifact is read. No approach step wired the pass into the upload handler, so
the acceptance agent's upload returned 201 and the criterion failed.

## What it represents

The issue adds `rollout` (`none`, `percentage`, `all`) to a flag definition
and requires that a definition declaring `rollout: none` while carrying
targeting rules be rejected, naming the offending rule id. Approach step 2
adds that rule to `flagdef.Check`, the definition package's semantic pass. A
risks entry, away from the criteria, states that the `PUT /flags/{key}`
handler checks the body against the JSON Schema only and stores it, and that
`flagdef.Check` runs when the evaluator loads a stored definition.
`internal/api/flags.go` is not in scope. The inferred criterion
`ac-put-none-with-rules-400` claims the PUT returns 400 naming the rule id.
Its rationale is plausible, and the HTTP route is never reached by the
change. The unit-level criterion `ac-check-rejects-rules-under-none` is
correct and is NOT the defect. The issue's central guarantee (a rollout-none
flag serves its default variant) has its own correct criterion,
`ac-none-serves-default`.

## The planted defect

- Criterion id: `ac-put-none-with-rules-400` (byte-identical in `miss.json`
  and the plan inside `review_input.json`; the loader enforces both).
- Plan-review criterion it violates: testability against the plan. The
  criterion cannot pass as planned, because the surface it observes never
  runs the check the plan adds. The defect is visible only by connecting the
  criterion to the risks entry and to the scope, which lacks the handler. The
  criterion's own text does not state it.
- Probe routes, all polarity-bearing and defect-specific: the check is not
  wired into the PUT handler, the handler only validates the schema, the
  rejection happens only at evaluation, the request still returns 201, and
  the criterion cannot pass as planned. Consequence-only phrases ("would still
  be accepted", "can never pass") are deliberately NOT probes, because a
  concern about an unrelated aspect of the criterion could use them.

## Calibration

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 30/30 with
conventions and 30/30 without: no misses. Kept as a documented CEILING by
Captain ruling 2026-10-16 (honest rate about 30/30); the combined batch
re-recording rescores it. Step 1d "Results" in
`docs/compliance/planreview-catchrate-evidence.md` is the record.
