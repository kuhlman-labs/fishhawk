# stallwatch

The page-only stall watch (#5497). A fishhawkd ticker that pages the operator
when a failed stage, or a dispatchable decomposition child, has seen no
operator or driver act for `--stall-watch-threshold` (default 30m).

This package only schedules. The two scanners are
`Server.ScanFailedStageStalls` (`backend/internal/server/stall_watch.go`) and
`Server.ScanUndispatchedChildStalls` (`stall_watch_children.go`). fishhawkd
adapts them to `stallwatch.Scanner`, because this package does not import
`server`. The page channel is the #5308 pings-only page class:
`issuecomment.pageClassEvents` projects each row and `notifyPageClass` fires it
at once.

**The watch only pages.** It never retries, revives, resumes or dispatches
anything. It holds no token identity, writes no `stage_retried` or
`run_auto_driven` row, and evaluates no delegation or campaign override. The
delegated auto-retry #5497 asked for is out of scope pending an operator
decision.

## Wiring

| Flag | Env | Default |
|---|---|---|
| `--enable-stall-watch` | `FISHHAWKD_ENABLE_STALL_WATCH` | off |
| `--stall-watch-threshold` | | 30m |
| `--stall-watch-interval` | | 5m |
| `--stall-watch-lookback` | | 168h |

With the watch enabled, a non-positive threshold or interval, or a lookback
shorter than the threshold, refuses to boot (`Ticker.Validate`). With
`RunRepo` or `AuditRepo` unwired, fishhawkd warns and starts no ticker. The
ticker ticks immediately and then every interval. Each tick runs both scanners,
each under its own `recover`, and an error or panic in one WARN-logs without
stopping the other. `scripts/dev` passes `--enable-stall-watch`, and
`FISHHAWK_DEV_STALL_WATCH=0` drops it.

## Condition 1: failed stage (`stage_failed_awaiting_operator`)

Page kind `failed_stage_stalled`, written on the failed stage's own run chain.
Candidates are every non-terminal run plus terminal-failed runs inside
Lookback. Per run the HIGHEST-sequence failed stage carrying a
`FailureCategory` is classified. The one legal remedy is:

| Run | Failure | `next_action` |
|---|---|---|
| non-terminal | `run.RetryableFailure` (A, C, timeout D) | `fishhawk_retry_stage` |
| failed | every failed stage `run.RetryableFailure` | `fishhawk_revive_run` |
| failed decomposition child, non-terminal parent | every failed stage `run.RecoverableInDecomposition` (B) | `fishhawk_resume_run` |
| anything else | | no page |

Exclusions (no page):

- a non-terminal run whose failure is category B, or a non-timeout D:
  `retry_stage` would refuse it;
- a decomposed parent's IMPLEMENT failure (its children's pages cover it;
  mirrors `run.ClassifyInfraFailurePark`). A parent's acceptance failure still
  pages;
- a spent infra-park budget (`InfraParkBudgetExhausted`);
- a failed run superseded by a newer non-slice run on the same `trigger_ref`,
  or by a recovery descendant;
- a failed top-level run with a B or D failure;
- cancelled and succeeded runs;
- a failed run whose failure `EndedAt` is older than Lookback.

## Condition 2: undispatched child (`child_dispatch_awaiting_operator`)

Page kind `child_dispatch_stalled`, written on the CHILD's chain with
`StageID` = its implement stage. It names `fishhawk_run_children` on the
parent. A child pages only when all of these hold:

- its run is non-terminal and its implement stage is `pending` or
  `awaiting_host_dispatch`;
