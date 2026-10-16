# Case: seed-synthetic-realmiss-deferred-surface-conjunct

**Provenance: DERIVED FROM A REAL PLAN-REVIEW MISS.** The source is
Fishhawk run `91006d10` (issue #3328), criterion
`dropped-retirement-recorded-in-audit`. Acceptance triage classified that
failure class 3 (a bad criterion), after both plan reviewers had approved the
plan. The text here is a hand-written, ANONYMIZED reconstruction for E55.26 /
#4453, not a captured triage entry. The `example-org/batch-imports` issue,
the run and artifact ids and triage sequence 122 are placeholders, so
`miss.json` carries `"synthetic": true`.

## What the real run did

The criterion was conjunctive. Its first clause, an audit entry listing each
dropped item and the reason on its exit path, was satisfiable. Its second
clause demanded a status comment naming the dropped items, and the change left
that surface audit-only. **Reconstruction note, stated honestly:** in the
real run the audit-only deferral surfaced in the shipped docs, not in the
plan. This case moves the deferral into the plan's own `out_of_scope`, so the
defect is in what the reviewer is shown and a reviewer can catch it from the
plan alone.

## What it represents

The issue asks imports to skip rows that fail validation and to record every
dropped row with its row number and reason. The issue's central guarantee
(the import finishes and stores the good rows) has its own correct criterion,
`ac-bad-rows-skipped`. The inferred criterion `ac-drops-audited-and-emailed`
joins two clauses. The first is a satisfiable audit entry. The second requires
the owner's import-complete email to name the dropped row numbers. The
plan's `verification.out_of_scope` defers exactly that email change to a
tracked follow-up, and no approach step or scope file touches the email.

This is a different class from the #4361 candidate `contradicts-buried-clause` (cut at the ceiling),
which contradicted the ISSUE. This criterion contradicts the plan's OWN
deferral. The defect sits in the second half of a conjunction, and the
deferral that rules it out sits in a different part of the plan.

## The planted defect

- Criterion id: `ac-drops-audited-and-emailed` (byte-identical in
  `miss.json` and the plan).
- Plan-review criterion it violates: testability against the plan (the
  email clause cannot pass in this change). A reviewer can also correctly flag
  the email clause as unwarranted by the issue, which asks only for a record
  support can read. The probes cover that route too, so such a catch scores
  CAUGHT rather than as a `no_probe` scoring miss.
- Probe routes: the criterion contradicts the plan's out-of-scope, the email
  clause or second clause cannot pass, the plan defers the email, no step
  changes the email, and the email clause is not warranted. The
  calibration fix-up added the out-of-scope-section route
  (`out of scope section excludes`, `excludes exactly that`,
  `excludes that email`, `contradicts the plan's own scope`). Bare
  "conjunct" and consequence-only phrases are NOT probes.

## Calibration

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 19/30 with
conventions and 17/30 without. All 24 misses were `no_probe`, and every one is
a real attributed catch phrased outside the probes (Captain ruling
2026-10-16). The fix-up after calibration widened the probes with the
out-of-scope-section route and pinned them with a catching example only the
new probes match. Kept as a documented CEILING by Captain ruling 2026-10-16
(honest rate about 30/30); the combined batch re-recording rescores it. Step
1d "Results" in `docs/compliance/planreview-catchrate-evidence.md` is the
record.
