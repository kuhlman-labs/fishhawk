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
	// IdempotencyKey, when non-nil, makes the create idempotent against
	// (Repo, *IdempotencyKey): a duplicate insert conflicts at the partial
	// unique index. The server resolves an Idempotency-Key header to the
	// existing campaign via GetCampaignByIdempotencyKey before insert (E25.13 /
	// #1455). Nil = no key (the unchanged default); mirrors
	// run.CreateRunParams.IdempotencyKey.
	IdempotencyKey *string
}

// CreateCampaignItemParams are the inputs needed to insert a new campaign
// item. RunID is intentionally absent: an item is created unlinked and the
// run is attached later via SetCampaignItemRun, mirroring how a run's PR URL
// is backfilled after the implement artifact lands.
type CreateCampaignItemParams struct {
	CampaignID uuid.UUID
	IssueRef   string
	DependsOn  []string
	// Autonomy is the item's autonomy tier (low|medium|high), persisted to the
	// campaign_items.autonomy column. Empty is the unknown/default tier (the
	// child carried no autonomy label). The column CHECK admits only the empty
	// tier plus the three known tiers, so an out-of-set value fails closed at
	// write time.
	Autonomy string
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

	// GetCampaignByIdempotencyKey returns the existing campaign for
	// (repo, key) if one exists. Used by POST /v0/campaigns to resolve an
	// Idempotency-Key header to an already-created campaign. Returns
	// ErrNotFound when no row matches. Mirrors
	// run.Repository.GetRunByIdempotencyKey.
	GetCampaignByIdempotencyKey(ctx context.Context, repo, key string) (*Campaign, error)

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

	// RestartCampaignItem resets an item in a restartable TERMINAL state
	// (cancelled or failed) back to pending and clears its run link, atomically
	// under the same SELECT … FOR UPDATE lock as the other transitions — the
	// operator-driven restart reset behind fishhawk_start_campaign_item_run
	// (#1729). It deliberately lives OUTSIDE the campaignItemTransitions table
	// (transition.go), which treats every terminal state as terminal
	// (ValidCampaignItemTransition returns false for any terminal `from`): a
	// restart is an operator reset, not a lifecycle transition, so it enforces
	// its OWN guard here — `from` must be in {cancelled, failed} — and returns
	// InvalidTransitionError for any other state (including running/succeeded)
	// and ErrNotFound for a missing item. A concurrent second call re-reads the
	// now-pending row under the lock and is rejected. On success the item is
	// pending with run_id NULL, ready to fall through the mint/link/transition
	// path for a fresh run.
	//
	// NOTE ON THE cancelled-vs-failed ASYMMETRY: this repository reset admits
	// BOTH cancelled and failed as the forward-compatible seam for the
	// failed-item recovery family. The OPERATOR VERB
	// (handleStartCampaignItemRun), however, currently admits ONLY cancelled
	// items: a failed item drives DeriveState to campaign `failed` (engine.go),
	// which the handler's campaign-state gate refuses (campaign_not_startable)
	// BEFORE item admission — so a failed item never reaches this reset through
	// the verb today. The broader repo contract is intentional; the layering
	// asymmetry is documented here at the definition site (operator arbitration,
	// #1729). See also SettleCampaignItemOutOfBand — the sibling guard-bypassing
	// terminal transition that settles (rather than restarts) a delivered item.
	RestartCampaignItem(ctx context.Context, id uuid.UUID) (*Item, error)

	// SettleCampaignItemOutOfBand settles a TERMINAL item (cancelled or failed)
	// to succeeded WITHOUT clearing the run link, atomically under the same
	// SELECT … FOR UPDATE lock as the other transitions — the out-of-band-delivery
	// settle behind reconcile-on-read pass 2 (#2029). It is the counterpart to
	// RestartCampaignItem: a re-shaped-then-delivered item whose linked run went
	// terminal-non-succeeded (cancelled/failed) but whose GitHub issue is now
	// closed-as-completed is settled succeeded so its rollup stops blocking
	// dependents and next_actions stops advising a restart of the closed,
	// delivered issue. Like RestartCampaignItem it lives OUTSIDE the
	// campaignItemTransitions table (transition.go), which refuses every terminal
	// `from` (ValidCampaignItemTransition returns false for any terminal state):
	// this is an operator/out-of-band settle, not a lifecycle transition, so it
	// enforces its OWN guard — `from` must be in {cancelled, failed} — and returns
	// InvalidTransitionError for any other state (including running/succeeded/
	// pending/blocked/paused) and ErrNotFound for a missing item. UNLIKE
	// RestartCampaignItem it deliberately RETAINS the run link (the dead run is
	// preserved as provenance to the run that was re-shaped and delivered
	// out-of-band). A concurrent second call re-reads the now-succeeded row under
	// the lock and is rejected.
	SettleCampaignItemOutOfBand(ctx context.Context, id uuid.UUID) (*Item, error)
}
