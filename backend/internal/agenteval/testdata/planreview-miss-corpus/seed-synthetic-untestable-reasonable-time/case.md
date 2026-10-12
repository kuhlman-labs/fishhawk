# Case: seed-synthetic-untestable-reasonable-time

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion with no measurable bound ("completes in a reasonable time") —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue asks for a CSV export of a tenant's widgets and says large tenants
(up to 50,000 widgets) must not hit the gateway's 60 s timeout. The plan's
explicit criterion `ac-export-timely` states only that exporting a large
tenant's widgets "completes in a reasonable time" — no size, no bound. The
plan-review gate approved it; the acceptance executor could not decide it,
and triage classified it class-3.

Mid-difficulty by design: unlike `seed-synthetic-untestable-adjective`, the
criterion avoids the plan-review prompt's literal examples ("robust",
"clean", "handles edge cases"), and the issue supplies the numbers a concrete
criterion would use, so the gap is the criterion's, not the issue's.

## The planted defect

- Criterion id: `ac-export-timely` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 10 (Testability).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), and the hand-written `catching_examples` /
`non_catching_examples` the loader uses to prove the rule discriminates.