- it is not queued for a concurrency slot with a live waiter;
- the host-dispatch marker's wave-order guard (`guardDecompositionWaveOrder`)
  and dependency-coverage guard (`resolveDependentChildBase`, i.e.
  `wavecoverage.Covered` over the parent's newest `slices_integrated`) both
  admit it. These are the SAME calls `host_dispatch.go` makes, and both are
  read-only (pinned by `TestScanUndispatchedChildStalls_GuardsStayReadOnly`);
- the host memory hold (`Config.HostAdmissionHold`) is not held. While it is,
  the marker refuses every new admission, so no child pages at all;
- it is not held behind a live fan-out's cap (below).

The child predicate mirrors those marker guards only, not every one. The
campaign dispatch hold, the deploy- and failed-stage-ahead holds and the
`max_parallel` override are not read (see residuals). A host-dispatched
(`auto_dispatch`) child still pages: a dispatcher that is down or refusing is
exactly the stall.

### Cap deferral

`effective_max_parallel` comes from the parent's newest `plan_decomposed` row
(0 or absent is unlimited). When it is above 0 and the in-flight siblings reach
it, the child does not page. A sibling is in flight when its run is
non-terminal and its implement stage either has left `pending` /
`awaiting_host_dispatch` without reaching a terminal state, or is still there
but queued for a slot with a live waiter (the way `fishhawk_run_children`
counts it). An undispatched sibling with no live waiter does not count, so
children that are all stuck still page.

## Episode identity and resolution

Each condition EPISODE gets one row, deduped on `episode_key`. No
stage-transition audit row exists to key on: about 15 sites write a stage
failure with no common category, and entering `pending` writes none. The key is
therefore built from the stage row's own per-transition stamps, through ONE
exported derivation the writer and the projection share:

- failed: `issuecomment.FailedStageStallEpisode`:
  `<stage_id>/<ended_at>/<self_retry_count>`;
- child: `issuecomment.ChildDispatchStallEpisode`:
  `<stage_id>/<self_retry_count>/<dispatched_at or never>`. `dispatched_at` is
  re-stamped on every entry into `dispatched` (migration 0072), so a
  re-dispatch that returns to `pending` without a retry bump is a new episode.

A later tick in the same episode is a dedup no-op. The projection
(`pageEventResolved`) suppresses an unposted row once its episode has ended:
the stage is gone, out of the condition, or carries a different key. It also
reads run state, which `TransitionRun` changes without touching the stage
stamps: a failed-stage page resolves once its run is cancelled or succeeded (a
failed run stays revivable), and a child page once its run is terminal. A
resolved row is recorded as pinged without posting.

## The quiet clock

The threshold predicate is `now.Sub(quietSince) >= threshold`. `quietSince` is
the later of a base and the newest user- or agent-kind row on the chain(s),
skipping the two stall categories.

- Failed stage: base is the stage's `EndedAt`; only its own chain is read.
- Child: base is the latest of the stage's `UpdatedAt`, the child's
  `CreatedAt`, the parent's newest `slices_integrated` row and the newest
  `UpdatedAt` of a sibling run that reached a terminal state (a freed slot).
  User- and agent-kind rows on BOTH the child's and the parent's chains
  restart it.

**Any user- or agent-kind row restarts the clock, whatever it is.** A comment, a
read-only verb that audits, or an agent row unrelated to the stall all count.
That is a defence-in-depth residual: the watch errs toward NOT paging an
operator who is visibly acting on the run.

## Residuals

- **First-enablement burst.** On the first tick after enabling, every
  qualifying failure inside Lookback (default a week) and every stalled child
  pages at once. Lower `--stall-watch-lookback` for the first boot to limit it.
- **created_at scan cut-off.** `ListRuns` orders by `created_at` DESC with no
  `updated_at` filter, so the failed-run scan stops at the first run created
  before Now-Lookback. A failed run created before the window whose stage failed
  inside it (a revived week-old run) never pages.
- **Clock domains.** The ticker's Go clock is compared with database-stamped
  stage times and application-stamped audit times. Skew shifts the deadline by
  the skew, a bounded error, as in `dispatchwatchdog`.
- **RLS.** `runs`, `stages` and `audit_entries` are FORCE row-level secured
  (migration 0057) and the ticker sets no `app.account_id`. Under a role without
  BYPASSRLS the scan under-reads tenanted runs (same as stalesweep and the ops
  monitor).
- **A lost page on a failed append.** Nothing is marked before the append
  lands, so the next tick retries it. A failed `notifyPageClass` leaves the row
  for the next status-driven `firePings`, which suppresses it if the episode has
  ended.
- **Single-instance fallback.** With an audit repository lacking
  `audit.DedupedChainAppender`, dedup is a non-atomic list-then-append, safe
  for one fishhawkd only. Postgres dedups under the run-row lock.
- **`max_parallel` override not visible.** `fishhawk_run_children`'s
  `max_parallel` is a clamp-down applied MCP-side and not persisted. A child the
  loop holds under a tighter override can page once a sibling has run past the
  threshold.
- **Campaign dispatch hold.** The scan does not read it, so a child held behind
  a campaign hotfix hold pages after the threshold.
- **Unread marker holds.** A child whose implement stage sits behind a failed or
  non-succeeded-deploy stage is not withheld. The failed stage pages on its own.

## Counterfactuals

Each control's mutation, the test that goes RED, and why the fixture isolates
it.

| Control | Mutation | RED test |
|---|---|---|
| threshold (children) | `if !stallQuietPastThreshold(...)` → `if false` | `TestScanUndispatchedChildStalls_BelowThresholdNoPage` (29m fixture otherwise eligible) |
| `slices_integrated` base | drop it from base | `_BelowThresholdNoPage` (every other clock 2h old) |
| wave order | ignore `guardDecompositionWaveOrder`'s refusal | `_WaveOrderBlockedNoPage` (the integration names the dependency, so coverage admits) |
| coverage | ignore `resolveDependentChildBase`'s refusal | `_UncoveredDependencyNoPage` (dependency succeeded, so wave order admits) |
| live waiter (child) | skip the check | `_LiveSlotWaiterNoPage` |
| cap deferral | make `inFlight >= cap` a no-op | `_CapDeferredWithSiblingsInFlightNoPage` (every clock 2h old) |
| in-flight definition | count every non-terminal sibling | `_UndispatchedSiblingsDoNotCountAsInFlight` |
| live-queued sibling (M3) | never count a waiter | `_LiveQueuedSiblingCountsAsInFlight` |
| sibling clock fold | drop the terminal-sibling `UpdatedAt` | `_SiblingSettledRestartsClock` |
| parent act fold | drop the parent chain from `quietSince` | `_ParentActRestartsClock` |
| child act fold | drop the child chain from `quietSince` | `_ChildActRestartsClock` |
| host memory hold (M2) | ignore `Held` | `_HostAdmissionHoldNoPage` |
| concurrency read error | treat it as no waiter | `_ConcurrencyReadErrorSkips` |
| episode gate | page outside pending / awaiting | `_DispatchedOrRunningNoPage` |
| per-child skip | fail the scan on one child | `_ReadErrorSkipsOnlyThatChild` |
| ticker config | each `Validate` arm made `false` | `stallwatch.TestRun_RefusesDegenerateConfig` |
| scanner isolation | drop `recover` / stop on an error | `stallwatch.TestTick_FailingScannerDoesNotStopTheOther` |
| boot refusal | skip the serve.go pre-check | `fishhawkd.TestServe_StallWatchDegenerateConfigRefusesToBoot` (boots and hangs) |

The failed-stage scanner's counterfactuals (threshold, act clock, list-then-append
dedup, episode resolution, implement-only parent exclusion, park budget, revive
pre-validate, body hygiene) are recorded in `stall_watch_test.go` and
`issuecomment/ping_test.go`.
