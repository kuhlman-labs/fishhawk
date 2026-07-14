# backend/internal/invariantmonitor

Self-consistency invariant monitor (#764): periodic sweep for cross-entity state inconsistencies.

## Ticker

`Ticker.Tick` runs two invariants per pass:

- **Invariant 1** ({all stages terminal, run non-terminal}) auto-reconciles via the `Reconcile` func wired to `orchestrator.ReconcileStuckRuns` — the same safe self-heal the one-shot startup path runs (#727), now periodic.
- **Invariant 2** ({review stage `awaiting_approval`, null/empty `pull_request_url`}) is **surface-only**: it pages `ListRuns(state=running)` + `ListStagesForRun` (no new sqlc query) and emits a `system`-actor `invariant_violation` audit entry (payload `{kind, run_id, reconciled:false}`) plus a WARN log — it mutates nothing because the missing PR is the unrecoverable fact (#742).

Invariant 2 fires **only** for runs that genuinely intended to open a PR: `runIntendsPR` parses the run's cached `WorkflowSpec` and flags only when a stage in the run's workflow produces a `pull_request` artifact, so non-PR (commit-yourself) workflows — whose null PR is the legitimate normal state — are never flagged (a run whose intent can't be determined is left silent).

## Configuration and posture

Off by default; enable with `--enable-invariant-monitor` (`FISHHAWKD_ENABLE_INVARIANT_MONITOR=true`), scan interval `--invariant-monitor-interval` (default 60s).

Mirrors the `dispatchwatchdog.Ticker` shape; per-run errors are best-effort/logged and never abort the sweep. The durable audit entry + structured WARN are the `metric` surface for now — a real prometheus/expvar exporter is deferred.
