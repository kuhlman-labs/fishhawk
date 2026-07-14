# backend/internal/childcompletion

Child-completion sweeper: resolves a decomposed parent's `awaiting_children` stage once its child runs settle. #455 / ADR-025 D4.

## Sweeper

`Sweeper.Run` polls `ListStagesAwaitingChildren` every `--child-completion-interval` (default 60s), groups stages by parent, fetches each parent's decomposed children via `ListRuns(DecomposedFrom=parent)`, and transitions the parent stage to `succeeded` once every child run reaches a terminal state successfully (or to `failed-C` when any child failed). Emits a `children_settled` audit entry on each resolution.

Off by default; enable with `--enable-child-completion-sweeper` (`FISHHAWKD_ENABLE_CHILD_COMPLETION_SWEEPER=true`).

60s is the upper bound on parent latency after the last child terminates — a direct-callback hook from the child's terminal transition is a follow-up that would drop happy-path latency to milliseconds.

## Park-on-recoverable (#698 / #1081)

`resolveParent` applies the same `run.ImplementFailureRecoverable` classification as the orchestrator hook — when every failed child is recoverable in decomposition (A/C/D-timeout, or category B via the in-place recover path) it leaves the parent parked rather than resolving to `failed-C`.

The sweeper does NOT emit `parent_awaiting_redrive` and drops its park log to debug, so an indefinitely-parked parent does not spam the audit chain or logs every tick; discoverability rests on the orchestrator hook's one-time entry (see `backend/internal/orchestrator/README.md`).

## Fan-in (ADR-041 / E24.2 / #1142)

On the all-succeeded path, `resolveParent` calls the nil-safe `Sweeper.Integrate` (an `Integrator` whose serve.go adapter delegates to `orchestrator.IntegrateSlices`) BEFORE stamping the stage succeeded:

- A clean fan-in falls through to `succeeded` + `Advance`.
- A `*SliceConflict` fails the stage category-B + emits `slice_integration_conflict` + does NOT `Advance`.
- A non-conflict error leaves the parent parked (the next tick re-enters; merges are idempotent).
- A nil `Integrate` (dev posture / pre-#1142) skips integration entirely, preserving the prior resolve behavior.

## Bounded-retry give-up (#1243)

A deterministically-failing `IntegrateSlices` (e.g. the pre-#1243 consolidated-branch D/F conflict) would otherwise be retried every 60s tick forever, log-spamming an unfixable error.

`resolveParent` counts CONSECUTIVE non-conflict integration errors per parent (an in-memory `map[uuid.UUID]int`, mutex-guarded for `-race`; reset on a clean integration OR a slice conflict) and, on the `maxIntegrationAttempts`-th (5) failing tick, fails the parent implement stage **category-B RECOVERABLE** with a reason naming the persistent error + attempt count and emits a `slice_integration_failed` audit (system actor, payload `{parent_stage_id, attempts, error}`).

Ticks 1..`maxIntegrationAttempts`-1 leave the parent parked (one WARN per tick, bounding spam to `maxIntegrationAttempts` lines); the give-up fires ON the `maxIntegrationAttempts`-th tick.

A process restart resets the counter (acceptable — it retries `maxIntegrationAttempts` more times then gives up again, still bounding steady-state spam).
