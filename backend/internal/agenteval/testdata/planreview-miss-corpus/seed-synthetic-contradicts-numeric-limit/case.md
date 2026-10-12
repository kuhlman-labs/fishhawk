# Case: seed-synthetic-contradicts-numeric-limit

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an explicit criterion whose number contradicts the issue —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue caps attachment uploads at 10 MB and requires HTTP 413 for anything
larger. The plan's explicit criterion `ac-upload-large-accepted`, citing that
same issue, expects a 20 MB upload to return HTTP 201. The rest of the plan is
consistent with the issue (its other oversize criterion uses 30 MB, which the
issue also rejects), so the contradiction is visible only by comparing the
criterion's number with the issue's cap. The implementation followed the
issue, acceptance failed the contradictory criterion, and triage classified it
class-3.

Mid-difficulty by design: unlike `seed-synthetic-contradicts-issue`, the
criterion does not invert the issue's wording; it carries a plausible number
on the wrong side of the issue's limit.

## The planted defect

- Criterion id: `ac-upload-large-accepted` (present, byte-identical, in both
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
