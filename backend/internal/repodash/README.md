# repodash

Pure folds behind the repo dashboard's rollup endpoints (E40.3 / #1714):
`GET /v0/repos/{owner}/{name}/{throughput,health,economics}`. No database, no
HTTP, no clock — the server (`backend/internal/server/repodash.go`) gathers the
runs, decodes their audit rows into `Event` / `CostEntry`, and calls
`FoldThroughput`, `FoldHealth`, `FoldEconomics`.

## Contract

- **Window.** `NewWindow(now, weeks)` is `weeks` ISO weeks (Monday 00:00 UTC
  starts) ending with the week containing `now`, half-open `[Start, End)`.
  `weeks` is bounded by `MinWeeks`..`MaxWeeks` (1..52, default 12) at the HTTP
  layer. Empty weeks are zero buckets, never dropped.
- **Scan completeness (server-side, constants here).** The server pages runs
  newest-first until one is older than `ScanBoundary()` = `Start − LookBack`
  (14 days) or the listing ends, under `MaxRunsScanned` (1000). A ceiling stop
  sets `truncated` on every rollup. Residual: a run created more than
  `LookBack` before the window and merged inside it is not counted.
- **Merge time = EARLIEST `pr_merged`** (`Run.MergedAt`). Never
  `post_merge_observed`, never the newest marker. Throughput buckets by merge
  time; a change (PR URL, else the run) counts once, in the week of its earliest
  merge. Cycle time = `CreatedAt` → merge time. A run merged in the window per
  `post_merge_observed` but carrying no `pr_merged` is excluded from the median
  and counted in `CycleTimeExcluded`.
- **Health population = runs CREATED in the window.** Plan first-shot: the first
  plan-stage approve was preceded by no plan-stage reject, `plan_revised` or
  `plan_review_failed` (events in chain order). Fixup: a run counts once however
  many `stage_fixup_triggered` rows it has. Acceptance: each run's LATEST
  verdict (`passed` / `not_validated` / `undecidable`; anything else is
  failed). Failure mix: failed stages per category A/B/C/D; unknown values are
  skipped.
- **Economics.** Every cost entry TIMESTAMPED in the window, folded through
  `cost.AggregateRunCost` (total, per week) and `cost.AggregateCacheEfficiency`
  (per-week cache-read ratio, reuse factor, net savings). Cost per merged change
  = total ÷ the throughput merged-change count.
- **`WaitOnHuman`** folds the per-run #1702 gate-latency rollups of runs created
  in the window and is `nil` when none resolved — the server omits the key.
- **Zero denominators.** `Ratio` and `Median` return 0 on empty input, and the
  cost-per-change division is guarded: encoding/json cannot encode NaN/Inf, so
  an unguarded division would 500 an empty window.

## Tests

`repodash_test.go` table-tests each fold. `TestRepoDash_EmptyWindowZeroNotNaN`
is the counterfactual vehicle for every zero-denominator guard;
`TestRepoDash_CycleTimeUsesEarliestPRMerged` pins the merge-time definition.
The HTTP seam, look-back, ceiling and wire goldens are tested in
`backend/internal/server/repodash_test.go`.
