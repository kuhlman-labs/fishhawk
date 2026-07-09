-- Run / stage queries consumed by the postgres adapter for the
-- run.Repository interface (E3.3 / #43). sqlc generates typed Go
-- into ./db per the config in /backend/sqlc.yaml.

-- name: CreateRun :one
INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, trigger_ref, state, installation_id, idempotency_key, parent_run_id, required_checks_snapshot, workflow_spec, retry_attempt, max_retries_snapshot, runner_kind, issue_context, decomposed_from, drive, slice_index, upstream_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: GetRunByIdempotencyKey :one
-- Used by POST /v0/runs to resolve an Idempotency-Key header to
-- a previously-created run. Active scope is (repo, idempotency_key);
-- the partial unique index covers this lookup with no full scan.
SELECT * FROM runs
 WHERE repo = $1
   AND idempotency_key = $2;

-- name: ListRuns :many
-- Empty string / nil in any filter means "no constraint." created_at
-- DESC + id DESC tiebreak so paginations are stable across concurrent
-- inserts at the same created_at microsecond.
--
-- pull_request_url and trigger_ref are nullable filters: pass NULL to
-- skip; pass a value to match exactly. They're indexed (partial,
-- WHERE NOT NULL) so an equality match on either is cheap. Used by
-- the threaded-runs view (#216) to render "every run on this PR."
SELECT * FROM runs
 WHERE (sqlc.arg('repo')::text = '' OR repo = sqlc.arg('repo'))
   AND (sqlc.arg('workflow_id')::text = '' OR workflow_id = sqlc.arg('workflow_id'))
   AND (sqlc.arg('state')::text = '' OR state = sqlc.arg('state'))
   AND (sqlc.narg('pull_request_url')::text IS NULL OR pull_request_url = sqlc.narg('pull_request_url'))
   AND (sqlc.narg('trigger_ref')::text IS NULL OR trigger_ref = sqlc.narg('trigger_ref'))
   AND (sqlc.narg('runner_kind')::text IS NULL OR runner_kind = sqlc.narg('runner_kind'))
   AND (sqlc.narg('decomposed_from')::uuid IS NULL OR decomposed_from = sqlc.narg('decomposed_from'))
   AND (sqlc.narg('parent_run_id')::uuid IS NULL OR parent_run_id = sqlc.narg('parent_run_id'))
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: LockRunForUpdate :one
SELECT * FROM runs WHERE id = $1 FOR UPDATE;

-- name: UpdateRunState :one
UPDATE runs
   SET state = $2
 WHERE id = $1
RETURNING *;

-- name: UpdateRunnerKind :one
-- Locks the observed execution channel onto the run (#1346 / ADR-045):
-- sets runner_kind to the runner's self-reported value and flips
-- runner_kind_resolved=true so the create-time hint is corrected exactly
-- once. ResolveRunnerKind calls this inside a FOR UPDATE transaction only
-- when the run is not yet resolved; a later disagreeing report takes the
-- mismatch branch and never reaches this UPDATE.
UPDATE runs
   SET runner_kind = $2,
       runner_kind_resolved = true
 WHERE id = $1
RETURNING *;

-- name: SetRunPullRequestURL :one
-- Backfills the implement-stage PR URL onto the run row when the
-- pull_request artifact lands (#216). Idempotent: a re-upload with
-- the same URL is a no-op the trigger keeps as a no-op against
-- updated_at (assignment of identical value).
UPDATE runs
   SET pull_request_url = $2
 WHERE id = $1
RETURNING *;

-- name: AddRunCost :one
-- Accumulates the estimated per-run cost rollup (#649). delta_usd is
-- the pricing-derived cost of one bundle's model usage; resolved_model
-- pins the agent model id (last-write-wins, skipped when empty so a
-- model-less bundle doesn't clobber a prior pin). Idempotency is NOT
-- claimed — each bundle receipt adds its own delta; the caller
-- (trace handler) records exactly once per bundle, keyed to the
-- cost_recorded audit entry that is the canonical per-invocation row.
UPDATE runs
   SET cost_usd_total = cost_usd_total + sqlc.arg('delta_usd'),
       resolved_model = CASE
           WHEN sqlc.arg('resolved_model')::text <> '' THEN sqlc.arg('resolved_model')::text
           ELSE resolved_model
       END
 WHERE id = sqlc.arg('id')
RETURNING *;

-- name: SumWorkflowCostInRange :one
-- Sums the per-run estimated cost rollup across every run of one
-- workflow in a repo whose created_at falls in the half-open calendar
-- period [from, to) (ADR-030 advisory budgets, #688). COALESCE returns
-- 0 when no runs match so the caller never special-cases an empty
-- period. created_at (not updated_at) buckets a run into the period it
-- was admitted under, matching budget.PeriodRange's period semantics.
SELECT COALESCE(SUM(cost_usd_total), 0)::float8 AS total_usd
  FROM runs
 WHERE repo = $1
   AND workflow_id = $2
   AND created_at >= $3
   AND created_at < $4;

-- name: CreateStage :one
INSERT INTO stages (
    id, run_id, sequence, stage_type, executor_kind, executor_ref, state,
    gate_sla, requires_approval,
    gate_type, gate_approvers
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetStage :one
SELECT * FROM stages WHERE id = $1;

-- name: ListStagesForRun :many
SELECT * FROM stages WHERE run_id = $1 ORDER BY sequence ASC;

-- name: LockStageForUpdate :one
SELECT * FROM stages WHERE id = $1 FOR UPDATE;

-- name: ListStagesAwaitingApproval :many
-- The SLA ticker's candidate listing for gate timeout (#1390 broadened it
-- to the deploy gate). Matches stages parked at EITHER the generic
-- awaiting_approval gate OR the deploy pre-execution gate
-- (awaiting_deploy_approval), both filtered to a non-null gate_sla so the
-- ticker doesn't pay for SLA parsing on rows where it isn't applicable.
-- Deploy stages already carry gate_sla and awaiting_deploy_approval->failed
-- (category D) is already a legal transition, so the broadening needs no new
-- transition or row field. Ordered by updated_at ASC: the oldest entry is the
-- most likely to be past SLA, so the ticker can early-exit if the first row
-- hasn't elapsed (when the parsed durations are uniform).
--
-- Consumers: the SLA ticker (backend/internal/sla) AND the reaction poller
-- (backend/internal/reactionpoller). The poller is unaffected by the deploy
-- broadening because it skips any stage whose Type != plan, so the newly
-- included deploy rows are filtered out before any GitHub call (#1390 binding
-- condition 1).
SELECT * FROM stages
 WHERE state IN ('awaiting_approval', 'awaiting_deploy_approval')
   AND gate_sla IS NOT NULL
 ORDER BY updated_at ASC;

-- name: ListReviewStagesAwaitingApproval :many
-- The merge reconciler's candidate listing — every review stage parked
-- in awaiting_approval, SLA-independent BY DESIGN. Unlike the adjacent
-- ListStagesAwaitingApproval (which the SLA ticker keeps using with its
-- `gate_sla IS NOT NULL` filter), this query must NOT filter on gate_sla:
-- the feature_change review gate has no sla, so an SLA filter would hide
-- every feature_change merge from the reconciler and park those runs at
-- review awaiting_approval forever (#725). Ordered updated_at ASC.
SELECT * FROM stages
 WHERE state = 'awaiting_approval'
   AND stage_type = 'review'
 ORDER BY updated_at ASC;

-- name: ListDeployStagesAwaitingDeployment :many
-- The deploy reconciler's candidate listing (#1386 / E23.6) — every deploy
-- stage parked in awaiting_deployment, polled to a terminal outcome against
-- the external pipeline's GitHub Actions run. Mirrors
-- ListReviewStagesAwaitingApproval's shape for the merge reconciler. Ordered
-- updated_at ASC so the oldest parked deploy is reconciled first.
SELECT * FROM stages
 WHERE state = 'awaiting_deployment'
   AND stage_type = 'deploy'
 ORDER BY updated_at ASC;

-- name: ListDeployStagesRollbackPending :many
-- The deploy reconciler's ROLLBACK candidate listing (#1398 / E23.6, #1386
-- binding condition 2) — every deploy stage with a deployment_rollback_initiated
-- audit entry that has NO matching deployment_rollback_completed entry. Keyed on
-- the rollback HANDLE (audit), not stage state: a rolled-back deploy stage is
-- already terminal (succeeded/failed), so it would never appear in
-- ListDeployStagesAwaitingDeployment. The reconciler polls each candidate's
-- github_actions rollback run to terminal and records rolled_back +
-- deployment_rollback_completed when the external pipeline never calls back.
-- Ordered updated_at ASC so the oldest pending rollback is reconciled first.
SELECT s.* FROM stages s
 WHERE s.stage_type = 'deploy'
   AND EXISTS (
     SELECT 1 FROM audit_entries ai
      WHERE ai.stage_id = s.id
        AND ai.category = 'deployment_rollback_initiated')
   AND NOT EXISTS (
     SELECT 1 FROM audit_entries ac
      WHERE ac.stage_id = s.id
        AND ac.category = 'deployment_rollback_completed')
 ORDER BY s.updated_at ASC;

-- name: ListStagesDispatched :many
-- Used by the dispatch watchdog (E8.4) to find stages stuck at
-- 'dispatched' past a configurable timeout. Ordered by updated_at
-- ASC so the oldest stuck stage is processed first; lets the
-- watchdog early-exit once it sees one that's still within the
-- window.
SELECT * FROM stages
 WHERE state = 'dispatched'
 ORDER BY updated_at ASC;

-- name: ListStagesAwaitingChildren :many
-- Used by the child-completion sweeper (#455) to find parent
-- implement stages whose decomposed child runs may have reached
-- terminal states. Ordered by updated_at ASC.
SELECT * FROM stages
 WHERE state = 'awaiting_children'
 ORDER BY updated_at ASC;

-- name: UpdateStageState :one
UPDATE stages
   SET state            = $2,
       started_at       = COALESCE(started_at, $3),
       ended_at         = $4,
       failure_category = $5,
       failure_reason   = $6
 WHERE id = $1
RETURNING *;

-- name: ParkScopeCompleteness :one
-- Atomically sets the stage's state and its scope_completeness_park
-- payload (#1231). Used by ParkScopeCompletenessAndAppend inside a
-- transaction that first locks the row and validates the running →
-- awaiting_scope_decision transition; the JSONB payload pins the held
-- commit the runner already pushed to the run branch so the operator's
-- exempt decision can open the PR from it with no agent re-run.
UPDATE stages
   SET state                   = $2,
       scope_completeness_park = $3
 WHERE id = $1
RETURNING *;

-- name: RetryStageState :one
-- Clears failure metadata + ended_at and increments self_retry_count
-- atomically. Used by the retry handler's explicit out-of-terminal
-- transition path so retry_ordinal is always consistent.
UPDATE stages
   SET state            = $2,
       failure_category = NULL,
       failure_reason   = NULL,
       ended_at         = NULL,
       self_retry_count = self_retry_count + 1
 WHERE id = $1
RETURNING *;
