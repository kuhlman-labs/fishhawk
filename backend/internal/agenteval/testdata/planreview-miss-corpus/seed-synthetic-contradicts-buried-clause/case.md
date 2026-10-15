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
clause says `GET /projects/{id}` returns 200 with the project "only to a
project admin", which shuts out every member who is not an admin. Approach
step 2 adds `archived_at IS NULL` to the list read and applies the admin role
check to an archived project's detail read, so the plan is internally
consistent. The implementation followed the issue,
acceptance failed the criterion on its last clause, and triage classified it
class-3.

Mid-difficulty by design: the contradiction is one narrow edge of one
clause among several correct ones. The clause even promises the 200 the issue
asks for, and only the trailing "only to a project admin" narrows who gets
it, so a reviewer who checks the criterion's status codes against the issue
sees them match and moves on.

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

## Calibration rework (Step 1c dry run, fix-up concern 4d935789)

Miss class: none. The case scored 30/30, ABOVE the band (too easy): the last
clause's 404 was a blatant inversion of the issue's 200. The defect was made
subtler by narrowing it to an edge that only some callers hit. The clause now
grants the detail read with a 200 "only to a project admin", so only members
who are not admins are wrong-footed. `miss.json` carries the new statement and
observation. The attribution phrases follow the statement: `a member's get
/projects/{id}` is gone and `only to a project admin` is added (mode p: it
occurs in no sibling statement). The defect phrases gained `only to a project
admin`. Probes for the narrowed shape were added, such as `not only to a project
admin`, `members who are not admins`, `excludes members`, `loses read access`
and `narrows the detail read`. The catching examples were rewritten for the
new shape, and one is attributed through `only to a project admin` alone. A non_catching_example that names the planted criterion and uses
admin vocabulary about the archive endpoint proves the added probes
discriminate. No added probe occurs in the issue or plan (mode m) or in a
non_catching_example (mode j). The C3 widening fixture
(`TestCatchCorpusWidening_RepairsScoringMissClasses`) still holds:
`sets archived_at on the project` remains in the planted statement only.
