# Case: secret-signed-url-in-debug-retry-log

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.19 / #4402), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. It replaces the #4314 ceiling case
of the same lens (all six sat at 30/30): the defect is a side effect inside a
larger legitimate change, never the headline (#4402 levers L1-L5).

## What it represents

A reliability change under `runner/internal/upload/`: part uploads gain a
bounded retry with a doubling backoff and a debug log per failed attempt;
`putPart` wraps the transport error so the retry can classify it; the
part URL gains a part number for idempotent repeats.

## The planted defect

- Kind `defect`, expect `catch`, lens `secret_custody`, anchored at the changed file
  `runner/internal/upload/parts.go`.
- `putPart` now wraps the `http.Client.Do` error with `%w` (it used to return
  a bare sentinel), and `putPartWithRetry` logs that error at debug level and
  returns it. `Client.Do`'s error is a `*url.Error` whose message embeds the
  request URL with its query string; the `session.go` hunk shows the part URL
  carries the session's upload key as the `uk` query value. Every retried
  part writes the upload key to the runner log, and an exhausted retry
  returns it in the stage error.
- Semantic verified on go1.25.6: `*url.Error` masks only a userinfo password
  and keeps the query string (https://pkg.go.dev/net/http#Client.Do: "Any
  returned error will be of type *url.Error"; https://pkg.go.dev/net/url#Error).

## Why it should be mid-difficulty

The defect is a side effect of a legitimate retry change (four hunks, three
files), and the error-wrapping hunk has a stated, plausible reason. No
credential-loaded identifier sits by the log line. Seeing it takes knowing
what a `*url.Error` prints and connecting the log line in `parts.go` to the
`uk` query value in `session.go`.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `parts.go` (`parts_test.go` does not carry it). Probes are
multi-word: the URL-exposure phrasings (`query string`, `full part url`,
`uk parameter`), the credential-flow phrasings (`upload key in the`,
`session credential`, `leaks the`) and the mechanism (`url.error includes`).
Approval condition 3 (#4402): no probe is a case-folded substring of any
`redaction.DefaultPatterns` name, so the sidecar's `[REDACTED:<rule>]`
marker and the bearer-header note of
`TestRescoreCatchRateTrials_RedactionMarkerNeverCatches` never score a catch. No probe occurs in the issue, the plan or the
diff, removed lines included (loader mode m), and every probe is multi-word, so
none is a case-folded substring of a `redaction.DefaultPatterns` name.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed. The difficulty is an a-priori design argument;
the operator's 30-trial calibration decides it.
