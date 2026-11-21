package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stagesReadCountingRepo counts ListStagesForRun calls and fails every call
// after failAfter (a negative failAfter never fails), so a test can fail only
// DriveCampaignRunGate's own read after AutoDriveRunGate's reads succeed.
type stagesReadCountingRepo struct {
	*autoDriveRepo
	mu        sync.Mutex
	calls     int
	failAfter int
}

func (r *stagesReadCountingRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	r.calls++
	n, fa := r.calls, r.failAfter
	r.mu.Unlock()
	if fa >= 0 && n > fa {
		return nil, errors.New("stages read down")
	}
	return r.autoDriveRepo.ListStagesForRun(ctx, runID)
}

func (r *stagesReadCountingRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newCountingAutoDriveServer is newAutoDriveServer over a stagesReadCountingRepo.
func newCountingAutoDriveServer(t *testing.T) (*Server, *stagesReadCountingRepo, *auditFake) {
	t.Helper()
	inner := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	repo := &stagesReadCountingRepo{autoDriveRepo: inner, failAfter: -1}
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	return s, repo, au
}

func TestDriveCampaignRunGate_HumanQuorumGate_CarriesDecision(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRunWithSpec(t, s, repo, autoDriveEscalationSpecYAML(""))
	plan := stages[0]
	seedCleanPlanApproval(t, au, runID)

	out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateHumanQuorumRequired || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want decision_required human_quorum_required approve", out)
	}
	if out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want neither acted nor paged", out)
	}
	if !out.GateKnown || out.GateStageID == nil || *out.GateStageID != plan.ID {
		t.Fatalf("gate = known:%v stage:%v, want known plan stage %s", out.GateKnown, out.GateStageID, plan.ID)
	}
	if len(out.ParkedGateStageIDs) != 1 || out.ParkedGateStageIDs[0] != plan.ID {
		t.Fatalf("parked = %v, want [%s]", out.ParkedGateStageIDs, plan.ID)
	}
	if out.Note == "" {
		t.Fatal("note is empty; the translation dropped it")
	}
}

func TestDriveCampaignRunGate_NoGateParked(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateRunning

	out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if out.DecisionRequired || out.Acted || out.Paged {
		t.Fatalf("outcome = %+v, want observe-only", out)
	}
	if !out.GateKnown || out.GateStageID != nil || len(out.ParkedGateStageIDs) != 0 {
		t.Fatalf("gate = known:%v stage:%v parked:%v, want known with nothing parked", out.GateKnown, out.GateStageID, out.ParkedGateStageIDs)
	}
}

// A parked review stage is listed as parked (so a run-level key does not
// clear) but is never the gate stage.
func TestDriveCampaignRunGate_ParkedReviewListedNotGate(t *testing.T) {
	s, repo, _, _ := newAutoDriveServer(t)
	runID, stages := startAutoDriveRun(t, s, repo)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	review := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval, Sequence: 3}
	repo.mu.Lock()
	repo.stagesByRun[runID] = append(repo.stagesByRun[runID], review)
	repo.mu.Unlock()

	out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if !out.GateKnown || out.GateStageID != nil {
		t.Fatalf("gate = known:%v stage:%v, want known with no plan/implement gate", out.GateKnown, out.GateStageID)
	}
	if len(out.ParkedGateStageIDs) != 1 || out.ParkedGateStageIDs[0] != review.ID {
		t.Fatalf("parked = %v, want [%s] (the review stage)", out.ParkedGateStageIDs, review.ID)
	}
}

func TestDriveCampaignRunGate_StagesReadError(t *testing.T) {
	s, repo, au := newCountingAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo.autoDriveRepo, autoDriveEscalationSpecYAML(""))
	seedCleanPlanApproval(t, au, runID)
	r := getRun(t, repo.autoDriveRepo, runID)

	// Count AutoDriveRunGate's own reads, then fail only the translator's.
	before := repo.callCount()
	if _, err := s.AutoDriveRunGate(context.Background(), r, campaignOperatorIdentity(), nil, nil); err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	evalReads := repo.callCount() - before
	repo.mu.Lock()
	repo.failAfter = repo.calls + evalReads
	repo.mu.Unlock()

	out, err := s.DriveCampaignRunGate(context.Background(), r, campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v (a failed stage read must not become an error)", err)
	}
	if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateHumanQuorumRequired {
		t.Fatalf("outcome = %+v, want the decision carried unchanged", out)
	}
	if out.GateKnown || out.GateStageID != nil || out.ParkedGateStageIDs != nil {
		t.Fatalf("gate = known:%v stage:%v parked:%v, want unknown after a failed read", out.GateKnown, out.GateStageID, out.ParkedGateStageIDs)
	}
}

