# backend/internal/unpricedmodel

Unpriced-model alert (`unpriced_model_alert`, #1870) and failed-request alert (`agent_request_failed_alert`, #2494): warn-only detection of cost-ledger rows the pricer could not act on, split into the two things they can actually mean.

## Detector

`unpricedmodel.go` — a pure `Evaluate(samples, priorAlerts, now, window) Decision` that scans priced cost samples in `[now-Window, now]` and collects the set of models that recorded a cost row the pricer could not act on: `known_model=false` (id absent from the pricing table) into `UnpricedModels`, `known_usage=false` (backend reported no usable token split) into `UnknownUsageModels`, each deduped + sorted.

`IsFailedRequestModel(model)` is the second classifier (#2494). A model id wrapped in angle brackets (`<synthetic>` is the value Claude Code stamps on a message it synthesized locally because the API request failed before any model ran) is a PLACEHOLDER, not a model identifier. `Evaluate` routes such a sample into `FailedRequestModels` and EXCLUDES it from both unpriced sets: there is no model to price, and reporting it as a pricing-coverage gap sends the operator hunting for a missing pricing entry instead of a failed request. The predicate keys on the bracket wrapper shape alone — no allow-list of specific ids — so a genuinely-unpriced REAL model id (a freshly released `claude-fable-5`) is unaffected.

`FailedRequestEvidence` carries the summed token counts across the reported failed-request samples (`InputTokens` fresh/cache-exclusive, `CacheReadInputTokens`, `CacheWriteInputTokens`, `OutputTokens`) plus the derived `CacheReadRatio` = cache_read / (fresh + cache_read + cache_write), **0 when the denominator is 0** (never a NaN). The ratio is what made the original diagnosis possible: a request that died before reaching a model still replays a large cached prompt, so a cache-read share near 1.0 against near-zero output is the signature of a failed request rather than real work.

`FailedRequestTripped` is a SECOND, independent emit gate — `Tripped` still gates only the unpriced/unknown-usage sets, so either, both, or neither alert can fire on one call.

**The two classes are STRICTLY INDEPENDENT — no cross-suppression** (#2494 approval condition 2). `Alert.FailedRequest` names which stream a prior alert came from, and each class dedups against its own history only. Historical placeholder occurrences were recorded under `unpriced_model_alert`, so on the first window after this ships a prior `unpriced_model_alert` naming `<synthetic>` does NOT suppress the first `agent_request_failed_alert`: the operator sees ONE duplicate report at that single upgrade boundary. That is cheaper than a cross-class suppression rule which would have to be reasoned about on every window forever.

`Window` is a 24h const (`unpricedmodel.Window`) — no config flag, since the trip condition is boolean (`known_model=false`) with no threshold analog to `spend_alert`'s multiple.

## Wiring

- Wired into trace ingest at `trace.go::checkUnpricedModel` (called from `recordCost` **right after `checkSpendAlert`**, on the same post-`cost_recorded`-append hook).
- It reads the cross-run cost ledger via `audit.Repository.ListAll(category="cost_recorded")` for the `{model, known_model, known_usage, input_tokens, cache_read_input_tokens, cache_write_input_tokens, output_tokens}` + timestamp of each `unpricedmodel.Sample`, then reads BOTH alert streams (`priorUnpricedAlerts`): `ListAll(category="unpriced_model_alert")` expands each prior payload's `unpriced_models`/`unknown_usage_models` arrays into unpriced-class `unpricedmodel.Alert`s, and `ListAll(category="agent_request_failed_alert")` expands `failed_request_models` into failed-request-class ones — so each class alarms **once per window** against its own history.
- It evaluates, and on a trip appends a **warn-only** `unpriced_model_alert` audit entry (`{unpriced_models, unknown_usage_models, model_count, triggering_model, window_start (RFC3339), window_hours}`) tied to the run.
- Independently, on `FailedRequestTripped` it appends a **warn-only** `agent_request_failed_alert` entry (`{failed_request_models, model_count, triggering_model, input_tokens, cache_read_input_tokens, cache_write_input_tokens, output_tokens, cache_read_ratio, window_start (RFC3339), window_hours}`) tied to the same run. Both categories are registered in `audit.KnownCategories`, so `fishhawk_await_audit` can be armed on either.
- `checkUnpricedModel` is best-effort throughout (a `ListAll` failure on either read, or the `AppendChained` write, logs at WARN and returns — never propagated, never unwinding the `cost_recorded` append or the upload), identical in posture to `checkSpendAlert`.
- The `ListAll -> Evaluate -> AppendChained` sequence is deliberately un-serialized: the dedup is noise-reduction, not a correctness invariant, so a rare duplicate warn-only alert under concurrent `recordCost` is acceptable.

## Posture

- Per ADR-044 the pricing table stays human-authoritative — this **alarms, it never auto-prices**.
- Closes the price-coverage gap the closed #1335/#1339 left open: a dispatched-but-unpriced model (fixed by hand in #1867) can no longer silently record $0 across the ledger unnoticed.
- Ledger-only (warn-only audit entries, no Notifier method — absent from `docs/issue-comment-surfaces.md`, like `spend_alert`).
- `unpriced_model_alert` now means only what it says: a real model with no price. The first external-repo run (#2494) reported a failed API request as an unpriced model, which cost the operator a pricing-table hunt for a model that never ran.
