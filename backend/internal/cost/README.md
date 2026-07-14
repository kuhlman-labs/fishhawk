# backend/internal/cost

Per-run cost rollup + reproducibility pin: control-plane-side pricing of stage-agent trace bundles and server-side reviewer invocations into the `cost_recorded` audit ledger and the per-run total.

## Pricing table

Shared pricing table: `pricing/` (module `github.com/kuhlman-labs/fishhawk/pricing`, resolved via `go.work` — no `require` in `backend/go.mod`, same as the runner). `pricing.Cost(model, in, out)` is the single source of truth for tokens→$ used by both the runner GenAI span and this backend rollup.

## Stage-agent rollup (`cost.go`)

- `cost.FromManifest` prices the **signed bundle manifest's** token split — control-plane-side, never trusted from a runner span, so a dropped/tampered span can't corrupt the ledger.
- Wired into trace ingest at `trace.go::recordCost` (called right after the `trace_uploaded` audit append): writes a `cost_recorded` audit entry (`{model, input_tokens, output_tokens, usd, known_model, known_usage, pricing_as_of, estimated}`) tied to the run, then accumulates the per-run total + pins the resolved model id via the optional `runCostRecorder` capability (`AddRunCost`).
- `recordCost` is best-effort (manifest-parse / audit-write / rollup failures log at WARN, never unwind the upload); an unknown model id records at `usd=0, known_model=false` rather than a guess.
- **No-usage honesty (#682):** a manifest with no usable token split (both counts zero — a future agent backend that didn't report usage; absence inferred from `input_tokens==0 && output_tokens==0`, since a real invocation always has >0 tokens) degrades to `usd=0, known_usage=false` rather than a silent `$0` indistinguishable from a real tiny run — mirroring `known_model=false`. claudecode, the sole current backend, always reports usage, so live runs record `known_usage=true`.
- Run-record surface: `runs.cost_usd_total` + `runs.resolved_model` (migration 0028; `run.Run.CostUSDTotal` / `run.Run.ResolvedModel`). `AddRunCost` is a method on the Postgres repo but **not** on `run.Repository` — the trace handler asserts the capability at runtime so the many `run.Repository` test fakes that don't roll cost need no stub.
- The rolled figure is an ESTIMATE (point-in-time pricing, see `pricing.AsOf`); the per-bundle `cost_recorded` audit entries are the canonical per-invocation ledger and the run's trajectory pointer (G6 reproducibility).
- Spend anomaly detection (`spend_alert`) reads these `cost_recorded` entries — see `backend/internal/spendalert/README.md`.

## Advisory reviewer cost (#681)

Plan-review / implement-review agents run server-side inside `fishhawkd` (claudecode subprocess or anthropic SDK) and never ship a trace bundle, so their tokens never reach `recordCost`. They are captured instead at the reviewer CONTRACT boundary:

- Token usage becomes part of `planreview.ReviewVerdict.Usage` (tagged `json:"-"` so it is sourced from the API/CLI envelope by the adapter, never from the agent-emitted verdict JSON); every backend populates it.
- `trace.go::recordReviewerCost` prices it ONCE at the `plan_reviewed` / `implement_reviewed` call site (inside `runPlanReviewLoop` / `runImplementReviewLoop`) via the same `cost.FromManifest` + `runCostRecorder.AddRunCost` path — never branching on which adapter ran.
- **Resolved-model pin protection (#684):** because the advisory review runs AFTER the stage ships its trace, `recordReviewerCost` would be the last `AddRunCost` writer and last-write-wins would clobber the stage-agent's G6 `resolved_model` pin with the reviewer's model. To prevent this, `recordReviewerCost` passes an EMPTY `resolved_model` to `AddRunCost` — reviewer cost still folds into `cost_usd_total`, but the `CASE WHEN '' <> '' … ELSE resolved_model` branch leaves the pin untouched. Only `recordCost` (the stage agent) ever pins `resolved_model`.
- Reviewer `cost_recorded` entries carry an extra `source` (`plan_review` / `implement_review`) field distinguishing them from runner stage-agent entries (which carry no `source`) — the `source` field alone is the distinguisher, since both backends now carry `known_usage` (#682); a backend that cannot report usage degrades to `usd=0, known_usage=false` (mirroring `known_model=false`).

## Reviewer context-blowup observability (#995, accounting normalized by #1010)

- `planreview.Usage.InputTokens` is the cache-EXCLUSIVE fresh input count for EVERY adapter (the codex adapter subtracts the CLI's cache-inclusive raw figure at the boundary, clamped at 0; claudecode/anthropic already report cache-exclusive), so cross-adapter input figures are directly comparable.
- Reviewer `cost_recorded` payloads additionally carry `turns` (model turns per invocation: summed `turn.completed` lines for codex, 1 for the single-shot claudecode/anthropic adapters), `cached_input_tokens` (the cache-served split, always ADDITIONAL to `input_tokens`), and `total_input_tokens` (= fresh + cached, preserving the raw input-side total for fresh-vs-cached pricing math). The `plan_reviewed` / `implement_reviewed` payloads carry the invocation's `input_tokens`/`output_tokens` (omitempty).
- `recordReviewerCost` WARN-logs at two advisory ceilings: `reviewerInputTokenWarnCeiling` (100k on FRESH tokens — a single composed review prompt should never reach six figures fresh; a breach signals a context-assembly blowup such as an agentic reviewer exploring its cwd) and `reviewerTotalInputTokenWarnCeiling` (500k on fresh + cached — a runaway total context that heavy caching keeps off the fresh ceiling, seeded by an observed 689k-total codex review).
- Observability-only: pricing is untouched. `recordReviewerCost` deliberately does NOT call `checkSpendAlert` — reviewer entries are swept by the next runner-triggered alert, which re-reads all `cost_recorded` entries.

## Cache-efficiency aggregation (`efficiency.go`)

`AggregateCacheEfficiency` is the pure fold behind `GET /v0/runs/{run_id}/cache-efficiency` — see the budget/cost display notes in `backend/internal/budget/README.md`.