// C4: with no RunRepo there is no read and the gate is unknown.
func TestDriveCampaignRunGate_NilRunRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	out, err := s.DriveCampaignRunGate(context.Background(), &run.Run{ID: uuid.New(), State: run.StateRunning}, campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if out.GateKnown || out.GateStageID != nil {
		t.Fatalf("gate = known:%v stage:%v, want unknown with no RunRepo", out.GateKnown, out.GateStageID)
	}
}

// C4: a non-decision outcome skips the read when the driver holds no key; a
// held key or a decision still reads.
func TestDriveCampaignRunGate_SkipsReadWithoutHeldKey(t *testing.T) {
	t.Run("non_decision_no_key_skips", func(t *testing.T) {
		s, repo, _ := newCountingAutoDriveServer(t)
		runID, stages := startAutoDriveRun(t, s, repo.autoDriveRepo)
		stages[0].State = run.StageStateRunning
		r := getRun(t, repo.autoDriveRepo, runID)

		before := repo.callCount()
		if _, err := s.AutoDriveRunGate(context.Background(), r, campaignOperatorIdentity(), nil, nil); err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		evalReads := repo.callCount() - before

		ctx := campaigndriver.WithDecisionKeyHeld(context.Background(), false)
		before = repo.callCount()
		out, err := s.DriveCampaignRunGate(ctx, r, campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v", err)
		}
		if got := repo.callCount() - before; got != evalReads {
			t.Fatalf("stage reads = %d, want %d (the evaluation's only)", got, evalReads)
		}
		if out.GateKnown {
			t.Fatal("GateKnown = true after a skipped read")
		}
	})
	t.Run("non_decision_held_key_reads", func(t *testing.T) {
		s, repo, _ := newCountingAutoDriveServer(t)
		runID, stages := startAutoDriveRun(t, s, repo.autoDriveRepo)
		stages[0].State = run.StageStateRunning
		ctx := campaigndriver.WithDecisionKeyHeld(context.Background(), true)
		out, err := s.DriveCampaignRunGate(ctx, getRun(t, repo.autoDriveRepo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v", err)
		}
		if !out.GateKnown {
			t.Fatal("GateKnown = false with a held key; the clear could never fire")
		}
	})
	t.Run("decision_no_key_reads", func(t *testing.T) {
		s, repo, au := newCountingAutoDriveServer(t)
		runID, _ := startAutoDriveRunWithSpec(t, s, repo.autoDriveRepo, autoDriveEscalationSpecYAML(""))
		seedCleanPlanApproval(t, au, runID)
		ctx := campaigndriver.WithDecisionKeyHeld(context.Background(), false)
		out, err := s.DriveCampaignRunGate(ctx, getRun(t, repo.autoDriveRepo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v", err)
		}
		if !out.DecisionRequired || !out.GateKnown || out.GateStageID == nil {
			t.Fatalf("outcome = %+v, want a known-gate decision", out)
		}
	})
}

func TestDriveCampaignRunGate_ActedAndPagedCarried(t *testing.T) {
	t.Run("acted", func(t *testing.T) {
		s, repo, au, _ := newAutoDriveServer(t)
		runID, _ := startAutoDriveRun(t, s, repo)
		seedCleanPlanApproval(t, au, runID)
		out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v", err)
		}
		if !out.Acted || out.Action != delegation.ActionApprove || out.DecisionRequired {
			t.Fatalf("outcome = %+v, want acted approve", out)
		}
		if !out.GateKnown || out.GateStageID != nil {
			t.Fatalf("gate = known:%v stage:%v, want known with the plan gate released", out.GateKnown, out.GateStageID)
		}
	})
	t.Run("paged", func(t *testing.T) {
		s, repo, au, _ := newAutoDriveServer(t)
		runID, stages := startAutoDriveRun(t, s, repo)
		stages[0].State = run.StageStateSucceeded
		stages[1].State = run.StageStateAwaitingApproval
		seedReviewEntry(t, au, runID, 1, "implement_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		seedReviewEntry(t, au, runID, 2, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		seedReviewEntry(t, au, runID, 3, "implement_reviewed", planreview.ImplementReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictReject})
		out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v", err)
		}
		if !out.Paged || out.PageEvent == "" || out.Acted {
			t.Fatalf("outcome = %+v, want paged with an event", out)
		}
		if !out.GateKnown || out.GateStageID == nil || *out.GateStageID != stages[1].ID {
			t.Fatalf("gate = known:%v stage:%v, want the parked implement stage", out.GateKnown, out.GateStageID)
		}
	})
}

// Both decision categories are registered, render no issue-comment activity,
// and stay readable to a run-bound caller (their payload names only the run's
// own campaign, issue, stage and closed-set tokens).
func TestCampaignGateDecisionCategories_RegisteredAndRunBoundReadable(t *testing.T) {
	cats := []string{campaigndriver.CategoryCampaignGateDecisionRequired, campaigndriver.CategoryCampaignGateDecisionCleared}
	runID := uuid.New()
	var entries []*audit.Entry
	for i, c := range cats {
		if !audit.IsKnownCategory(c) {
			t.Errorf("%s is not in audit.KnownCategories", c)
		}
		if issuecomment.RendersActivity(c) {
			t.Errorf("%s renders issue-comment activity; it is INTERNAL", c)
		}
		rid := runID
		entries = append(entries, &audit.Entry{ID: uuid.New(), RunID: &rid, Sequence: int64(i + 1), Category: c})
	}
	kept := withholdScopeOverlapFromRunBound(entries, "mcp:run:"+runID.String())
	if len(kept) != len(cats) {
		t.Fatalf("run-bound caller kept %d of %d decision rows, want all", len(kept), len(cats))
	}
}

// TestDriveCampaignRunGate_ServerCheckOnly_CarriesDecision pins the #5477
// decision through the campaign translation on both flows: commit-yourself
// (the implement stage is the gate the marker keys on) and push_and_open_pr
// (a review stage is parked, so the marker is run-level).
func TestDriveCampaignRunGate_ServerCheckOnly_CarriesDecision(t *testing.T) {
	tests := []struct {
		name      string
		runLevel  bool
		wantGate  func(impl *run.Stage) *uuid.UUID
		wantParks func(impl, review *run.Stage) []uuid.UUID
	}{
		{
			name:      "commit-yourself: implement stage is the gate",
			wantGate:  func(impl *run.Stage) *uuid.UUID { return &impl.ID },
			wantParks: func(impl, _ *run.Stage) []uuid.UUID { return []uuid.UUID{impl.ID} },
		},
		{
			name:      "push_and_open_pr: run-level key, review parked",
			runLevel:  true,
			wantGate:  func(*run.Stage) *uuid.UUID { return nil },
			wantParks: func(_, review *run.Stage) []uuid.UUID { return []uuid.UUID{review.ID} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, repo, au, cr := newAutoDriveServer(t)
			runID, stages := startAutoDriveRun(t, s, repo)
			plan, impl := stages[0], stages[1]
			plan.State = run.StageStateSucceeded
			impl.State = run.StageStateAwaitingApproval
			var review *run.Stage
			if tt.runLevel {
				impl.State = run.StageStateSucceeded
				review = &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval, Sequence: 3}
				repo.mu.Lock()
				repo.stagesByRun[runID] = append(repo.stagesByRun[runID], review)
				repo.mu.Unlock()
			}
			seedSettledImplementRound(t, au, runID, planreview.VerdictApproveWithConcerns)
			seedServerCheckConcern(t, cr, runID, impl.ID, "implement", "high", "leaked-token")

			out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
			if err != nil {
				t.Fatalf("DriveCampaignRunGate: %v", err)
			}
			if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateServerCheckAwaitingHuman || out.Action != delegation.ActionRouteFixup {
				t.Fatalf("outcome = %+v, want decision_required server_check_awaiting_human route_fixup", out)
			}
			if !out.GateKnown {
				t.Fatal("GateKnown = false, want the gate read")
			}
			want := tt.wantGate(impl)
			switch {
			case want == nil && out.GateStageID != nil:
				t.Fatalf("GateStageID = %v, want nil (run-level key)", *out.GateStageID)
			case want != nil && (out.GateStageID == nil || *out.GateStageID != *want):
				t.Fatalf("GateStageID = %v, want %s", out.GateStageID, *want)
			}
			parks := tt.wantParks(impl, review)
			if len(out.ParkedGateStageIDs) != len(parks) || out.ParkedGateStageIDs[0] != parks[0] {
				t.Fatalf("ParkedGateStageIDs = %v, want %v", out.ParkedGateStageIDs, parks)
			}
		})
	}
}

