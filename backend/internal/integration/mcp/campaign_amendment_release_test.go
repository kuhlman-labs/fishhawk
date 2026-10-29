package mcpe2e_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// e2eCampaignAmendment decodes one campaign pending-amendment entry the way an
// MCP consumer would, independently of the mcpserver mirror: a json-tag drift
// between the server's campaignPendingAmendmentPayload and the client mirror
// drops a field here (E86.85 / #4486).
type e2eCampaignAmendment struct {
	IssueRef     string `json:"issue_ref"`
	ItemRunID    string `json:"item_run_id"`
	StageState   string `json:"stage_state"`
	DecisionLive bool   `json:"decision_live"`
	Amendment    struct {
		ID      string `json:"id"`
		RunID   string `json:"run_id"`
		StageID string `json:"stage_id"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Paths   []struct {
			Path      string `json:"path"`
			Operation string `json:"operation"`
		} `json:"paths"`
	} `json:"amendment"`
}

// e2eAmendmentRelease is the slice of an await verb's amendment_pending
// release this test reads.
type e2eAmendmentRelease struct {
	Status                     string `json:"status"`
	PendingAmendmentRunID      string `json:"pending_amendment_run_id"`
	PendingAmendmentCampaignID string `json:"pending_amendment_campaign_id"`
	PendingAmendmentIssueRef   string `json:"pending_amendment_issue_ref"`
	PendingAmendment           *struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	} `json:"pending_amendment"`
	NextStep *struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	} `json:"next_step"`
	Message string `json:"message"`
}

// campaignAmendmentHarness is the real stack for the #4486 tests: real Postgres
// run, campaign, scope-amendment and audit repos behind the real server.
// Approval condition C3: campaign.NewPostgresRepository is wired, the campaign
// and items are seeded straight through it, and CampaignDependencyRefresh is
// left unset, so no forge call is ever made.
type campaignAmendmentHarness struct {
	fx        *e2eFixture
	ctx       context.Context
	url       string
	camps     campaign.Repository
	amends    scopeamendment.Repository
	auditRepo audit.Repository
	campaign  *campaign.Campaign
}

func newCampaignAmendmentHarness(t *testing.T, ctx context.Context) *campaignAmendmentHarness {
	t.Helper()
	fx := newFixture(t)
	h := &campaignAmendmentHarness{
		fx:        fx,
		ctx:       ctx,
		camps:     campaign.NewPostgresRepository(fx.pool),
		amends:    scopeamendment.NewPostgresRepository(fx.pool),
		auditRepo: audit.NewPostgresRepository(fx.pool),
	}
	srv := server.New(server.Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            fx.runRepo,
		AuditRepo:          h.auditRepo,
		CampaignRepo:       h.camps,
		ScopeAmendmentRepo: h.amends,
		MCPTokenRepo:       fx.mcpTokenRepo,
		APITokenRepo:       fx.apitokenRepo,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	h.url = httpSrv.URL
	c, err := h.camps.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: "acme/widgets", EpicRef: "issue:100", PausePolicy: campaign.PausePolicyPauseCampaign,
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if c, err = h.camps.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("campaign → running: %v", err)
	}
	h.campaign = c
	return h
}

// run mints a running run on acme/widgets (optionally a slice or a recovery
// child) with one implement stage walked to stageState.
func (h *campaignAmendmentHarness) run(t *testing.T, parent, decomposedFrom *uuid.UUID, slice *int, stagePath ...runpkg.StageState) (*runpkg.Run, *runpkg.Stage) {
	t.Helper()
	r, err := h.fx.runRepo.CreateRun(h.ctx, runpkg.CreateRunParams{
		Repo: "acme/widgets", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: runpkg.TriggerCLI, RunnerKind: runpkg.RunnerKindLocal,
		ParentRunID: parent, DecomposedFrom: decomposedFrom, SliceIndex: slice,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := h.fx.runRepo.TransitionRun(h.ctx, r.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("run → running: %v", err)
	}
	st, err := h.fx.runRepo.CreateStage(h.ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	for _, to := range stagePath {
		if st, err = h.fx.runRepo.TransitionStage(h.ctx, st.ID, to, nil); err != nil {
			t.Fatalf("stage → %s: %v", to, err)
		}
	}
	return r, st
}

// item seeds a campaign item linked to runID and walks it through states.
func (h *campaignAmendmentHarness) item(t *testing.T, ref string, pos int, runID uuid.UUID, states ...campaign.ItemState) {
	t.Helper()
	it, err := h.camps.CreateCampaignItem(h.ctx, campaign.CreateCampaignItemParams{
		CampaignID: h.campaign.ID, IssueRef: ref, DependsOn: []string{}, Position: pos,
	})
	if err != nil {
		t.Fatalf("CreateCampaignItem %s: %v", ref, err)
	}
	id := runID
	if _, err := h.camps.SetCampaignItemRun(h.ctx, it.ID, &id); err != nil {
		t.Fatalf("SetCampaignItemRun %s: %v", ref, err)
	}
	for _, to := range states {
		if _, err := h.camps.TransitionCampaignItem(h.ctx, it.ID, to); err != nil {
			t.Fatalf("item %s → %s: %v", ref, to, err)
		}
	}
}

// amend files a PENDING amendment on the run's stage.
func (h *campaignAmendmentHarness) amend(t *testing.T, runID, stageID uuid.UUID, path string) *scopeamendment.Amendment {
	t.Helper()
	a, err := h.amends.Create(h.ctx, scopeamendment.CreateParams{
		RunID: runID, StageID: stageID, Reason: "the coupled test must change",
		Paths: []scopeamendment.PathEntry{{Path: path, Operation: "modify"}},
	})
	if err != nil {
		t.Fatalf("create amendment: %v", err)
	}
	return a
}

// callTool calls a tool through the real binary and decodes its structured
// result into dst.
func callTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, dst any) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, toolContentString(t, res))
	}
	decodeStructured(t, res, dst)
}

// campaignStatusAmendments reads fishhawk_get_campaign_status's
// pending_amendments block through the real binary.
func campaignStatusAmendments(t *testing.T, ctx context.Context, session *mcp.ClientSession, campaignID uuid.UUID) []e2eCampaignAmendment {
	t.Helper()
	var st struct {
		PendingAmendments *struct {
			Items     []e2eCampaignAmendment `json:"items"`
			Truncated bool                   `json:"truncated"`
		} `json:"pending_amendments"`
	}
	callTool(t, ctx, session, "fishhawk_get_campaign_status", map[string]any{"campaign_id": campaignID.String()}, &st)
	if st.PendingAmendments == nil {
		t.Fatal("fishhawk_get_campaign_status carried no pending_amendments block for an operator token with read:audit")
	}
	if st.PendingAmendments.Truncated {
		t.Error("pending_amendments.truncated = true on a five-run campaign")
	}
	return st.PendingAmendments.Items
}

func assertCampaignRelease(t *testing.T, got e2eAmendmentRelease, filing uuid.UUID, am *scopeamendment.Amendment, campaignID uuid.UUID, issueRef string) {
	t.Helper()
	if got.Status != "amendment_pending" {
		t.Fatalf("status = %q (message %q), want amendment_pending from the campaign-wide arm", got.Status, got.Message)
	}
	if got.PendingAmendmentRunID != filing.String() {
		t.Errorf("pending_amendment_run_id = %q, want the filing run %s", got.PendingAmendmentRunID, filing)
	}
	if got.PendingAmendmentCampaignID != campaignID.String() {
		t.Errorf("pending_amendment_campaign_id = %q, want %s", got.PendingAmendmentCampaignID, campaignID)
	}
	if got.PendingAmendmentIssueRef != issueRef {
		t.Errorf("pending_amendment_issue_ref = %q, want %s", got.PendingAmendmentIssueRef, issueRef)
	}
	if got.PendingAmendment == nil || got.PendingAmendment.ID != am.ID.String() || got.PendingAmendment.RunID != filing.String() {
		t.Errorf("pending_amendment = %+v, want %s on %s", got.PendingAmendment, am.ID, filing)
	}
	if got.NextStep == nil || got.NextStep.Action != "fishhawk_decide_scope_amendment" ||
		got.NextStep.Params["run_id"] != filing.String() || got.NextStep.Params["amendment_id"] != am.ID.String() {
		t.Errorf("next_step = %+v, want decide %s on the filing run %s", got.NextStep, am.ID, filing)
	}
}

// TestE2E_AwaitChildrenReleasesOnOtherParentsSliceAmendment is the #4486
// cross-layer test: persistence → ancestry walk → REST payload → MCP client
// mirror → tool result, through the REAL fishhawk-mcp binary with the operator
// token. Campaign C (acme/widgets): item issue:1 → parent A with running slice
// A0 (plan_decomposed on A names it); item issue:2 → parent B with running
// slice B0 holding a PENDING amendment, plus a recovery child R of B carrying
// ONLY parent_run_id (approval condition C4) holding a later one.
// fishhawk_await_children on A — whose own child has nothing to release on —
// releases amendment_pending on B0's (the oldest), and
// fishhawk_get_campaign_status lists both, decision_live true.
func TestE2E_AwaitChildrenReleasesOnOtherParentsSliceAmendment(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h := newCampaignAmendmentHarness(t, ctx)

	running := []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning}
	zero := 0
	parentA, _ := h.run(t, nil, nil, nil, runpkg.StageStateAwaitingChildren)
	sliceA0, _ := h.run(t, &parentA.ID, &parentA.ID, &zero, running...)
	appendParentAudit(t, ctx, h.auditRepo, parentA.ID, "plan_decomposed", map[string]any{
		"child_run_ids":          []string{sliceA0.ID.String()},
		"effective_max_parallel": 0,
	})
	parentB, _ := h.run(t, nil, nil, nil, runpkg.StageStateAwaitingChildren)
	sliceB0, sliceB0Stage := h.run(t, &parentB.ID, &parentB.ID, &zero, running...)
	recoveryR, recoveryRStage := h.run(t, &parentB.ID, nil, nil, running...)
	h.item(t, "issue:1", 0, parentA.ID, campaign.ItemStateRunning)
	h.item(t, "issue:2", 1, parentB.ID, campaign.ItemStateRunning)
	amB0 := h.amend(t, sliceB0.ID, sliceB0Stage.ID, "pkg/b0.go")
	time.Sleep(5 * time.Millisecond) // a strictly later requested_at
	amR := h.amend(t, recoveryR.ID, recoveryRStage.ID, "pkg/r.go")

	session := connectMCPClient(t, ctx, h.fx.mcpBinary, h.fx.operatorTok, h.url)
	var rel e2eAmendmentRelease
	callTool(t, ctx, session, "fishhawk_await_children", map[string]any{"run_id": parentA.ID.String(), "timeout_seconds": 5}, &rel)
	assertCampaignRelease(t, rel, sliceB0.ID, amB0, h.campaign.ID, "issue:2")

	items := campaignStatusAmendments(t, ctx, session, h.campaign.ID)
	if len(items) != 2 {
		t.Fatalf("pending_amendments.items = %+v, want B0's and R's (oldest first)", items)
	}
	for i, want := range []struct {
		am  *scopeamendment.Amendment
		run uuid.UUID
	}{{amB0, sliceB0.ID}, {amR, recoveryR.ID}} {
		got := items[i]
		if got.Amendment.ID != want.am.ID.String() || got.Amendment.RunID != want.run.String() {
			t.Errorf("items[%d] amendment = %s on %s, want %s on %s", i, got.Amendment.ID, got.Amendment.RunID, want.am.ID, want.run)
		}
		if got.IssueRef != "issue:2" || got.ItemRunID != parentB.ID.String() {
			t.Errorf("items[%d] issue_ref/item_run_id = %q/%q, want issue:2/%s", i, got.IssueRef, got.ItemRunID, parentB.ID)
		}
		if !got.DecisionLive || got.StageState != "running" || got.Amendment.Status != "pending" {
			t.Errorf("items[%d] decision_live/stage_state/status = %v/%q/%q, want true/running/pending", i, got.DecisionLive, got.StageState, got.Amendment.Status)
		}
		if len(got.Amendment.Paths) != 1 || got.Amendment.Reason == "" {
			t.Errorf("items[%d] paths/reason = %+v/%q, want the full row", i, got.Amendment.Paths, got.Amendment.Reason)
		}
	}
}

// TestE2E_AwaitStageReleasesOnCancelledItemsAmendment is approval condition
// C1's wait half: a CANCELLED campaign item whose linked run is still running
// with a pending amendment still releases a campaign-scoped
// fishhawk_await_stage on a different item's run, and is listed on the status
// block.
func TestE2E_AwaitStageReleasesOnCancelledItemsAmendment(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h := newCampaignAmendmentHarness(t, ctx)

	running := []runpkg.StageState{runpkg.StageStateDispatched, runpkg.StageStateRunning}
	runX, _ := h.run(t, nil, nil, nil, running...)
	runY, stageY := h.run(t, nil, nil, nil, running...)
	h.item(t, "issue:1", 0, runX.ID, campaign.ItemStateRunning)
	h.item(t, "issue:2", 1, runY.ID, campaign.ItemStateCancelled)
	amY := h.amend(t, runY.ID, stageY.ID, "pkg/y.go")

	session := connectMCPClient(t, ctx, h.fx.mcpBinary, h.fx.operatorTok, h.url)
	var rel e2eAmendmentRelease
	callTool(t, ctx, session, "fishhawk_await_stage", map[string]any{"run_id": runX.ID.String(), "stage": "implement", "timeout_seconds": 5}, &rel)
	assertCampaignRelease(t, rel, runY.ID, amY, h.campaign.ID, "issue:2")

	items := campaignStatusAmendments(t, ctx, session, h.campaign.ID)
	if len(items) != 1 || items[0].Amendment.ID != amY.ID.String() || !items[0].DecisionLive {
		t.Fatalf("pending_amendments.items = %+v, want the cancelled item's live amendment %s", items, amY.ID)
	}
}
