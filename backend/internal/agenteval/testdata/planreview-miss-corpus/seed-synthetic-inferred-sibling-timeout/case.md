# Case: seed-synthetic-inferred-sibling-timeout

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an inferred criterion whose timeout is borrowed from a sibling client —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue asks `POST /quotes` to price an order through pricing-service and to
return HTTP 502 when pricing-service answers with a non-2xx. It states no
timeout. The plan added an INFERRED criterion (`ac-quote-pricing-timeout`)
fixing a 30 second timeout and an HTTP 504 response, with a rationale that
cites the inventory client's 30 s setting in the same repository. The
plan-review gate approved it; the implementation used the shared HTTP client,
a slow pricing-service surfaced as 502, acceptance failed the criterion, and
triage classified it class-3.

Mid-difficulty by design: unlike `seed-synthetic-unwarranted-rate-limit`, the
rationale is not generic. It names real sibling code, so it reads as
repository context, but a sibling client's tuning is not a requirement the
issue or this endpoint's contract carries.

## The planted defect

- Criterion id: `ac-quote-pricing-timeout` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 9 (Warrant of inferred criteria).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), and the hand-written `catching_examples` /
`non_catching_examples` the loader uses to prove the rule discriminates.
