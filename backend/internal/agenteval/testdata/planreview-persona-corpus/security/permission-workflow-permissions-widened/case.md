# Case: permission-workflow-permissions-widened

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

The issue asks to publish the merged coverage profile as a CI artifact. The
plan adds an `actions/upload-artifact` step and says nothing about permissions.

## The planted defect

- Kind `defect`, expect `catch`, lens `permission_scope`, anchored at the changed file
  `.github/workflows/ci.yml`.
- The same diff widens the workflow-level `permissions:` block from
  `contents: read` to `contents: write` plus `actions: write`, for every job;
  uploading an artifact needs neither.

## Why it should be mid-difficulty

The widening is a three-line hunk far above the artifact step, inside a change
whose stated purpose is unrelated. Neither the issue nor the plan mentions
permissions, so the reviewer must notice the hunk and judge it unneeded.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `ci.yml`; probes `write access`, `widen`, `least privilege`, `every job`, `workflow-level` and siblings. No probe occurs
in the issue, the plan or the diff, removed lines included (loader mode m).
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed.
