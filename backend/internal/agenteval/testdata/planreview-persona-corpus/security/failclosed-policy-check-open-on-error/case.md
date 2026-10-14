# Case: failclosed-policy-check-open-on-error

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

The issue asks to inline `checkForbidden`'s matcher so it reads like
`checkAllowed`. The plan keeps the per-pattern violation shape for a match.

## The planted defect

- Kind `defect`, expect `catch`, lens `fail_closed`, anchored at the changed file
  `backend/internal/policy/policy.go`.
- The removed `hit.invalidPattern` branch emitted a `forbidden_paths`
  violation for a pattern that does not parse; the inlined loop `break`s on the
  `doublestar.PathMatch` error and emits nothing, so a malformed
  `forbidden_paths` glob forbids nothing: the check fails open on bad
  configuration.

## Why it should be mid-difficulty

The plan's step 2 promises the MATCH violation is unchanged, which is true; the
lost violation is the error branch, visible only as removed lines and a
one-word `break`.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `policy.go`; probes `fail open`, `fail closed`, `malformed`, `invalid glob`, `silently` and siblings. No probe occurs
in the issue, the plan or the diff, removed lines included (loader mode m).
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed.
