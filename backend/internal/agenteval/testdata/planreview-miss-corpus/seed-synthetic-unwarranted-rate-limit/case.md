# Case: seed-synthetic-unwarranted-rate-limit

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.4 / #2245), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an inferred rate-limit criterion the issue never asked for —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue asks for an archive endpoint and says nothing about throttling. The plan added an INFERRED criterion (`ac-archive-rate-limit`) requiring HTTP 429 after 10 requests per minute, justified only by a generic "write endpoints should be rate limited" rationale. The plan-review gate approved it; acceptance failed it because no rate limit was ever implemented (and none was requested).

## The planted defect

- Criterion id: `ac-archive-rate-limit` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 9 (Warrant of inferred criteria).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), and the
hand-written `catching_examples` / `non_catching_examples` the loader uses to
prove the probes discriminate.
