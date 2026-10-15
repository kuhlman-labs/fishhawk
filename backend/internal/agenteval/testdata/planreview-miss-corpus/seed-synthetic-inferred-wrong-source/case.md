# Case: seed-synthetic-inferred-wrong-source

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an inferred
criterion whose value is cited from the wrong clause of the issue and whose
count is invented — so the plan-review catch-rate gate (`planreviewcatch.go`)
has a committed, discriminating fixture. Nothing here is a real run, issue or
repository: the `example-org/accounts-service` issue, the run ids and the
triage sequence are fabricated placeholders, and `miss.json` carries
`"synthetic": true`.

## What it represents

The issue adds password-reset emails. It gives one duration — the reset link
expires after 30 minutes — and separately asks that repeated reset requests
for the same email be throttled, with no window and no count. The plan's
INFERRED criterion `ac-reset-throttle-fourth` requires a fourth request within
30 minutes to send no reset link, and its rationale calls that "the issue's
30-minute window". The 30 minutes is the link's lifetime, not a throttle
window, and the count of three is invented. The implementation throttled on
the product owner's later answer, acceptance failed the criterion, and triage
classified it class-3.

Mid-difficulty by design: the rationale points at real issue text and the
issue really does ask for a throttle, so the criterion looks grounded; only
reading which clause the 30 minutes belongs to shows the warrant is missing.

## The planted defect

- Criterion id: `ac-reset-throttle-fourth` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 9 (Warrant of inferred criteria).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), the `defect_phrases` the prompt-leak self-check reads,
and the hand-written `catching_examples` / `non_catching_examples` the loader
uses to prove the rule discriminates.
