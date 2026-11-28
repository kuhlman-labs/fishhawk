package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// approveAs drives the REAL approval path (approveStageAs → finishApprovalAdvance
// → the detached split launch) with the given observed stage snapshot.
func (h *splitFilingHarness) approveAs(t *testing.T, ctx context.Context, subject string, observed run.Stage) {
	t.Helper()
	if _, err := h.s.approveStageAs(ctx, approverIdentity(subject), approveActionParams{
		Stage:    &observed,
		Decision: approval.DecisionApprove,
	}); err != nil {
		t.Fatalf("approve as %s: %v", subject, err)
	}
}

func (h *splitFilingHarness) launches() int64 { return h.s.splitFilingLaunches.Load() }

// TestSplitFilingRegistry_AdmissionRule pins every admission branch: an empty
// run admits any kind; a first approval co-holds ONLY with first approvals of
// the SAME stage; re-approval and resume claims are exclusive; a release
// re-admits.
func TestSplitFilingRegistry_AdmissionRule(t *testing.T) {
	stageA, stageB := uuid.New(), uuid.New()
	kinds := []splitHoldKind{splitHoldFirstApproval, splitHoldReapproval, splitHoldResume}
	for _, held := range kinds {
		for _, claim := range kinds {
			for _, sameStage := range []bool{true, false} {
				name := held.String() + "_then_" + claim.String()
				if sameStage {
					name += "_same_stage"
				} else {
					name += "_other_stage"
				}
				t.Run(name, func(t *testing.T) {
					reg := newSplitFilingRegistry()
					runID := uuid.New()
					first := reg.claim(held, runID, stageA)
					if first == nil {
						t.Fatalf("claim on an empty run refused")
					}
					stage := stageB
					if sameStage {
						stage = stageA
					}
					second := reg.claim(claim, runID, stage)
					want := held == splitHoldFirstApproval && claim == splitHoldFirstApproval && sameStage
					if got := second != nil; got != want {
						t.Fatalf("second claim admitted = %v, want %v", got, want)
					}
					first.release()
					first.release() // idempotent
					second.release()
					if n := reg.holdersFor(runID); n != 0 {
						t.Fatalf("holders after release = %d, want 0", n)
					}
					if again := reg.claim(claim, runID, stage); again == nil {
						t.Fatalf("claim after release refused; release must re-admit")
					}
				})
			}
		}
	}
	t.Run("other_run_independent", func(t *testing.T) {
		reg := newSplitFilingRegistry()
		first := reg.claim(splitHoldResume, uuid.New(), stageA)
		second := reg.claim(splitHoldResume, uuid.New(), stageA)
		if first == nil || second == nil {
			t.Fatal("claims on two different runs must not interfere")
		}
	})
}

