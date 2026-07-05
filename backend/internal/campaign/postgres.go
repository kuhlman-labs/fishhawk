package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	campaigndb "github.com/kuhlman-labs/fishhawk/backend/internal/campaign/db"
)

// postgresRepo is the production Repository implementation. State
// transitions are wrapped in a transaction with SELECT … FOR UPDATE to
// prevent two concurrent transitions from observing the same prior state —
// the same posture as run.postgresRepo.
type postgresRepo struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps a pgxpool.Pool to satisfy Repository. Caller
// retains ownership of the pool and is responsible for Close.
func NewPostgresRepository(pool *pgxpool.Pool) Repository {
	return &postgresRepo{pool: pool}
}

// --- Campaign methods ---

func (r *postgresRepo) CreateCampaign(ctx context.Context, p CreateCampaignParams) (*Campaign, error) {
	q := campaigndb.New(r.pool)
	row, err := q.CreateCampaign(ctx, campaigndb.CreateCampaignParams{
		ID:      uuid.New(),
		Repo:    p.Repo,
		EpicRef: p.EpicRef,
		State:   string(StatePending),
		// Normalize a zero policy to the conservative block-the-campaign
		// default so a direct caller that omits PausePolicy never hands the
		// column CHECK an empty string. campaign.Persist normalizes too; this
		// is the defensive last line for any other repository caller.
		PausePolicy: string(normalizePausePolicy(p.PausePolicy)),
		// OperatorAgent is the OPTIONAL campaign-level delegation override
		// (E25.12), stored opaquely. Nil persists as NULL — no override.
		OperatorAgent: p.OperatorAgent,
		// IdempotencyKey is the OPTIONAL create idempotency key (E25.13). Nil
		// persists as NULL — no key; the partial unique index excludes NULLs.
		IdempotencyKey: p.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}
	return rowToCampaign(row), nil
}

func (r *postgresRepo) GetCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	q := campaigndb.New(r.pool)
	row, err := q.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return rowToCampaign(row), nil
}

func (r *postgresRepo) GetCampaignByIdempotencyKey(ctx context.Context, repo, key string) (*Campaign, error) {
	q := campaigndb.New(r.pool)
	row, err := q.GetCampaignByIdempotencyKey(ctx, campaigndb.GetCampaignByIdempotencyKeyParams{
		Repo:           repo,
		IdempotencyKey: &key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get campaign by idempotency_key: %w", err)
	}
	return rowToCampaign(row), nil
}

func (r *postgresRepo) ListCampaigns(ctx context.Context, f ListCampaignsFilter) ([]*Campaign, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("list campaigns: limit must be > 0")
	}
	if f.Offset < 0 {
		return nil, fmt.Errorf("list campaigns: offset must be >= 0")
	}
	q := campaigndb.New(r.pool)
	rows, err := q.ListCampaigns(ctx, campaigndb.ListCampaignsParams{
		Repo:  f.Repo,
		State: f.State,
		Lim:   int32(f.Limit),
		Off:   int32(f.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list campaigns: %w", err)
	}
	out := make([]*Campaign, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToCampaign(row))
	}
	return out, nil
}

func (r *postgresRepo) TransitionCampaign(ctx context.Context, id uuid.UUID, to State) (*Campaign, error) {
	var result *Campaign
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := campaigndb.New(tx)
		current, err := q.LockCampaignForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock campaign: %w", err)
		}
		from := State(current.State)
		if from == to {
			result = rowToCampaign(current)
			return nil
		}
		if !ValidCampaignTransition(from, to) {
			return InvalidTransitionError{Kind: "campaign", From: string(from), To: string(to)}
		}
		updated, err := q.UpdateCampaignState(ctx, campaigndb.UpdateCampaignStateParams{
			ID:    id,
			State: string(to),
		})
		if err != nil {
			return fmt.Errorf("update campaign state: %w", err)
		}
		result = rowToCampaign(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- Campaign item methods ---

func (r *postgresRepo) CreateCampaignItem(ctx context.Context, p CreateCampaignItemParams) (*Item, error) {
	dependsOn, err := marshalDependsOn(p.DependsOn)
	if err != nil {
		return nil, err
	}
	q := campaigndb.New(r.pool)
	row, err := q.CreateCampaignItem(ctx, campaigndb.CreateCampaignItemParams{
		ID:         uuid.New(),
		CampaignID: p.CampaignID,
		IssueRef:   p.IssueRef,
		DependsOn:  dependsOn,
		State:      string(ItemStatePending),
		// Autonomy tier ("", "low", "medium", "high") sourced from the epic
		// child's labels (E32.4 / #1551). An unset value is "" — the column's
		// NOT NULL DEFAULT '' matches, and the CHECK admits it.
		Autonomy: p.Autonomy,
	})
	if err != nil {
		return nil, fmt.Errorf("create campaign item: %w", err)
	}
	return rowToCampaignItem(row), nil
}

func (r *postgresRepo) GetCampaignItem(ctx context.Context, id uuid.UUID) (*Item, error) {
	q := campaigndb.New(r.pool)
	row, err := q.GetCampaignItem(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get campaign item: %w", err)
	}
	return rowToCampaignItem(row), nil
}

func (r *postgresRepo) ListCampaignItemsForCampaign(ctx context.Context, campaignID uuid.UUID) ([]*Item, error) {
	q := campaigndb.New(r.pool)
	rows, err := q.ListCampaignItemsForCampaign(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list campaign items for campaign: %w", err)
	}
	out := make([]*Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToCampaignItem(row))
	}
	return out, nil
}

func (r *postgresRepo) ListCampaignItemsForRun(ctx context.Context, runID uuid.UUID) ([]*Item, error) {
	q := campaigndb.New(r.pool)
	rows, err := q.ListCampaignItemsForRun(ctx, &runID)
	if err != nil {
		return nil, fmt.Errorf("list campaign items for run: %w", err)
	}
	out := make([]*Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToCampaignItem(row))
	}
	return out, nil
}

