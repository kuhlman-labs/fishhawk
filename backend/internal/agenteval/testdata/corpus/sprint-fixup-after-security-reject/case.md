# Case: sprint-fixup-after-security-reject

**Provenance: PRODUCTION.** This trace was captured from a real Fishhawk
production run (#1432), not hand-authored. It was sourced from the REDACTED
trace-bundle variant (the only variant GET /v0/stages/{stage_id}/trace
serves; the raw variant is object-locked and unredacted), so it carries no
unredacted secrets.

Scaffolded by `fishhawk-distill-corpus` (#1290). The scorecard below
was derived deterministically by agenteval.Score; the sections marked TODO
are operator curation and must be completed before this case lands (#819).

## What it represents

Derived outcome: `diff_produced`.

Implement of the MCP deploy-approve verbs (#1432, run 55846b0b). The first implement drew a HIGH security REJECT from one of two heterogeneous reviewers: a flag-injection vector — --override-freeze could be smuggled via the Environment/Reason inputs that get composed into the approval comment the backend parses token-wise. A single operator-routed fixup pass added input validation (reject any whitespace or --flag token in Environment/Reason, failing locally with zero HTTP calls) plus tests, inverting the reject to a clean dual-approve. A security-remediation fixup that converges a real HIGH concern in one pass.

## Distilled signal

Signal: `diff_produced / fixup pass remediating a HIGH security reject (flag injection)`.

Implement of the MCP deploy-approve verbs (#1432, run 55846b0b). The first implement drew a HIGH security REJECT from one of two heterogeneous reviewers: a flag-injection vector — --override-freeze could be smuggled via the Environment/Reason inputs that get composed into the approval comment the backend parses token-wise. A single operator-routed fixup pass added input validation (reject any whitespace or --flag token in Environment/Reason, failing locally with zero HTTP calls) plus tests, inverting the reject to a clean dual-approve. A security-remediation fixup that converges a real HIGH concern in one pass.
