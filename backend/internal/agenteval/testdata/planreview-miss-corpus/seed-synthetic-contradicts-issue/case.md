# Case: seed-synthetic-contradicts-issue

**Provenance: SYNTHETIC.** This case is a hand-authored plan-review miss
(E55.4 / #2245), NOT a captured production triage entry. It plants exactly
one defect the standard plan-review criteria should catch — an explicit criterion that contradicts the issue —
so the plan-review catch-rate gate (`planreviewcatch.go`) has a committed,
discriminating fixture. Nothing here is a real run, issue or repository: the
`example-org/widgets-service` issue, the run ids and the triage sequence are fabricated
placeholders, and `miss.json` carries `"synthetic": true`.

## What it represents

The issue requires a SOFT delete (set `deleted_at`, keep the row). The plan's explicit criterion `ac-delete-hard`, citing that same issue, requires the row to be REMOVED. The implementation followed the issue, acceptance failed the contradictory criterion, and triage classified it class-3. The plan-review gate approved a criterion that contradicts the issue it cites.

## The planted defect

- Criterion id: `ac-delete-hard` (present, byte-identical, in both
  `miss.json` and the plan inside `review_input.json` — the loader enforces
  both).
- Plan-review criterion it violates: 2 (Approach feasibility — addresses the issue) and 8 (Coverage).

## Review input

`review_input.json` carries the issue text the reviewer sees, the complete
`standard_v1` plan under review, the `catch_probes` (case-folded substrings
matched against each emitted concern's note and category), and the
hand-written `catching_examples` / `non_catching_examples` the loader uses to
prove the probes discriminate.
