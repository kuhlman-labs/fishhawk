# Case: seed-synthetic-untestable-user-friendly

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.18 / #4253), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — a criterion phrased as a subjective quality ("clear and user-friendly") —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue asks `POST /widgets` to reject an invalid name with HTTP 422 and
error code `invalid_name`, and says the message should tell the user what to
fix. The plan's explicit criteria pin the status and error code for each
invalid-name shape concretely, but its criterion `ac-name-message-friendly`
states only that validation error messages "are clear and user-friendly" — no
content an executor could assert. The plan-review gate approved it; the
acceptance executor could not decide it, and triage classified it class-3.

Mid-difficulty by design: the criterion avoids the plan-review prompt's
literal examples ("robust", "clean", "handles edge cases") and sits among
concrete sibling criteria, so the plan reads as well specified at a glance.

## The planted defect

- Criterion id: `ac-name-message-friendly` (present, byte-identical, in both
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
