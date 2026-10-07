# Case: seed-synthetic-untestable-adjective

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.4 / #2245), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion phrased as an untestable adjective —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The plan carried an explicit criterion (`ac-retry-robust`) stating that retry handling is "robust and handles edge cases gracefully" — a vague adjective with no observable pass/fail condition. The plan-review gate approved it; the acceptance executor could not decide it, and triage classified it class-3.

## The planted defect

- Criterion id: `ac-retry-robust` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 10 (Testability).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), and the
hand-written `catching_examples` / `non_catching_examples` the loader uses to
prove the probes discriminate.
