# Case: seed-synthetic-contradicts-buried-clause

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.23 / #4361), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an explicit
criterion whose LAST clause contradicts the issue it cites — so the
plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/projects-service` issue, the run ids and the triage sequence are
fabricated placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue adds `POST /projects/{id}/archive`: an admin's archive returns 200
and sets `archived_at`, an archived project drops out of `GET /projects`, but
it stays readable by its members — `GET /projects/{id}` keeps returning 200
for a member. The plan's explicit criterion `ac-archive-by-admin` is one long
statement whose first clauses match the issue (200 with the project,
`archived_at` set, hidden from the list for every caller) and whose last
clause requires a member's `GET /projects/{id}` to return 404. Approach step 2
adds `archived_at IS NULL` to the predicate the list and detail reads share,
so the plan is internally consistent. The implementation followed the issue,
acceptance failed the criterion on its last clause, and triage classified it
class-3.

Mid-difficulty by design: the contradiction is one clause among several
correct ones, so a reviewer who checks the criterion's opening against the
issue sees it match and moves on.

## The planted defect

- Criterion id: `ac-archive-by-admin` (present, byte-identical, in both
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
