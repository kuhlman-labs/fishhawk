package campaign_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// makeCampaign creates a campaign with sensible defaults.
func makeCampaign(t *testing.T, repo campaign.Repository) *campaign.Campaign {
	t.Helper()
	c, err := repo.CreateCampaign(context.Background(), campaign.CreateCampaignParams{
		Repo:    "kuhlman-labs/fishhawk",
		EpicRef: "issue:1439",
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	return c
}

func TestPostgres_CreateAndGetCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	created := makeCampaign(t, repo)
	if created.State != campaign.StatePending {
		t.Errorf("initial state = %q, want pending", created.State)
	}
	if created.EpicRef != "issue:1439" {
		t.Errorf("epic_ref = %q, want issue:1439", created.EpicRef)
	}

	got, err := repo.GetCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.ID != created.ID || got.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("read-back mismatch: %+v", got)
	}
}

func TestPostgres_GetCampaign_NotFound(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	_, err := repo.GetCampaign(context.Background(), uuid.New())
	if !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("GetCampaign(missing) err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_ListCampaigns(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	c := makeCampaign(t, repo)

	// Repo + state filter narrows to the created campaign.
	got, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{
		Repo:  "kuhlman-labs/fishhawk",
		State: string(campaign.StatePending),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	var found bool
	for _, item := range got {
		if item.ID == c.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("created campaign %s not in filtered list (%d rows)", c.ID, len(got))
	}

	// A non-matching state filter excludes it.
	none, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{
		State: string(campaign.StateSucceeded),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("list campaigns (succeeded): %v", err)
	}
	for _, item := range none {
		if item.ID == c.ID {
			t.Errorf("pending campaign %s leaked into succeeded filter", c.ID)
		}
	}
}

func TestPostgres_ListCampaigns_BadPagination(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	if _, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 0}); err == nil {
		t.Error("ListCampaigns with Limit=0 should error")
	}
	if _, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Limit: 10, Offset: -1}); err == nil {
		t.Error("ListCampaigns with Offset=-1 should error")
	}
}

func TestPostgres_TransitionCampaign(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)

	// Valid edge: pending → running.
	running, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("transition pending→running: %v", err)
	}
	if running.State != campaign.StateRunning {
		t.Errorf("state after transition = %q, want running", running.State)
	}

	// Idempotent same-state no-op returns the unchanged row.
	same, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("idempotent running→running: %v", err)
	}
	if same.State != campaign.StateRunning {
		t.Errorf("idempotent transition state = %q, want running", same.State)
	}

	// Invalid edge: running → pending is refused with InvalidTransitionError.
	_, err = repo.TransitionCampaign(ctx, c.ID, campaign.StatePending)
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("running→pending err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign" || ite.From != "running" || ite.To != "pending" {
		t.Errorf("InvalidTransitionError = %+v, want campaign running→pending", ite)
	}

	// Terminal transition then any further move is refused.
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateSucceeded); err != nil {
		t.Fatalf("transition running→succeeded: %v", err)
	}
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateFailed); !errors.As(err, &ite) {
		t.Errorf("succeeded→failed err = %v, want InvalidTransitionError", err)
	}

	// A missing campaign is ErrNotFound.
	if _, err := repo.TransitionCampaign(ctx, uuid.New(), campaign.StateRunning); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("transition(missing) err = %v, want ErrNotFound", err)
	}
}

