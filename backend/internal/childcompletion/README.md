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

## Between-wave fan-in (#2363)

The fan-in above is the SETTLING one: it runs only once every child is terminal. A decomposition with `depends_on` also needs a fan-in at each WAVE boundary, because a dependent child must spawn against a base carrying its predecessors' commits. That re-base used to be the blocking `fishhawk_run_children` driver's job (a client-side `POST /integrate-wave` between waves), so a fan-out with no client alive never advanced past a wave boundary.

`resolveParent`'s NOT-all-terminal branch now calls the nil-safe `Sweeper.WaveIntegrate` (a `WaveIntegrator` whose serve.go adapter delegates to `orchestrator.IntegrateCompletedWave`) IMMEDIATELY BEFORE the `Dispatch` backstop. The ordering is load-bearing: topping the dispatch up first would hand a newly-unblocked child to a host-dispatch that refuses it `409 wave_not_integrated`.

`integrateCompletedWave` returns whether the tick may PROCEED to the dispatch backstop. An **error** or a **conflict** returns false and `resolveParent` short-circuits (`return nil`) rather than falling through to `DispatchChildren`: the consolidated base is not carrying its predecessors' commits, so a child topped up now is refused `409 wave_not_integrated`, and — on the error path — a dispatch attempt would mask the un-integrated state behind a spawn. A **clean** integration (or nothing to integrate, or a nil `WaveIntegrate`) returns true so a newly-unblocked wave is still topped up. `TestSweeper_WaveIntegration{Error,Conflict}SkipsDispatchBackstop` pin the short-circuit; `TestSweeper_WaveIntegrationCleanRunsDispatchBackstop` discriminates it from a blanket skip.

`IntegrateCompletedWave` attempts a merge only when a NON-TERMINAL child's slice declares dependencies that have all succeeded AND are not already covered by the newest `slices_integrated` entry (the steady-state short-circuit — see `backend/internal/orchestrator/README.md`), so the sweeper does not re-merge on every tick for the rest of the fan-out.

Failure posture — this runs on a parent MID-FAN-OUT that it must never settle:

- **error** → WARN + swallow. The parent stays parked; the next tick re-enters (merges are idempotent). It increments the SAME per-parent consecutive-error counter as the settling path for log-spam bounding (past `maxIntegrationAttempts` the line drops to debug) but deliberately NOT its give-up transition: failing a parent whose fan-out is still running is the all-terminal path's decision, and doing it here too would race that path. One observable coupling follows from sharing the counter: a parent that accumulated failing between-wave attempts reaches the all-terminal give-up sooner. That is the correct direction (the integration has been failing all along), but it is deliberate, not accidental.
- **conflict** → WARN + emit `slice_integration_conflict` (so `next_actions`/`children_status` surface it through the existing arm) and NO transition. The dependent child then stays refused at the host-dispatch `409` rather than spawning on a base missing its predecessors. Emission is **deduped per distinct conflict** (`{slice_index}/{child_run_id}`): the coverage predicate stays false for as long as the wave is blocked, so an undeduped emit would append an identical audit entry every tick forever. A DIFFERENT conflict still emits; a clean integration clears the key so a recurrence after a fix is surfaced again.
- **clean** (an integration ran) **or no-op** (nothing to integrate, `(false, nil, nil)`) → clears both the error counter and the conflict key. Still no transition. The no-op MUST clear too: a no-op after prior errors means those errors were not consecutive (leaving the shared counter set would trip the all-terminal give-up prematurely), and a no-op after a conflict — e.g. an external/manual integration resolved it — must clear the dedup key or a later recurrence of the same conflict is silently suppressed.
- **nil `WaveIntegrate`** → total no-op (pre-#2363 posture, and what every behavioral test that omits the field gets). `newChildCompletionSweeper` wires it non-nil, pinned by `cmd/fishhawkd`'s `TestNewChildCompletionSweeper_WiresDispatchBackstop`.

The conflict dedup key is ALSO cleared on the SETTLING path ([#2695](https://github.com/kuhlman-labs/fishhawk/issues/2695) item 5), not only on the between-wave clean/no-op above. The acceptance criterion is "no entry remains after settlement", so `resolveParent`'s all-terminal branch clears the key on **every** settling exit — the any-child-failed settle, the bounded-retry give-up, the slice-conflict transition, and the clean integration (the one shared `TransitionStage` covers the anyFailed / clean / nil-`Integrate` settles in one place; the give-up and slice-conflict early returns clear on their own paths). The map is keyed by parent run id, so this is bookkeeping/memory hygiene only — without it a long-lived sweeper accumulates one stale entry per ever-conflicted parent. `TestSweeper_WaveConflictClearedOnEverySettlingExit` pins all four exits.

## Bounded-retry give-up (#1243)

A deterministically-failing `IntegrateSlices` (e.g. the pre-#1243 consolidated-branch D/F conflict) would otherwise be retried every 60s tick forever, log-spamming an unfixable error.

`resolveParent` counts CONSECUTIVE non-conflict integration errors per parent (an in-memory `map[uuid.UUID]int`, mutex-guarded for `-race`; reset on a clean integration OR a slice conflict) and, on the `maxIntegrationAttempts`-th (5) failing tick, fails the parent implement stage **category-B RECOVERABLE** with a reason naming the persistent error + attempt count and emits a `slice_integration_failed` audit (system actor, payload `{parent_stage_id, attempts, error}`).

Ticks 1..`maxIntegrationAttempts`-1 leave the parent parked (one WARN per tick, bounding spam to `maxIntegrationAttempts` lines); the give-up fires ON the `maxIntegrationAttempts`-th tick.

A process restart resets the counter (acceptable — it retries `maxIntegrationAttempts` more times then gives up again, still bounding steady-state spam).
