package scopeamendment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	amendmentdb "github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment/db"
)

// postgresRepo is the production Repository implementation.
type postgresRepo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository.
// Caller retains ownership of pool.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

func (r *postgresRepo) Create(ctx context.Context, p CreateParams) (*Amendment, error) {
	if p.RunID == uuid.Nil {
		return nil, errors.New("scopeamendment: run_id required")
	}
	if p.StageID == uuid.Nil {
		return nil, errors.New("scopeamendment: stage_id required")
	}
	paths, err := ValidatePaths(p.Paths)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: %w", err)
	}
	pathsJSON, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: marshal paths: %w", err)
	}
	q := amendmentdb.New(r.pool)
	row, err := q.CreateScopeAmendment(ctx, amendmentdb.CreateScopeAmendmentParams{
		ID:      uuid.New(),
		RunID:   p.RunID,
		StageID: p.StageID,
		Paths:   pathsJSON,
		Reason:  p.Reason,
	})
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: create: %w", err)
	}
	return rowToAmendment(row)
}

func (r *postgresRepo) GetByID(ctx context.Context, id uuid.UUID) (*Amendment, error) {
	q := amendmentdb.New(r.pool)
	row, err := q.GetScopeAmendmentByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: get: %w", err)
	}
	return rowToAmendment(row)
}

