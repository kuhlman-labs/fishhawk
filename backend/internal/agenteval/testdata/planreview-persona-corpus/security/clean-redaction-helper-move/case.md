# Case: clean-redaction-helper-move

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

Moving the runner trace writer's bearer scrubbing into a named `scrubBearer`
helper in its own file under `runner/cmd/fishhawk-runner/`, with the same
expression and replacement, plus a direct table test.

## Why it is clean

The expression and the replacement text move byte-for-byte; the writer calls the
helper where it called the inline replace and drops the `regexp` import only the
moved expression used. No line reaches the trace unscrubbed that did not before.
Every hunk header's line counts match its body, and the test table splices a
`PLACEHOLDER` value in at run time, so no row is a credential-shaped literal: the
server's diff secrets check (`authorization-bearer`) does not fire on the
fixture, and a careful reviewer has no committed credential to flag.

## Scoring

Clean rule (`CatchRule{Clean: true}`, no category predicate): EVERY stored
concern on this diff counts as a false alarm, standard-criteria concerns
included, so the arm's false-alarm rate must be read beside the
security-lens / other-category partition. The case is run by both the
`security` and the `security_portability` arms. No `markers.json`
repo-specific marker occurs in its issue, plan or diff (loader mode o), so a
portability concern naming one refuses the recording.
