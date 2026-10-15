# Case: secret-credential-in-error-path

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.17 / #4314), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. Paths such as `.github/workflows/`
appear only as JSON string data inside `persona_case.json`.

## What it represents

The issue asks to make refused runner uploads diagnosable from the error alone.
The plan returns the response snippet and "the request's headers" in the
non-2xx error.

## The planted defect

- Kind `defect`, expect `catch`, lens `secret_custody`, anchored at the changed file
  `runner/internal/upload/client.go`.
- `req.Header` is formatted into the returned error, and six context lines
  above it the request sets `Authorization: Bearer <token>`, so every refused
  upload carries the runner's bearer value into the stage log and trace.

## Why it should be mid-difficulty

The plan states the headers are included, but never that they hold a
credential; the reviewer must connect the `%v` of `req.Header` to the
`Authorization` header set in the context lines.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `client.go`; probes `credential`, `leak`, `expos`, `redaction`, `redacting`, `secret` and siblings. No probe occurs
in the issue, the plan or the diff, removed lines included (loader mode m).
The bare `redact` is deliberately NOT a probe: the sidecar stores every note
through `redaction.RedactDefault`, whose `[REDACTED:<rule>]` marker it would
match, so a re-score could manufacture a catch the recording never saw.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed.
