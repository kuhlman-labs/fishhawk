# Case: seed-synthetic-restates-approach

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.4 / #2245), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion that restates an approach step —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The plan's criterion `ac-step-two-done` states that "the code in approach step 2 was written" — a restatement of the approach, not an observable outcome. It passes on the struct field alone while the issue's real outcome (the API response carries `archived_at`) went unverified. The plan-review gate approved it.

## The planted defect

- Criterion id: `ac-step-two-done` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 11 (Independence).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), and the
hand-written `catching_examples` / `non_catching_examples` the loader uses to
prove the probes discriminate.
