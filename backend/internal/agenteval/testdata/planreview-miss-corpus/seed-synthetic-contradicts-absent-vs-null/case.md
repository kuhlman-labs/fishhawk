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
The plan's explicit criterion `ac-patch-no-bio-key` reads correctly: a body
with no `bio` key returns 200 and applies the fields the body carries. Its
`verify_hint` carries the contradiction: set bio to "hello", PATCH with only
`timezone`, and expect bio null — the absent case treated as a clear.
Approach step 1 decodes into `*string` fields and step 3 writes all four
columns in one UPDATE, so the plan is internally consistent. The implementation
followed the issue, acceptance failed the criterion, and triage classified it
class-3.

Mid-difficulty by design: the planted criterion's statement is correct, and
its sibling criteria (a value replaces, a null clears) are correct. Catching
it means reading the verify_hint and checking presence against null against
the issue.

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

## Calibration rework (Step 1c dry run, fix-up concern 4d935789)

Miss classes: `unattributed` and `no_probe`, both scoring misses on real
catches. Concerns naming `ac-patch-no-bio-key` wrote "does not match the
issue", "weaker than the issue", "cannot fail against the plan's defect", "not
falsifiable", "vacuous", "an existing bio keeps that exact value", "stored bio
to be unchanged" and "is left unchanged". For `no_probe` the probes were
widened with those phrases, except "is left unchanged": the issue itself says
"is left unchanged", so mode m refuses it, and the narrower `bio is left
unchanged` stands in. Probes for the new verify_hint shape were added as well
(`expects the bio to be null`, `expects bio to be null`, `expects a null bio`,
`expects the bio cleared`, `expects bio cleared`, `hint contradicts`). For
`unattributed` the attribution phrases were widened with `with no bio` (mode p:
it occurs only in the planted statement); one catching example is attributed
through that phrase alone.

Those repairs alone would put the case ABOVE the band (too easy), so the
defect was made subtler: the statement no longer says the profile ends with
no bio. The contradiction moved into the criterion's `verify_hint`, which
the plan-review prompt renders inline, and approach step 3 no longer spells
out "a nil pointer writes NULL". A careful reviewer who reads the hint still
catches it; a skim of the statement does not. `miss.json` carries the new
statement and the hint-based expectation, the catching examples were
rewritten for the new shape, the defect phrases gained `bio is null`, and a
non_catching_example naming the planted criterion (a sibling null-clears test)
proves the added probes discriminate. No added probe occurs in the issue or
plan (mode m) or in a non_catching_example (mode j).
