package campaign_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
)

// TestBaseFake_ItemStartClaims pins the BaseFake item-start defaults (#5266):
// claim and link report ErrNotFound (a fake that drives a start overrides
// them), and release is the real adapter's idempotent no-row success.
func TestBaseFake_ItemStartClaims(t *testing.T) {
	ctx := context.Background()
	var f campaign.BaseFake
	if got, err := f.ClaimCampaignItemStart(ctx, campaign.ClaimCampaignItemStartParams{ItemID: uuid.New()}); got != nil || !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("ClaimCampaignItemStart = %+v, %v; want nil, ErrNotFound", got, err)
	}
	if got, err := f.LinkClaimedCampaignItemRun(ctx, campaign.LinkClaimedCampaignItemRunParams{ItemID: uuid.New()}); got != nil || !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("LinkClaimedCampaignItemRun = %+v, %v; want nil, ErrNotFound", got, err)
	}
	if err := f.ReleaseCampaignItemStart(ctx, uuid.New(), uuid.New()); err != nil {
		t.Errorf("ReleaseCampaignItemStart = %v, want nil", err)
	}
}