// TestSplitFilingBudgetFor pins the pure budget: the floor up to the
// crossover, then phases × per-child.
func TestSplitFilingBudgetFor(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{0, 5 * time.Minute}, {3, 5 * time.Minute}, {5, 5 * time.Minute}, {6, 6 * time.Minute}, {20, 20 * time.Minute},
	} {
		if got := splitFilingBudgetFor(tc.n); got != tc.want {
			t.Errorf("splitFilingBudgetFor(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestSplitFiling_ApprovalSurvivesRequestCancel (T1, AC1): the approval
// request's context is cancelled after the first child files; the detached
// filing finishes all three. The ctx-honouring provider fails every File after
// the cancel, as a real forge client does, so a filer still on the request
// context stops at 1.
func TestSplitFiling_ApprovalSurvivesRequestCancel(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true, ctxHonouring: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.provider.onFile = func(call int) {
		if call == 1 {
			cancel()
		}
	}

	h.approveAs(t, ctx, "approver-one", *h.planStage)
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 3 {
		t.Errorf("File requests = %d, want 3 (the filing must survive the request cancel)", got)
	}
	if got := h.childMarkerCount(t); got != 3 {
		t.Errorf("per-phase markers = %d, want 3", got)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("split_children_filed entries = %d, want 1", n)
	}
}

// TestApproveAdvance_ReapprovalWhileFilingParked_SkipsSecondLaunch (S2b): a
// fresh-load re-approval by a second subject while the first filer is parked
// inside File for phase 0 is admitted as an approval but launches no second
// filer. No querier is registered, so forge adoption cannot mask a duplicate.
func TestApproveAdvance_ReapprovalWhileFilingParked_SkipsSecondLaunch(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true, parkOnCall: 1})

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	<-h.provider.parked

	fresh := *h.planStage // the stage as a fresh load now sees it
	if fresh.State != run.StageStateSucceeded {
		t.Fatalf("fresh load state = %q, want succeeded", fresh.State)
	}
	h.approveAs(t, context.Background(), "approver-two", fresh)
	if got := h.planStage.State; got != run.StageStateSucceeded {
		t.Errorf("stage state after the re-approval = %q, want succeeded", got)
	}

	h.s.splitFiling.waitHoldersAtMost(h.runID, 1)
	h.provider.releasePark()
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 3 {
		t.Errorf("File requests = %d, want 3 (one per phase; the re-approval must not launch a second filer)", got)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("split_children_filed entries = %d, want 1", n)
	}
	if got := h.launches(); got != 1 {
		t.Errorf("launches = %d, want 1", got)
	}
	if !strings.Contains(h.logs.String(), "another split filing for this run holds the single-flight registry") {
		t.Errorf("no busy-skip WARN logged; logs:\n%s", h.logs.String())
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders after settle = %d, want 0", n)
	}
}

// TestApproveAdvance_FirstApprovalSkipsWhileResumeHolds: a seeded resume hold
// is the only thing in the path (no markers, no querier), and the approval
// still succeeds but launches nothing.
func TestApproveAdvance_FirstApprovalSkipsWhileResumeHolds(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true})
	hold := h.s.splitFiling.claim(splitHoldResume, h.runID, h.planStage.ID)
	if hold == nil {
		t.Fatal("seed resume hold refused")
	}
	defer hold.release()

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	h.s.waitSplitFiling()

	if got := h.planStage.State; got != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded (a refused claim never refuses the approval)", got)
	}
	if got := h.launches(); got != 0 {
		t.Errorf("launches = %d, want 0", got)
	}
	if got := h.provider.requestCount(); got != 0 {
		t.Errorf("File requests = %d, want 0", got)
	}
	if !strings.Contains(h.logs.String(), "split-filing/resume") {
		t.Errorf("busy WARN does not name the resume verb; logs:\n%s", h.logs.String())
	}
}

// TestStartSplitFilingDetached_LoadErrorLogsNotBusy: the own-artifact read
// fails after the stage commit (seeded through the pre-launch seam, so the
// approval's own reads are unaffected). The approval succeeds, the WARN names
// the load failure, nothing launches, the hold is released, and the failure is
// never reported as busy.
func TestStartSplitFilingDetached_LoadErrorLogsNotBusy(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true})
	h.s.splitFilingPreLaunch = func(*run.Stage) {
		h.art.mu.Lock()
		h.art.listErr = errors.New("artifact store down")
		h.art.mu.Unlock()
	}

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	h.s.waitSplitFiling()

	logs := h.logs.String()
	if !strings.Contains(logs, "load approved plan failed") {
		t.Errorf("no load-failure WARN; logs:\n%s", logs)
	}
	if strings.Contains(logs, "single-flight registry") {
		t.Errorf("a load error was reported as busy; logs:\n%s", logs)
	}
	if got := h.launches(); got != 0 {
		t.Errorf("launches = %d, want 0", got)
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders = %d, want 0", n)
	}
}

// TestStartSplitFilingDetached_NoSplitProposalSpawnsNothing (T11).
func TestStartSplitFilingDetached_NoSplitProposalSpawnsNothing(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: false, approvalGate: true})
	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	h.s.waitSplitFiling()
	if got := h.launches(); got != 0 {
		t.Errorf("launches = %d, want 0", got)
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders = %d, want 0", n)
	}
	if strings.Contains(h.logs.String(), "split filing:") {
		t.Errorf("no-split approval logged a split-filing WARN; logs:\n%s", h.logs.String())
	}
}

