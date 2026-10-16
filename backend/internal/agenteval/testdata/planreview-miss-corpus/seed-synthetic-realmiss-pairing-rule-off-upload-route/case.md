# Case: seed-synthetic-realmiss-pairing-rule-off-upload-route

**Provenance: DERIVED FROM A REAL PLAN-REVIEW MISS.** The source is
Fishhawk run `0697f4cb` (issue #4085), criterion
`partial-without-remaining-scope-refused`. Acceptance triage classified that
failure class 3 (a bad criterion), after both plan reviewers had approved the
plan. The text here is a hand-written, ANONYMIZED reconstruction for E55.26 /
#4453, not a captured triage entry. The `example-org/report-scheduler` issue,
the run and artifact ids and triage sequence 121 are placeholders, so
`miss.json` carries `"synthetic": true`. The real run's vocabulary is kept out
of `review_input.json` for the same reason as in
`seed-synthetic-realmiss-guard-off-upload-route`.

## What the real run did

Two new optional plan fields were paired: a mode value and a text field that
mode requires. The JSON Schema enforced the text field's PRESENCE with a root
if/then. The parse-time semantic pass rejected a blank or whitespace-only text
and a text without the mode. The upload handler validates schema-only. The
criterion claimed that all four bad shapes are refused at upload. Only the
absent shape was: the acceptance agent observed one 400 and three 201s.

## What it represents

The issue adds `delivery` (`now`, `deferred`) and `deferral_note` to a report
schedule. A deferred schedule needs a non-blank note, and a note on a schedule
that is not deferred must be refused. Step 1 adds a root if/then to the schema
that requires the note's presence. Step 2 adds the blank, whitespace-only and
unpaired rules to `schedule.Validate`. A risks entry states that
`POST /schedules` checks the body against the schema only, and that
`schedule.Validate` runs when the dispatcher loads a schedule.
`internal/api/schedules.go` is not in scope. The inferred criterion
`ac-post-deferral-note-400` claims that all four bodies get a 400 at POST, and
only the absent-note body does. The schema, unit and dispatcher criteria are
correct and give the issue's guarantees their own coverage.

## Why a second case of the same class

The corpus carries two or more shapes per weak class. This is the subtler,
PARTLY satisfiable shape of `seed-synthetic-realmiss-guard-off-upload-route`.
One of the four cases does reach the route through the schema, so a reviewer
who sees that the schema enforces the note does not see a wholly unwired
surface. The defect is that the schema covers presence only. Real reviewers
missed this class twice in separate runs, and this is the second instance.

## The planted defect

- Criterion id: `ac-post-deferral-note-400` (byte-identical in `miss.json`
  and the plan).
- Plan-review criterion it violates: testability against the plan. The
  criterion cannot pass as planned for three of its four bodies.
- Probe routes: the POST handler never runs `schedule.Validate`, the schema
  enforces only presence, only the absent note is refused, three of the four
  return 201, and the criterion is only partly satisfiable. Bare counts ("other
  three", "three of four") are NOT probes, because a concern about fixtures or
  test layout could use them.

## Calibration

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 30/30 with
conventions and 30/30 without: no misses. Kept as a documented CEILING by
Captain ruling 2026-10-16 (honest rate about 30/30); the combined batch
re-recording rescores it. Step 1d "Results" in
`docs/compliance/planreview-catchrate-evidence.md` is the record.
