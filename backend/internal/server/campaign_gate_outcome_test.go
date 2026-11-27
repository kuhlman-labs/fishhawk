package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stagesReadCountingRepo counts ListStagesForRun calls and fails every call
// after failAfter (a negative failAfter never fails), so a test can fail only
// DriveCampaignRunGate's own read after AutoDriveRunGate's reads succeed.
// failCalls fails only the listed 1-based calls. rewrite, when set, maps the
// result of each successful call (by its 1-based number) so a test can hand
// one read a different view of the stages.
type stagesReadCountingRepo struct {
	*autoDriveRepo
	mu        sync.Mutex
	calls     int
	failAfter int
	failCalls map[int]bool
	rewrite   func(call int, stages []*run.Stage) []*run.Stage
}

func (r *stagesReadCountingRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	r.calls++
	n, fa, fail, rw := r.calls, r.failAfter, r.failCalls[r.calls], r.rewrite
	r.mu.Unlock()
	if fail || (fa >= 0 && n > fa) {
		return nil, errors.New("stages read down")
	}
	stages, err := r.autoDriveRepo.ListStagesForRun(ctx, runID)
	if err != nil || rw == nil {
		return stages, err
	}
	return rw(n, stages), nil
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

// --- #5562: the campaign-driver act page --------------------------------------

// driverActPageFixture is the #5562 seam harness: the autoDrive stack over a
// stagesReadCountingRepo, with a page-class spy that snapshots the projected
// page kinds at each notifyPageClass call.
type driverActPageFixture struct {
	s     *Server
	repo  *stagesReadCountingRepo
	au    *auditFake
	cr    *fakeConcernRepo
	spy   *fixupPageSpy
	runID uuid.UUID
}

func newDriverActPageFixture(t *testing.T) *driverActPageFixture {
	t.Helper()
	s, repo, au := newCountingAutoDriveServer(t)
	f := &driverActPageFixture{s: s, repo: repo, au: au, cr: s.cfg.ConcernRepo.(*fakeConcernRepo)}
	f.spy = &fixupPageSpy{snapshot: func() []string { return f.pageKinds(t) }}
	s.issueNotifier = f.spy
	return f
}

// pageKinds projects the run's chain and stages through the same
// issuecomment.PageClassEventKinds the ping and push channels fire.
func (f *driverActPageFixture) pageKinds(t *testing.T) []string {
	t.Helper()
	entries, err := f.au.ListForRun(t.Context(), f.runID)
	if err != nil {
		t.Fatalf("list run chain: %v", err)
	}
	return issuecomment.PageClassEventKinds(entries, f.repo.stagesFor(f.runID))
}

// setRunner stamps the run's runner kind (resolved) and auto_dispatch mark,
// under the fake repo's own mutex (the counting wrapper's mu shadows it).
func (f *driverActPageFixture) setRunner(kind string, marked bool) {
	f.repo.autoDriveRepo.mu.Lock()
	defer f.repo.autoDriveRepo.mu.Unlock()
	r := f.repo.runs[f.runID]
	r.RunnerKind = kind
	r.RunnerKindResolved = true
	r.AutoDispatch = marked
}

func (f *driverActPageFixture) runRow(t *testing.T) *run.Run {
	t.Helper()
	return getRun(t, f.repo.autoDriveRepo, f.runID)
}

func (f *driverActPageFixture) drive(t *testing.T, ctx context.Context) campaigndriver.GateActionOutcome {
	t.Helper()
	out, err := f.s.DriveCampaignRunGate(ctx, f.runRow(t), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	return out
}

// rows returns the appended #5562 page rows.
func (f *driverActPageFixture) rows() []audit.ChainAppendParams {
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, ap := range f.au.appended {
		if ap.Category == CategoryCampaignDriverActAwaitingOperator {
			out = append(out, ap)
		}
	}
	return out
}

func (f *driverActPageFixture) stageState(t *testing.T, id uuid.UUID) run.StageState {
	t.Helper()
	st, err := f.repo.GetStage(t.Context(), id)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	return st.State
}

// assertOnePage pins exactly one page row, stage-scoped to stageID, with the
// action, stage type and closed next_action, and exactly one notifyPageClass
// call whose snapshot already holds the projected page kind.
func (f *driverActPageFixture) assertOnePage(t *testing.T, stageID uuid.UUID, action string, stageType run.StageType) {
	t.Helper()
	rows := f.rows()
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want exactly 1 (only the stage the act parked)", CategoryCampaignDriverActAwaitingOperator, len(rows))
	}
	row := rows[0]
	if row.StageID == nil || *row.StageID != stageID {
		t.Fatalf("row stage = %v, want stage-scoped to %s", row.StageID, stageID)
	}
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem || row.ActorSubject == nil || *row.ActorSubject != campaignDriverActorSubject {
		t.Errorf("row actor = %v/%v, want system/%s", row.ActorKind, row.ActorSubject, campaignDriverActorSubject)
	}
	var p map[string]any
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p["action"] != action || p["stage_type"] != string(stageType) || p["stage_id"] != stageID.String() ||
		p["stage_state"] != string(run.StageStateAwaitingHostDispatch) || p["next_action"] != "fishhawk_dispatch_stage" ||
		p["auto_dispatch"] != false || p["runner_kind"] != run.RunnerKindLocal || p["reason"] != driverActPageReason {
		t.Errorf("payload = %v, want action=%s stage_type=%s next_action=fishhawk_dispatch_stage auto_dispatch=false runner_kind=local", p, action, stageType)
	}
	if key, _ := p["episode_key"].(string); !strings.HasPrefix(key, stageID.String()+"/") {
		t.Errorf("episode_key = %v, want the stage's park-episode key", p["episode_key"])
	}
	if got := f.pageKinds(t); !slices.Contains(got, "driver_act_awaiting_operator_dispatch") {
		t.Errorf("PageClassEventKinds = %v, want driver_act_awaiting_operator_dispatch (the projection must key on the server's category)", got)
	}
	if len(f.spy.pageClass) != 1 {
		t.Fatalf("notifyPageClass calls = %d, want exactly 1", len(f.spy.pageClass))
	}
	if !slices.Contains(f.spy.kindsAtCall[0], "driver_act_awaiting_operator_dispatch") {
		t.Errorf("kinds at notify = %v, want the page already on the chain (notify after append)", f.spy.kindsAtCall[0])
	}
}

func (f *driverActPageFixture) assertNoPage(t *testing.T) {
	t.Helper()
	if n := len(f.rows()); n != 0 {
		t.Fatalf("%s rows = %d, want 0", CategoryCampaignDriverActAwaitingOperator, n)
	}
	if n := len(f.spy.pageClass); n != 0 {
		t.Fatalf("notifyPageClass calls = %d, want 0", n)
	}
}

// seedHotfixFixup starts the SHIPPED hotfix_change preset (plan, implement,
// acceptance, review) and seeds the push_and_open_pr fix-up shape the preset
// produces (#5562 condition P1): the gateless implement SUCCEEDED, acceptance
// succeeded, the separate review stage parked at awaiting_approval
// (fixupEligibleState), a settled dual approve_with_concerns round and one
// open implement concern, so the medium tier's delegated route_fixup
// (convergent_concerns) is met.
func (f *driverActPageFixture) seedHotfixFixup(t *testing.T, marked bool) (impl, review *run.Stage) {
	t.Helper()
	out, err := f.s.StartAlertRun(context.Background(), alertParams(readHotfixChangePreset(t)))
	if err != nil || out.Kind != AlertStartStarted {
		t.Fatalf("StartAlertRun: outcome = %+v, err = %v; want started", out, err)
	}
	f.runID = out.RunID
	stages := f.repo.stagesFor(f.runID)
	var types []run.StageType
	for _, st := range stages {
		types = append(types, st.Type)
	}
	want := []run.StageType{run.StageTypePlan, run.StageTypeImplement, run.StageTypeAcceptance, run.StageTypeReview}
	if !slices.Equal(types, want) {
		t.Fatalf("hotfix_change stages = %v, want %v (the shipped preset shape)", types, want)
	}
	if r := f.runRow(t); r.WorkflowID != "hotfix_change" {
		t.Fatalf("workflow_id = %q, want hotfix_change", r.WorkflowID)
	}
	if stages[1].RequiresApproval {
		t.Fatal("hotfix implement stage requires approval; the preset's implement is gateless")
	}
	f.setRunner(run.RunnerKindLocal, marked)
	f.repo.autoDriveRepo.mu.Lock()
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	stages[2].State = run.StageStateSucceeded
	stages[3].State = run.StageStateAwaitingApproval
	f.repo.autoDriveRepo.mu.Unlock()
	seedSettledImplementRound(t, f.au, f.runID, planreview.VerdictApproveWithConcerns)
	seedOpenConcern(t, f.cr, f.runID, stages[1].ID, concern.StageKindImplement, "medium", "scope", "tighten the seam")
	return stages[1], stages[3]
}

// TestDriveCampaignRunGate_DriverActAwaitingOperator_HotfixFixup is the #5562
// done-means: a driver-routed fix-up on a hotfix_change run, through the
// driver's gate seam (serve.go's campaignGateActor delegates straight to
// DriveCampaignRunGate), the real autoFixup / fixupStageAs and the real
// orchestrator advance, over the shipped preset bytes.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_HotfixFixup(t *testing.T) {
	t.Run("unmarked_local_pages", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		impl, review := f.seedHotfixFixup(t, false)

		out := f.drive(t, context.Background())
		if !out.Acted || out.Action != delegation.ActionRouteFixup {
			t.Fatalf("outcome = %+v, want acted route_fixup", out)
		}
		if got := f.stageState(t, review.ID); got != run.StageStatePending {
			t.Fatalf("review stage = %s, want re-parked pending by the fix-up", got)
		}
		if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("implement stage = %s, want awaiting_host_dispatch (the real advance parks the re-opened stage)", got)
		}
		f.assertOnePage(t, impl.ID, delegation.ActionRouteFixup, run.StageTypeImplement)
	})
	t.Run("marked_local_no_page", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		impl, review := f.seedHotfixFixup(t, true)

		out := f.drive(t, context.Background())
		if !out.Acted || out.Action != delegation.ActionRouteFixup {
			t.Fatalf("outcome = %+v, want acted route_fixup", out)
		}
		if got := f.stageState(t, review.ID); got != run.StageStatePending {
			t.Fatalf("review stage = %s, want re-parked pending by the fix-up", got)
		}
		if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("implement stage = %s, want awaiting_host_dispatch", got)
		}
		if kind, reason := hostDispatchKind(f.runRow(t)); kind != awaitingKindHostDispatcher || reason != awaitingReasonHostDispatch {
			t.Fatalf("hostDispatchKind = %s/%s, want host_dispatcher/host_dispatch (the re-opened fix-up stage is host-dispatched)", kind, reason)
		}
		f.assertNoPage(t)
	})
}

