# backend/internal/diagnostics

Product-facts diagnostic bundle backing `GET /v0/runs/{id}/diagnostics` (#1006).

## Collector

`bundle.go` — pure `Collect(run, stages, auditEntries, versions) DiagnosticBundle`, no I/O.

Carries STRUCTURED product facts only: run id, ordered stage states, the failing stage's category + audit surface + failure detail class, audit sequence `[min,max]`, this binary's fishhawkd version + git SHA from `internal/version` + the required min runner version, workflow spec hash, runner kind.

By construction NO diffs, paths, prompts, free text, or audit payload bodies — the failing stage's free-text `FailureReason` is excluded (a `bundle_test.go` assertion).

## Failure-detail classifier

`detailclass.go` — pure `ClassifyFailureDetail(reason) string` reduces the failing stage's free-text `FailureReason` (wrapped git stderr) to a CLOSED enum via an ordered marker table: `auth-401`, `bad-object-ref`, `target-unreachable`, or `""` (unclassified). It never returns any part of its input — only a table-owned enum literal — so `FailingStage.FailureDetailClass` is redaction-safe by construction. Ordering is load-bearing: git prefixes both auth and network failures with `fatal: unable to access '<url>':`, so `unable to access` is NOT a marker and `auth-401` is checked before `target-unreachable`.

The class is a fourth fingerprint component (`fingerprint.go`), included ONLY when non-empty, so it splits distinct root causes that share a failing surface (#1962) while keeping every unclassified failure's pre-change 3-component fingerprint.

## Read handler and consumers

Read handler: `backend/internal/server/diagnostics.go::handleGetRunDiagnostics` loads run + stages + audit (`GetRun` / `ListStagesForRun` / `ListForRun`) and returns the bundle; pure read, no egress.

Backs the `fishhawk diagnose <run-id>` CLI verb (`cli/cmd/fishhawk/diagnose.go`). Foundation of the product-feedback feature; the deduped egress path + operator surfaces ride on top in sibling slices.
