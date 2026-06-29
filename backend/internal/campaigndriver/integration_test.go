package campaigndriver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seamStarter is the integration test's RunStarter: it mints a REAL run row
// through the run.Repository so the driver's GetRun terminal detection reads
// genuine persisted state. It records the run id it created for each issue ref
// so the test can drive that run to a terminal state between ticks. This is
// the "real/seam run-starter" the plan calls for — exercising the engine →
// campaign persistence → run creation → audit emission seam end-to-end over a
// real Postgres, where per-layer unit tests would pass while the seam breaks
// (#618).
type seamStarter struct {
	runs    run.Repository
	byRef   map[string]uuid.UUID
	started []string
}

func newSeamStarter(runs run.Repository) *seamStarter {
	return &seamStarter{runs: runs, byRef: map[string]uuid.UUID{}}
}

func (s *seamStarter) StartCampaignRun(ctx context.Context, item *campaign.Item, c *campaign.Campaign) (*run.Run, error) {
	ref := item.IssueRef
	r, err := s.runs.CreateRun(ctx, run.CreateRunParams{
		Repo:          c.Repo,
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &ref,
	})
	if err != nil {
		return nil, err
	}
	s.byRef[ref] = r.ID
	s.started = append(s.started, ref)
	return r, nil
}

// driveRunTerminal advances a run pending → running → succeeded so the next
// tick observes it as terminal-succeeded.
func driveRunTerminal(t *testing.T, runs run.Repository, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := runs.TransitionRun(ctx, runID, run.StateRunning); err != nil {
		t.Fatalf("transition run to running: %v", err)
	}
	if _, err := runs.TransitionRun(ctx, runID, run.StateSucceeded); err != nil {
		t.Fatalf("transition run to succeeded: %v", err)
	}
}

// TestIntegration_DependsOnCampaign_AdvancesAcrossTicks asserts the
// cross-boundary done-means of #1444 over a real Postgres: a 2-issue
// depends_on campaign starts ONLY the predecessor on tick 1; once the
// predecessor's run reaches terminal, tick 2 settles it and starts the
// dependent (the predecessor merging unblocks the dependent); tick 3 settles
// the dependent and advances the campaign to succeeded.
func TestIntegration_DependsOnCampaign_AdvancesAcrossTicks(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	campaigns := campaign.NewPostgresRepository(pool)
	runs := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	// Assemble + persist a 2-issue depends_on campaign in state running:
	// issue:2 depends on issue:1.
	c, err := campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo:    "kuhlman-labs/fishhawk",
		EpicRef: "issue:1000",
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	pred, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID, IssueRef: "issue:1",
	})
	if err != nil {
		t.Fatalf("create predecessor item: %v", err)
	}
	dep, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID, IssueRef: "issue:2", DependsOn: []string{"issue:1"},
	})
	if err != nil {
		t.Fatalf("create dependent item: %v", err)
	}
	if _, err := campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("transition campaign to running: %v", err)
	}

	starter := newSeamStarter(runs)
	tk := &campaigndriver.Ticker{
		Campaigns:   campaigns,
		Runs:        runs,
		Starter:     starter,
		Audit:       auditRepo,
		MaxParallel: 4,
	}

	// --- tick 1: ONLY the predecessor starts; the dependent stays blocked ---
	tk.Tick(ctx)

	if len(starter.started) != 1 || starter.started[0] != "issue:1" {
		t.Fatalf("tick 1 started %v, want only issue:1", starter.started)
	}
	predItem := getItem(t, campaigns, c.ID, pred.ID)
	if predItem.State != campaign.ItemStateRunning || predItem.RunID == nil {
		t.Fatalf("tick 1: predecessor = %s runID=%v, want running+linked", predItem.State, predItem.RunID)
	}
	depItem := getItem(t, campaigns, c.ID, dep.ID)
	if depItem.State != campaign.ItemStatePending {
		t.Fatalf("tick 1: dependent = %s, want still pending (blocked)", depItem.State)
	}

	// Drive the predecessor's run to terminal-succeeded.
	driveRunTerminal(t, runs, starter.byRef["issue:1"])

	// --- tick 2: predecessor settles AND the dependent now starts ---
	tk.Tick(ctx)

	predItem = getItem(t, campaigns, c.ID, pred.ID)
	if predItem.State != campaign.ItemStateSucceeded {
		t.Fatalf("tick 2: predecessor = %s, want succeeded", predItem.State)
	}
	if len(starter.started) != 2 || starter.started[1] != "issue:2" {
		t.Fatalf("tick 2 started %v, want issue:2 second", starter.started)
	}
	depItem = getItem(t, campaigns, c.ID, dep.ID)
	if depItem.State != campaign.ItemStateRunning || depItem.RunID == nil {
		t.Fatalf("tick 2: dependent = %s runID=%v, want running+linked", depItem.State, depItem.RunID)
	}

	// Drive the dependent's run to terminal-succeeded.
	driveRunTerminal(t, runs, starter.byRef["issue:2"])

	// --- tick 3: dependent settles and the campaign advances to succeeded ---
	tk.Tick(ctx)

	got, err := campaigns.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if got.State != campaign.StateSucceeded {
		t.Fatalf("tick 3: campaign = %s, want succeeded", got.State)
	}

	// The campaign-level audit surface recorded the advancement on the global
	// chain: a campaign_advanced entry for the running→succeeded transition.
	assertCampaignAdvanced(t, auditRepo, c.ID)
}

func getItem(t *testing.T, repo campaign.Repository, campaignID, itemID uuid.UUID) *campaign.Item {
	t.Helper()
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.ID == itemID {
			return it
		}
	}
	t.Fatalf("item %s not found", itemID)
	return nil
}

func assertCampaignAdvanced(t *testing.T, au audit.Repository, campaignID uuid.UUID) {
	t.Helper()
	entries, err := au.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("list global audit: %v", err)
	}
	for _, e := range entries {
		if e.Category != "campaign_advanced" {
			continue
		}
		if strings.Contains(string(e.Payload), campaignID.String()) {
			return
		}
	}
	t.Fatalf("no campaign_advanced audit entry found for campaign %s", campaignID)
}
