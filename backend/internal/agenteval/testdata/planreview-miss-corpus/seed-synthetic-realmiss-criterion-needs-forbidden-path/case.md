# Case: seed-synthetic-realmiss-criterion-needs-forbidden-path

**Provenance: DERIVED FROM A RECORDED REPOSITORY LESSON.** This case
(E55.26 / #4453) is not a class-3 triage entry. It reconstructs a lesson this
repository recorded after a real run: a criterion that demands a change to a
path the implement stage's `forbidden_paths` rejects is unsatisfiable from
inside the run, and every review round re-raises it without a fix (run
f199dcf1: flagged six times across three rounds). The lesson is recorded in
two places, cited here rather than quoted: the plan prompt's
Unenforceable-criteria rule (`backend/internal/prompt/prompt.go`) and
`docs/METHODOLOGY.md` § "Changes that span a forbidden governance file". The
text below is a hand-written, anonymized reconstruction: the
`example-org/reports-service` issue, the plan, the run and artifact ids and
the triage sequence are placeholders, and `miss.json` carries
`"synthetic": true`. Triage sequence 123 is unused: the committed set is 42,
101-111 and 116, and #4453 uses 120-124.

## What it represents

The issue asks for a `make lint-templates` command that lists every broken
report template with its file and line, so broken templates are caught before
they merge. The plan delivers the command and gives that central guarantee
its own correct explicit criteria (`ac-lint-names-broken-template`,
`ac-lint-lists-every-broken`, `ac-lint-clean-exits-zero`). It also adds an
INFERRED criterion, `ac-merge-check-runs-template-lint`, requiring
`deploy/pipelines/verify.yml` to run the lint as a required step on every
push, with a plausible rationale (the issue wants errors caught before merge,
and the sibling `lint-sql` step already runs there).

Separately, in `risks_and_assumptions` and away from the criteria, the plan
states that paths under `deploy/pipelines/**` are platform-owned and that a
diff touching that tree fails the implementing workflow's path check. No
`scope.files` entry and no approach step touches the pipeline. The implement
run left the pipeline untouched, as it had to; acceptance failed the
criterion, and the correct plan would have recorded the pipeline step as an
operator-side edit (`verification.out_of_scope`) instead of a criterion.

## The planted defect

- Criterion id: `ac-merge-check-runs-template-lint` (byte-identical in
  `miss.json` and in the plan inside `review_input.json`).
- Plan-review criterion it violates: testability / satisfiability. The
  criterion's only route to passing is a file the plan's own risk entry puts
  outside what the run may write, so it can only fail.
- not_mentioned difficulty: the defect is visible only by connecting the
  criterion's concrete path (`deploy/pipelines/verify.yml`) to the GLOB in a
  separate risk entry (`deploy/pipelines/**`). The plan text never says
  "forbidden", and nothing beside the criterion names the conflict.
- A concern that names the criterion and asks to ADD `verify.yml` to
  `scope.files` is declared a non-catching example: it sees a scope gap but
  not that the file cannot be written, and its remedy would fail the path
  check.

## Probes

The `catch_probes` are polarity-bearing and defect-specific, one family per
honest phrasing route: unsatisfiable from inside the run (`unsatisfiable
from`, `cannot be satisfied by this`), the run cannot edit the pipeline
(`cannot edit verify.yml`, `this change cannot touch`, `not writable`),
the plan never writes the pipeline (`not in scope.files`,
`absent from scope.files`, `no approach step edits`), the criterion
contradicts the plan's own risk entry
(`contradicts the plan's own risk`, `falls under deploy/pipelines`,
`fails the path check`), and the remedy (`platform-team change`,
`operator-side edit`, `move it to out_of_scope`, `separate pull request`).
None occurs in the issue or plan (mode m), in a non-catching example (mode
j), or in the conventions fixture. No bare consequence phrase is a probe.

## Review input

`review_input.json` carries the issue, the complete `standard_v1` plan, the
probes, the `attribution_phrases` (substrings of the planted statement and of
no other criterion), the `defect_phrases` the prompt-leak self-check reads,
and the catching and non-catching examples the loader checks.

## Calibration

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 15/30 with
conventions and 14/30 without. All 31 misses were `no_probe`, and every one is
a real attributed catch phrased outside the probes (Captain ruling
2026-10-16). The fix-up after calibration widened the probes with `not in
scope.files`, `absent from scope.files` and `no approach step edits`, and
pinned them with a catching example only those probes match. It also dropped
`is forbidden` and `off-limits`: neither matched in the 30-trial calibration,
both are short enough for an unrelated anchored note to reach, and no catching
example depends on either (the one that contains `off-limits` also matches
`not writable`). Kept as a documented CEILING by Captain ruling 2026-10-16
(honest rate about 30/30); the combined batch re-recording rescores it. Step
1d "Results" in `docs/compliance/planreview-catchrate-evidence.md` is the
record.
