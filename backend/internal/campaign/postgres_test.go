package campaign_test

import (
	"context"
	"errors"
	"sync"
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

// TestPostgres_ConcurrentTransitionCampaign_ExactlyOneWins exercises the
// SELECT … FOR UPDATE serialization that repository.go documents as a
// load-bearing contract ("two concurrent transition calls observing the same
// prior state cannot both succeed"). Two goroutines race two DIFFERENT
// terminal targets from running; exactly one must win and the loser must
// observe the now-terminal post-transition state and fail with
// InvalidTransitionError (terminal → terminal is forbidden). Mirrors
// run.TestPostgres_ConcurrentDifferentTargets_ExactlyOneWins.
func TestPostgres_ConcurrentTransitionCampaign_ExactlyOneWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateSucceeded)
		results[0] = err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateFailed)
		results[1] = err
	}()
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		var ite campaign.InvalidTransitionError
		if !errors.As(err, &ite) {
			t.Errorf("non-winner err = %v, want InvalidTransitionError", err)
			continue
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Errorf("winners=%d losers=%d, want 1 of each", winners, losers)
	}

	// The campaign settled in exactly one terminal state.
	final, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get final campaign: %v", err)
	}
	if !final.State.IsTerminal() {
		t.Errorf("final state = %q, want a terminal state", final.State)
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

// TestPostgres_ConcurrentTransitionCampaignItem_ExactlyOneWins is the
// campaign_item analogue of the campaign concurrency test: it covers the
// LockCampaignItemForUpdate serialization path that CI cannot infer from the
// sequential happy-path cases. Two goroutines race succeeded vs failed from
// running; exactly one wins and the loser observes the now-terminal state via
// InvalidTransitionError.
func TestPostgres_ConcurrentTransitionCampaignItem_ExactlyOneWins(t *testing.T) {
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
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateSucceeded)
		results[0] = err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateFailed)
		results[1] = err
	}()
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		var ite campaign.InvalidTransitionError
		if !errors.As(err, &ite) {
			t.Errorf("non-winner err = %v, want InvalidTransitionError", err)
			continue
		}
		losers++
	}
	if winners != 1 || losers != 1 {
		t.Errorf("winners=%d losers=%d, want 1 of each", winners, losers)
	}

	final, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get final item: %v", err)
	}
	if !final.State.IsTerminal() {
		t.Errorf("final item state = %q, want a terminal state", final.State)
	}
}

// TestPostgres_CreateCampaign_PausePolicy covers the pause_policy column added
// by 0040: makeCampaign (no policy set) defaults to the conservative
// pause_campaign, and an explicit pause_item round-trips through create + read.
func TestPostgres_CreateCampaign_PausePolicy(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	// Zero policy normalizes to the block-the-campaign default.
	def := makeCampaign(t, repo)
	if def.PausePolicy != campaign.PausePolicyPauseCampaign {
		t.Errorf("default PausePolicy = %q, want pause_campaign", def.PausePolicy)
	}

	// Explicit pause_item is preserved end-to-end.
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:        "kuhlman-labs/fishhawk",
		EpicRef:     "issue:1439",
		PausePolicy: campaign.PausePolicyPauseItem,
	})
	if err != nil {
		t.Fatalf("create campaign with pause_item: %v", err)
	}
	if c.PausePolicy != campaign.PausePolicyPauseItem {
		t.Errorf("created PausePolicy = %q, want pause_item", c.PausePolicy)
	}
	got, err := repo.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.PausePolicy != campaign.PausePolicyPauseItem {
		t.Errorf("read-back PausePolicy = %q, want pause_item", got.PausePolicy)
	}
}

// TestPostgres_TransitionCampaign_Paused covers the campaign-level paused
// overlay edges admitted by 0040: running → paused, then resume paused →
// running. The insert succeeding proves the widened campaigns_state_check.
func TestPostgres_TransitionCampaign_Paused(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	ctx := context.Background()

	c := makeCampaign(t, repo)
	if _, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}
	paused, err := repo.TransitionCampaign(ctx, c.ID, campaign.StatePaused)
	if err != nil {
		t.Fatalf("running→paused: %v", err)
	}
	if paused.State != campaign.StatePaused {
		t.Errorf("state after pause = %q, want paused", paused.State)
	}
	// Resume.
	running, err := repo.TransitionCampaign(ctx, c.ID, campaign.StateRunning)
	if err != nil {
		t.Fatalf("paused→running (resume): %v", err)
	}
	if running.State != campaign.StateRunning {
		t.Errorf("state after resume = %q, want running", running.State)
	}
}

