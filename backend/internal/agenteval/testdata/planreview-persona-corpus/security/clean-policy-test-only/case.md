# Case: clean-policy-test-only

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

A test-only change in `backend/internal/policy/`: two `TestCheckAllowed` rows pin
the invalid-pattern violation `checkAllowed` already reports.

## Why it is clean

No production code changes; the rows assert the fail-closed behaviour that
exists today. Their expected `Violation` rows are the real
`backend/internal/policy` `checkAllowed` output (`doublestar.PathMatch("[", …)`
returns a syntax error, so it sets the invalid flag and matches nothing): `[`
alone yields the invalid-pattern row then the files-outside row for
`docs/a.md`; `[` listed before `docs/**` yields the invalid-pattern row alone,
because `docs/**` then admits the file. The second row's name says "listed
before" because `checkAllowed` stops trying patterns at a file's first match,
so the row claims no order independence it does not test. The hunk header's
line counts match its body.

## Scoring

Clean rule (`CatchRule{Clean: true}`, no category predicate): EVERY stored
concern on this diff counts as a false alarm, standard-criteria concerns
included, so the arm's false-alarm rate must be read beside the
security-lens / other-category partition. The case is run by both the
`security` and the `security_portability` arms. No `markers.json`
repo-specific marker occurs in its issue, plan or diff (loader mode o), so a
portability concern naming one refuses the recording.
