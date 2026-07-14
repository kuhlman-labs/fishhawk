# backend/internal/dispatchwatchdog

Category-C watchdog for stages stuck in the `dispatched` state.

## Dispatch watchdog (category-C)

`Ticker` walks `dispatched`-state stages whose `UpdatedAt` is past `--dispatch-watchdog-timeout` and fails them as category C ("infrastructure failure" — runner action timed out, GitHub-side dispatch failure, network partition).

- Mirrors the SLA ticker pattern: `FailStage(stageID, FailureC, …)` plus a chained `dispatch_watchdog_elapsed` audit entry.
- Off by default; enable with `--enable-dispatch-watchdog`.
- Default timeout 1h covers GitHub Actions dispatch + queue + first checkin.
- Slow-but-eventual fallback for the same class of failure that #243 catches faster via the workflow_run webhook.
- Closes the C-emitter half of [#158](https://github.com/kuhlman-labs/fishhawk/issues/158); the A-emitter (runner-side `agent_failed` flag in the trace bundle) is the remaining half.
