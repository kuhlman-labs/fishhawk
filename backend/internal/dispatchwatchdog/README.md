# backend/internal/dispatchwatchdog

Category-C watchdog for stages stuck in the `dispatched` state.

## Dispatch watchdog (category-C)

`Ticker` walks `dispatched`-state stages whose **`dispatched_at`** is past `--dispatch-watchdog-timeout` and fails them as category C ("infrastructure failure" — runner action timed out, GitHub-side dispatch failure, network partition).

- **Deadline base is `stages.dispatched_at`** (migration 0072, #2744), NOT `updated_at`. The ~15s progress heartbeat (0070) bumps `updated_at` on every write, so reading it let a wedged-but-heartbeating stage forge the watchdog's clock. `dispatched_at` is stamped only on the transition INTO `dispatched` (a transition-keyed trigger), so a heartbeat cannot advance it. The signal is read through the optional `run.DispatchLivenessLister` capability.
- **Fails closed on a missing capability.** `Run()` returns an error and the ticker does NOT start when neither an explicit `Liveness` nor a capability-carrying `Repo` is wired — falling back to `Stage.UpdatedAt` is exactly the defeatable signal this removes. `serve.go` refuses to boot (non-zero exit) with the same diagnosis when `--enable-dispatch-watchdog` is set on a RunRepo that lacks the capability.
- **Two recorded modes** (the two operator situations stop collapsing into one category-C reason): `mode=never_checked_in` (no runner heartbeat ever arrived — a truly infra-stuck stage) vs `mode=wedged_after_checkin` (heartbeats arrived, attempt-relative, then the dispatch-relative deadline elapsed anyway). The mode rides BOTH the recorded `failure_reason` (embedded as a `dispatch_watchdog[mode=<value>]:` prefix DERIVED from the mode constant, so a wording edit cannot silently decouple the reason from the classification) and the `dispatch_watchdog_elapsed` audit payload, which also carries `dispatched_at` and `last_heartbeat_at` (RFC3339Nano or null) alongside the existing `stage_id` / `timeout_seconds` / `elapsed_seconds` / `updated_at` / `transitioned_at` / `failure_category` keys.
- **Legacy nil-`dispatched_at` degrade.** A row that somehow escaped the 0072 backfill (nil `dispatched_at`) degrades to `updated_at` with a warn — a conservative fallback reproducing exactly the pre-#2744 behaviour for that one row rather than skipping or instantly failing it.
- Mirrors the SLA ticker pattern: `FailStage(stageID, FailureC, …)` plus the chained `dispatch_watchdog_elapsed` audit entry (no new audit category).
- Off by default; enable with `--enable-dispatch-watchdog`. **Sizing:** the 1h default sits at the same boundary as the runner's 3600s agent wall clock, and a healthy implement pass has been observed at 59m49s — settle the timeout before enabling.
- Slow-but-eventual fallback for the same class of failure that #243 catches faster via the workflow_run webhook.
- Closes the C-emitter half of [#158](https://github.com/kuhlman-labs/fishhawk/issues/158); the A-emitter (runner-side `agent_failed` flag in the trace bundle) is the remaining half.
