package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// pgxQueries is the minimum surface used by both *pgxpool.Pool and
// pgx.Tx, satisfied by both via their respective Begin/Acquire APIs.
// Keeping the adapter agnostic here lets the same query code run
// inside or outside a transaction.
type pgxQueries interface {
	rundb.DBTX
}

// postgresRepo is the production Repository implementation. State
// transitions are wrapped in a SERIALIZABLE-eligible transaction
// with SELECT … FOR UPDATE to prevent two concurrent transitions
// from observing the same prior state.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of the pool and is responsible for Close.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

// --- Run methods ---

func (r *postgresRepo) CreateRun(ctx context.Context, p CreateRunParams) (*Run, error) {
	q := rundb.New(r.pool)

	var snapshotBytes []byte
	if p.RequiredChecksSnapshot != nil {
		b, err := json.Marshal(p.RequiredChecksSnapshot)
		if err != nil {
			return nil, fmt.Errorf("marshal required_checks_snapshot: %w", err)
		}
		snapshotBytes = b
	}
	var issueContextBytes []byte
	if p.IssueContext != nil {
		b, err := json.Marshal(p.IssueContext)
		if err != nil {
			return nil, fmt.Errorf("marshal issue_context: %w", err)
		}
		issueContextBytes = b
	}

	// The migration's column-level DEFAULT 1 only applies when the
	// column is omitted from INSERT — since sqlc lists it in every
	// generated INSERT, a zero in CreateRunParams would persist as 0
	// and trip the SPA's "Retry 0/0" rendering. Promote 0 → 1 here so
	// callers that don't set the field still get the sane default.
	maxRetries := p.MaxRetriesSnapshot
	if maxRetries <= 0 {
		maxRetries = 1
	}
	// runner_kind: empty input → migration default `github_actions`.
	// Same pattern as max_retries: sqlc emits the column on every
	// INSERT, so we substitute the default at the repo layer
	// instead of relying on the column default (which only fires
	// when the INSERT omits the column).
	runnerKind := p.RunnerKind
	if runnerKind == "" {
		runnerKind = RunnerKindGitHubActions
	}
	row, err := q.CreateRun(ctx, rundb.CreateRunParams{
		ID:                     uuid.New(),
		Repo:                   p.Repo,
		WorkflowID:             p.WorkflowID,
		WorkflowSha:            p.WorkflowSHA,
		TriggerSource:          string(p.TriggerSource),
		TriggerRef:             p.TriggerRef,
		State:                  string(StatePending),
		InstallationID:         p.InstallationID,
		IdempotencyKey:         p.IdempotencyKey,
		ParentRunID:            p.ParentRunID,
		RequiredChecksSnapshot: snapshotBytes,
		WorkflowSpec:           p.WorkflowSpec,
		RetryAttempt:           int32(p.RetryAttempt),
		MaxRetriesSnapshot:     int32(maxRetries),
		RunnerKind:             runnerKind,
		IssueContext:           issueContextBytes,
		DecomposedFrom:         p.DecomposedFrom,
	})
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	return rowToRun(row), nil
}

func (r *postgresRepo) GetRunByIdempotencyKey(ctx context.Context, repo, key string) (*Run, error) {
	q := rundb.New(r.pool)
	row, err := q.GetRunByIdempotencyKey(ctx, rundb.GetRunByIdempotencyKeyParams{
		Repo:           repo,
		IdempotencyKey: &key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run by idempotency_key: %w", err)
	}
	return rowToRun(row), nil
}

func (r *postgresRepo) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	q := rundb.New(r.pool)
	row, err := q.GetRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return rowToRun(row), nil
}