// TestDriveCampaignRunGate_NoChangePass_CarriesConcernIDs runs the #5390
// no-change fixture (a real delegated route, a re-parked gate, and the pass's
// fixup_no_changes after its trigger) through DriveCampaignRunGate: the
// translated outcome carries the guard's decision AND its ConcernIDs, with the
// implement stage as the known gate.
func TestDriveCampaignRunGate_NoChangePass_CarriesConcernIDs(t *testing.T) {
	s, repo, au, cr := newAutoDriveServer(t)
	au.stampSequence = true
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	a := soleOpenImplementConcern(t, cr, runID)
	if first := autoDriveGate(t, s, repo, runID); !first.Acted {
		t.Fatalf("pass 1 = %+v, want the real delegated route", first)
	}
	impl.State = run.StageStateAwaitingApproval
	seedFixupNoChangesSeq(au, runID, impl.ID, 5000)

	out, err := s.DriveCampaignRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if !out.DecisionRequired || out.DecisionState != operatorrole.DecisionStateFixupNoChangesUnaddressed || out.Action != delegation.ActionRouteFixup {
		t.Fatalf("outcome = %+v, want decision_required fixup_no_changes_unaddressed route_fixup", out)
	}
	if !reflect.DeepEqual(out.ConcernIDs, []string{a.ID.String()}) {
		t.Fatalf("ConcernIDs = %v, want [%s] (the translation dropped the guard's ids)", out.ConcernIDs, a.ID)
	}
	if !out.GateKnown || out.GateStageID == nil || *out.GateStageID != impl.ID {
		t.Fatalf("gate = known:%v stage:%v, want known implement stage %s", out.GateKnown, out.GateStageID, impl.ID)
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != 1 {
		t.Errorf("%d %s entries, want 1 (the guard must route nothing)", n, CategoryStageFixupTriggered)
	}
}

// TestCampaignDriverTick_NoChangePass_MarkerNamesConcernIDs_E2E is the #5390
// full path (E3): a real delegated route by the campaign Ticker, the persisted
// stage_fixup_triggered, the runner's REAL fixup_no_changes report through
// POST /v0/runs/{run_id}/pull-request (succeedFixupNoChangesStage →
// advanceVariantStageAfterReport, which re-parks the stage and appends the
// real fixup_no_changes row), the guard, DriveCampaignRunGate and the Ticker's
// trackDecision / recordDecision marker. An open server-check concern X rides
// along so #5477's filter composes: X is never routed and never named.
//
// Tick 2's state is fixup-eligible with convergent_concerns met and the
// no-change pass refunded, so without the guard the Ticker would route a
// second pass and record no decision.
func TestCampaignDriverTick_NoChangePass_MarkerNamesConcernIDs_E2E(t *testing.T) {
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	au.stampSequence = true
	cr := newFakeConcernRepo()
	camps := &pauseNoopCampaignRepo{fakeCampaignRepo: newFakeCampaignRepo()}
	sf := newSigningFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  cr,
		CampaignRepo: camps.fakeCampaignRepo,
		ApprovalRepo: newFakeApprovalRepo(),
		SigningRepo:  sf,
		ArtifactRepo: newFakeArtifactRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	runID, impl := seedRouteFixupReady(t, s, repo, au, cr)
	a := soleOpenImplementConcern(t, cr, runID)
	x := seedServerCheckConcern(t, cr, runID, impl.ID, concern.StageKindImplement, "high", "leaked-token")
	camps.seedCampaignWithItems("acme/widgets", "issue:10", []*campaign.Item{
		{IssueRef: "issue:1", State: campaign.ItemStateRunning, RunID: idp(runID), Position: 0},
	})
	tk := &campaigndriver.Ticker{
		Campaigns: camps,
		Runs:      repo,
		Starter:   neverStarter{t: t},
		Audit:     au,
		GateActor: gateActorFunc(func(ctx context.Context, runRow *run.Run) (campaigndriver.GateActionOutcome, error) {
			return s.DriveCampaignRunGate(ctx, runRow, campaignOperatorIdentity(), nil, nil)
		}),
	}
	markers := func() []*audit.Entry {
		t.Helper()
		m, err := au.ListForRunByCategory(context.Background(), runID, campaigndriver.CategoryCampaignGateDecisionRequired)
		if err != nil {
			t.Fatalf("list markers: %v", err)
		}
		return m
	}

	// Tick 1: the real delegated route of the routable set [A] (X excluded).
	tk.Tick(context.Background())
	if n := countAudit(au, CategoryStageFixupTriggered); n != 1 {
		t.Fatalf("tick 1: %d %s entries, want 1 (the real delegated route)", n, CategoryStageFixupTriggered)
	}
	trig := auditEntry(t, au, CategoryStageFixupTriggered)
	var tp struct {
		ConcernIDs []string `json:"concern_ids"`
	}
	if err := json.Unmarshal(trig.Payload, &tp); err != nil || !reflect.DeepEqual(tp.ConcernIDs, []string{a.ID.String()}) {
		t.Fatalf("tick 1 trigger concern_ids = %v (%v), want [%s] (server-check %s excluded)", tp.ConcernIDs, err, a.ID, x.ID)
	}
	if n := len(markers()); n != 0 {
		t.Fatalf("tick 1: %d decision markers, want 0", n)
	}

	// The runner picks the fix-up pass up and reports it produced no commit
	// through the REAL pull-request handler.
	stage, err := repo.GetStage(context.Background(), impl.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	stage.State = run.StageStateRunning
	priv, _ := sf.issue(t, runID)
	body, err := json.Marshal(map[string]any{
		"outcome":             "fixup_no_changes",
		"branch":              "fishhawk/run-e2e/stage-impl",
		"base_sha":            "base-def",
		"files_changed_count": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := shipPRRequest(t, s, runID, impl.ID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("fixup_no_changes report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got, _ := repo.GetStage(context.Background(), impl.ID); got.State != run.StageStateAwaitingApproval {
		t.Fatalf("implement stage = %s after the no-change report, want awaiting_approval (the real re-park)", got.State)
	}
	if n := countAudit(au, CategoryFixupNoChanges); n != 1 {
		t.Fatalf("%d %s entries after the report, want 1 (written by succeedFixupNoChangesStage)", n, CategoryFixupNoChanges)
	}

	// Tick 2: the guard declines and the Ticker records the marker.
	tk.Tick(context.Background())
	m := markers()
	if len(m) != 1 {
		t.Fatalf("tick 2: %d decision markers, want 1", len(m))
	}
	var payload map[string]any
	if err := json.Unmarshal(m[0].Payload, &payload); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	var keys []string
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "action,campaign_id,concern_ids,decision_state,gate,issue_ref,run_id" {
		t.Fatalf("marker payload keys = %s, want the six base keys plus concern_ids", got)
	}
	ids, ok := payload["concern_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != a.ID.String() {
		t.Fatalf("marker concern_ids = %#v, want JSON array [%s]", payload["concern_ids"], a.ID)
	}
	if strings.Contains(string(m[0].Payload), x.ID.String()) {
		t.Fatalf("marker payload %s names the server-check concern %s (the #5477 filter did not compose)", m[0].Payload, x.ID)
	}
	if payload["decision_state"] != operatorrole.DecisionStateFixupNoChangesUnaddressed || payload["action"] != delegation.ActionRouteFixup || payload["gate"] != impl.ID.String() {
		t.Fatalf("marker payload = %v, want fixup_no_changes_unaddressed/route_fixup keyed on the implement stage %s", payload, impl.ID)
	}

	// Ticks 3 and 4: the same occurrence appends nothing more.
	tk.Tick(context.Background())
	tk.Tick(context.Background())
	if n := len(markers()); n != 1 {
		t.Errorf("after ticks 3-4: %d decision markers, want 1", n)
	}
	if n := countAudit(au, CategoryStageFixupTriggered); n != 1 {
		t.Errorf("after ticks 3-4: %d %s entries, want 1 (zero automatic re-routes)", n, CategoryStageFixupTriggered)
	}
}
