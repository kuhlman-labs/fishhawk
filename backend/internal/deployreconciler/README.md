# backend/internal/deployreconciler

Delegating deploy executor (E23.6 / #1386, ADR-038): trigger, poll-to-terminal, and rollback for delegated deploys.

Server-side (NOT a runner subprocess): the `awaiting_deployment` park state is non-settled (`run.IsSettled` excludes it) and the `DispatchWorkflow`/`GetWorkflowRun` primitives + the App installation token are backend-only.

## Trigger

`backend/internal/server/deploy_trigger.go::triggerDeploy`, fired from `approvals.go::advanceForDecision` once a deploy gate approves `awaiting_deploy_approval → dispatched`:

- Reads the deploy stage's `executor.delegate` config and, by target, fires a github_actions `workflow_dispatch` of `workflow_ref` (carrying the `fishhawk_run_id`/`fishhawk_stage_id` correlation as inputs, then best-effort `ResolveDispatchedRun` for the run id) or POSTs to a webhook `url`.
- Records the external run handle into a `deployment_dispatched` audit entry, and parks `dispatched → running → awaiting_deployment`.
- A trigger error fails the stage category-C with a `deployment_dispatch_failed` audit — never a silent park; a nil GitHub client is the un-wired demo posture that leaves the stage at `dispatched`.

## Poll / terminal

`Ticker` (this package) mirrors the merge reconciler. Each tick:

- Lists `awaiting_deployment` deploy stages (`run.Repository.ListDeployStagesAwaitingDeployment`) and reads the `deployment_dispatched` handle.
- Polls the GHA run (re-resolving via the correlation token when the handle's run id is 0, returning INDETERMINATE rather than guessing under concurrency — binding condition 1).
- On a terminal conclusion, maps it to a `DeployOutcome` (success→succeeded, neutral→partial, failure/cancelled/…→failed) and hands off to `server.ResolveDeploymentFromPollState` (persist the `deployment` artifact, write `deployment_outcome_recorded` + the `deploy_run` trace event, transition `awaiting_deployment → succeeded`/`failed`, advance the run).

Webhook targets have no GHA run to poll, so they reach terminal via the external pipeline calling back into `POST /v0/runs/{run_id}/deployment` (`deployment.go::handleShipDeployment`, #1395).

## Rollback

`backend/internal/server/deploy_rollback.go::handleRollbackDeployment`, route `POST /v0/runs/{run_id}/deployment/rollback` — the operator-triggered rollback sub-action.

- Auth: operator bearer + `write:runs` AND `write:deploy` per ADR-038 / #1390 — a bearer missing `write:deploy` → `403 insufficient_scope`; a run-bound `mcp:run:<uuid>` token is EXEMPT from `write:deploy` and may roll back only its own run → else `cross_run_rollback`.
- Precondition: a settled deploy stage (`succeeded`/`failed`, else `409 deploy_not_settled`).
- It re-dispatches the SAME delegate down its rollback path (github_actions: `workflow_dispatch` of `workflow_ref` + the `fishhawk_rollback=true` input — re-using the same ref avoids a `workflow-v*` bump; a distinct `rollback_workflow_ref` is a deferred additive spec field; webhook: POST `{fishhawk_rollback:true}`) and writes a `deployment_rollback_initiated` audit carrying the rollback run handle **distinct** from the initial `deployment_dispatched` handle.

**Initiate-only**: the `rolled_back` outcome + `deployment_rollback_completed` are recorded when the rollback run reaches terminal, via either the same `POST /v0/runs/{run_id}/deployment` callback (`{outcome:rolled_back, rollback_action:completed}`) OR — for a github_actions rollback pipeline that does NOT call back (#1398 / #1386 binding condition 2) — the reconciler's SECOND scan.

The reconciler's `Tick` runs a distinct second pass after the forward scan:

- `ListDeployStagesRollbackPending` (deploy stages with a `deployment_rollback_initiated` audit and NO `deployment_rollback_completed` — keyed on the rollback HANDLE, not stage state, since a rolled-back deploy is already terminal).
- Reads the rollback handle → re-resolves via the correlation token **extended with `fishhawk_rollback=true`** (so it never mis-associates the forward deploy run, which echoes the same `run_id`/`stage_id`; INDETERMINATE-on-ambiguous, same as the forward scan) → polls `GetWorkflowRun`.
- On ANY terminal conclusion hands off to `server.ResolveDeploymentRollbackFromPollState`, which persists a `rolled_back` deployment artifact and writes `deployment_outcome_recorded` + `deploy_run` + `deployment_rollback_completed` (idempotent on a pre-existing `_completed`), WITHOUT transitioning the already-terminal stage or advancing the run.

The rolled_back deployment artifact is the durable carrier (`Stage.DeployOutcome` is in-memory only, E23.5).

## Wiring

Deploy audit categories (`deployment_dispatched`, `deployment_outcome_recorded`, `deployment_rollback_initiated`/`_completed`, `deploy_run`) live in `deployment.go`; both reconciler + rollback ticker goroutines wire in `backend/cmd/fishhawkd/serve.go`.
