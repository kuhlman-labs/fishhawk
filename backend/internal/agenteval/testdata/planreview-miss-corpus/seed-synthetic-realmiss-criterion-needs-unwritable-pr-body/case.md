# Case: seed-synthetic-realmiss-criterion-needs-unwritable-pr-body

**Provenance: DERIVED FROM A RECORDED REPOSITORY LESSON.** This case
(E55.26 / #4453) is not a class-3 triage entry. It reconstructs a lesson the
catch-rate run-book (`docs/compliance/planreview-catchrate-evidence.md`)
records twice, in Step 1b (the architect-arm calibration) and Step 1c (the
#4361 calibration): "a fix-up pass cannot edit the PR body", so a table
measured after the pull request opens is posted as a PR comment or
committed, never promised in the description. A criterion that
demands the description carry such a table names a surface the run's own
delivery mechanism cannot write, so it is unsatisfiable from inside the run.
The text below is a hand-written, anonymized reconstruction: the
`example-org/thumbnail-service` issue, the plan, the run and artifact ids and
the triage sequence are placeholders, and `miss.json` carries
`"synthetic": true`. Triage sequence 124 is unused: the committed set is 42,
101-111 and 116, and #4453 uses 120-124.

## What it represents

The issue asks to retune three JPEG thumbnail presets to the lowest quality
holding a minimum SSIM of 0.95 on a sample image set, and says reviewers must
see the measured numbers for every preset before they approve. The plan gives
the central guarantee its own correct explicit criteria
(`ac-presets-meet-ssim-floor`, `ac-presets-lowest-passing-quality`). Approach
step 4 names the only mechanism that ever has the numbers: the sample set
lives on a shared media runner the change's automation cannot reach, so after
the pull request opens, the runner's benchmark job pushes the regenerated
presets and `bench/results/presets.md` to the branch in one commit, with a
token scoped to repository contents only. A separate risk entry says the
pull request's body is filled in from the plan summary when the pull request
opens.

The plan's INFERRED criterion `ac-results-table-in-description` requires the
pull request description to carry the measured table, with a plausible
rationale (reviewers read the description first). The description is written
before the numbers exist, and the only later writer can push commits but not
edit the description. The job pushed the table into the committed file;
acceptance failed the criterion against the description.

## The planted defect

- Criterion id: `ac-results-table-in-description` (byte-identical in
  `miss.json` and in the plan inside `review_input.json`).
- Plan-review criterion it violates: testability / satisfiability, read
  against the plan's own approach. The reviewer-visibility requirement is
  real and explicit in the issue; the defect is the SURFACE the plan chose
  for it. A criterion observing the committed `bench/results/presets.md`
  would have passed.
- not_mentioned difficulty: the defect needs two cross-references, the
  criterion against approach step 4's contents-only token and against the
  risk entry saying when the body is written. Nothing beside the criterion
  names the conflict.
- A concern asking to LINK `bench/results/presets.md` from the description
  is declared a non-catching example: it still relies on writing the
  description.

## Probes

The `catch_probes` are polarity-bearing and defect-specific, one family per
honest phrasing route: the description cannot be edited or updated
(`cannot edit the description`, `no step updates the description`,
`description is written before`), the token lacks the permission
(`contents-only token`, `pull-requests: write`, `only pushes commits`), the
criterion is unsatisfiable as planned (`unsatisfiable as planned`,
`cannot be satisfied by the benchmark`, `contradicts step 4`), and the
remedy (`in a committed file`, `committed bench/results`,
`presets.md instead`). None occurs in the issue or plan (mode m), in a
non-catching example (mode j), or in the conventions fixture. No bare
consequence phrase is a probe.

## Review input

`review_input.json` carries the issue, the complete `standard_v1` plan, the
probes, the `attribution_phrases` (substrings of the planted statement and of
no other criterion), the `defect_phrases` the prompt-leak self-check reads,
and the catching and non-catching examples the loader checks.

## Calibration

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 28/30 with
conventions and 30/30 without; both misses were `no_probe` and both are real
catches. Reviewers led with the planted description-surface defect ("cannot
edit the pr description", 25-26/30). The implement review's low-tier concern
that `scope.files` creates `bench/results/presets.md` and
`internal/thumbs/presets_gen.go` while approach step 4 has the media-runner
job produce them after the pull request opens (the "later-job files"
adjacency) did not displace it, so the case is kept unchanged. Kept as a
documented CEILING by Captain ruling 2026-10-16 (honest rate about 30/30); the
combined batch re-recording rescores it. Step 1d "Results" in
`docs/compliance/planreview-catchrate-evidence.md` is the record.
