package account

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// SupportedRegions is the closed set of data-residency regions a cell will
// accept a pin for (ADR-062, E44.7 / #1831). It is a CLOSED set on purpose:
// the region string reaches the cell across a trust boundary, and an
// unrecognized value must be rejected rather than persisted verbatim into
// accounts.home_region where it would later resolve to no cell at all.
var SupportedRegions = []string{"au", "eu", "us"}

// Errors the region-pin write path distinguishes. Every one is fail-closed —
// none of them leaves a partial write behind.
var (
	// ErrRegionUnsupported reports a region outside SupportedRegions (or an
	// empty one). The cell persists nothing.
	ErrRegionUnsupported = errors.New("account: unsupported home region")
	// ErrRegionForeign reports the residency invariant: a pin for a region
	// other than the one THIS cell serves. A valid EU pin arriving at a US
	// cell is a routing fault, and honoring it would place EU data in the US.
	ErrRegionForeign = errors.New("account: region pin does not match this cell's home region")
	// ErrRegionConflict reports the replay bound: the account already carries
	// a DIFFERENT home_region. Region assignment is first-write-wins, so a
	// replayed or re-issued pin can never move an account between regions.
	ErrRegionConflict = errors.New("account: account is already pinned to a different home region")
	// ErrRegionUnavailable reports that no account store is wired on this
	// deployment, so the pin cannot be recorded.
	ErrRegionUnavailable = errors.New("account: no account store is configured")
)

// IsSupportedRegion reports whether region is one of SupportedRegions.
// Comparison is on the lower-cased, space-trimmed value: the region arrives
// as a URL query parameter and casing is not meaningful.
func IsSupportedRegion(region string) bool {
	r := NormalizeRegion(region)
	if r == "" {
		return false
	}
	i := sort.SearchStrings(SupportedRegions, r)
	return i < len(SupportedRegions) && SupportedRegions[i] == r
}

// NormalizeRegion lower-cases and trims a region string. It performs NO
// validation — callers pair it with IsSupportedRegion.
func NormalizeRegion(region string) string {
	return strings.ToLower(strings.TrimSpace(region))
}

// RegionQuerier is the narrow query surface RegionPinner needs.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a fake.
type RegionQuerier interface {
	GetAccountByKey(ctx context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error)
	UpsertAccount(ctx context.Context, arg accountdb.UpsertAccountParams) (accountdb.Account, error)
}

var _ RegionQuerier = (*accountdb.Queries)(nil)

// RegionPinner stamps accounts.home_region from a region the DIRECTORY already
// decided (ADR-062). It never derives a region itself and never writes back to
// the directory: the cell is authoritative-on-write for its own accounts row
// and nothing more.
//
// It enforces the two cell-side invariants that sit above the handoff
// signature check:
//
//   - the RESIDENCY invariant — the pinned region must equal this cell's own
//     configured home region (ErrRegionForeign);
//   - the REPLAY bound — home_region is first-write-wins. A pin is accepted
//     only when the column is currently NULL or already equals the incoming
//     value, so replaying an old signed pin is idempotent and can never move
//     an account to a different region (ErrRegionConflict).
type RegionPinner struct {
	q RegionQuerier
	// homeRegion is the cell's own region tag (FISHHAWKD_HOME_REGION). Empty
	// disables the residency self-check — the untenanted single-region
	// posture, where every cell is the only cell.
	homeRegion string
}

// NewRegionPinner wraps a querier and this cell's home-region tag. A nil
// querier is tolerated at construction; Pin then reports ErrRegionUnavailable
// rather than panicking (the no-database posture).
func NewRegionPinner(q RegionQuerier, cellHomeRegion string) *RegionPinner {
	return &RegionPinner{q: q, homeRegion: NormalizeRegion(cellHomeRegion)}
}

// HomeRegion returns the cell's configured region tag ("" when unset).
func (p *RegionPinner) HomeRegion() string {
	if p == nil {
		return ""
	}
	return p.homeRegion
}

// PinParams is one region-pin write: the forge-neutral account identity plus
// the directory-decided region.
type PinParams struct {
	Provider    string
	AccountKey  string
	DisplayName string
	Region      string
}

// Pin records region on the (provider, account_key) account, creating the row
// when it does not exist yet.
//
// The write reuses the existing UpsertAccount query — home_region already
// exists on accounts (migration 0052), so no migration is added. Because that
// query's ON CONFLICT clause overwrites home_region unconditionally, the
// first-write-wins bound is enforced HERE, ahead of the write: an account
// already pinned to a different region is rejected with ErrRegionConflict and
// no statement is issued. An existing row's display_name and granularity are
// carried through unchanged so a pin never clobbers them.
func (p *RegionPinner) Pin(ctx context.Context, in PinParams) (accountdb.Account, error) {
	if p == nil || p.q == nil {
		return accountdb.Account{}, ErrRegionUnavailable
	}
	region := NormalizeRegion(in.Region)
	if !IsSupportedRegion(region) {
		return accountdb.Account{}, fmt.Errorf("%w: %q (supported: %s)", ErrRegionUnsupported, in.Region, strings.Join(SupportedRegions, ", "))
	}
	// Residency self-check. Skipped only when this cell carries no region tag
	// at all (single-region deployment).
	if p.homeRegion != "" && region != p.homeRegion {
		return accountdb.Account{}, fmt.Errorf("%w: pin is for %q, this cell serves %q", ErrRegionForeign, region, p.homeRegion)
	}
	provider := strings.TrimSpace(in.Provider)
	accountKey := strings.TrimSpace(in.AccountKey)
	if provider == "" || accountKey == "" {
		return accountdb.Account{}, fmt.Errorf("%w: provider and account_key are required", ErrRegionUnsupported)
	}

	existing, err := p.q.GetAccountByKey(ctx, accountdb.GetAccountByKeyParams{Provider: provider, AccountKey: accountKey})
	switch {
	case err == nil:
		// Replay bound: NULL or equal accepted, anything else refused.
		if existing.HomeRegion != nil && NormalizeRegion(*existing.HomeRegion) != region {
			return accountdb.Account{}, fmt.Errorf("%w: %q is pinned to %q, pin carries %q",
				ErrRegionConflict, accountKey, *existing.HomeRegion, region)
		}
		return p.q.UpsertAccount(ctx, accountdb.UpsertAccountParams{
			ID:          existing.ID,
			Provider:    existing.Provider,
			AccountKey:  existing.AccountKey,
			DisplayName: existing.DisplayName,
			Granularity: existing.Granularity,
			HomeRegion:  &region,
		})
	case errors.Is(err, pgx.ErrNoRows):
		params := accountdb.UpsertAccountParams{
			ID:          uuid.New(),
			Provider:    provider,
			AccountKey:  accountKey,
			Granularity: "enterprise",
			HomeRegion:  &region,
		}
		if dn := strings.TrimSpace(in.DisplayName); dn != "" {
			params.DisplayName = &dn
		}
		return p.q.UpsertAccount(ctx, params)
	default:
		// A real DB fault: propagate so the caller fails closed rather than
		// creating a duplicate account row on a transient read error.
		return accountdb.Account{}, fmt.Errorf("account: look up %s/%s: %w", provider, accountKey, err)
	}
}
