package campaign

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing campaign or campaign item. The Postgres
// adapter translates pgx.ErrNoRows into this; callers can errors.Is against
// it without depending on the database driver. Mirrors run.ErrNotFound.
var ErrNotFound = errors.New("not found")

// CreateCampaignParams are the inputs needed to insert a new campaign.
//
// PausePolicy is OPTIONAL: a zero value is normalized to
// PausePolicyPauseCampaign by the adapter before insert (and by
// campaign.Persist before it builds these params), so callers that do not set
// a policy get the conservative block-the-campaign default and the column
// CHECK is never handed an empty string.
type CreateCampaignParams struct {
	Repo        string
	EpicRef     string
	PausePolicy PausePolicy
	// OperatorAgent is the OPTIONAL campaign-level delegation override, carried
	// as raw JSONB bytes (E25.12). Nil persists as NULL — no override. The
	// campaign package never interprets these bytes; the server validates them
	// against spec.OperatorAgent before they reach here.
	OperatorAgent []byte
}

// CreateCampaignItemParams are the inputs needed to insert a new campaign
// item. RunID is intentionally absent: an item is created unlinked and the
// run is attached later via SetCampaignItemRun, mirroring how a run's PR URL
// is backfilled after the implement artifact lands.
type CreateCampaignItemParams struct {
	CampaignID uuid.UUID
	IssueRef   string
	DependsOn  []string
}

// ListCampaignsFilter scopes a ListCampaigns query. Empty strings mean "no
// constraint" — same convention as run.ListRunsFilter and the underlying
// SQL. Limit must be > 0; Offset must be >= 0.
type ListCampaignsFilter struct {
	Repo   string
	State  string
	Limit  int
	Offset int
}

// Repository persists campaigns and campaign items and applies
// state-machine transitions atomically.
//
// Implementations MUST guarantee that two concurrent transition calls
// observing the same prior state cannot both succeed. The Postgres adapter
// does this with row-level SELECT … FOR UPDATE inside a transaction; in-
// memory test fakes use a mutex. This is the same atomicity contract as
// run.Repository.
type Repository interface {
	CreateCampaign(ctx context.Context, p CreateCampaignParams) (*Campaign, error)
	GetCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error)

	// ListCampaigns returns campaigns matching filter, ordered created_at
	// DESC with an id tiebreak. Caller owns the pagination math; this
	// method just hands back the page.
	ListCampaigns(ctx context.Context, f ListCampaignsFilter) ([]*Campaign, error)

	// TransitionCampaign moves a campaign to the target state. Returns
	// InvalidTransitionError if the campaign is in a state from which the
	// target is not reachable. Same-state (idempotent) calls return the
	// unchanged campaign.
	TransitionCampaign(ctx context.Context, id uuid.UUID, to State) (*Campaign, error)

	CreateCampaignItem(ctx context.Context, p CreateCampaignItemParams) (*Item, error)
	GetCampaignItem(ctx context.Context, id uuid.UUID) (*Item, error)

	// ListCampaignItemsForCampaign returns every item of a campaign,
	// ordered created_at ASC with an id tiebreak (insertion order).
	ListCampaignItemsForCampaign(ctx context.Context, campaignID uuid.UUID) ([]*Item, error)

	// ListCampaignItemsForRun returns every campaign item linked to a run
	// via run_id — the reverse-discovery query ("which campaign owns this
	// run") served by the campaign_items_run_idx index. Empty (not an
	// error) when no item references the run.
	ListCampaignItemsForRun(ctx context.Context, runID uuid.UUID) ([]*Item, error)

	// SetCampaignItemRun attaches (or clears) the run linkage on an item.
	// Idempotent: setting the same run twice is a no-op against
	// updated_at. Returns ErrNotFound when the item doesn't exist.
	SetCampaignItemRun(ctx context.Context, itemID uuid.UUID, runID *uuid.UUID) (*Item, error)

	// TransitionCampaignItem moves an item to the target state. Returns
	// InvalidTransitionError if the item is in a state from which the
	// target is not reachable. Same-state (idempotent) calls return the
	// unchanged item.
	TransitionCampaignItem(ctx context.Context, id uuid.UUID, to ItemState) (*Item, error)

	// PauseCampaignItem transitions an item running → paused and records the
	// PauseReason, atomically under the same FOR UPDATE lock as the other
	// transitions. Returns InvalidTransitionError if the item is not in a
	// state from which paused is reachable (only running → paused is valid),
	// and ErrNotFound for a missing item. An already-paused item is an
	// idempotent no-op returning the unchanged item (its first PauseReason is
	// preserved). This is the driver's gate-handoff entry point (E25.7).
	PauseCampaignItem(ctx context.Context, id uuid.UUID, reason PauseReason) (*Item, error)
}
