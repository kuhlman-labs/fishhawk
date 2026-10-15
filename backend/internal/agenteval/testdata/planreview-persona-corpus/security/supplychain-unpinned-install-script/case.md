# Case: supplychain-unpinned-install-script

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

The issue asks to run actionlint over the workflow files in CI. The plan adds a
job that downloads actionlint with the project's official download script.

## The planted defect

- Kind `defect`, expect `catch`, lens `supply_chain`, anchored at the changed file
  `.github/workflows/ci.yml`.
- The script is fetched from the `main` branch of the upstream repository and
  executed with `bash <(curl ...)`, so an upstream change runs arbitrary code in
  CI with no change on this side.

## Why it should be mid-difficulty

"The official download script" sounds safe and is the upstream-documented
install; the ref (`main`) is a path segment in a long URL. The reviewer must
apply a pin policy to it.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `ci.yml`; probes `unpinned`, `pinned to`, `floating`, `mutable`, `release tag` and siblings. No probe occurs
in the issue, the plan or the diff, removed lines included (loader mode m).
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed.
