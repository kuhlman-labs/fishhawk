# Case: 1255-wait-parser-healthy-minimal

**Provenance: PRODUCTION.** This trace was captured from a real Fishhawk
production run (#1255), not hand-authored. It was sourced from the REDACTED
trace-bundle variant (the only variant GET /v0/stages/{stage_id}/trace
serves; the raw variant is object-locked and unredacted), so it carries no
unredacted secrets.

Scaffolded by `fishhawk-distill-corpus` (#1290). The scorecard below
was derived deterministically by agenteval.Score; the sections marked TODO
are operator curation and must be completed before this case lands (#819).

## What it represents

Derived outcome: `diff_produced`.

Real production implement trajectory (PR #1266, ?wait parser boundary test). A minimal, well-scoped test-only change: the agent read the parser and existing tests before adding one table-driven boundary test (evidence_before_edit true), producing a small clean diff_produced with no scope drift, no out-of-tree writes, no loops. Positive control for the small-mechanical-change shape from real production.

## Distilled signal

Signal: `healthy_minimal`.

Real production implement trajectory (PR #1266, ?wait parser boundary test). A minimal, well-scoped test-only change: the agent read the parser and existing tests before adding one table-driven boundary test (evidence_before_edit true), producing a small clean diff_produced with no scope drift, no out-of-tree writes, no loops. Positive control for the small-mechanical-change shape from real production.
