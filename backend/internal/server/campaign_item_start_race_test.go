package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Race tests (#5266 slice 3): the operator item start and the campaign
// driver race for one item over REAL Postgres, and exactly one of them mints.
// The interleaving is deterministic, never timed: the hook on the fake GitHub
// contents route, which only GetWorkflowSpec inside StartRunForCampaignIssue
// reaches on an item start (after the claim), runs the OTHER starter while
// the first one is inside its mint.
//
// U4b: the campaign and its item are seeded through the pg repository
// (newItemStartServerPG) and the hook is armed only AFTER seeding, so neither
// the create-time admission screen nor the hotfix route can consume it. X6b:
// the driver Ticker's Now and the server's nowFunc are the same
// itemStartClock, so every claim stamp and expiry comparison uses one source.

// raceStarter is the driver's RunStarter over the real server: it mints
// through s.StartRunForCampaignIssue with the driver's AutoDispatch stamp and
// cancels through s.CancelCampaignRun (the serve.go campaignRunStarter shape).
type raceStarter struct {
	f *itemStartPG
}

func (r raceStarter) StartCampaignRun(ctx context.Context, item *campaign.Item, c *campaign.Campaign) (*run.Run, error) {
	rn, err := r.f.s.StartRunForCampaignIssue(ctx, StartRunForCampaignIssueParams{
		Repo: c.Repo, IssueRef: item.IssueRef, WorkflowID: "feature_change",
		RunnerKind: run.RunnerKindLocal, WorkingDir: seededCampaignWorkingDir, AutoDispatch: true,
	})
	if err != nil {
		var incomplete *RunStartIncompleteError
		if errors.As(err, &incomplete) {
			return nil, &campaigndriver.StartIncompleteError{RunID: incomplete.RunID, Err: err}
		}
		return nil, err
	}
	return rn, nil
}

func (r raceStarter) CancelCampaignRun(ctx context.Context, runID uuid.UUID) error {
	return r.f.s.CancelCampaignRun(ctx, runID)
}

func newRaceTicker(f *itemStartPG) *campaigndriver.Ticker {
	return &campaigndriver.Ticker{
		Campaigns:   f.fc,
		Runs:        f.runs,
		Starter:     raceStarter{f: f},
		Audit:       f.aud,
		MaxParallel: 4,
		Now:         f.clock.Now,
	}
}

const raceItemStartBody = `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`

// assertOneLinkedRun asserts by read-back: exactly one run row, the item
// linked to it, the armed spec route reached exactly once, and zero
// campaign_item_run_abandoned rows (the loser minted nothing to abandon).
func (f *itemStartPG) assertOneLinkedRun(t *testing.T) *run.Run {
	t.Helper()
	runID, _ := f.onlyRun(t)
	it := f.readItem(t)
	if it.RunID == nil || *it.RunID != runID {
		t.Fatalf("item run_id = %v, want the one run %s", it.RunID, runID)
	}
	if it.State != campaign.ItemStateRunning {
		t.Errorf("item state = %s, want running", it.State)
	}
	if n := f.gh.armedHits(); n != 1 {
		t.Errorf("armed spec route hits = %d, want exactly 1 (only the winner mints)", n)
	}
	if ps := f.aud.payloads(t, categoryCampaignItemRunAbandoned); len(ps) != 0 {
		t.Errorf("campaign_item_run_abandoned rows = %v, want none", ps)
	}
	if n := f.claimRows(t); n != 0 {
		t.Errorf("claim rows left = %d, want 0 (the winner's link consumed its claim)", n)
	}
	rn, err := f.runs.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("read run back: %v", err)
	}
	return rn
}

// TestStartCampaignItemRun_RacesDriver_OperatorClaimsFirst_DriverMintsNothing:
// inside the operator's HTTP start (after its claim, inside its mint), the
// driver ticks once. The driver's claim finds the operator's live claim and
// skips without minting; the operator's start links its run.
func TestStartCampaignItemRun_RacesDriver_OperatorClaimsFirst_DriverMintsNothing(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	tk := newRaceTicker(f)
	f.gh.arm(func() { tk.Tick(context.Background()) })

	w := postStartItemRun(t, f.s, f.c.ID, raceItemStartBody)

	if w.Code != http.StatusCreated {
		t.Fatalf("operator start = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	if !f.gh.fired.Load() {
		t.Fatal("the hook never fired, so the driver never raced the operator")
	}
	rn := f.assertOneLinkedRun(t)
	if rn.AutoDispatch {
		t.Error("the operator's unflagged run carries the auto_dispatch mark, want unmarked")
	}
}

// TestStartCampaignItemRun_RacesDriver_DriverClaimsFirst_Operator409MintsNothing:
// inside the driver's mint (after its claim), the operator POSTs the item
// start. The operator's claim finds the driver's live claim and answers 409
// start_in_progress without minting; the driver's run links and keeps its
// auto_dispatch mark.
func TestStartCampaignItemRun_RacesDriver_DriverClaimsFirst_Operator409MintsNothing(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	tk := newRaceTicker(f)
	var opCode int
	var opErrCode string
	var opDetails map[string]any
	f.gh.arm(func() {
		w := postStartItemRun(t, f.s, f.c.ID, raceItemStartBody)
		opCode = w.Code
		if w.Code != http.StatusCreated {
			opErrCode, opDetails = decodeItemStartError(t, w)
		}
	})

	tk.Tick(context.Background())

	if !f.gh.fired.Load() {
		t.Fatal("the hook never fired, so the operator never raced the driver")
	}
	if opCode != http.StatusConflict || opErrCode != "item_not_eligible" {
		t.Fatalf("operator start = %d %s, want 409 item_not_eligible", opCode, opErrCode)
	}
	if opDetails["start_in_progress"] != true || opDetails["claim_starter"] != campaign.StarterCampaignDriver {
		t.Errorf("operator 409 details = %v, want start_in_progress true and claim_starter campaign_driver", opDetails)
	}
	rn := f.assertOneLinkedRun(t)
	if !rn.AutoDispatch {
		t.Error("the driver's surviving run lost its auto_dispatch mark")
	}
}
