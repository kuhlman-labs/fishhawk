# Case: seed-synthetic-contradicts-absent-vs-null

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It is a SYNTHETIC
reconstruction of a defect class this repository hit, not a captured run: it
plants exactly one defect the standard plan-review criteria should catch — an
explicit criterion that treats a field left out of a merge-patch body as an
explicit null — so the plan-review catch-rate gate (`planreviewcatch.go`) has
a committed, discriminating fixture. Nothing here is a real run, issue or
repository: the `example-org/profiles-service` issue, the run ids and the
triage sequence are fabricated placeholders, and `miss.json` carries
`"synthetic": true`.

## The class it reconstructs

Absent, JSON `null` and empty are three states, and code that decodes into a
pointer collapses the first two. This repository hit that class in #2787: the
runner's `undecidable_reason` was decoded as a `*string`, which admitted an
explicit `null` as if the field were absent
(`runner/cmd/fishhawk-runner/README.md`, the `undecidable_reason` paragraph).

## What it represents

The issue defines `PATCH /profiles/{id}` with merge-patch semantics: a value
replaces, `null` clears, and a field left out of the body is left unchanged.
The plan's explicit criterion `ac-patch-no-bio-key` says a body with no `bio`
key leaves the profile with no bio — the absent case treated as a clear.
Approach step 1 decodes into `*string` fields and step 3 writes `NULL` for
every nil pointer, so the plan is internally consistent. The implementation
followed the issue, acceptance failed the criterion, and triage classified it
class-3.

Mid-difficulty by design: the planted criterion reads as ordinary clear
semantics, and its sibling criteria (a value replaces, a null clears) are
correct. Catching it means checking presence against null, field by field,
against the issue.

## The planted defect

- Criterion id: `ac-patch-no-bio-key` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 8a (Issue fidelity — an explicit
  criterion requires what the issue requires).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), the
`attribution_phrases` (substrings of the planted criterion's statement and of
no other criterion's), the `defect_phrases` the prompt-leak self-check reads,
and the hand-written `catching_examples` / `non_catching_examples` the loader
uses to prove the rule discriminates.