func (r *postgresRepo) ListByRun(ctx context.Context, runID uuid.UUID) ([]*Amendment, error) {
	q := amendmentdb.New(r.pool)
	rows, err := q.ListScopeAmendmentsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: list: %w", err)
	}
	out := make([]*Amendment, 0, len(rows))
	for _, row := range rows {
		a, err := rowToAmendment(row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *postgresRepo) CountByStage(ctx context.Context, stageID uuid.UUID) (int, error) {
	q := amendmentdb.New(r.pool)
	n, err := q.CountScopeAmendmentsByStage(ctx, stageID)
	if err != nil {
		return 0, fmt.Errorf("scopeamendment: count: %w", err)
	}
	return int(n), nil
}

func (r *postgresRepo) Decide(ctx context.Context, p DecideParams) (*Amendment, error) {
	if p.Status != StatusApproved && p.Status != StatusDenied {
		return nil, fmt.Errorf("scopeamendment: decision status must be %q or %q, got %q",
			StatusApproved, StatusDenied, p.Status)
	}
	q := amendmentdb.New(r.pool)
	reason := p.Reason
	decidedBy := p.DecidedBy
	row, err := q.DecideScopeAmendment(ctx, amendmentdb.DecideScopeAmendmentParams{
		ID:             p.ID,
		Status:         string(p.Status),
		DecisionReason: &reason,
		DecidedBy:      &decidedBy,
		DecidedAt:      pgtype.Timestamptz{Time: r.now(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The pending-only WHERE matched nothing: the row lapsed
		// undecided (#4127), is already decided, or never existed.
		// Disambiguate with a plain read so handlers can map 409
		// amendment_lapsed vs 409 already-decided vs 404.
		existing, gerr := r.GetByID(ctx, p.ID)
		if gerr != nil {
			return nil, ErrNotFound
		}
		if existing.Status == StatusLapsed {
			return nil, ErrLapsed
		}
		return nil, ErrAlreadyDecided
	}
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: decide: %w", err)
	}
	return rowToAmendment(row)
}

// lapseDecisionReason is the decision_reason a lapsed row carries; it names
// the triggering transition (e.g. "stage succeeded", "run cancelled").
func lapseDecisionReason(trigger string) string {
	return "lapsed undecided: " + trigger + " before an operator decision"
}

// LapseForStage implements Lapser for one stage's pending rows.
func (r *postgresRepo) LapseForStage(ctx context.Context, p LapseParams) ([]*Amendment, error) {
	if p.StageID == uuid.Nil {
		return nil, errors.New("scopeamendment: lapse: stage_id required")
	}
	return r.lapse(ctx, p, func(q *amendmentdb.Queries, reason *string, at pgtype.Timestamptz) ([]amendmentdb.ScopeAmendment, error) {
		return q.LapsePendingScopeAmendmentsForStage(ctx, amendmentdb.LapsePendingScopeAmendmentsForStageParams{
			StageID: p.StageID, DecisionReason: reason, DecidedAt: at,
		})
	})
}

// LapseForRun implements Lapser for every pending row on the run.
func (r *postgresRepo) LapseForRun(ctx context.Context, p LapseParams) ([]*Amendment, error) {
	if p.RunID == uuid.Nil {
		return nil, errors.New("scopeamendment: lapse: run_id required")
	}
	return r.lapse(ctx, p, func(q *amendmentdb.Queries, reason *string, at pgtype.Timestamptz) ([]amendmentdb.ScopeAmendment, error) {
		return q.LapsePendingScopeAmendmentsForRun(ctx, amendmentdb.LapsePendingScopeAmendmentsForRunParams{
			RunID: p.RunID, DecisionReason: reason, DecidedAt: at,
		})
	})
}

// lapse runs one lapse UPDATE and appends one chained scope_amendment_lapsed
// entry per returned row, all in ONE transaction: any error rolls back both
// the flips and the entries. Zero rows takes no audit lock.
func (r *postgresRepo) lapse(ctx context.Context, p LapseParams,
	update func(q *amendmentdb.Queries, reason *string, at pgtype.Timestamptz) ([]amendmentdb.ScopeAmendment, error),
) ([]*Amendment, error) {
	if strings.TrimSpace(p.Trigger) == "" {
		return nil, errors.New("scopeamendment: lapse: trigger required")
	}
	if p.Source == "" {
		return nil, errors.New("scopeamendment: lapse: source required")
	}
	now := r.now()
	reason := lapseDecisionReason(p.Trigger)
	var out []*Amendment
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		out = out[:0]
		rows, err := update(amendmentdb.New(tx), &reason, pgtype.Timestamptz{Time: now, Valid: true})
		if err != nil {
			return fmt.Errorf("lapse update: %w", err)
		}
		for _, row := range rows {
			a, err := rowToAmendment(row)
			if err != nil {
				return err
			}
			if err := appendLapsedEntry(ctx, tx, a, p, now); err != nil {
				return err
			}
			out = append(out, a)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scopeamendment: lapse: %w", err)
	}
	if out == nil {
		out = []*Amendment{}
	}
	return out, nil
}

// lapsedPayload is the scope_amendment_lapsed audit payload.
type lapsedPayload struct {
	AmendmentID    string      `json:"amendment_id"`
	StageID        string      `json:"stage_id"`
	Trigger        string      `json:"trigger"`
	Paths          []PathEntry `json:"paths"`
	RequestedAt    time.Time   `json:"requested_at"`
	DecisionReason string      `json:"decision_reason"`
	Source         string      `json:"source"`
}

func appendLapsedEntry(ctx context.Context, tx pgx.Tx, a *Amendment, p LapseParams, now time.Time) error {
	payload, err := json.Marshal(lapsedPayload{
		AmendmentID:    a.ID.String(),
		StageID:        a.StageID.String(),
		Trigger:        p.Trigger,
		Paths:          a.Paths,
		RequestedAt:    a.RequestedAt,
		DecisionReason: lapseDecisionReason(p.Trigger),
		Source:         p.Source,
	})
	if err != nil {
		return fmt.Errorf("marshal lapsed payload: %w", err)
	}
	kind := audit.ActorSystem
	subject := "fishhawkd"
	stageID := a.StageID
	if _, err := audit.AppendChainedTx(ctx, tx, audit.ChainAppendParams{
		RunID:        a.RunID,
		StageID:      &stageID,
		Timestamp:    now,
		Category:     CategoryLapsed,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		return fmt.Errorf("append %s for amendment %s: %w", CategoryLapsed, a.ID, err)
	}
	return nil
}

// LapseStranded implements Lapser: it lapses each stranded stage's pending
// rows in its OWN transaction (one bad stage cannot block the rest), naming
// the stage transition when the stage state lapses and the run transition
// otherwise, and joins the per-stage failures.
func (r *postgresRepo) LapseStranded(ctx context.Context) (int, error) {
	stages, err := amendmentdb.New(r.pool).ListStrandedPendingScopeAmendmentStages(ctx)
	if err != nil {
		return 0, fmt.Errorf("scopeamendment: list stranded: %w", err)
	}
	total := 0
	var errs []error
	for _, st := range stages {
		trigger := "run " + st.RunState
		if StageStateLapses(st.StageState) {
			trigger = "stage " + st.StageState
		}
		lapsed, err := r.LapseForStage(ctx, LapseParams{StageID: st.StageID, Trigger: trigger, Source: LapseSourceStartupBackfill})
		if err != nil {
			errs = append(errs, fmt.Errorf("stage %s: %w", st.StageID, err))
			continue
		}
		total += len(lapsed)
	}
	return total, errors.Join(errs...)
}

func rowToAmendment(r amendmentdb.ScopeAmendment) (*Amendment, error) {
	var paths []PathEntry
	if err := json.Unmarshal(r.Paths, &paths); err != nil {
		return nil, fmt.Errorf("scopeamendment: unmarshal paths: %w", err)
	}
	out := &Amendment{
		ID:             r.ID,
		RunID:          r.RunID,
		StageID:        r.StageID,
		Paths:          paths,
		Reason:         r.Reason,
		Status:         Status(r.Status),
		DecisionReason: r.DecisionReason,
		DecidedBy:      r.DecidedBy,
	}
	if r.RequestedAt.Valid {
		out.RequestedAt = r.RequestedAt.Time
	}
	if r.DecidedAt.Valid {
		t := r.DecidedAt.Time
		out.DecidedAt = &t
	}
	return out, nil
}

// Compile-time checks.
var (
	_ Repository = (*postgresRepo)(nil)
	_ Lapser     = (*postgresRepo)(nil)
)
