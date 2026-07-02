# Case: seed-synthetic-inferred-criterion

**Provenance: SYNTHETIC.** This case is a hand-authored reconstruction of a
class-3 acceptance-triage shape (an inferred-source criterion that failed
validation — a bad criterion the plan gate approved), NOT a captured
production triage entry. It exists so `LoadPlanReviewMissCorpus` has a
committed fixture and the miss.json shape is demonstrated without
fabricating a production run (the same seed discipline as the Tier-A/Tier-B
synthetic corpus seeds).

## What it represents

A plan whose `verification.acceptance_criteria` carried an inferred
pagination criterion (`ac-list-pagination`, source `inferred`) that the
plan-review gate approved. Acceptance validation failed the criterion —
the running instance never paginated — and E31.8 triage classified the
failure class-3 (bad/ambiguous criterion), paging the human instead of
routing a fix-up.

## Distilled signal

The plan-review gate approved an inferred criterion whose statement did not
hold against the delivered behavior. This is the ADR-049 decision #4
feedback signal the plan-review-miss corpus accumulates: cases the plan
reviewer should have challenged (missing source grounding, untestable or
wrong inferred expectations) before the plan was approved.
