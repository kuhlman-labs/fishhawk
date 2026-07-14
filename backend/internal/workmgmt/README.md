# backend/internal/workmgmt

Work-management conventions + provider abstraction: canonical work-item model, conventions rendering (`Apply`), and the `Provider` interface with optional capabilities (`Transitioner`, `EpicChildrenQuerier`, `NumberDiscoverer`). Provider impls live in `github/` and `jira/` (see their READMEs).

## Board-state sync (#1012: run-lifecycle-driven work-item board transitions)

- **Config:** the `states` (canonical state → provider board option) + `transitions` (run-lifecycle event → canonical state) blocks in `docs/spec/work-management-v0.{md,schema.json}` + the shipped default, typed onto `workmgmt.Conventions.{States,Transitions}` with a transitions→states cross-reference semantic check.
- **Capability:** the optional `workmgmt.Transitioner` interface (`Transition(ctx, TransitionRequest) (*TransitionResult, error)`) — distinct from `Provider` so a non-boarding provider (jira) need not implement it. The `github_projects` impl in `backend/internal/workmgmt/github/provider.go` moves ONLY the Status column and honors never-fight-the-human: it advances only from a status in the request's expected-source set; an unset Status counts as Backlog.
- **Hook:** `backend/internal/server/boardsync.go::notifyBoardTransition` — best-effort (modelled on `notifyStatusUpdate`). It resolves the run's repo/issue/installation, maps the lifecycle event through `Conventions.Transitions`, derives the expected-source set from the prior lifecycle edges, dispatches the `Transitioner`, and appends a `work_item_transitioned` audit entry (every move AND skip; see `docs/issue-comment-surfaces.md`).
- **Call sites:**
  - run created → `run_started` (webhook `Dispatcher.BoardSyncer`, wired in `serve.go`, AND `handleCreateRun` in `runs.go` for local-runner / API-created issue runs, closing the #1123 webhook-exclusive gap)
  - PR opened → `pr_opened` (`pullrequest.go`)
  - run failed → `run_failed` (`trace.go::advanceAfterFailure`, only when the run reached terminal `failed`)
  - PR merged → `run_merged` (`pullrequest_review_events.go`)
- Errors log and never unwind the run.

## Issue-level `depends_on` relation + epic-children query (ADR-047 / #1437, E25.1: the campaign DAG source)

- **Model:** `workmgmt.Relations.DependsOn []string` (`model.go`) — the issue-level dependency edge a campaign derives its wave DAG from. Entries are issue refs (`#N`/`N`) among the epic's children, threaded through `fishhawk_file_issue` → `POST /v0/work-items` → `FilingRequest.Relations`.
- **Validation:** `apply.go::resolveRelations` format-validates each entry (positive `#N`/`N`). Cycle and existence checks are deferred to campaign-assembly time (E25.3) because `Apply` is pure and cycle detection needs the full DAG.
- **Persistence:** GitHub has no native depends_on relation, so `github/provider.go::File` stamps a `Depends on: #X, #Y` body marker (the single-source-of-truth `renderDependsOnMarker`/`parseDependsOnMarker` pair, idempotent via `ensureDependsOnMarker`), mirroring the `Parent epic: #N` convention.
- **Query:** the optional `workmgmt.EpicChildrenQuerier` capability (`EpicChildren(ctx, EpicChildrenRequest) (*EpicChildrenResult, error)`) — distinct from `Provider` like `Transitioner`/`NumberDiscoverer`. The `github_projects` impl resolves the epic node id, reads `githubclient.ListSubIssues` (a single `subIssues(first:100)` GraphQL page in v0), parses each child body's marker, and returns the children (ascending) plus the depends_on edges restricted to the sibling set.
- A reference to a non-child is kept OUT of `Edges` and surfaced in `EpicChildrenResult.DroppedEdges` (deterministically sorted, not silently discarded) so campaign assembly can fail closed on a dangling/mis-targeted dependency. E25.3 feeds the result into `plan.Waves`.
