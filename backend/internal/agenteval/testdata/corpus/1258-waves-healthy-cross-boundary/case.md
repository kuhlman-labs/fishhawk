# Case: 1258-waves-healthy-cross-boundary

**Provenance: PRODUCTION.** This trace was captured from a real Fishhawk
production run (#1258), not hand-authored. It was sourced from the REDACTED
trace-bundle variant (the only variant GET /v0/stages/{stage_id}/trace
serves; the raw variant is object-locked and unredacted), so it carries no
unredacted secrets.

Scaffolded by `fishhawk-distill-corpus` (#1290). The scorecard below
was derived deterministically by agenteval.Score; the sections marked TODO
are operator curation and must be completed before this case lands (#819).

## What it represents

Derived outcome: `diff_produced`.

Real production implement trajectory (PR #1281, topological-wave decomposition dispatch). A healthy cross-boundary change spanning the orchestrator plan_decomposed audit, a new non-settling /integrate-wave server endpoint, the MCP client, the run_children wave loop, and docs. The agent read extensively across the existing #989/#1218 re-invoke code and the consolidate/run_children seams BEFORE editing (evidence_before_edit true), produced a 12-file diff_produced with no scope drift, no out-of-tree writes, and no loops. Positive control with a REAL messy 50+-tool sequence, vs the synthetic healthy-cross-boundary seed.

## Distilled signal

Signal: `healthy_cross_boundary`.

Real production implement trajectory (PR #1281, topological-wave decomposition dispatch). A healthy cross-boundary change spanning the orchestrator plan_decomposed audit, a new non-settling /integrate-wave server endpoint, the MCP client, the run_children wave loop, and docs. The agent read extensively across the existing #989/#1218 re-invoke code and the consolidate/run_children seams BEFORE editing (evidence_before_edit true), produced a 12-file diff_produced with no scope drift, no out-of-tree writes, and no loops. Positive control with a REAL messy 50+-tool sequence, vs the synthetic healthy-cross-boundary seed.
