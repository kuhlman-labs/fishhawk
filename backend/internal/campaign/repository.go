package campaign

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing campaign or campaign item. The Postgres
// adapter translates pgx.ErrNoRows into this; callers can errors.Is against
// it without depending on the database driver. Mirrors run.ErrNotFound.
var ErrNotFound = errors.New("not found")

// ErrCampaignNotFound and ErrCampaignItemNotFound REFINE ErrNotFound for the
// multi-row methods (today: ReopenCampaignForItemRestart), which can miss on
// EITHER the campaign row or the item row. Both WRAP ErrNotFound, so every
// existing `errors.Is(err, ErrNotFound)` caller is unaffected; a caller that
// needs to report WHICH row was missing — so a campaign-shaped miss is never
// reported under an item-shaped error code — errors.Is against the specific
// sentinel first and falls back to the umbrella.
//
// An item that exists but belongs to a DIFFERENT campaign is reported as
// ErrCampaignItemNotFound: from the named campaign's perspective that item
// does not exist, which is exactly what the item-shaped code says.
var (
	ErrCampaignNotFound     = fmt.Errorf("campaign: %w", ErrNotFound)
	ErrCampaignItemNotFound = fmt.Errorf("campaign item: %w", ErrNotFound)
)

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
	// WorkingDir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527) every item run minted from the campaign inherits. Empty persists as
	// the '' no-binding default. Carried verbatim: absolute-path validation is
	// the caller's (the REST handler / MCP tool), not this package's.
	WorkingDir string
	// GroomingSource is the OPTIONAL durable provenance block for a campaign
	// created from an approved grooming order (E54.6 / #2238), carried as raw
	// JSONB bytes and written by the campaigns INSERT itself so the campaign row
	// and its provenance are created atomically. Nil persists as NULL — not
	// grooming-sourced. The campaign package never interprets these bytes.
	GroomingSource []byte
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
	// Position is the item's 0-based place in the campaign queue, persisted to
	// campaign_items.queue_position (migration 0074). The caller assigns dense
	// ascending positions from the assembled item order; the column is not
	// unique, so a duplicate position degrades to the retained (created_at, id)
	// tiebreak rather than failing the insert.
	Position int
}