func (r *postgresRepo) ListRuns(ctx context.Context, f ListRunsFilter) ([]*Run, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("list runs: limit must be > 0")
	}
	if f.Offset < 0 {
		return nil, fmt.Errorf("list runs: offset must be >= 0")
	}
	q := rundb.New(r.pool)
	rows, err := q.ListRuns(ctx, rundb.ListRunsParams{
		Repo:           f.Repo,
		WorkflowID:     f.WorkflowID,
		State:          f.State,
		PullRequestUrl: f.PullRequestURL,
		TriggerRef:     f.TriggerRef,
		RunnerKind:     f.RunnerKind,
		DecomposedFrom: f.DecomposedFrom,
		Lim:            int32(f.Limit),
		Off:            int32(f.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	out := make([]*Run, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToRun(row))
	}
	return out, nil
}

func (r *postgresRepo) TransitionRun(ctx context.Context, id uuid.UUID, to State) (*Run, error) {
	var result *Run
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := rundb.New(tx)
		current, err := q.LockRunForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock run: %w", err)
		}
		from := State(current.State)
		if from == to {
			result = rowToRun(current)
			return nil
		}
		if !ValidRunTransition(from, to) {
			return InvalidTransitionError{Kind: "run", From: string(from), To: string(to)}
		}
		updated, err := q.UpdateRunState(ctx, rundb.UpdateRunStateParams{
			ID:    id,
			State: string(to),
		})
		if err != nil {
			return fmt.Errorf("update run state: %w", err)
		}
		result = rowToRun(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- Stage methods ---

func (r *postgresRepo) SetRunPullRequestURL(ctx context.Context, id uuid.UUID, url string) (*Run, error) {
	if url == "" {
		return nil, fmt.Errorf("set run pull_request_url: url required")
	}
	q := rundb.New(r.pool)
	row, err := q.SetRunPullRequestURL(ctx, rundb.SetRunPullRequestURLParams{
		ID:             id,
		PullRequestUrl: &url,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set run pull_request_url: %w", err)
	}
	return rowToRun(row), nil
}

// AddRunCost accumulates the estimated per-run cost rollup (#649) and
// pins the resolved model id. deltaUSD is added to the running total;
// resolvedModel is last-write-wins, skipped when empty so a model-less
// bundle doesn't clobber a prior pin. Returns ErrNotFound when the run
// doesn't exist.
//
// Not part of the run.Repository interface: the trace handler consumes
// it through an optional capability assertion (best-effort, like the
// rest of that handler), so test fakes that don't roll cost need no
// stub.
func (r *postgresRepo) AddRunCost(ctx context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*Run, error) {
	q := rundb.New(r.pool)
	row, err := q.AddRunCost(ctx, rundb.AddRunCostParams{
		ID:            id,
		DeltaUsd:      deltaUSD,
		ResolvedModel: resolvedModel,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("add run cost: %w", err)
	}
	return rowToRun(row), nil
}

// SumWorkflowCostInRange sums runs.cost_usd_total across every run of
// one workflow in a repo whose created_at falls in the half-open
// calendar period [from, to) (ADR-030 advisory budgets, #688). The
// trace handler calls this to total a workflow's period spend before
// evaluating it against an advisory budget ceiling. Returns 0 (not an
// error) when no runs match the window.
//
// Like AddRunCost, NOT part of the run.Repository interface: the trace
// handler consumes it through an optional capability assertion
// (best-effort), so test fakes that don't sum cost need no stub.
func (r *postgresRepo) SumWorkflowCostInRange(ctx context.Context, repo, workflowID string, from, to time.Time) (float64, error) {
	q := rundb.New(r.pool)
	total, err := q.SumWorkflowCostInRange(ctx, rundb.SumWorkflowCostInRangeParams{
		Repo:        repo,
		WorkflowID:  workflowID,
		CreatedAt:   pgtype.Timestamptz{Time: from, Valid: true},
		CreatedAt_2: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("sum workflow cost in range: %w", err)
	}
	return total, nil
}

func (r *postgresRepo) CreateStage(ctx context.Context, p CreateStageParams) (*Stage, error) {
	q := rundb.New(r.pool)

	var gateType *string
	var approversBytes []byte
	if p.Gate != nil {
		k := string(p.Gate.Kind)
		gateType = &k
		if p.Gate.Approvers != nil {
			b, err := json.Marshal(p.Gate.Approvers)
			if err != nil {
				return nil, fmt.Errorf("marshal gate approvers: %w", err)
			}
			approversBytes = b
		}
	}

	row, err := q.CreateStage(ctx, rundb.CreateStageParams{
		ID:               uuid.New(),
		RunID:            p.RunID,
		Sequence:         int32(p.Sequence),
		StageType:        string(p.Type),
		ExecutorKind:     string(p.ExecutorKind),
		ExecutorRef:      p.ExecutorRef,
		State:            string(StageStatePending),
		GateSla:          p.GateSLA,
		RequiresApproval: p.RequiresApproval,
		GateType:         gateType,
		GateApprovers:    approversBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("create stage: %w", err)
	}
	return rowToStage(row), nil
}

func (r *postgresRepo) GetStage(ctx context.Context, id uuid.UUID) (*Stage, error) {
	q := rundb.New(r.pool)
	row, err := q.GetStage(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get stage: %w", err)
	}
	return rowToStage(row), nil
}

func (r *postgresRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*Stage, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages: %w", err)
	}
	out := make([]*Stage, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToStage(row))
	}
	return out, nil
}

func (r *postgresRepo) ListStagesAwaitingApproval(ctx context.Context) ([]*Stage, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListStagesAwaitingApproval(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stages awaiting approval: %w", err)
	}
	out := make([]*Stage, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToStage(row))
	}
	return out, nil
}

func (r *postgresRepo) ListStagesDispatched(ctx context.Context) ([]*Stage, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListStagesDispatched(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stages dispatched: %w", err)
	}
	out := make([]*Stage, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToStage(row))
	}
	return out, nil
}

func (r *postgresRepo) ListStagesAwaitingChildren(ctx context.Context) ([]*Stage, error) {
	q := rundb.New(r.pool)
	rows, err := q.ListStagesAwaitingChildren(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stages awaiting children: %w", err)
	}
	out := make([]*Stage, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToStage(row))
	}
	return out, nil
}

func (r *postgresRepo) TransitionStage(ctx context.Context, id uuid.UUID, to StageState, completion *StageCompletion) (*Stage, error) {
	if to == StageStateFailed && (completion == nil || completion.FailureCategory == nil) {
		return nil, errors.New("transition to failed requires StageCompletion with FailureCategory")
	}
	if to != StageStateFailed && completion != nil && completion.FailureCategory != nil {
		return nil, errors.New("FailureCategory only valid when transitioning to failed")
	}

	now := time.Now().UTC()

	var result *Stage
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := rundb.New(tx)
		current, err := q.LockStageForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock stage: %w", err)
		}
		from := StageState(current.State)
		if from == to {
			result = rowToStage(current)
			return nil
		}
		if !ValidStageTransition(from, to) {
			return InvalidTransitionError{Kind: "stage", From: string(from), To: string(to)}
		}

		params := rundb.UpdateStageStateParams{
			ID:    id,
			State: string(to),
		}
		// Stamp started_at the first time we leave Pending/Dispatched.
		if to == StageStateRunning && !current.StartedAt.Valid {
			params.StartedAt = pgtype.Timestamptz{Time: now, Valid: true}
		}
		// Stamp ended_at when entering a terminal state.
		if to.IsTerminal() {
			params.EndedAt = pgtype.Timestamptz{Time: now, Valid: true}
		}
		if completion != nil {
			if completion.FailureCategory != nil {
				cat := string(*completion.FailureCategory)
				params.FailureCategory = &cat
			}
			if completion.FailureReason != nil {
				params.FailureReason = completion.FailureReason
			}
		}

		updated, err := q.UpdateStageState(ctx, params)
		if err != nil {
			return fmt.Errorf("update stage state: %w", err)
		}
		result = rowToStage(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RetryStage is the explicit override out of a terminal state. The
// failure_category, failure_reason, and ended_at fields are
// cleared; the updated_at trigger fires on the row update so any
// timer keyed off it (notably the SLA ticker) restarts.
func (r *postgresRepo) RetryStage(ctx context.Context, id uuid.UUID, to StageState) (*Stage, error) {
	var result *Stage
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := rundb.New(tx)
		current, err := q.LockStageForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock stage: %w", err)
		}
		from := StageState(current.State)
		if !ValidStageRetryTransition(from, to) {
			return InvalidTransitionError{Kind: "stage", From: string(from), To: string(to)}
		}

		// RetryStageState atomically clears failure_category,
		// failure_reason, ended_at and increments self_retry_count.
		updated, err := q.RetryStageState(ctx, rundb.RetryStageStateParams{
			ID:    id,
			State: string(to),
		})
		if err != nil {
			return fmt.Errorf("retry stage state: %w", err)
		}
		result = rowToStage(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- Conversions between DB and domain types ---

func rowToRun(r rundb.Run) *Run {
	out := &Run{
		ID:                 r.ID,
		Repo:               r.Repo,
		WorkflowID:         r.WorkflowID,
		WorkflowSHA:        r.WorkflowSha,
		TriggerSource:      TriggerSource(r.TriggerSource),
		TriggerRef:         r.TriggerRef,
		InstallationID:     r.InstallationID,
		IdempotencyKey:     r.IdempotencyKey,
		ParentRunID:        r.ParentRunID,
		PullRequestURL:     r.PullRequestUrl,
		WorkflowSpec:       r.WorkflowSpec,
		RetryAttempt:       int(r.RetryAttempt),
		MaxRetriesSnapshot: int(r.MaxRetriesSnapshot),
		RunnerKind:         r.RunnerKind,
		State:              State(r.State),
		CreatedAt:          r.CreatedAt.Time,
		UpdatedAt:          r.UpdatedAt.Time,
	}
	// JSONB → struct. Empty bytes means the column is NULL — the
	// run pre-dates the snapshot wiring or skipped protection
	// lookup (CLI / UI path). We tolerate a malformed payload by
	// dropping the field rather than failing the read; the postgres
	// adapter is on the request hot path and a corrupt snapshot
	// shouldn't 500 every run-detail page. The audit log is the
	// source of truth on what was captured at run-create.
	if len(r.RequiredChecksSnapshot) > 0 {
		var snap RequiredChecksSnapshot
		if err := json.Unmarshal(r.RequiredChecksSnapshot, &snap); err == nil {
			out.RequiredChecksSnapshot = &snap
		}
	}
	// Same tolerance posture as RequiredChecksSnapshot: drop a
	// corrupt blob rather than 500 the run-detail page. The audit
	// log records what was captured at run-create.
	if len(r.IssueContext) > 0 {
		var ic IssueContext
		if err := json.Unmarshal(r.IssueContext, &ic); err == nil {
			out.IssueContext = &ic
		}
	}
	out.DecomposedFrom = r.DecomposedFrom
	out.CostUSDTotal = r.CostUsdTotal
	out.ResolvedModel = r.ResolvedModel
	return out
}

func rowToStage(s rundb.Stage) *Stage {
	out := &Stage{
		ID:               s.ID,
		RunID:            s.RunID,
		Sequence:         int(s.Sequence),
		Type:             StageType(s.StageType),
		ExecutorKind:     ExecutorKind(s.ExecutorKind),
		ExecutorRef:      s.ExecutorRef,
		State:            StageState(s.State),
		FailureReason:    s.FailureReason,
		GateSLA:          s.GateSla,
		RequiresApproval: s.RequiresApproval,
		SelfRetryCount:   int(s.SelfRetryCount),
		CreatedAt:        s.CreatedAt.Time,
		UpdatedAt:        s.UpdatedAt.Time,
	}
	if s.StartedAt.Valid {
		t := s.StartedAt.Time
		out.StartedAt = &t
	}
	if s.EndedAt.Valid {
		t := s.EndedAt.Time
		out.EndedAt = &t
	}
	if s.FailureCategory != nil {
		fc := FailureCategory(*s.FailureCategory)
		out.FailureCategory = &fc
	}
	if s.GateType != nil {
		// Pre-#213 rows have NULL gate_type; nil Gate is the right
		// projection in that case (mirror dispatcher's logic that
		// only writes Gate when the spec defines one). The
		// gate_blocking_checks column was dropped in migration 0018
		// (#254).
		gate := &Gate{
			Kind: GateKind(*s.GateType),
		}
		if len(s.GateApprovers) > 0 {
			var ap GateApprovers
			// Persist failure shouldn't crash the read path — drop the
			// approvers payload and keep the rest. The DB-side write
			// went through json.Marshal so this should never trigger
			// in practice.
			if err := json.Unmarshal(s.GateApprovers, &ap); err == nil {
				gate.Approvers = &ap
			}
		}
		out.Gate = gate
	}
	return out
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)

// pgxQueries is unused at the type level today; keeping it ensures
// future tx-scoped helpers can declare the right interface without
// importing rundb.
var _ pgxQueries = rundb.DBTX(nil)