func TestPostgres_CampaignItem_RoundTripAndDependsOn(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440", "issue:1437"},
	})
	if err != nil {
		t.Fatalf("create campaign item: %v", err)
	}
	if item.State != campaign.ItemStatePending {
		t.Errorf("initial item state = %q, want pending", item.State)
	}
	if item.RunID != nil {
		t.Errorf("new item RunID = %v, want nil", item.RunID)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get campaign item: %v", err)
	}
	if len(got.DependsOn) != 2 || got.DependsOn[0] != "issue:1440" || got.DependsOn[1] != "issue:1437" {
		t.Errorf("depends_on round-trip = %v, want [issue:1440 issue:1437]", got.DependsOn)
	}

	// An item created with no dependencies reads back a nil/empty slice
	// (the column holds '[]', never NULL).
	bare, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1442",
	})
	if err != nil {
		t.Fatalf("create bare item: %v", err)
	}
	if len(bare.DependsOn) != 0 {
		t.Errorf("bare item depends_on = %v, want empty", bare.DependsOn)
	}

	// Listing by campaign returns both items in insertion order.
	list, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("list items for campaign: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("items for campaign = %d, want 2", len(list))
	}
	if list[0].IssueRef != "issue:1441" || list[1].IssueRef != "issue:1442" {
		t.Errorf("item order = [%q %q], want [issue:1441 issue:1442]", list[0].IssueRef, list[1].IssueRef)
	}

	if _, err := repo.GetCampaignItem(ctx, uuid.New()); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("GetCampaignItem(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_CampaignItem_MalformedDependsOn_Tolerated asserts the
// rowToCampaignItem tolerance branch: a depends_on payload that is valid
// JSONB but not a []string (so json.Unmarshal into []string fails) is
// DROPPED to a nil slice rather than failing the read — mirroring
// run.rowToRun's drop-don't-500 posture on its JSONB columns. The bad
// payload is written via raw SQL because the repo write path always
// produces a well-formed array.
func TestPostgres_CampaignItem_MalformedDependsOn_Tolerated(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Overwrite depends_on with valid JSONB that is NOT a string array.
	if _, err := pool.Exec(ctx,
		`UPDATE campaign_items SET depends_on = '{"not":"an-array"}'::jsonb WHERE id = $1`,
		item.ID,
	); err != nil {
		t.Fatalf("write malformed depends_on: %v", err)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item with malformed depends_on should not error: %v", err)
	}
	if got.DependsOn != nil {
		t.Errorf("malformed depends_on = %v, want nil (dropped, not surfaced)", got.DependsOn)
	}
}

func TestPostgres_TransitionCampaignItem(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
		DependsOn:  []string{"issue:1440"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// pending → blocked (dependency open) → running (dependency cleared).
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateBlocked); err != nil {
		t.Fatalf("pending→blocked: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("blocked→running: %v", err)
	}

	// Invalid: running → blocked is refused.
	_, err = repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateBlocked)
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("running→blocked err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign_item" {
		t.Errorf("error kind = %q, want campaign_item", ite.Kind)
	}

	// running → succeeded, then terminal rejects further moves.
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateSucceeded); err != nil {
		t.Fatalf("running→succeeded: %v", err)
	}
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateFailed); !errors.As(err, &ite) {
		t.Errorf("succeeded→failed err = %v, want InvalidTransitionError", err)
	}

	if _, err := repo.TransitionCampaignItem(ctx, uuid.New(), campaign.ItemStateRunning); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("transition(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_RunLinkage_EndToEnd spans domain → persistence → run linkage:
// it inserts a REAL runs row (via the run repo, exercising the cross-package
// boundary), attaches it to a campaign item, and asserts both forward
// (GetCampaignItem.RunID) and reverse (ListCampaignItemsForRun) discovery
// resolve the link.
func TestPostgres_RunLinkage_EndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	campaignRepo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	c := makeCampaign(t, campaignRepo)
	item, err := campaignRepo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Attach the run.
	linked, err := campaignRepo.SetCampaignItemRun(ctx, item.ID, &r.ID)
	if err != nil {
		t.Fatalf("set item run: %v", err)
	}
	if linked.RunID == nil || *linked.RunID != r.ID {
		t.Fatalf("SetCampaignItemRun RunID = %v, want %s", linked.RunID, r.ID)
	}

	// Forward discovery.
	got, err := campaignRepo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.RunID == nil || *got.RunID != r.ID {
		t.Errorf("GetCampaignItem.RunID = %v, want %s", got.RunID, r.ID)
	}

	// Reverse discovery: which campaign item owns this run.
	forRun, err := campaignRepo.ListCampaignItemsForRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("list items for run: %v", err)
	}
	if len(forRun) != 1 || forRun[0].ID != item.ID {
		t.Fatalf("ListCampaignItemsForRun = %+v, want one item %s", forRun, item.ID)
	}

	// SetCampaignItemRun on a missing item is ErrNotFound.
	if _, err := campaignRepo.SetCampaignItemRun(ctx, uuid.New(), &r.ID); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("SetCampaignItemRun(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPostgres_RunDelete_SetsItemRunNull asserts the ON DELETE SET NULL FK:
// deleting the linked run nulls the item's run_id and preserves the item row
// (campaign history survives a run deletion).
func TestPostgres_RunDelete_SetsItemRunNull(t *testing.T) {
	pool := pgtest.NewPool(t)
	campaignRepo := campaign.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	ctx := context.Background()

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	c := makeCampaign(t, campaignRepo)
	item, err := campaignRepo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := campaignRepo.SetCampaignItemRun(ctx, item.ID, &r.ID); err != nil {
		t.Fatalf("set item run: %v", err)
	}

	// Delete the run row directly (no run-repo delete verb exists).
	if _, err := pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, r.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}

	// The item survives with run_id nulled.
	got, err := campaignRepo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item after run delete: %v", err)
	}
	if got.RunID != nil {
		t.Errorf("item RunID after run delete = %v, want nil (ON DELETE SET NULL)", got.RunID)
	}
}

// TestPostgres_CampaignDelete_CascadesItems asserts the ON DELETE CASCADE FK:
// deleting a campaign removes its items.
func TestPostgres_CampaignDelete_CascadesItems(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	item, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID,
		IssueRef:   "issue:1441",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM campaigns WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("delete campaign: %v", err)
	}

	if _, err := repo.GetCampaignItem(ctx, item.ID); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("item after campaign delete err = %v, want ErrNotFound (ON DELETE CASCADE)", err)
	}
}
