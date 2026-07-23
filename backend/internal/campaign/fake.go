package campaign

import (
	"context"

	"github.com/google/uuid"
)

// BaseFake is a no-op implementation of Repository. Embed it in a test fake
// and override only the methods the test exercises — the same pattern as
// run.BaseFake, so a test author doesn't write interface-completeness stubs
// that break every time Repository gains a method.
//
// Single-entity methods return nil, ErrNotFound. Slice methods return nil, nil.
type BaseFake struct{}

// compile-time check: BaseFake must satisfy Repository.
var _ Repository = BaseFake{}

// CreateCampaign returns nil, ErrNotFound.
func (BaseFake) CreateCampaign(_ context.Context, _ CreateCampaignParams) (*Campaign, error) {
	return nil, ErrNotFound
}

// GetCampaign returns nil, ErrNotFound.
func (BaseFake) GetCampaign(_ context.Context, _ uuid.UUID) (*Campaign, error) {
	return nil, ErrNotFound
}

// GetCampaignAccountID returns "", ErrNotFound. It is what makes BaseFake
// satisfy the REQUIRED AccountGetter portion of Repository (ADR-057 / #1830,
// promoted by E44.11 / #2074) — no longer an optional freebie. Because
// enforceCampaignAccount now calls it unconditionally and fails CLOSED on any
// error, a BaseFake-backed repo yields 503 on the get-campaign ownership gate
// rather than the old untenanted-allow; a fake that wants the gate to pass
// must return a real account.
func (BaseFake) GetCampaignAccountID(_ context.Context, _ uuid.UUID) (string, error) {
	return "", ErrNotFound
}

// GetCampaignByIdempotencyKey returns nil, ErrNotFound.
func (BaseFake) GetCampaignByIdempotencyKey(_ context.Context, _, _ string) (*Campaign, error) {
	return nil, ErrNotFound
}

// ListCampaigns returns nil, nil.
func (BaseFake) ListCampaigns(_ context.Context, _ ListCampaignsFilter) ([]*Campaign, error) {
	return nil, nil
}

// TransitionCampaign returns nil, ErrNotFound.
func (BaseFake) TransitionCampaign(_ context.Context, _ uuid.UUID, _ State) (*Campaign, error) {
	return nil, ErrNotFound
}

// CreateCampaignItem returns nil, ErrNotFound.
func (BaseFake) CreateCampaignItem(_ context.Context, _ CreateCampaignItemParams) (*Item, error) {
	return nil, ErrNotFound
}

// GetCampaignItem returns nil, ErrNotFound.
func (BaseFake) GetCampaignItem(_ context.Context, _ uuid.UUID) (*Item, error) {
	return nil, ErrNotFound
}

// ListCampaignItemsForCampaign returns nil, nil.
func (BaseFake) ListCampaignItemsForCampaign(_ context.Context, _ uuid.UUID) ([]*Item, error) {
	return nil, nil
}

// ListCampaignItemsForRun returns nil, nil.
func (BaseFake) ListCampaignItemsForRun(_ context.Context, _ uuid.UUID) ([]*Item, error) {
	return nil, nil
}

// SetCampaignItemRun returns nil, ErrNotFound.
func (BaseFake) SetCampaignItemRun(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*Item, error) {
	return nil, ErrNotFound
}

// TransitionCampaignItem returns nil, ErrNotFound.
func (BaseFake) TransitionCampaignItem(_ context.Context, _ uuid.UUID, _ ItemState) (*Item, error) {
	return nil, ErrNotFound
}

// PauseCampaignItem returns nil, ErrNotFound.
func (BaseFake) PauseCampaignItem(_ context.Context, _ uuid.UUID, _ PauseReason) (*Item, error) {
	return nil, ErrNotFound
}

// RestartCampaignItem returns nil, ErrNotFound.
func (BaseFake) RestartCampaignItem(_ context.Context, _ uuid.UUID) (*Item, error) {
	return nil, ErrNotFound
}

// SettleCampaignItemOutOfBand returns nil, ErrNotFound.
func (BaseFake) SettleCampaignItemOutOfBand(_ context.Context, _ uuid.UUID) (*Item, error) {
	return nil, ErrNotFound
}