// TestPostgres_PauseCampaignItem is the round-trip done-means for the item
// pause carrier: a running item paused via PauseCampaignItem persists state
// 'paused' (proving the widened campaign_items_state_check) with the
// PauseReason JSONB intact on both the returned row and an independent read.
// A re-pause is an idempotent no-op preserving the first reason, and the item
// resumes paused → running.
func TestPostgres_PauseCampaignItem(t *testing.T) {
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
	if item.PauseReason != nil {
		t.Errorf("new item PauseReason = %v, want nil", item.PauseReason)
	}
	// Only running → paused is valid; advance the item first.
	if _, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("→running: %v", err)
	}

	runID := uuid.New()
	stageID := uuid.New()
	reason := campaign.PauseReason{
		PageEvent: "campaign_gate_paged",
		RunID:     &runID,
		StageID:   &stageID,
		Gate:      "implement_review",
	}
	paused, err := repo.PauseCampaignItem(ctx, item.ID, reason)
	if err != nil {
		t.Fatalf("pause item: %v", err)
	}
	if paused.State != campaign.ItemStatePaused {
		t.Errorf("state after pause = %q, want paused", paused.State)
	}
	assertReason := func(label string, pr *campaign.PauseReason) {
		t.Helper()
		if pr == nil {
			t.Fatalf("%s: PauseReason = nil, want round-tripped reason", label)
		}
		if pr.PageEvent != "campaign_gate_paged" || pr.Gate != "implement_review" {
			t.Errorf("%s: PauseReason = %+v, want page=campaign_gate_paged gate=implement_review", label, pr)
		}
		if pr.RunID == nil || *pr.RunID != runID {
			t.Errorf("%s: PauseReason.RunID = %v, want %s", label, pr.RunID, runID)
		}
		if pr.StageID == nil || *pr.StageID != stageID {
			t.Errorf("%s: PauseReason.StageID = %v, want %s", label, pr.StageID, stageID)
		}
	}
	assertReason("returned", paused.PauseReason)

	// Independent read-back carries the same reason.
	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get paused item: %v", err)
	}
	assertReason("read-back", got.PauseReason)

	// Idempotent re-pause: unchanged item, first reason preserved.
	re, err := repo.PauseCampaignItem(ctx, item.ID, campaign.PauseReason{PageEvent: "different"})
	if err != nil {
		t.Fatalf("re-pause: %v", err)
	}
	assertReason("idempotent-repause", re.PauseReason)

	// Resume re-engages the item.
	resumed, err := repo.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning)
	if err != nil {
		t.Fatalf("paused→running (resume): %v", err)
	}
	if resumed.State != campaign.ItemStateRunning {
		t.Errorf("state after resume = %q, want running", resumed.State)
	}
}

// TestPostgres_CampaignItem_MalformedPauseReason_Tolerated asserts the
// rowToCampaignItem tolerance branch for pause_reason: a JSONB blob that is
// valid JSONB but not a PauseReason object (so json.Unmarshal fails) is
// DROPPED to a nil *PauseReason rather than failing the read — same posture as
// the depends_on tolerance. Written via raw SQL because the repo write path
// always marshals a well-formed object.
func TestPostgres_CampaignItem_MalformedPauseReason_Tolerated(t *testing.T) {
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

	// Overwrite pause_reason with valid JSONB that is NOT a PauseReason object.
	if _, err := pool.Exec(ctx,
		`UPDATE campaign_items SET pause_reason = '["not","an-object"]'::jsonb WHERE id = $1`,
		item.ID,
	); err != nil {
		t.Fatalf("write malformed pause_reason: %v", err)
	}

	got, err := repo.GetCampaignItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item with malformed pause_reason should not error: %v", err)
	}
	if got.PauseReason != nil {
		t.Errorf("malformed pause_reason = %+v, want nil (dropped, not surfaced)", got.PauseReason)
	}
}

// TestPostgres_PauseCampaignItem_InvalidAndMissing covers the two error
// branches of PauseCampaignItem: pausing a non-running (pending) item is
// refused with InvalidTransitionError (only running → paused is valid), and a
// missing item is ErrNotFound.
func TestPostgres_PauseCampaignItem_InvalidAndMissing(t *testing.T) {
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

	// pending → paused is refused (item must be running first).
	_, err = repo.PauseCampaignItem(ctx, item.ID, campaign.PauseReason{PageEvent: "x"})
	var ite campaign.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("pause(pending) err = %v, want InvalidTransitionError", err)
	}
	if ite.Kind != "campaign_item" || ite.To != "paused" {
		t.Errorf("InvalidTransitionError = %+v, want campaign_item →paused", ite)
	}

	// A missing item is ErrNotFound.
	if _, err := repo.PauseCampaignItem(ctx, uuid.New(), campaign.PauseReason{}); !errors.Is(err, campaign.ErrNotFound) {
		t.Errorf("pause(missing) err = %v, want ErrNotFound", err)
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