// TestApproveAdvance_ArtifactAfterSuccess_ReapprovalLaunchesNothing (AA1): the
// plan stage succeeded at EndedAt; a second, valid split artifact is uploaded
// AFTER that; a second-subject re-approval succeeds but files nothing, because
// a same-state re-approval does not move EndedAt and the binding is the
// transition that succeeded the stage.
func TestApproveAdvance_ArtifactAfterSuccess_ReapprovalLaunchesNothing(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true, approvedAlready: true})
	endedBefore := *h.planStage.EndedAt
	sv := "standard_v1"
	a2, err := h.art.Create(context.Background(), artifact.CreateParams{
		StageID: h.planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: splitPlanBytes(t, true),
	})
	if err != nil {
		t.Fatalf("seed A2: %v", err)
	}
	a2.CreatedAt = endedBefore.Add(time.Minute) // uploaded after the gate closed

	h.approveAs(t, context.Background(), "approver-two", *h.planStage)
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 0 {
		t.Errorf("File requests = %d, want 0 (an artifact uploaded after the gate closed is never filed)", got)
	}
	if got := h.launches(); got != 0 {
		t.Errorf("launches = %d, want 0", got)
	}
	if !h.planStage.EndedAt.Equal(endedBefore) {
		t.Errorf("EndedAt moved %v → %v on a same-state re-approval", endedBefore, *h.planStage.EndedAt)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "changed after the plan gate closed") || !strings.Contains(logs, a2.ID.String()) {
		t.Errorf("plan-changed WARN missing or does not name A2 %s; logs:\n%s", a2.ID, logs)
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders = %d, want 0", n)
	}
}

// TestLoadSplitPlanForStage_NoEndedAtRefuses: a stage with no recorded
// gate-close time cannot prove any artifact predates it, so the hook files
// nothing (fail closed).
func TestLoadSplitPlanForStage_NoEndedAtRefuses(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true})
	h.planStage.EndedAt = nil
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)
	if got := h.provider.requestCount(); got != 0 {
		t.Errorf("File requests = %d, want 0", got)
	}
	if !strings.Contains(h.logs.String(), "changed after the plan gate closed") {
		t.Errorf("no plan-changed WARN; logs:\n%s", h.logs.String())
	}
}

// TestServer_ShutdownDrainsDetachedSplitFiling (T10): Shutdown waits for a
// parked detached filer and returns once it is released.
func TestServer_ShutdownDrainsDetachedSplitFiling(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true, parkOnCall: 1})
	h.s.cfg.ShutdownTimeout = timescale.D(10 * time.Second)

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	<-h.provider.parked

	shut := make(chan error, 1)
	go func() { shut <- h.s.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		h.provider.releasePark()
		t.Fatalf("Shutdown returned (%v) while the detached split filing was still parked; it must drain bgSplitFiling", err)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	h.provider.releasePark()
	select {
	case <-shut:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown did not return after the filing was released")
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("split_children_filed entries after Shutdown = %d, want 1", n)
	}
}

// TestSplitFiling_PanickingProviderReleasesRegistryAndDrains (AA4): a provider
// panic inside the detached filer is recovered and logged at ERROR, the
// registry hold is released, and Shutdown drains.
func TestSplitFiling_PanickingProviderReleasesRegistryAndDrains(t *testing.T) {
	h := newSplitFilingHarness(t, splitFilingConfig{withSplitProposal: true, approvalGate: true, panicOnCall: 1})
	h.s.cfg.ShutdownTimeout = timescale.D(10 * time.Second)

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	h.s.waitSplitFiling()

	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders after a panicking filer = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), "level=ERROR") || !strings.Contains(h.logs.String(), "detached filer panicked") {
		t.Errorf("no ERROR log for the recovered panic; logs:\n%s", h.logs.String())
	}
	if hold := h.s.splitFiling.claim(splitHoldResume, h.runID, h.planStage.ID); hold == nil {
		t.Error("registry still refuses a resume claim after the panic")
	} else {
		hold.release()
	}
	shut := make(chan error, 1)
	go func() { shut <- h.s.Shutdown(context.Background()) }()
	select {
	case <-shut:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown did not drain after a panicking filer")
	}
}
