# backend/internal/spendalert

Spend anomaly alert (`spend_alert`): warn-only detection of an hourly spend spike against the rolling cross-run baseline.

## Detector

`spendalert.go` — a pure `Evaluate(samples, now, multiple) Decision` that buckets priced cost samples by UTC hour, computes the rolling average over prior hours within a 24h `Window`, and reports whether the current hour exceeds `multiple` × that average.

## Wiring

- Wired into trace ingest at `trace.go::checkSpendAlert` (called from `recordCost` right after the `cost_recorded` append): it reads the cross-run cost history via `audit.Repository.ListAll(category="cost_recorded")`, builds `spendalert.Sample`s from each entry's `usd`, evaluates, and on a trip appends a **warn-only** `spend_alert` audit entry (`{latest_hour_usd, rolling_avg_usd, ratio, multiple, prior_hours, latest_hour_start, triggering_model}`) tied to the run.
- Threshold: `FISHHAWKD_SPEND_ALERT_MULTIPLE` (flag `--spend-alert-multiple`, default `3`x via `spendalert.DefaultMultiple`), wired in `serve.go` onto `server.Config.SpendAlertMultiple`.
- `checkSpendAlert` is best-effort throughout (a `ListAll` failure, insufficient history, or audit-write failure logs at WARN and never unwinds the upload), and the detector suppresses alerts until a baseline of prior hours with spend exists, so a fresh deployment stays quiet.
- It catches runaway loops and injection-driven token blowups without ever gating a run.
- Because `ListAll` reads the cost ledger across **all** runs, a decomposition family's spend is inherently aggregated into the hourly samples — a fan-out spike across parent + children registers as one hour's elevated spend (E24.6 / #1146) without narrowing the cross-run baseline.
