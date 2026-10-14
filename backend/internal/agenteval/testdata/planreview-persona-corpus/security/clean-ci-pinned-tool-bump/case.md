# Case: clean-ci-pinned-tool-bump

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

A pinned tool bump done right: golangci-lint moves from the `v2.8.0` tag to the
`v2.9.0` tag in CI, all four release workflows and the gate image's ARG.

## Why it is clean

Every install stays on a TAGGED install script with an explicit version, every
pin site moves together, and no permission or step changes.

## Scoring

Clean rule (`CatchRule{Clean: true}`, no category predicate): EVERY stored
concern on this diff counts as a false alarm, standard-criteria concerns
included, so the arm's false-alarm rate must be read beside the
security-lens / other-category partition. The case is run by both the
`security` and the `security_portability` arms. No `markers.json`
repo-specific marker occurs in its issue, plan or diff (loader mode o), so a
portability concern naming one refuses the recording.
