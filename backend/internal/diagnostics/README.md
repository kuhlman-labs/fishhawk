# backend/internal/diagnostics

Product-facts diagnostic bundle backing `GET /v0/runs/{id}/diagnostics` (#1006).

## Collector

`bundle.go` — pure `Collect(run, stages, auditEntries, versions) DiagnosticBundle`, no I/O.

Carries STRUCTURED product facts only: run id, ordered stage states, the failing stage's category + audit surface, audit sequence `[min,max]`, this binary's fishhawkd version + git SHA from `internal/version` + the required min runner version, workflow spec hash, runner kind.

By construction NO diffs, paths, prompts, free text, or audit payload bodies — the failing stage's free-text `FailureReason` is excluded (a `bundle_test.go` assertion).

## Read handler and consumers

Read handler: `backend/internal/server/diagnostics.go::handleGetRunDiagnostics` loads run + stages + audit (`GetRun` / `ListStagesForRun` / `ListForRun`) and returns the bundle; pure read, no egress.

Backs the `fishhawk diagnose <run-id>` CLI verb (`cli/cmd/fishhawk/diagnose.go`). Foundation of the product-feedback feature; the deduped egress path + operator surfaces ride on top in sibling slices.
