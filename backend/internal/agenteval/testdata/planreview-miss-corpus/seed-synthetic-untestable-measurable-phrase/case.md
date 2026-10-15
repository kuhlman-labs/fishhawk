# Case: seed-synthetic-untestable-measurable-phrase

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion that
names a percentile and a budget but defines neither the budget, the load nor
the window — so the plan-review catch-rate gate (`planreviewcatch.go`) has a
committed, discriminating fixture. Nothing here is a real run, issue or
repository: the `example-org/search-service` issue, the run ids and the
triage sequence are fabricated placeholders, and `miss.json` carries
`"synthetic": true`.

## What it represents

The issue adds `GET /notes/search` and says search must respond fast enough
for search-as-you-type. The plan's explicit criterion `ac-search-p95-in-budget`
requires search to keep "p95 latency within the service latency budget under
production load". Neither the issue nor the plan gives a budget value, a load
profile or a measurement window, so no executor can decide pass/fail. The
plan-review gate approved it; the acceptance executor could not decide it,
and triage classified it class-3.

Mid-difficulty by design: unlike `seed-synthetic-untestable-reasonable-time`,
the criterion sounds measurable — a percentile, a named budget, a load — and
avoids the plan-review prompt's literal examples ("robust", "clean", "handles
edge cases"). The gap shows only when a reviewer asks what number the budget
is.

## The planted defect

- Criterion id: `ac-search-p95-in-budget` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 10 (Testability).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), the `defect_phrases` the prompt-leak self-check reads,
and the hand-written `catching_examples` / `non_catching_examples` the loader
uses to prove the rule discriminates.