// ListCampaignsFilter scopes a ListCampaigns query. Empty strings mean "no
// constraint" — same convention as run.ListRunsFilter and the underlying
// SQL. Limit must be > 0; Offset must be >= 0.
type ListCampaignsFilter struct {
	Repo  string
	State string
	// AccountID scopes the listing to a tenant workspace account
	// (ADR-057 / #1830). Empty = no constraint, mirroring
	// run.ListRunsFilter.AccountID. When set, the query keeps rows whose
	// account_id equals it OR whose account_id is NULL (untenanted
	// campaigns stay visible — the #1829 NULL-allow window) — the
	// account-scoped list contract the /v0/campaigns handler enforces
	// against the caller's Identity.AccountID.
	AccountID string
	Limit     int
	Offset    int
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
	// AccountGetter is a REQUIRED part of the interface (E44.11 / #2074):
	// every Repository implementation MUST be able to resolve a campaign's
	// owning tenant account, so the ownership gate can never be skipped by
	// a repo that happens not to carry the method.
	AccountGetter

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

	// ListCampaignItemsForCampaign returns every item of a campaign in QUEUE
	// ORDER: queue_position ASC, then the historical (created_at, id) tiebreak.
	// queue_position is the durable assembled order (migration 0074) — for a
	// grooming-sourced campaign it is the ratified rank order — and this is the
	// order the engine's readiness partition is built in.
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

	// SetCampaignItemAutonomy overwrites the item's autonomy tier — the routing
	// metadata re-read from the issue's autonomy:* label on the reconcile-on-read
	// refresh (#2355), so relabelling a child unblocks a campaign parked on
	// attend_human_led instead of the tier staying frozen at assembly time. It
	// NORMALIZES the input to the CHECK-permitted set ("", low, medium, high) —
	// an out-of-set tier persists as "" (the unknown/default tier) rather than
	// tripping the migration-0049 column CHECK. Idempotent when the tier is
	// unchanged (the caller only writes on a real difference, but a same-value
	// write is harmless). Returns ErrNotFound for a missing item. Deliberately
	// NOT a lifecycle transition (autonomy is routing metadata, not state), so —
	// unlike TransitionCampaignItem — it needs no FOR UPDATE state guard.
	SetCampaignItemAutonomy(ctx context.Context, itemID uuid.UUID, autonomy string) (*Item, error)

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
	// NOTE ON THE cancelled-vs-failed ASYMMETRY (RESOLVED by #2681): this
	// repository reset admits BOTH cancelled and failed, and the OPERATOR VERB
	// (handleStartCampaignItemRun) now reaches BOTH from-states. A failed item
	// whose siblings are all terminal drives DeriveState to campaign `failed`
	// (engine.go), which this method cannot recover on its own — it resets the
	// item but leaves the campaign terminal. That campaign-plus-item reset is
	// ReopenCampaignForItemRestart's job (below); RestartCampaignItem stays the
	// path for a still-pending/running campaign, where only the item needs
	// resetting. See also SettleCampaignItemOutOfBand — the sibling
	// guard-bypassing terminal transition that settles (rather than restarts) a
	// delivered item.
	RestartCampaignItem(ctx context.Context, id uuid.UUID) (*Item, error)

	// ReopenCampaignForItemRestart reopens a TERMINAL-FAILED campaign and resets
	// one of its restartable-terminal items in ONE transaction: the campaign
	// moves failed -> running AND the item moves {cancelled|failed} -> pending
	// with its run link cleared. It is the recovery primitive behind
	// fishhawk_start_campaign_item_run for the #2681 wedge — a campaign whose
	// LAST unsettled item failed goes terminal (DeriveState: anyFailed &&
	// allTerminal), and a terminal campaign refuses every recovery verb, so the
	// item was unrecoverable inside the campaign.
	//
	// SINGLE TRANSACTION, BOTH ROWS LOCKED. The two mutations are applied inside
	// one transaction holding SELECT … FOR UPDATE row locks on the campaign and
	// then the item (that fixed order — campaign row, then item row — is the
	// package-wide lock order; no other method locks an item before a campaign
	// inside one transaction, so this introduces no lock cycle). That is not
	// cosmetic: reconcileCampaignItemsOnRead runs on EVERY status read
	// (server/campaigns.go), and a running campaign whose every item is still
	// terminal derives straight back to failed. Splitting the pair into two
	// writes would expose exactly that window; one transaction means no
	// concurrent read can ever observe it.
	//
	// It lives OUTSIDE campaignTransitions/campaignItemTransitions
	// (transition.go refuses EVERY transition out of a terminal `from`, for the
	// campaign AND the item) because a reopen-for-restart is an operator
	// recovery, not a lifecycle transition — so, like RestartCampaignItem, it
	// enforces its OWN guards:
	//
	//   - campaign missing                 -> ErrCampaignNotFound
	//   - campaign state != failed         -> InvalidTransitionError{Kind:"campaign"}
	//     (a paused, cancelled, succeeded, running or pending campaign is NEVER
	//     reopened: cancellation and success are verdicts this verb must not
	//     undo, and a non-terminal campaign needs no reopen — RestartCampaignItem
	//     is its path)
	//   - item missing                     -> ErrCampaignItemNotFound
	//   - item owned by another campaign   -> ErrCampaignItemNotFound
	//   - item state not in {cancelled, failed}
	//     -> InvalidTransitionError{Kind:"campaign_item"}
	//
	// On ANY guard failure NOTHING is written — the whole point of the single
	// transaction. A caller may re-read both rows and find them byte-unchanged,
	// including in the case where the campaign guard PASSED and the item guard
	// then rejected (the campaign is still failed).
	ReopenCampaignForItemRestart(ctx context.Context, campaignID, itemID uuid.UUID) (*Campaign, *Item, error)

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

// AccountGetter is the cheap tenant-account lookup (ADR-057 / #1830) that
// returns just a campaign's account_id ("" for an untenanted NULL row, the
// account UUID string otherwise) without the domain Campaign type carrying the
// column.
//
// It is REQUIRED, not optional: Repository embeds it (E44.11 / #2074),
// mirroring run.AccountGetter. The named interface survives as a readable name
// for the capability and as the anchor for the
// `var _ campaign.AccountGetter = campaign.Repository(nil)` compile-time
// assertion a future refactor must break rather than silently restore the
// skip-the-gate path.
//
// enforceCampaignAccount (GET /v0/campaigns/{id}) calls it UNCONDITIONALLY for
// the ownership check — caller's Identity.AccountID vs the campaign's account,
// untenanted ("") allowed, mismatch 403, and any lookup ERROR fails CLOSED with
// 503. BaseFake provides a stub so a fake embedding it satisfies Repository.
type AccountGetter interface {
	GetCampaignAccountID(ctx context.Context, id uuid.UUID) (string, error)
}
