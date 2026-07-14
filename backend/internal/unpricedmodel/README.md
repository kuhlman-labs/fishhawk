# backend/internal/unpricedmodel

Unpriced-model alert (`unpriced_model_alert`, #1870): warn-only detection of cost-ledger rows the pricer could not act on.

## Detector

`unpricedmodel.go` — a pure `Evaluate(samples, priorAlerts, now, window) Decision` that scans priced cost samples in `[now-Window, now]` and collects the set of models that recorded a cost row the pricer could not act on: `known_model=false` (id absent from the pricing table) into `UnpricedModels`, `known_usage=false` (backend reported no usable token split) into `UnknownUsageModels`, each deduped + sorted.

`Window` is a 24h const (`unpricedmodel.Window`) — no config flag, since the trip condition is boolean (`known_model=false`) with no threshold analog to `spend_alert`'s multiple.

## Wiring

- Wired into trace ingest at `trace.go::checkUnpricedModel` (called from `recordCost` **right after `checkSpendAlert`**, on the same post-`cost_recorded`-append hook).
- It reads the cross-run cost ledger via `audit.Repository.ListAll(category="cost_recorded")` for the `{model, known_model, known_usage}` + timestamp of each `unpricedmodel.Sample`, then reads `ListAll(category="unpriced_model_alert")` and expands each prior payload's `unpriced_models`/`unknown_usage_models` arrays into `unpricedmodel.Alert`s so a persistently-unpriced model alarms **once per window** rather than once per invocation.
- It evaluates, and on a trip appends a **warn-only** `unpriced_model_alert` audit entry (`{unpriced_models, unknown_usage_models, model_count, triggering_model, window_start (RFC3339), window_hours}`) tied to the run.
- `checkUnpricedModel` is best-effort throughout (a `ListAll` failure on either read, or the `AppendChained` write, logs at WARN and returns — never propagated, never unwinding the `cost_recorded` append or the upload), identical in posture to `checkSpendAlert`.
- The `ListAll -> Evaluate -> AppendChained` sequence is deliberately un-serialized: the dedup is noise-reduction, not a correctness invariant, so a rare duplicate warn-only alert under concurrent `recordCost` is acceptable.

## Posture

- Per ADR-044 the pricing table stays human-authoritative — this **alarms, it never auto-prices**.
- Closes the price-coverage gap the closed #1335/#1339 left open: a dispatched-but-unpriced model (fixed by hand in #1867) can no longer silently record $0 across the ledger unnoticed.
- Ledger-only (warn-only audit entry, no Notifier method — absent from `docs/issue-comment-surfaces.md`, like `spend_alert`).