// seedLocalApprove starts the autoDrive plan+implement run on an unmarked local
// runner with a clean plan review, so the delegated approve releases the plan
// gate and the real advance parks implement.
func (f *driverActPageFixture) seedLocalApprove(t *testing.T) (plan, impl *run.Stage) {
	t.Helper()
	runID, stages := startAutoDriveRun(t, f.s, f.repo.autoDriveRepo)
	f.runID = runID
	f.setRunner(run.RunnerKindLocal, false)
	seedCleanPlanApproval(t, f.au, runID)
	return stages[0], stages[1]
}

func TestDriveCampaignRunGate_DriverActAwaitingOperator_ApproveUnmarkedPages(t *testing.T) {
	f := newDriverActPageFixture(t)
	_, impl := f.seedLocalApprove(t)

	out := f.drive(t, context.Background())
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve", out)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("implement stage = %s, want awaiting_host_dispatch after the approve advance", got)
	}
	f.assertOnePage(t, impl.ID, delegation.ActionApprove, run.StageTypeImplement)
}

func TestDriveCampaignRunGate_DriverActAwaitingOperator_RetryUnmarkedPages(t *testing.T) {
	f := newDriverActPageFixture(t)
	runID, stages := startAutoDriveRun(t, f.s, f.repo.autoDriveRepo)
	f.runID = runID
	f.setRunner(run.RunnerKindLocal, false)
	plan, impl := stages[0], stages[1]
	plan.State = run.StageStateSucceeded
	impl.State = run.StageStateFailed
	cat := run.FailureA
	reason := "verify command \"scripts/test\" still failing: verify_infra_flake_retry"
	impl.FailureCategory = &cat
	impl.FailureReason = &reason
	if _, err := f.repo.TransitionRun(context.Background(), runID, run.StateFailed); err != nil {
		t.Fatalf("TransitionRun -> failed: %v", err)
	}

	out := f.drive(t, context.Background())
	if !out.Acted || out.Action != delegation.ActionRetry {
		t.Fatalf("outcome = %+v, want acted retry", out)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("implement stage = %s, want awaiting_host_dispatch after the retry re-open", got)
	}
	f.assertOnePage(t, impl.ID, delegation.ActionRetry, run.StageTypeImplement)
}

