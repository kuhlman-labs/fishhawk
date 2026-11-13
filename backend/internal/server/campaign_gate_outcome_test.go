package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
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
