# backend/internal/stalesweep

Reconciles stale non-terminal TOP-LEVEL runs — `pending`/`running` runs nothing has driven for longer than a threshold, which otherwise feed the liveness guards, restart blockers and run views forever (#4185). Core of the one-shot operator subcommand `fishhawkd sweep-stale-runs` (`backend/cmd/fishhawkd/sweep_stale_runs.go`; operator walk in `backend/cmd/fishhawkd/README.md`).

Decomposition children (`decomposed_from` set) are OUT of scope: they belong to `fishhawkd reconcile-orphan-children` (`backend/internal/childcancel`, #4186). **Operator order: `fishhawkd reconcile-orphan-children --apply` first, then `fishhawkd sweep-stale-runs`.**

## Contract

### `Find(ctx, runs, au, probe, opts)`

1. Pages `ListRuns{State}` for `pending` then `running` (100/page, `AccountID` empty = every tenant) and COLLECTS the full set before reading anything else (the `childcancel.FindOrphans` shape), so a later apply cannot shift an offset page.
2. Drops runs with `DecomposedFrom != nil`.
3. Reads each remaining run's stages (`ListStagesForRun`) and full audit chain (`ListForRun`), then pages its decomposition children (`ListRuns{DecomposedFrom}`, every state) and reads each child's stages and chain the same way (`readEvidence`). **Any read error fails the WHOLE scan closed** (error, no candidates): unreadable evidence must never default a run into a cancelling class.
4. Calls the live-runner probe ONCE. A probe failure (or a nil probe) does NOT fail the scan; it lands in `ProbeReport.Err`, no run is classified `live_runner`, and the caller must refuse to apply.
5. Classifies each run. **Last activity** = max(`run.updated_at`, every `stage.updated_at` — heartbeats bump it, the newest audit entry `ts`), taken over the run AND every decomposition child. `run.updated_at` alone is not trusted: many stale rows carry a bulk-touched 2026-08-24 `updated_at`. The child half exists because a decomposed parent parked at `awaiting_children` is quiet on its own rows while its children work: child progress writes the CHILD's rows and chain (the parent chain sees only settlement events, `backend/internal/childcompletion`), and a child's runner carries the child's `--run-id`. Without it such a parent would classify `abandoned`, and the cancel cascade would then cancel its busy children.

| # | Class | Matches when | Action → target | Reason |
|---|---|---|---|---|
| 1 | `fresh` | `now - last_activity < threshold` (run or any child) | `skip` | — |
| 2 | `live_runner` | the probe saw a `fishhawk-runner … --run-id <id>` process for the run or any of its decomposition children | `skip` | — |
| 3 | `stages_settled` | `orchestrator.Advance`'s own walk would complete the run (below) | `reconcile_failed` / `reconcile_cancelled` / `reconcile_succeeded` → the `completeRun` target | `stages_settled` |
| 4 | `merged` | `pr_merged`, `post_merge_observed` or `merge_observation_recorded` on the chain | `delegate_reconcile_merge` (no transition; the CLI prints the `reconcile-merge` remedy) | — |
| 5 | `pr_closed` | `pr_closed_without_merge` on the chain | `cancel` → `cancelled` | `pr_closed_without_merge` |
| 6 | `pr_unobserved` | a recorded `pull_request_url` or a `pull_request_opened` row, no observation | `skip`; `cancel` → `cancelled` only with `CancelUnobservedPR` | `stale_sweep_pr_unobserved` |
| 7 | `abandoned` | none of the above (no PR evidence) | `cancel` → `cancelled` | `stale_sweep` |

**Settled walk** (mirrors `orchestrator.Advance` + `completeRun`): walking stages in `Sequence` order, a `failed`/`cancelled` stage reached before the first `pending` one completes the run (Advance walks PAST a non-pending, non-terminal stage, so a gated stage before a failed one still completes it); with no `pending` stage and no non-terminal stage either (and at least one stage), every stage is terminal and the run completes. Target: `failed` if any stage failed, else `cancelled` if any was cancelled, else `succeeded`. **A `succeeded` target never overrides closed-unmerged or unobserved PR evidence** — such a run falls through to class 5/6, because stamping it `succeeded` would claim a change shipped that did not or may not have. A `succeeded` target with merge evidence stays `stages_settled`; a `failed`/`cancelled` target wins over any PR evidence.

The default never records `cancelled` for a change that may have shipped (`pr_unobserved`) without an explicit operator choice: either record the observation (`fishhawk_record_merge_observation`) and `reconcile-merge`, or pass `--cancel-unobserved-pr`.

### `Apply(ctx, runs, au, candidates, opts)`

For every candidate whose action transitions (`reconcile_*`, `cancel`), in order:

1. **Re-read** the run (`GetRun`). Terminal now → `skipped_terminal`, no transition, no row (a same-state `TransitionRun` would otherwise succeed and write a spurious row). Then re-read its evidence exactly as `Find` did (`readEvidence`: stages, chain, children's): `run.updated_at` newer than the snapshot, any activity signal newer than the scanned last activity (a stage heartbeat or audit row that landed after the scan), or the newest signal now inside the threshold → `skipped_changed`. A read error → `failed`.
2. **Transition path.** `pending → succeeded` is not a legal edge (`runTransitions`), so that class walks `[running, succeeded]`; every other target is one step. Each step goes through `TransitionRun`. `run.InvalidTransitionError` → `skipped_terminal`, no row; any other error → `failed`, `Err` set, no row. A failure after the first of two steps leaves the run `running`; a re-run classifies it again (still settled) and finishes it via `[succeeded]` — the path that LANDED is what the result and row record.
3. **Cancelled target only:** every non-terminal scanned stage is CAS-cancelled (`TransitionStageFrom(stage, scannedState, cancelled)`), so the merge reconciler, SLA ticker and reaction poller — whose `awaiting_approval` scans do not filter on run state — stop walking the swept run. A refused CAS (`StageStateChangedError`, or any error) leaves that stage untouched and OUT of the record: a refused CAS is a missing entry, never a false one. Then `childcancel.CascadeFromParent(…, Source)` cancels any non-terminal decomposition children (`cancel_source: stale_sweep`), so the sweep creates no new orphans. It reaches only children that are themselves stale and runner-less at scan time: a fresh or live child keeps its parent `fresh`/`live_runner` (`Find` step 5), and a child that turns fresh after the scan trips the re-read. A cascade list or child error is recorded in `Err` and does not undo the run cancel. (`POST /v0/runs/{id}/cancel` leaves stages parked; this matches the PR-close path instead, whose cancelled review stage `ReviveRunOnReopen` re-parks.)
4. **ONE system-actor `stale_run_swept` row** on the run's chain, AFTER every transition (transition first, then audit, as `merge_supersede.go` orders it): `{source, class, reason, from_state, to_state, transition_path, threshold_days, last_activity_at, pr_evidence, pull_request_url|null, stages_cancelled:[{stage_id, stage_type, from_state}], children_cancelled}`. An append error is returned in `Err` with the outcome still `transitioned`.

**Dedup:** per run, key `source = stale_sweep`. On a store implementing `audit.DedupedChainAppender` (the Postgres audit repo) the scan runs inside the append transaction under the run-row lock (`DedupeSpec{PayloadKey: "source"}`); a `*DedupedDuplicateError` is benign (`Appended: false`). Otherwise a NON-atomic list-then-append fallback (a list error proceeds without the guard), mirroring `childcancel`. Consequence: a run swept, revived and swept again transitions again but gets no second row.

### Live-runner probe (`probe.go`)

`PSProbe` runs `ps -axww -o pid=,args=` (the `scripts/dev _scan_live_runs` invocation; `-ww` stops BSD `ps` truncating the `--run-id` pair away). `ParseRunnerProcesses` matches exactly as `scripts/dev _parse_live_runs` and `runner/cmd/fishhawk-runner/lockholder.go` do: numeric pid; argv[0]'s basename, stripped from its first `.`, must EQUAL `fishhawk-runner` (a process merely MENTIONING it in a later argument is ignored); the run id is the token after the FIRST token exactly equal to `--run-id`. **The single-token `--run-id=<id>` form is UNATTRIBUTED** (neither reference parser recognises it), as is a missing value or a non-UUID one; any unattributed runner, or a probe error (no `ps` — the fishhawkd runtime image is distroless — or a non-zero exit), makes the CLI refuse `--apply` before any write. A dry run still classifies and prints a warning.

## Role requirement (RLS)

`runs` AND `audit_entries` are both FORCE row-level secured (migration 0057), and the sweep sets no `app.account_id`. Run it as a role that is superuser or `BYPASSRLS` (the role that runs migrations). A role without it **silently under-sweeps candidate runs** (tenant-scoped `runs` rows are invisible to the `ListRuns` scan) **and misses audit evidence** (an invisible `pr_merged` / `pr_closed_without_merge` / heartbeat row could misclassify a run as `abandoned`). `reconcile-orphan-children` has the same exposure.

## Root cause of the "pending with every stage succeeded" class

The three such runs on the dogfood host (eae1778f, 58ad153e, 4152c113) were created 2026-05-07 19:26 / 20:37 / 21:42 UTC — before #227 (commit `f1e4ad6c`, 2026-05-07 23:21:42 UTC), which added `Advance`'s `pending → running` walk (`backend/internal/orchestrator/orchestrator.go`, `if r.State == run.StatePending` before the stage walk). Both `completeRun` call sites (the failed/cancelled-stage return and the every-stage-terminal return in the same function) now sit AFTER that walk, so a run cannot reach `completeRun` while `pending` and the path is not reachable today. The sweep's `stages_settled` class reconciles the historical rows.

## Residuals

- A crash between a transition and its append leaves a terminal run with no `stale_run_swept` row; a re-run cannot find it (the scan reads only `pending`/`running`). The CLI exits 1 naming the run when the append fails in-process (the same residual `childcancel` documents).
- The re-read narrows, but does not close, a race with concurrent operator activity between the re-read and the transition. The live-runner probe runs once per scan; `Apply` does not re-probe.
- The fallback dedup leg is non-atomic on a store without `DedupedChainAppender`; the Postgres repo has the atomic leg.
- The probe sees only THIS host's runners. Run the sweep on the host that runs the fishhawk-runners (it relies on each runner carrying `--run-id <uuid>` on argv, as `fishhawk_run_stage` / `fishhawk_run_children` spawns do).

## Tests

`stalesweep_test.go` (in-memory store over `run.BaseFake`, copy-on-read, same-state-success `TransitionRun`, CAS-refusing `TransitionStageFrom`, audit store with and without `DedupedChainAppender`): each last-activity leg isolated, live-runner skip, child exclusion, a stale parent kept by a fresh or live child (each child signal isolated), the settled walk + the succeeded-vs-PR-evidence fall-through, merged / pr_closed / pr_unobserved, fail-closed reads, paging past 100, every `Apply` branch (re-read terminal / changed — each run, stage, audit and child leg isolated / error, transition error and race, append error, both dedup legs, parked-stage cancel with a refused CAS, cascade + cascade errors, the two-step path). `probe_test.go`: the parser fixture table (including `--run-id=`) and the exec seam. End to end over real Postgres: `backend/cmd/fishhawkd/sweep_stale_runs_test.go`.