// A delegated merge parks nothing, so it never pages.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_MergeNoPage(t *testing.T) {
	f := newDriverActPageFixture(t)
	runID, stages := startAutoDriveRun(t, f.s, f.repo.autoDriveRepo)
	f.runID = runID
	f.setRunner(run.RunnerKindLocal, false)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateSucceeded
	if _, err := f.repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
		t.Fatalf("TransitionRun -> running: %v", err)
	}
	if _, err := f.repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	seedReviewEntry(t, f.au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})

	merger := &fakeMerger{}
	out, err := f.s.DriveCampaignRunGate(context.Background(), f.runRow(t), campaignOperatorIdentity(), merger, nil)
	if err != nil {
		t.Fatalf("DriveCampaignRunGate: %v", err)
	}
	if !out.Acted || out.Action != delegation.ActionMerge || merger.called != 1 {
		t.Fatalf("outcome = %+v (merger calls %d), want acted merge", out, merger.called)
	}
	f.assertNoPage(t)
}

// An observe-only tick never pages, even when a stage became parked during the
// tick by some other path. The pre-act read is handed a pending plan stage, so
// only the Acted guard keeps the post-tick park from paging.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_NotActedNoPage(t *testing.T) {
	f := newDriverActPageFixture(t)
	runID, stages := startAutoDriveRun(t, f.s, f.repo.autoDriveRepo)
	f.runID = runID
	f.setRunner(run.RunnerKindLocal, false)
	plan := stages[0]
	plan.State = run.StageStateAwaitingHostDispatch
	f.repo.rewrite = func(call int, in []*run.Stage) []*run.Stage {
		if call != 1 {
			return in
		}
		out := make([]*run.Stage, 0, len(in))
		for _, st := range in {
			cp := *st
			if cp.ID == plan.ID {
				cp.State = run.StageStatePending
			}
			out = append(out, &cp)
		}
		return out
	}

	out := f.drive(t, context.Background())
	if out.Acted || out.Paged || out.DecisionRequired {
		t.Fatalf("outcome = %+v, want observe-only", out)
	}
	if got := f.stageState(t, plan.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("plan stage = %s, want awaiting_host_dispatch", got)
	}
	f.assertNoPage(t)
}

