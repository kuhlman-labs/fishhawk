# Case: seed-synthetic-inferred-sibling-retention

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an inferred criterion whose retention period is borrowed from a sibling table —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue asks to record a `widget_views` row on every widget read and to
expose the recorded view count. It says nothing about retention. The plan
added an INFERRED criterion (`ac-views-purged-after-90-days`) requiring the
nightly purge job to delete view rows older than 90 days, with a rationale
that cites the `audit_events` table's 90-day purge in the same repository. The
plan-review gate approved it; the implementation never touched the purge job,
acceptance failed the criterion, and triage classified it class-3.

Mid-difficulty by design: the rationale names real sibling code rather than a
generic best practice, and the purge would also silently shrink the view count
the issue asks for — a reviewer must notice that the issue grants no
retention policy at all.

## The planted defect

- Criterion id: `ac-views-purged-after-90-days` (present, byte-identical, in
  both `miss.json` and the plan inside `review_input.json` — the loader
  enforces both).
- Plan-review criterion it violates: 9 (Warrant of inferred criteria).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), and the hand-written `catching_examples` /
`non_catching_examples` the loader uses to prove the rule discriminates.