func (r *postgresRepo) SetCampaignItemRun(ctx context.Context, itemID uuid.UUID, runID *uuid.UUID) (*Item, error) {
	q := campaigndb.New(r.pool)
	row, err := q.SetCampaignItemRun(ctx, campaigndb.SetCampaignItemRunParams{
		ID:    itemID,
		RunID: runID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set campaign item run: %w", err)
	}
	return rowToCampaignItem(row), nil
}

func (r *postgresRepo) TransitionCampaignItem(ctx context.Context, id uuid.UUID, to ItemState) (*Item, error) {
	var result *Item
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := campaigndb.New(tx)
		current, err := q.LockCampaignItemForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock campaign item: %w", err)
		}
		from := ItemState(current.State)
		if from == to {
			result = rowToCampaignItem(current)
			return nil
		}
		if !ValidCampaignItemTransition(from, to) {
			return InvalidTransitionError{Kind: "campaign_item", From: string(from), To: string(to)}
		}
		updated, err := q.UpdateCampaignItemState(ctx, campaigndb.UpdateCampaignItemStateParams{
			ID:    id,
			State: string(to),
		})
		if err != nil {
			return fmt.Errorf("update campaign item state: %w", err)
		}
		result = rowToCampaignItem(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *postgresRepo) PauseCampaignItem(ctx context.Context, id uuid.UUID, reason PauseReason) (*Item, error) {
	payload, err := json.Marshal(reason)
	if err != nil {
		return nil, fmt.Errorf("marshal pause_reason: %w", err)
	}
	var result *Item
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := campaigndb.New(tx)
		current, err := q.LockCampaignItemForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock campaign item: %w", err)
		}
		from := ItemState(current.State)
		// Already paused: idempotent no-op preserving the first PauseReason.
		if from == ItemStatePaused {
			result = rowToCampaignItem(current)
			return nil
		}
		if !ValidCampaignItemTransition(from, ItemStatePaused) {
			return InvalidTransitionError{Kind: "campaign_item", From: string(from), To: string(ItemStatePaused)}
		}
		updated, err := q.SetCampaignItemPause(ctx, campaigndb.SetCampaignItemPauseParams{
			ID:          id,
			PauseReason: payload,
		})
		if err != nil {
			return fmt.Errorf("set campaign item pause: %w", err)
		}
		result = rowToCampaignItem(updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- Conversions between DB and domain types ---

// marshalDependsOn renders the depends_on edge slice as a JSONB array. A nil
// or empty slice marshals to `[]` (never `null`) so the column always holds
// a well-formed array, matching the migration's DEFAULT '[]'.
func marshalDependsOn(deps []string) ([]byte, error) {
	if len(deps) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(deps)
	if err != nil {
		return nil, fmt.Errorf("marshal depends_on: %w", err)
	}
	return b, nil
}

func rowToCampaign(c campaigndb.Campaign) *Campaign {
	out := &Campaign{
		ID:          c.ID,
		Repo:        c.Repo,
		EpicRef:     c.EpicRef,
		State:       State(c.State),
		PausePolicy: PausePolicy(c.PausePolicy),
		CreatedAt:   c.CreatedAt.Time,
		UpdatedAt:   c.UpdatedAt.Time,
	}
	// JSONB → raw []byte passthrough. A NULL/empty column yields nil (no
	// override) rather than an empty slice, so the unchanged-behavior path is
	// nil-clean — same len()>0 tolerance posture as the pause_reason carrier.
	if len(c.OperatorAgent) > 0 {
		out.OperatorAgent = c.OperatorAgent
	}
	// Nullable idempotency_key: a *string passthrough. NULL yields nil (no
	// key) — the unchanged-behavior default.
	out.IdempotencyKey = c.IdempotencyKey
	return out
}

func rowToCampaignItem(i campaigndb.CampaignItem) *Item {
	out := &Item{
		ID:         i.ID,
		CampaignID: i.CampaignID,
		IssueRef:   i.IssueRef,
		RunID:      i.RunID,
		State:      ItemState(i.State),
		// Autonomy tier column → domain, a plain passthrough. '' (the
		// NOT NULL DEFAULT for pre-E32.4 rows and unlabelled items) round-trips
		// as "", treated as autonomous by the readiness partition.
		Autonomy:  i.Autonomy,
		CreatedAt: i.CreatedAt.Time,
		UpdatedAt: i.UpdatedAt.Time,
	}
	// JSONB → []string. An empty/NULL payload yields a nil slice (no
	// dependencies). Tolerate a malformed blob by dropping it rather than
	// failing the read — the column is written through json.Marshal so this
	// should never trigger in practice; mirrors run.rowToRun's tolerance
	// posture on its JSONB columns.
	if len(i.DependsOn) > 0 {
		var deps []string
		if err := json.Unmarshal(i.DependsOn, &deps); err == nil {
			out.DependsOn = deps
		}
	}
	// JSONB → *PauseReason. NULL/empty yields nil (item never paused). A
	// malformed blob is dropped to nil rather than failing the read, same
	// tolerance posture as depends_on above.
	if len(i.PauseReason) > 0 {
		var pr PauseReason
		if err := json.Unmarshal(i.PauseReason, &pr); err == nil {
			out.PauseReason = &pr
		}
	}
	return out
}

// Compile-time check that postgresRepo implements Repository.
var _ Repository = (*postgresRepo)(nil)