// A stage already parked before the act is not the act's park and never pages;
// only the stage the act parked does.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_PreExistingParkNoPage(t *testing.T) {
	f := newDriverActPageFixture(t)
	_, impl := f.seedLocalApprove(t)
	parked := &run.Stage{ID: uuid.New(), RunID: f.runID, Type: run.StageTypeAcceptance, State: run.StageStateAwaitingHostDispatch, Sequence: 3}
	f.repo.autoDriveRepo.mu.Lock()
	f.repo.stagesByRun[f.runID] = append(f.repo.stagesByRun[f.runID], parked)
	f.repo.autoDriveRepo.mu.Unlock()

	out := f.drive(t, context.Background())
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve", out)
	}
	if got := f.stageState(t, parked.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("pre-parked stage = %s, want still awaiting_host_dispatch", got)
	}
	if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("implement stage = %s, want awaiting_host_dispatch after the approve advance", got)
	}
	f.assertOnePage(t, impl.ID, delegation.ActionApprove, run.StageTypeImplement)
}

// A second acted tick on the same park episode pages nothing more, even when
// its pre-act read predates the park (a concurrent tick); a new park episode of
// the same stage pages again.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_SecondActSamePark(t *testing.T) {
	f := newDriverActPageFixture(t)
	impl, _ := f.seedHotfixFixup(t, false)
	if out := f.drive(t, context.Background()); !out.Acted {
		t.Fatalf("tick 1 = %+v, want the delegated route", out)
	}
	f.assertOnePage(t, impl.ID, delegation.ActionRouteFixup, run.StageTypeImplement)

	// Tick 2: a real driver pass sees the park as pre-existing (observe-only).
	f.drive(t, context.Background())
	// A second acted tick whose pre-act read did not see the park.
	stale := hostDispatchParkSnapshot{known: true, episodes: map[uuid.UUID]string{}}
	f.s.pageDriverActAwaitingOperator(context.Background(), f.runRow(t), AutoDriveOutcome{Acted: true, Action: delegation.ActionRetry}, stale)
	if n := len(f.rows()); n != 1 {
		t.Fatalf("rows after a second act on the same park = %d, want 1 (dedup per stage and park episode)", n)
	}
	if n := len(f.spy.pageClass); n != 1 {
		t.Fatalf("notifyPageClass calls = %d, want 1", n)
	}

	// The stage is dispatched and parks again: a NEW episode, which pages.
	f.repo.autoDriveRepo.mu.Lock()
	dispatched := time.Now().UTC()
	impl.DispatchedAt = &dispatched
	f.repo.autoDriveRepo.mu.Unlock()
	f.s.pageDriverActAwaitingOperator(context.Background(), f.runRow(t), AutoDriveOutcome{Acted: true, Action: delegation.ActionRouteFixup}, stale)
	if n := len(f.rows()); n != 2 {
		t.Fatalf("rows after a new park episode = %d, want 2", n)
	}
}

