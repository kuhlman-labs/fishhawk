package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// RegionPinnerErrors. Callers match with errors.Is; the cell's HTTP
// middleware maps each onto a status code.
var (
	// ErrRegionDisabled reports a cell with no configured home region. The
	// pin surface is then disabled ENTIRELY — a cell that does not know
	// which region it is cannot honor a residency claim, so it refuses
	// rather than stamping an unverifiable value (ADR-062 A2.4).
	ErrRegionDisabled = errors.New("account: region pin is disabled (this cell has no configured home region)")
	// ErrRegionMismatch reports a handoff naming a region OTHER than this
	// cell's own — the residency self-check. A cell in eu never records a
	// us handoff, whatever the signature says.
	ErrRegionMismatch = errors.New("account: handoff names a different region than this cell")
	// ErrAlreadyPinned reports an account already homed in a DIFFERENT
	// region. First-write-wins: the existing assignment stands.
	ErrAlreadyPinned = errors.New("account: account is already pinned to a different region")
	// ErrUnknownAccount reports a handoff for an account this cell has no
	// row for. The pin is UPDATE-only, so an unknown account is refused,
	// never created (ADR-062 A2.5).
	ErrUnknownAccount = errors.New("account: no such account on this cell")
	// ErrInvalidPin reports a malformed pin request (empty identity).
	ErrInvalidPin = errors.New("account: invalid pin request")
)

// RegionPinnerQueries is the single query surface RegionPinner needs.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a fake
// for the branches that need no database.
type RegionPinnerQueries interface {
	PinAccountHomeRegion(ctx context.Context, arg accountdb.PinAccountHomeRegionParams) (accountdb.Account, error)
	GetAccountByKey(ctx context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error)
}

var _ RegionPinnerQueries = (*accountdb.Queries)(nil)

// RegionPinner stamps accounts.home_region from a verified directory handoff
// (ADR-062, E44.7 / #1831).
//
// It is the cell-side half of the regional control plane: the directory owns
// (provider, account_key) -> region and hands the caller off to the owning
// cell; this type records that assignment locally so the cell's own reads can
// answer "is this account mine?" without a directory round-trip.
type RegionPinner struct {
	q          RegionPinnerQueries
	cellRegion string
}

// NewRegionPinner returns a pinner for a cell whose own region is cellRegion.
//
// An EMPTY cellRegion is deliberately constructible: the resulting pinner
// refuses every Pin with ErrRegionDisabled rather than being nil. A nil-means-
// disabled design puts the fail-closed decision in every caller; this one puts
// it in exactly one place, and a caller that forgets the nil check gets a
// refusal instead of a panic or a silent bypass.
func NewRegionPinner(q RegionPinnerQueries, cellRegion string) *RegionPinner {
	return &RegionPinner{q: q, cellRegion: cellRegion}
}

// CellRegion reports the region this cell believes it is, or "" when the pin
// surface is disabled.
func (p *RegionPinner) CellRegion() string {
	if p == nil {
		return ""
	}
	return p.cellRegion
}

// Enabled reports whether this pinner will attempt a pin at all. A nil
// pinner, an unset cell region, or a missing query surface all report false.
func (p *RegionPinner) Enabled() bool {
	return p != nil && p.cellRegion != "" && p.q != nil
}

// Pin records region as the home region of (provider, accountKey).
//
// Refusal branches, in order:
//
//   - the cell has no configured region (or no query surface): ErrRegionDisabled
//   - provider/accountKey/region empty: ErrInvalidPin
//   - region != this cell's own region: ErrRegionMismatch
//   - the conditional UPDATE matched no row: either ErrUnknownAccount (no such
//     account here) or ErrAlreadyPinned (homed elsewhere), disambiguated by a
//     follow-up read
//
// A successful Pin is idempotent: re-pinning an account to the region it
// already holds matches the row and is a no-op, which is exactly what makes
// replaying an unexpired handoff harmless.
func (p *RegionPinner) Pin(ctx context.Context, provider, accountKey, region string) error {
	if !p.Enabled() {
		return ErrRegionDisabled
	}
	if provider == "" || accountKey == "" || region == "" {
		return fmt.Errorf("%w: provider, account_key and region are all required", ErrInvalidPin)
	}
	if region != p.cellRegion {
		return fmt.Errorf("%w: handoff names %q, this cell is %q", ErrRegionMismatch, region, p.cellRegion)
	}

	_, err := p.q.PinAccountHomeRegion(ctx, accountdb.PinAccountHomeRegionParams{
		Provider:   provider,
		AccountKey: accountKey,
		HomeRegion: &region,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("account: pin home region: %w", err)
	}

	// Zero rows means the guarded UPDATE did not match. Read the row back to
	// say WHICH refusal it was — an operator debugging a failed handoff needs
	// "already homed in eu" and "no such account" to be distinguishable.
	return p.classifyMiss(ctx, provider, accountKey, region)
}

// classifyMiss turns a zero-row UPDATE into the specific typed refusal. A
// read error here is itself propagated: an unclassifiable miss must not
// degrade into a success.
func (p *RegionPinner) classifyMiss(ctx context.Context, provider, accountKey, region string) error {
	row, err := p.q.GetAccountByKey(ctx, accountdb.GetAccountByKeyParams{
		Provider:   provider,
		AccountKey: accountKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s/%s", ErrUnknownAccount, provider, accountKey)
	case err != nil:
		return fmt.Errorf("account: classify pin miss: %w", err)
	}
	held := ""
	if row.HomeRegion != nil {
		held = *row.HomeRegion
	}
	return fmt.Errorf("%w: %s/%s is homed in %q, handoff named %q",
		ErrAlreadyPinned, provider, accountKey, held, region)
}
