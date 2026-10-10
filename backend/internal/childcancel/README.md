# backend/internal/childcancel

Cancels the non-terminal decomposition children of a cancelled (or otherwise terminal) parent run (#4186). One core, two consumers: the server's run-cancel sinks (`CascadeFromParent`, wired in `backend/internal/server/decomposition_cancel_cascade.go`) and the one-shot backfill `fishhawkd reconcile-orphan-children` (`FindOrphans` + `Reconcile`, `backend/cmd/fishhawkd/reconcile_children.go`).

## Contract

- **`CancelChild(ctx, runs, au, child, parentID, parentState, reason, source)`** — one child:
  1. A child whose snapshot `State` is terminal → `skipped_terminal`, NO `TransitionRun`, no row. The skip must precede the transition: `postgresRepo.TransitionRun` returns success for a same-state call, so an already-cancelled child would otherwise "transition" and get a spurious row.
  2. Stages are read best-effort to find the implement stage. `LiveStage` = it is `dispatched` or `running` (a spawn attempt exists). A read error records a null stage id and `""` state; it never aborts the cancel.
  3. `TransitionRun(child, cancelled)`. `run.InvalidTransitionError` (the child raced to `succeeded`/`failed`) → `skipped_terminal`, no row. Any other error → `failed`, `Err` set, no row.
  4. ONE system-actor `decomposition_child_cancelled` row on the CHILD's chain: `{parent_run_id, parent_state, reason, cancel_source, from_state, implement_stage_id, implement_stage_state, live_stage}`. An append error is returned in `Err` with the outcome still `cancelled` — the state change landed.
- **Live runner: cancel, not refuse.** A `live_stage` child's run is cancelled exactly as `POST /v0/runs/{child}/cancel` does; its stage row and runner are untouched (the runner finishes, `Advance` no-ops on the terminal run, host-dispatch refuses a further spawn, #4035). The row's `live_stage: true` keeps it visible.
- **Dedup.** Per (child chain, `parent_run_id`). On a store implementing `audit.DedupedChainAppender` (the Postgres audit repo) the scan runs inside the append transaction under the run-row lock (`DedupeSpec{StageID: nil, PayloadKey: "parent_run_id"}`); a `*DedupedDuplicateError` is benign (`Appended: false`, no error). Otherwise a NON-atomic list-then-append fallback (a list error proceeds without the guard) — the same residual `server.appendRetirementDropOnce` documents. Two cancel sinks racing on one parent therefore collapse to one row per child.
- **`CascadeFromParent(ctx, runs, au, parent, source)`** pages `ListRuns{DecomposedFrom}` (100 per page) to exhaustion BEFORE touching any child, then calls `CancelChild` with `reason: parent_cancelled`. A list error returns with no child touched.
- **`FindOrphans(ctx, runs)`** pages `ListRuns{State}` for `pending` and `running` to exhaustion and COLLECTS the full set before resolving parents (so a later apply cannot shift an offset page), keeps `DecomposedFrom != nil`, reads each parent once, and keeps only `cancelled` / `succeeded` parents. A `failed` parent is excluded because `ReviveRun` can re-admit it. A parent read error fails the scan closed. `AccountID` is empty: every tenant.
- **`Reconcile(ctx, runs, au, orphans)`** calls `CancelChild` with `reason: parent_terminal`, `cancel_source: orphan_backfill`.

## Downstream effect

The child-completion sweeper is run-state-blind: once every child of a cancelled parent is terminal, its next tick settles the parent's `awaiting_children` implement stage `failed`-C with one `children_settled` row (`backend/internal/childcompletion/README.md`).

## Tests

`childcancel_test.go` (in-memory store over `run.BaseFake`, copy-on-read like Postgres): every `CancelChild` branch, both dedup legs, the parent-state filter, fail-closed parent read and list, paging past 100. `childcancel_pg_test.go` (pgtest): the real state machine + chained audit + `DedupedChainAppender`, re-reading child rows and verifying each child's hash chain, including a stale-snapshot replay the deduped append rejects.