// A github_actions run never parks at awaiting_host_dispatch, so the page path
// takes no snapshot, reads nothing extra and writes nothing.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_GitHubActionsNoPage(t *testing.T) {
	evalReads := func(t *testing.T) int {
		f := newDriverActPageFixture(t)
		f.seedLocalApprove(t)
		f.setRunner(run.RunnerKindGitHubActions, false)
		before := f.repo.callCount()
		if out, err := f.s.AutoDriveRunGate(context.Background(), f.runRow(t), campaignOperatorIdentity(), nil, nil); err != nil || !out.Acted {
			t.Fatalf("AutoDriveRunGate = %+v, %v; want acted", out, err)
		}
		return f.repo.callCount() - before
	}(t)

	f := newDriverActPageFixture(t)
	f.seedLocalApprove(t)
	f.setRunner(run.RunnerKindGitHubActions, false)
	ctx := campaigndriver.WithDecisionKeyHeld(context.Background(), false)
	before := f.repo.callCount()
	out := f.drive(t, ctx)
	if !out.Acted || out.Action != delegation.ActionApprove {
		t.Fatalf("outcome = %+v, want acted approve", out)
	}
	if got := f.repo.callCount() - before; got != evalReads {
		t.Errorf("stage reads = %d, want %d (the evaluation's only; no page reads on a github_actions run)", got, evalReads)
	}
	f.assertNoPage(t)
}

// The page is best-effort: a failed read or append loses it and never changes
// the acted outcome or error.
func TestDriveCampaignRunGate_DriverActAwaitingOperator_BestEffort(t *testing.T) {
	// totalReads counts the stage reads of one full acted pass whose last read
	// is the page path's post-act read.
	totalReads := func(t *testing.T) int {
		f := newDriverActPageFixture(t)
		f.seedHotfixFixup(t, false)
		before := f.repo.callCount()
		f.drive(t, campaigndriver.WithDecisionKeyHeld(context.Background(), false))
		if len(f.rows()) != 1 {
			t.Fatal("baseline pass did not page")
		}
		return f.repo.callCount() - before
	}(t)
	assertActedNoPage := func(t *testing.T, f *driverActPageFixture, out campaigndriver.GateActionOutcome, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("DriveCampaignRunGate: %v (a failed page must not become an error)", err)
		}
		if !out.Acted || out.Action != delegation.ActionRouteFixup {
			t.Fatalf("outcome = %+v, want the acted route_fixup unchanged", out)
		}
		f.assertNoPage(t)
	}
	ctx := campaigndriver.WithDecisionKeyHeld(context.Background(), false)

	t.Run("post_act_read_fails", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		f.seedHotfixFixup(t, false)
		f.repo.failCalls = map[int]bool{f.repo.callCount() + totalReads: true}
		out, err := f.s.DriveCampaignRunGate(ctx, f.runRow(t), campaignOperatorIdentity(), nil, nil)
		assertActedNoPage(t, f, out, err)
	})
	t.Run("pre_act_read_fails", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		impl, _ := f.seedHotfixFixup(t, false)
		f.repo.failCalls = map[int]bool{f.repo.callCount() + 1: true}
		out, err := f.s.DriveCampaignRunGate(ctx, f.runRow(t), campaignOperatorIdentity(), nil, nil)
		if got := f.stageState(t, impl.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("implement stage = %s, want awaiting_host_dispatch (the act still parked it)", got)
		}
		assertActedNoPage(t, f, out, err)
	})
	t.Run("prior_pages_read_fails", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		f.seedHotfixFixup(t, false)
		f.au.listByCategoryErrCategory = CategoryCampaignDriverActAwaitingOperator
		out, err := f.s.DriveCampaignRunGate(ctx, f.runRow(t), campaignOperatorIdentity(), nil, nil)
		assertActedNoPage(t, f, out, err)
	})
	t.Run("append_fails", func(t *testing.T) {
		f := newDriverActPageFixture(t)
		f.seedHotfixFixup(t, false)
		f.au.appendErrCategory = CategoryCampaignDriverActAwaitingOperator
		out, err := f.s.DriveCampaignRunGate(ctx, f.runRow(t), campaignOperatorIdentity(), nil, nil)
		assertActedNoPage(t, f, out, err)
	})
}
