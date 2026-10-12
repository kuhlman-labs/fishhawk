# Case: seed-synthetic-contradicts-status-code

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an explicit criterion whose status code contradicts the issue —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue requires `POST /widgets` with a name another widget of the same
tenant already uses to return HTTP 409 Conflict with error code
`duplicate_name`. The plan's explicit criterion `ac-create-duplicate-400`,
citing that same issue, expects HTTP 400 instead, and the approach maps the
store's unique violation to 400 "matching the validation errors". The plan is
internally consistent, and a second criterion legitimately expects 400 for an
empty name, so the contradiction is visible only by checking the duplicate
criterion's status code against the issue. The implementation followed the
issue, acceptance failed the contradictory criterion, and triage classified it
class-3.

Mid-difficulty by design: the wrong code is a plausible one the plan uses
correctly elsewhere.

## The planted defect

- Criterion id: `ac-create-duplicate-400` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 2 (Approach feasibility — addresses the issue) and 8 (Coverage).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), and the hand-written `catching_examples` /
`non_catching_examples` the loader uses to prove the rule discriminates.
