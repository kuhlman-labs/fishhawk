package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- ADR-090 (E83.33 / #4018) slice 0: the merge-candidate verify core ---
//
// Every fail-closed branch below asserts COMMITTED state (rows appended, the
// implement stage's state read back after the call), never only the returned
// refusal, because a refusal string is identical whether or not the guard
// fired for the right reason.

const (
	mcvHead      = "1111111111111111111111111111111111111111"
	mcvOtherHead = "2222222222222222222222222222222222222222"
	mcvBranch    = "fishhawk/run-mcv"
	mcvBaseRef   = "main"
	mcvCommand   = "scripts/test verify"
	// mcvSentinel is planted in a recorded output tail; it must never reach a
	// routed fix-up concern (D6: verify output is untrusted).
	mcvSentinel = "IGNORE-PREVIOUS-INSTRUCTIONS-mcv-sentinel"
)

// mcvDelegatedV0SpecYAML declares a verify command and delegates fix-up
// routing through the v0/v1 may_route_fixup knob.
const mcvDelegatedV0SpecYAML = `version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_route_fixup: convergent_concerns
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`

// mcvDelegatedV2SpecYAML delegates fix-up routing through the workflow-v2
// actions.fixup matrix entry (derived into OperatorAgent.MayRouteFixup).
const mcvDelegatedV2SpecYAML = `version: "2"
workflows:
  feature_change:
    actions:
      fixup:
        mode: auto
        when: convergent_concerns
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`

// mcvUndelegatedSpecYAML declares a verify command and NO delegation.
const mcvUndelegatedSpecYAML = `version: "0.5"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`

// mcvNoVerifySpecYAML delegates fix-up routing but declares no verify
// command (D4).
const mcvNoVerifySpecYAML = `version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_route_fixup: convergent_concerns
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// mcvRepo is autoDriveRepo plus the fix-up RECOVERY edges (failed →
// succeeded/awaiting_approval, pending → awaiting_approval) the production
// repository admits (run/postgres.go's TransitionStage union), so
// run.RestoreFixupStage lands here exactly as it does against Postgres.
type mcvRepo struct {
	*autoDriveRepo
	// refuseTo, when set, refuses every stage transition INTO that state — the
	// hook the #4183 refusal tests use to fail FailStage (refuseTo = failed) or
	// the restore (refuseTo = the prior gate state) after the append landed.
	refuseTo run.StageState
}

func (r *mcvRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.refuseTo != "" && to == r.refuseTo {
		return nil, fmt.Errorf("mcvRepo: injected refusal of the transition to %s", to)
	}
	r.mu.Lock()
	for _, stages := range r.stagesByRun {
		for _, st := range stages {
			if st.ID == id && run.ValidStageFixupRecoveryTransition(st.State, to) {
				st.State = to
				st.FailureCategory, st.FailureReason = nil, nil
				r.mu.Unlock()
				return st, nil
			}
		}
	}
	r.mu.Unlock()
	return r.autoDriveRepo.TransitionStage(ctx, id, to, c)
}

type mcvFixture struct {
	s     *Server
	repo  *mcvRepo
	au    *auditFake
	runID uuid.UUID
	impl  *run.Stage
}

// newMCVFixture creates a run under specYAML with its implement stage parked
// at awaiting_approval (the commit-yourself gate). The audit fake stamps real
// append-order sequences so consumption comparisons behave like the chain.
func newMCVFixture(t *testing.T, specYAML string) *mcvFixture {
	t.Helper()
	repo := &mcvRepo{autoDriveRepo: &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}}
	au := newAuditFake()
	au.stampSequence = true
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ConcernRepo:  newFakeConcernRepo(),
		ApprovalRepo: newFakeApprovalRepo(),
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
	})
	runID, _ := startDriveE2ERun(t, s, repo.driveE2ERepo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": specYAML,
	})
	stages := repo.stagesFor(runID)
	stages[0].State = run.StageStateSucceeded
	stages[1].State = run.StageStateAwaitingApproval
	return &mcvFixture{s: s, repo: repo, au: au, runID: runID, impl: stages[1]}
}

// addOpenReviewStage turns the fixture into the push_and_open_pr shape: the
// implement stage succeeded and a review stage parked at awaiting_approval.
func (f *mcvFixture) addOpenReviewStage() *run.Stage {
	review := &run.Stage{ID: uuid.New(), RunID: f.runID, Type: run.StageTypeReview,
		State: run.StageStateAwaitingApproval, Sequence: 3}
	f.repo.mu.Lock()
	f.repo.stagesByRun[f.runID] = append(f.repo.stagesByRun[f.runID], review)
	f.repo.mu.Unlock()
	f.impl.State = run.StageStateSucceeded
	return review
}

func (f *mcvFixture) runRow(t *testing.T) *run.Run {
	t.Helper()
	return getRun(t, f.repo.autoDriveRepo, f.runID)
}

// appendRow seeds a chain row in append order (so it gets a real sequence).
func (f *mcvFixture) appendRow(stageID *uuid.UUID, category string, payload any) {
	raw, _ := json.Marshal(payload)
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	f.au.appended = append(f.au.appended, audit.ChainAppendParams{
		RunID: f.runID, StageID: stageID, Category: category, Payload: raw,
	})
}

func (f *mcvFixture) seedTrigger(head string) {
	id := f.impl.ID
	f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
		Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: head,
		Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		PriorState: string(run.StageStateAwaitingApproval),
	})
}

// seedVerdict appends a merge_candidate_verified row. Because it follows any
// earlier trigger in append order, it consumes it.
func (f *mcvFixture) seedVerdict(head, result string) {
	id := f.impl.ID
	f.appendRow(&id, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: head, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		Result: result, OutputUntrusted: true,
	})
}

func (f *mcvFixture) seedRebased(newHead, mergeCommit string, upToDate bool) {
	f.appendRow(nil, CategoryBranchRebased, map[string]any{
		"new_head_sha": newHead, "merge_commit_sha": mergeCommit, "already_up_to_date": upToDate,
	})
}

func (f *mcvFixture) start(t *testing.T, head, cause string, actor mergeCandidateActor) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
	t.Helper()
	return f.s.startMergeCandidateVerifyPass(context.Background(), f.runID, mergeCandidateVerifyParams{
		Branch: mcvBranch, BaseRef: mcvBaseRef, HeadSHA: head, Cause: cause,
	}, actor)
}

func mcvOperator() mergeCandidateActor {
	return mergeCandidateActor{Kind: audit.ActorUser, Subject: "github:operator"}
}

func mcvTriggerRows(au *auditFake) []audit.ChainAppendParams {
	return auditEntries(au, CategoryStageMergeCandidateVerifyTriggered)
}

// --- startMergeCandidateVerifyPass ----------------------------------------

func TestStartMergeCandidateVerifyPass_AppendsTriggerThenReopens(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)

	start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
	if refusal != nil {
		t.Fatalf("refusal = %+v, want a started pass", refusal)
	}
	if !start.Triggered || start.State != mergeCandidateStateInFlight || start.StageID != f.impl.ID {
		t.Fatalf("start = %+v, want triggered in_flight on the implement stage", start)
	}
	rows := mcvTriggerRows(f.au)
	if len(rows) != 1 {
		t.Fatalf("trigger rows = %d, want 1", len(rows))
	}
	var p mergeCandidateVerifyTrigger
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if !p.populated() || p.ExpectedHeadSHA != mcvHead || p.VerifyCommand != mcvCommand ||
		p.Cause != mergeCandidateCauseBaseAdvance || p.PriorState != string(run.StageStateAwaitingApproval) {
		t.Errorf("trigger payload = %+v", p)
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser ||
		rows[0].ActorSubject == nil || *rows[0].ActorSubject != "github:operator" {
		t.Errorf("trigger actor = %v/%v, want the request-bound operator", rows[0].ActorKind, rows[0].ActorSubject)
	}
	if f.impl.State != run.StageStatePending {
		t.Errorf("implement state = %q, want pending (re-opened for the pass)", f.impl.State)
	}
}

// TestStartMergeCandidateVerifyPass_FailedAppendLeavesStageUntouched pins
// APPEND-THEN-REOPEN by committed state: a failed append must leave the
// implement stage at its gate. Reversing the order re-opens it first and this
// read shows pending.
func TestStartMergeCandidateVerifyPass_FailedAppendLeavesStageUntouched(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	f.au.appendErrCategory = CategoryStageMergeCandidateVerifyTriggered

	start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
	if start != nil || refusal == nil {
		t.Fatalf("start=%+v refusal=%+v, want a refusal", start, refusal)
	}
	if f.impl.State != run.StageStateAwaitingApproval {
		t.Errorf("implement state = %q, want awaiting_approval (untouched when the trigger cannot be recorded)", f.impl.State)
	}
}

func TestStartMergeCandidateVerifyPass_PerHeadIdempotence(t *testing.T) {
	t.Run("live trigger for the head returns it", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal != nil || start.State != mergeCandidateStateInFlight || start.Triggered {
			t.Fatalf("start=%+v refusal=%+v, want the live pass returned untriggered", start, refusal)
		}
		if n := len(mcvTriggerRows(f.au)); n != 1 {
			t.Errorf("trigger rows = %d, want 1 (the seeded one) — a live pass for the head must not be re-triggered", n)
		}
		if f.impl.State != run.StageStateAwaitingApproval {
			t.Errorf("implement state = %q, want untouched", f.impl.State)
		}
	})
	for _, result := range []string{mergeCandidateResultPassed, mergeCandidateResultFailed} {
		t.Run("recorded "+result+" starts nothing", func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			f.seedTrigger(mcvHead)
			f.seedVerdict(mcvHead, result)
			start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
			if refusal != nil || start.State != result || start.Triggered {
				t.Fatalf("start=%+v refusal=%+v, want state %s untriggered", start, refusal, result)
			}
			if n := len(mcvTriggerRows(f.au)); n != 1 {
				t.Errorf("trigger rows = %d, want 1 — a recorded verdict must not be re-triggered", n)
			}
		})
	}
	t.Run("not_executed may be re-triggered", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.seedVerdict(mcvHead, mergeCandidateResultNotExecuted)
		start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal != nil || !start.Triggered {
			t.Fatalf("start=%+v refusal=%+v, want a fresh trigger", start, refusal)
		}
		if n := len(mcvTriggerRows(f.au)); n != 2 {
			t.Errorf("trigger rows = %d, want 2", n)
		}
	})
}

// TestStartMergeCandidateVerifyPass_NeverSpendsFixupBudget is D3: with the
// fix-up budget at its ceiling the pass still starts, and the fix-up counter
// is unmoved. Handing run.FixupStage the fix-up counts would refuse here.
func TestStartMergeCandidateVerifyPass_NeverSpendsFixupBudget(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	seedFixupPass(t, f.au, f.runID, f.impl.ID, defaultFixupCeiling)

	start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
	if refusal != nil || !start.Triggered {
		t.Fatalf("start=%+v refusal=%+v, want a started pass despite a spent fix-up budget", start, refusal)
	}
	if n := len(auditEntries(f.au, CategoryStageFixupTriggered)); n != 0 {
		t.Errorf("appended stage_fixup_triggered rows = %d, want 0", n)
	}
	if n, _ := f.s.countFixupPasses(context.Background(), f.runID, f.impl.ID); n != defaultFixupCeiling {
		t.Errorf("fix-up pass count = %d, want %d (unmoved)", n, defaultFixupCeiling)
	}
}

// TestStartMergeCandidateVerifyPass_NoVerifyCommandIsNotRequired is D4.
func TestStartMergeCandidateVerifyPass_NoVerifyCommandIsNotRequired(t *testing.T) {
	f := newMCVFixture(t, mcvNoVerifySpecYAML)
	start, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
	if refusal != nil || start.State != mergeCandidateStateNotRequired || start.Triggered {
		t.Fatalf("start=%+v refusal=%+v, want not_required", start, refusal)
	}
	if n := len(mcvTriggerRows(f.au)); n != 0 {
		t.Errorf("trigger rows = %d, want 0", n)
	}
	if f.impl.State != run.StageStateAwaitingApproval {
		t.Errorf("implement state = %q, want untouched", f.impl.State)
	}
}

func TestStartMergeCandidateVerifyPass_SystemActorForFanIn(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	if _, refusal := f.start(t, mcvHead, mergeCandidateCauseFanIn, mergeCandidateSystemActor()); refusal != nil {
		t.Fatalf("refusal = %+v", refusal)
	}
	rows := mcvTriggerRows(f.au)
	if len(rows) != 1 || rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorSystem ||
		*rows[0].ActorSubject != mergeCandidateSystemSubject {
		t.Fatalf("trigger rows = %+v, want one system-actor row", rows)
	}
}

func TestStartMergeCandidateVerifyPass_Refusals(t *testing.T) {
	t.Run("unanchored params", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		_, refusal := f.start(t, "", mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 {
			t.Fatalf("refusal=%+v rows=%d, want refusal and no row", refusal, len(mcvTriggerRows(f.au)))
		}
	})
	t.Run("unknown cause", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		_, refusal := f.start(t, mcvHead, "bogus", mcvOperator())
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 {
			t.Fatalf("refusal=%+v, want refusal and no row", refusal)
		}
	})
	t.Run("no actor", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		_, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mergeCandidateActor{})
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 {
			t.Fatalf("refusal=%+v, want refusal and no row", refusal)
		}
	})
	t.Run("stage not at its gate", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.impl.State = run.StageStateRunning
		_, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 || f.impl.State != run.StageStateRunning {
			t.Fatalf("refusal=%+v state=%q, want refusal, no row, stage untouched", refusal, f.impl.State)
		}
	})
	t.Run("chain read error", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.au.listByCategoryErrCategory = CategoryStageMergeCandidateVerifyTriggered
		_, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal == nil || f.impl.State != run.StageStateAwaitingApproval {
			t.Fatalf("refusal=%+v state=%q, want refusal with the stage untouched", refusal, f.impl.State)
		}
	})
	t.Run("verdict read error", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
		_, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 {
			t.Fatalf("refusal=%+v, want refusal and no row", refusal)
		}
	})
	t.Run("unreadable run", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		_, refusal := f.s.startMergeCandidateVerifyPass(context.Background(), uuid.New(),
			mergeCandidateVerifyParams{Branch: mcvBranch, BaseRef: mcvBaseRef, HeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance}, mcvOperator())
		if refusal == nil || len(mcvTriggerRows(f.au)) != 0 {
			t.Fatalf("refusal=%+v, want refusal and no row", refusal)
		}
	})
}

// TestStartMergeCandidateVerifyPass_ReopenFailureSettlesTrigger: a re-open
// refused after the trigger landed (here: a terminal run) must not wedge the
// head as in flight with nothing running — the trigger is consumed by a
// not_executed row, so the head reads unverified and is re-triggerable.
func TestStartMergeCandidateVerifyPass_ReopenFailureSettlesTrigger(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	f.runRow(t).State = run.StateSucceeded
	f.seedRebased(mcvHead, mcvHead, false)

	_, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator())
	if refusal == nil {
		t.Fatal("want a refusal when the re-open fails")
	}
	verdicts := auditEntries(f.au, CategoryMergeCandidateVerified)
	if len(verdicts) != 1 {
		t.Fatalf("merge_candidate_verified rows = %d, want 1 (the not_executed settlement)", len(verdicts))
	}
	st, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvHead)
	if err != nil || st.State != mergeCandidateStateUnverified {
		t.Fatalf("state = %+v err=%v, want unverified (not wedged in_flight)", st, err)
	}
}

// --- resolveMergeCandidateVerifyTrigger -----------------------------------

func TestResolveMergeCandidateVerifyTrigger(t *testing.T) {
	ctx := context.Background()
	t.Run("live trigger is served", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID)
		if got == nil || got.ExpectedHeadSHA != mcvHead {
			t.Fatalf("trigger = %+v, want the live one", got)
		}
	})
	t.Run("consumed trigger is not served", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.seedVerdict(mcvHead, mergeCandidateResultPassed)
		if got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID); got != nil {
			t.Fatalf("trigger = %+v, want nil (consumed)", got)
		}
	})
	t.Run("a later trigger is live again", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.seedVerdict(mcvHead, mergeCandidateResultNotExecuted)
		f.seedTrigger(mcvHead)
		if got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID); got == nil {
			t.Fatal("trigger = nil, want the newer live trigger")
		}
	})
	t.Run("half-populated trigger is not served", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		id := f.impl.ID
		f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{Branch: mcvBranch})
		if got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID); got != nil {
			t.Fatalf("trigger = %+v, want nil", got)
		}
	})
	t.Run("malformed trigger is not served", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		id := f.impl.ID
		f.au.mu.Lock()
		f.au.appended = append(f.au.appended, audit.ChainAppendParams{RunID: f.runID, StageID: &id,
			Category: CategoryStageMergeCandidateVerifyTriggered, Payload: []byte("{not json")})
		f.au.mu.Unlock()
		if got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID); got != nil {
			t.Fatalf("trigger = %+v, want nil", got)
		}
	})
	t.Run("read error serves nothing", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
		if got := f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID); got != nil {
			t.Fatalf("trigger = %+v, want nil on an unreadable consumption read", got)
		}
	})
}

// --- recordMergeCandidateVerified -----------------------------------------

func TestRecordMergeCandidateVerified(t *testing.T) {
	ctx := context.Background()
	t.Run("records the head-bound result and consumes the trigger", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		rec, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultFailed,
			"", "tail "+mcvSentinel, audit.ActorAgent, "runner")
		if err != nil || rec.Duplicate || rec.Result != mergeCandidateResultFailed {
			t.Fatalf("rec=%+v err=%v", rec, err)
		}
		rows := auditEntries(f.au, CategoryMergeCandidateVerified)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		var p mergeCandidateVerifiedPayload
		_ = json.Unmarshal(rows[0].Payload, &p)
		if p.HeadSHA != mcvHead || !p.OutputUntrusted || p.TriggerSequence == 0 || p.VerifyCommand != mcvCommand {
			t.Errorf("payload = %+v", p)
		}
		if f.s.resolveMergeCandidateVerifyTrigger(ctx, f.runID, f.impl.ID) != nil {
			t.Error("trigger still live after the result was recorded")
		}
	})
	t.Run("a retried report appends nothing", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		for i := 0; i < 2; i++ {
			if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed,
				"", "", audit.ActorAgent, "runner"); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if n := len(auditEntries(f.au, CategoryMergeCandidateVerified)); n != 1 {
			t.Fatalf("rows = %d, want 1 (stage-keyed idempotency)", n)
		}
		rec, _ := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultFailed, "", "", audit.ActorAgent, "runner")
		if rec == nil || !rec.Duplicate || rec.Result != mergeCandidateResultPassed {
			t.Errorf("replay = %+v, want a duplicate carrying the first-recorded result", rec)
		}
	})
	t.Run("out-of-set result is refused", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, "green", "", "", audit.ActorAgent, "runner"); !errors.Is(err, errMergeCandidateInvalidResult) {
			t.Fatalf("err = %v, want errMergeCandidateInvalidResult", err)
		}
		if n := len(auditEntries(f.au, CategoryMergeCandidateVerified)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
	t.Run("no trigger is refused", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); !errors.Is(err, errMergeCandidateNoLiveTrigger) {
			t.Fatalf("err = %v, want errMergeCandidateNoLiveTrigger", err)
		}
	})
	t.Run("a trigger consumed by a different pass is refused", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		// A consumption row naming a different trigger sequence is not a replay.
		id := f.impl.ID
		f.appendRow(&id, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{HeadSHA: mcvHead, Result: mergeCandidateResultNotExecuted, TriggerSequence: 999})
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); !errors.Is(err, errMergeCandidateNoLiveTrigger) {
			t.Fatalf("err = %v, want errMergeCandidateNoLiveTrigger", err)
		}
	})
	t.Run("half-populated trigger is refused", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		id := f.impl.ID
		f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{Branch: mcvBranch})
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); !errors.Is(err, errMergeCandidateTriggerPayload) {
			t.Fatalf("err = %v, want errMergeCandidateTriggerPayload", err)
		}
	})
	t.Run("read errors are returned", func(t *testing.T) {
		for _, cat := range []string{CategoryStageMergeCandidateVerifyTriggered, CategoryMergeCandidateVerified} {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			f.seedTrigger(mcvHead)
			f.au.listByCategoryErrCategory = cat
			if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); err == nil {
				t.Errorf("%s read error: err = nil, want an error", cat)
			}
		}
	})
	t.Run("append error is returned", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.au.appendErrCategory = CategoryMergeCandidateVerified
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, ""); err == nil {
			t.Fatal("err = nil, want the append error")
		}
	})
	t.Run("tail is re-bounded server-side", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		long := strings.Repeat("x", 3*mergeCandidateOutputTailMax) + "END"
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultFailed, "", long, audit.ActorAgent, "runner"); err != nil {
			t.Fatal(err)
		}
		var p mergeCandidateVerifiedPayload
		_ = json.Unmarshal(auditEntries(f.au, CategoryMergeCandidateVerified)[0].Payload, &p)
		if len(p.OutputTail) > mergeCandidateOutputTailMax || !strings.HasSuffix(p.OutputTail, "END") {
			t.Errorf("tail len = %d (suffix END=%v), want <= %d keeping the end", len(p.OutputTail), strings.HasSuffix(p.OutputTail, "END"), mergeCandidateOutputTailMax)
		}
	})
	// #4250: the failure evidence, read back from the STORED row.
	recordEvidence := func(t *testing.T, ev mergeCandidateOutputEvidence) (mergeCandidateVerifiedPayload, map[string]json.RawMessage) {
		t.Helper()
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		if _, err := f.s.recordMergeCandidateVerifiedEvidence(ctx, f.runID, f.impl.ID, mergeCandidateResultFailed, "", ev, audit.ActorAgent, "runner"); err != nil {
			t.Fatal(err)
		}
		row := auditEntries(f.au, CategoryMergeCandidateVerified)[0].Payload
		var p mergeCandidateVerifiedPayload
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(row, &p)
		_ = json.Unmarshal(row, &keys)
		return p, keys
	}
	t.Run("failures are re-bounded server-side keeping the head", func(t *testing.T) {
		long := "START" + strings.Repeat("f", mergeCandidateFailuresMax-6) + "\u20ac" + strings.Repeat("z", 2*mergeCandidateFailuresMax)
		p, _ := recordEvidence(t, mergeCandidateOutputEvidence{Failures: long})
		if len(p.Failures) > mergeCandidateFailuresMax || !strings.HasPrefix(p.Failures, "START") {
			t.Errorf("failures len = %d, want <= %d keeping the head", len(p.Failures), mergeCandidateFailuresMax)
		}
		if !utf8.ValidString(p.Failures) || strings.Contains(p.Failures, "z") || !p.OutputUntrusted {
			t.Errorf("failures must be valid UTF-8 cut at the head bound under output_untrusted (untrusted=%v)", p.OutputUntrusted)
		}
	})
	t.Run("the summary is stored and re-bounded server-side", func(t *testing.T) {
		in := &mergeCandidateFailureSummary{Packages: []string{"m/a"}, Tests: []string{"TestA"}, BuildFailed: []string{"m/a"}}
		p, _ := recordEvidence(t, mergeCandidateOutputEvidence{Summary: in})
		if !reflect.DeepEqual(p.FailureSummary, in) {
			t.Errorf("stored summary = %+v, want %+v as shipped", p.FailureSummary, in)
		}

		// A runner-reported lossy flag survives the re-bound even when the
		// server itself cuts nothing.
		lossyIn := &mergeCandidateFailureSummary{Packages: []string{"m/a"}, Tests: []string{"TestA"}, Lossy: true}
		p, _ = recordEvidence(t, mergeCandidateOutputEvidence{Summary: lossyIn})
		if s := p.FailureSummary; s == nil || !s.Lossy || !reflect.DeepEqual(s, lossyIn) {
			t.Errorf("runner-lossy summary: stored %+v, want %+v (lossy kept)", s, lossyIn)
		}

		many := make([]string, 3*mergeCandidateSummaryMaxEntries)
		for i := range many {
			many[i] = fmt.Sprintf("m/p%02d", i)
		}
		p, _ = recordEvidence(t, mergeCandidateOutputEvidence{Summary: &mergeCandidateFailureSummary{Packages: many}})
		if s := p.FailureSummary; s == nil || len(s.Packages) != mergeCandidateSummaryMaxEntries || s.Packages[0] != "m/p00" || !s.Lossy {
			t.Errorf("over-long list: stored %+v, want the first %d entries and lossy", s, mergeCandidateSummaryMaxEntries)
		}

		longName := "Test" + strings.Repeat("N", 2*mergeCandidateSummaryMaxEntryBytes)
		p, _ = recordEvidence(t, mergeCandidateOutputEvidence{Summary: &mergeCandidateFailureSummary{Tests: []string{longName}}})
		if s := p.FailureSummary; s == nil || len(s.Tests) != 1 || len(s.Tests[0]) != mergeCandidateSummaryMaxEntryBytes || !s.Lossy {
			t.Errorf("over-long entry: stored %+v, want it cut to %d bytes and lossy", s, mergeCandidateSummaryMaxEntryBytes)
		}
	})
	t.Run("no evidence stores neither key", func(t *testing.T) {
		for name, ev := range map[string]mergeCandidateOutputEvidence{
			"absent":        {},
			"empty summary": {Summary: &mergeCandidateFailureSummary{Lossy: true}},
		} {
			_, keys := recordEvidence(t, ev)
			for _, k := range []string{"failures", "failure_summary"} {
				if _, ok := keys[k]; ok {
					t.Errorf("%s: stored row carries %q, want it absent", name, k)
				}
			}
		}
	})
}

// --- maybeRecoverMergeCandidateVerifyFailure ------------------------------

func TestMaybeRecoverMergeCandidateVerifyFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("live trigger: not_executed first, then the gate is restored", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		review := f.addOpenReviewStage()
		f.seedRebased(mcvHead, mcvHead, false)
		if _, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator()); refusal != nil {
			t.Fatal(refusal.Reason)
		}
		if review.State != run.StageStatePending {
			t.Fatalf("review state = %q, want pending (re-parked by the re-open)", review.State)
		}
		f.impl.State = run.StageStateFailed
		if !f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "runner crashed") {
			t.Fatal("recovered = false, want true")
		}
		if f.impl.State != run.StageStateSucceeded || review.State != run.StageStateAwaitingApproval {
			t.Errorf("implement=%q review=%q, want succeeded/awaiting_approval (restored)", f.impl.State, review.State)
		}
		st, err := f.s.mergeCandidateVerifyState(ctx, f.runRow(t), mcvHead)
		if err != nil || st.State != mergeCandidateStateUnverified {
			t.Errorf("state = %+v err=%v, want unverified (re-triggerable, not wedged in flight)", st, err)
		}
	})
	t.Run("no trigger: today's path unchanged", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.impl.State = run.StageStateFailed
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "pull_request_failed") {
			t.Fatal("recovered = true, want false without a live trigger (#4079 non-regression)")
		}
		if f.impl.State != run.StageStateFailed || len(auditEntries(f.au, CategoryMergeCandidateVerified)) != 0 {
			t.Errorf("state=%q rows=%d, want untouched", f.impl.State, len(auditEntries(f.au, CategoryMergeCandidateVerified)))
		}
	})
	t.Run("consumed trigger: today's path unchanged", func(t *testing.T) {
		// The trigger was already settled by its own result, so the recorder
		// would answer a no-op duplicate: only the live-trigger key keeps the
		// recovery from restoring a stage whose pass already reported.
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		if _, err := f.s.recordMergeCandidateVerified(ctx, f.runID, f.impl.ID, mergeCandidateResultPassed, "", "", audit.ActorAgent, "runner"); err != nil {
			t.Fatal(err)
		}
		f.impl.State = run.StageStateFailed
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "pull_request_failed") {
			t.Fatal("recovered = true, want false for a consumed trigger")
		}
		if f.impl.State != run.StageStateFailed {
			t.Errorf("implement state = %q, want failed (untouched)", f.impl.State)
		}
	})
	t.Run("consumption append fails: no restore", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		f.impl.State = run.StageStateFailed
		f.au.appendErrCategory = CategoryMergeCandidateVerified
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "x") {
			t.Fatal("recovered = true, want false")
		}
		if f.impl.State != run.StageStateFailed {
			t.Errorf("implement state = %q, want failed (no restore without the consumption row)", f.impl.State)
		}
	})
	t.Run("unparseable review stage id: no recovery", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		id := f.impl.ID
		f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
			Branch: mcvBranch, BaseRef: mcvBaseRef, ExpectedHeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance,
			VerifyCommand: mcvCommand, PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: "not-a-uuid",
		})
		f.impl.State = run.StageStateFailed
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "x") {
			t.Fatal("recovered = true, want false")
		}
		if n := len(auditEntries(f.au, CategoryMergeCandidateVerified)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
	t.Run("restore not applicable: false", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedTrigger(mcvHead)
		// The stage is not failed, so RestoreFixupStage does not apply.
		f.impl.State = run.StageStateRunning
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, f.impl.ID, "x") {
			t.Fatal("recovered = true, want false")
		}
	})
	t.Run("not an implement stage", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		plan := f.repo.stagesFor(f.runID)[0]
		if f.s.maybeRecoverMergeCandidateVerifyFailure(ctx, f.runID, plan.ID, "x") {
			t.Fatal("recovered = true, want false")
		}
	})
}

// --- mergeCandidateVerifyState --------------------------------------------

func TestMergeCandidateVerifyState_Classification(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		seed      func(f *mcvFixture)
		wantState string
		wantCause string
	}{
		{"base advance via new_head_sha", func(f *mcvFixture) { f.seedRebased(mcvHead, mcvOtherHead, false) },
			mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance},
		{"base advance via merge_commit_sha only", func(f *mcvFixture) { f.seedRebased("", mcvHead, false) },
			mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance},
		{"already-up-to-date rebase classifies nothing", func(f *mcvFixture) { f.seedRebased(mcvHead, "", true) },
			mergeCandidateStateNotRequired, ""},
		{"conflict resolution", func(f *mcvFixture) {
			f.appendRow(nil, CategoryConflictResolutionPushed, map[string]any{"head_sha": mcvHead})
		}, mergeCandidateStateUnverified, mergeCandidateCauseConflictResolution},
		{"runner-gated fix-up head", func(f *mcvFixture) {
			f.appendRow(nil, "fixup_pushed", map[string]any{"head_sha": mcvHead})
			f.appendRow(nil, auditcomplete.CategorySlicesIntegrated, map[string]any{})
		}, mergeCandidateStateNotRequired, ""},
		{"decomposed parent", func(f *mcvFixture) {
			f.appendRow(nil, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{"merge_sha": mcvOtherHead})
		}, mergeCandidateStateUnverified, mergeCandidateCauseFanIn},
		// The probed arms of rule 4 (D8 Amendment 1, #4368) live in TestClassifyVouchedHead.
		{"operator-vouched head, deployment cannot probe (D7 fallback)", func(f *mcvFixture) {
			f.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
			f.appendRow(nil, auditcomplete.CategorySlicesIntegrated, map[string]any{})
		}, mergeCandidateStateNotRequired, ""},
		{"rebase vouch does not exempt a base-advance head", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": mcvHead})
		}, mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance},
		{"unrelated head", func(f *mcvFixture) { f.seedRebased(mcvOtherHead, mcvOtherHead, false) },
			mergeCandidateStateNotRequired, ""},
		{"passed for exactly the head", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.seedTrigger(mcvHead)
			f.seedVerdict(mcvHead, mergeCandidateResultPassed)
		}, mergeCandidateStatePassed, mergeCandidateCauseBaseAdvance},
		{"failed for exactly the head", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.seedTrigger(mcvHead)
			f.seedVerdict(mcvHead, mergeCandidateResultFailed)
		}, mergeCandidateStateFailed, mergeCandidateCauseBaseAdvance},
		{"a passed verdict for another head does not satisfy this one", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.seedTrigger(mcvOtherHead)
			f.seedVerdict(mcvOtherHead, mergeCandidateResultPassed)
		}, mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance},
		{"not_executed reads unverified", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.seedTrigger(mcvHead)
			f.seedVerdict(mcvHead, mergeCandidateResultNotExecuted)
		}, mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance},
		{"live trigger reads in flight", func(f *mcvFixture) {
			f.seedRebased(mcvHead, mcvHead, false)
			f.seedTrigger(mcvHead)
		}, mergeCandidateStateInFlight, mergeCandidateCauseBaseAdvance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			tc.seed(f)
			got, err := f.s.mergeCandidateVerifyState(ctx, f.runRow(t), mcvHead)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.State != tc.wantState || got.Cause != tc.wantCause {
				t.Errorf("state = %+v, want state %q cause %q", got, tc.wantState, tc.wantCause)
			}
			if got.State == mergeCandidateStateInFlight && got.StageID != f.impl.ID {
				t.Errorf("in-flight stage = %s, want %s", got.StageID, f.impl.ID)
			}
		})
	}
}

// TestMergeCandidateVerifyState_NoVerifyIsNotRequired is D4 on the predicate:
// a base-advance head needs nothing when no verify command is declared.
func TestMergeCandidateVerifyState_NoVerifyIsNotRequired(t *testing.T) {
	f := newMCVFixture(t, mcvNoVerifySpecYAML)
	f.seedRebased(mcvHead, mcvHead, false)
	got, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvHead)
	if err != nil || got.State != mergeCandidateStateNotRequired {
		t.Fatalf("state = %+v err=%v, want not_required", got, err)
	}
}

// TestMergeCandidateVerifyState_ReadErrorsAreReturned: D2's last clause — no
// chain read error is ever read as "no requirement".
func TestMergeCandidateVerifyState_ReadErrorsAreReturned(t *testing.T) {
	for _, cat := range []string{
		CategoryBranchRebased, CategoryConflictResolutionPushed, "fixup_pushed",
		CategoryOperatorCommitVouched, auditcomplete.CategoryIntegrationCommitRecorded,
		CategoryMergeCandidateVerified, CategoryStageMergeCandidateVerifyTriggered,
	} {
		t.Run(cat, func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			// A fan-in parent head reaches every read (classification falls
			// through to the integration rows, then the verdict and trigger
			// reads), so each injected error is on the path.
			f.appendRow(nil, auditcomplete.CategorySlicesIntegrated, map[string]any{})
			f.au.listByCategoryErrCategory = cat
			got, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvHead)
			if err == nil {
				t.Fatalf("state = %+v, err = nil; want the read error returned", got)
			}
		})
	}
	t.Run("empty head", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		if _, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), ""); err == nil {
			t.Fatal("err = nil, want an error for an empty head")
		}
	})
}

// --- routeMergeCandidateFailure (D6) --------------------------------------

// recordFailedPass drives a real failed pass for mcvHead with the sentinel in
// its output tail, returning the trigger the result settled.
func recordFailedPass(t *testing.T, f *mcvFixture) *mergeCandidateVerifyTrigger {
	t.Helper()
	f.seedRebased(mcvHead, mcvHead, false)
	if _, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator()); refusal != nil {
		t.Fatal(refusal.Reason)
	}
	rec, err := f.s.recordMergeCandidateVerified(context.Background(), f.runID, f.impl.ID,
		mergeCandidateResultFailed, "", "FAIL: duplicate migration 0096\n"+mcvSentinel, audit.ActorAgent, "runner")
	if err != nil {
		t.Fatal(err)
	}
	// The report arm returns the implement stage to its gate before routing.
	f.impl.State = run.StageStateAwaitingApproval
	return &rec.Trigger
}

func TestRouteMergeCandidateFailure_DelegatedRoutesOneTrustedFixup(t *testing.T) {
	for name, specYAML := range map[string]string{"v0 may_route_fixup": mcvDelegatedV0SpecYAML, "v2 actions.fixup": mcvDelegatedV2SpecYAML} {
		t.Run(name, func(t *testing.T) {
			f := newMCVFixture(t, specYAML)
			trigger := recordFailedPass(t, f)

			routed, refusal := f.s.routeMergeCandidateFailure(context.Background(), f.runRow(t), f.impl.ID, trigger)
			if !routed || refusal != "" {
				t.Fatalf("routed=%v refusal=%q, want one routed fix-up", routed, refusal)
			}
			rows := auditEntries(f.au, CategoryStageFixupTriggered)
			if len(rows) != 1 {
				t.Fatalf("stage_fixup_triggered rows = %d, want exactly 1", len(rows))
			}
			payload := string(rows[0].Payload)
			for _, want := range []string{mcvHead, mergeCandidateCauseBaseAdvance, mcvCommand} {
				if !strings.Contains(payload, want) {
					t.Errorf("routed fix-up payload missing trusted field %q:\n%s", want, payload)
				}
			}
			if strings.Contains(payload, mcvSentinel) || strings.Contains(payload, "duplicate migration") {
				t.Errorf("routed fix-up payload carries untrusted verify output:\n%s", payload)
			}
			if rows[0].ActorSubject == nil || *rows[0].ActorSubject != mergeCandidateSystemSubject {
				t.Errorf("fix-up actor = %v, want %s", rows[0].ActorSubject, mergeCandidateSystemSubject)
			}
		})
	}
}

func TestRouteMergeCandidateFailure_NotDelegatedRoutesNothing(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	trigger := recordFailedPass(t, f)

	routed, refusal := f.s.routeMergeCandidateFailure(context.Background(), f.runRow(t), f.impl.ID, trigger)
	if routed {
		t.Fatal("routed = true, want false when fix-up routing is not delegated")
	}
	if !strings.Contains(refusal, "fishhawk_fixup_stage") {
		t.Errorf("refusal %q does not name fishhawk_fixup_stage", refusal)
	}
	if n := len(auditEntries(f.au, CategoryStageFixupTriggered)); n != 0 {
		t.Errorf("stage_fixup_triggered rows = %d, want 0", n)
	}
	if f.impl.State != run.StageStateAwaitingApproval {
		t.Errorf("implement state = %q, want untouched", f.impl.State)
	}
}

func TestRouteMergeCandidateFailure_BudgetSpentRefuses(t *testing.T) {
	f := newMCVFixture(t, mcvDelegatedV0SpecYAML)
	trigger := recordFailedPass(t, f)
	seedFixupPass(t, f.au, f.runID, f.impl.ID, defaultFixupCeiling)

	routed, refusal := f.s.routeMergeCandidateFailure(context.Background(), f.runRow(t), f.impl.ID, trigger)
	if routed {
		t.Fatal("routed = true, want false at the fix-up ceiling")
	}
	if !strings.Contains(refusal, "fishhawk_fixup_stage") {
		t.Errorf("refusal %q does not name fishhawk_fixup_stage", refusal)
	}
	if n := len(auditEntries(f.au, CategoryStageFixupTriggered)); n != 0 {
		t.Errorf("appended stage_fixup_triggered rows = %d, want 0", n)
	}
}

func TestRouteMergeCandidateFailure_FailClosedArms(t *testing.T) {
	ctx := context.Background()
	t.Run("no trigger", func(t *testing.T) {
		f := newMCVFixture(t, mcvDelegatedV0SpecYAML)
		routed, refusal := f.s.routeMergeCandidateFailure(ctx, f.runRow(t), f.impl.ID, nil)
		if routed || !strings.Contains(refusal, "fishhawk_fixup_stage") {
			t.Fatalf("routed=%v refusal=%q", routed, refusal)
		}
	})
	t.Run("unevaluable delegation", func(t *testing.T) {
		f := newMCVFixture(t, mcvDelegatedV0SpecYAML)
		trigger := recordFailedPass(t, f)
		row := f.runRow(t)
		row.WorkflowSpec = []byte("::: not yaml")
		routed, refusal := f.s.routeMergeCandidateFailure(ctx, row, f.impl.ID, trigger)
		if routed || !strings.Contains(refusal, "fishhawk_fixup_stage") {
			t.Fatalf("routed=%v refusal=%q", routed, refusal)
		}
		if n := len(auditEntries(f.au, CategoryStageFixupTriggered)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
	t.Run("fix-up count unreadable", func(t *testing.T) {
		f := newMCVFixture(t, mcvDelegatedV0SpecYAML)
		trigger := recordFailedPass(t, f)
		f.au.listByCategoryErrCategory = CategoryStageFixupTriggered
		routed, refusal := f.s.routeMergeCandidateFailure(ctx, f.runRow(t), f.impl.ID, trigger)
		if routed || !strings.Contains(refusal, "fishhawk_fixup_stage") {
			t.Fatalf("routed=%v refusal=%q", routed, refusal)
		}
	})
}

// --- runner-capability gate (#4183) ----------------------------------------

// newMCVStaleRunnerFixture starts a real base-advance pass for mcvHead and
// walks the re-opened implement stage to dispatched (the orchestrator's walk),
// the state a runner's prompt fetch meets. The run carries an issue trigger and
// NO approved plan, so a fetch that got PAST the capability gate would reach the
// forge (or record issue_context_unresolved) and append
// plan_missing_for_implement — which is what pins the gate's EARLY placement.
func newMCVStaleRunnerFixture(t *testing.T) (*mcvShipFixture, *stubIssueGetter) {
	t.Helper()
	f := newMCVShipFixture(t, mcvUndelegatedSpecYAML)
	f.seedRebased(mcvHead, mcvHead, false)
	if _, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator()); refusal != nil {
		t.Fatal(refusal.Reason)
	}
	runRow := f.runRow(t)
	ref := "issue:42"
	f.repo.mu.Lock()
	f.impl.State = run.StageStateDispatched
	runRow.TriggerRef = &ref
	f.repo.mu.Unlock()
	ig := &stubIssueGetter{}
	f.s.promptIssueGetterOverride = ig
	return f, ig
}

// mcvRefusal decodes a 409 runner_capability_missing body.
func mcvRefusal(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 %s:\n%s", w.Code, mergeCandidateReasonRunnerCapabilityMissing, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != mergeCandidateReasonRunnerCapabilityMissing {
		t.Fatalf("code = %q, want %q", env.Error.Code, mergeCandidateReasonRunnerCapabilityMissing)
	}
	if env.Error.Details["required_capability"] != capabilityMergeCandidateVerify ||
		env.Error.Details["expected_head_sha"] != mcvHead {
		t.Errorf("details = %+v", env.Error.Details)
	}
	return env.Error
}

// assertNoPreGateWork pins approval condition C2: a refused fetch never got
// past the gate, so it made no forge call and appended no
// issue_context_unresolved / plan_missing_for_implement row.
func assertNoPreGateWork(t *testing.T, f *mcvShipFixture, ig *stubIssueGetter) {
	t.Helper()
	if ig.called {
		t.Error("the refused fetch called the forge for the issue")
	}
	for _, category := range []string{issueContextUnresolvedCategory, "plan_missing_for_implement"} {
		if n := len(auditEntries(f.au, category)); n != 0 {
			t.Errorf("%s rows = %d, want 0 (the gate must run before prompt construction)", category, n)
		}
	}
}

// TestGetStagePrompt_MergeCandidateStaleRunnerRefused is the #4183 end-to-end
// crossing through the real mux: a live merge-candidate trigger fetched by a
// runner that does not advertise merge-candidate-verify is refused 409 with
// NO prompt, the pass is settled not_executed (FailStage C then restore to
// the pre-pass gate), the run stays live, the head stays re-triggerable, and a
// rebuilt runner is then served the re-started pass.
func TestGetStagePrompt_MergeCandidateStaleRunnerRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(f *mcvShipFixture) *httptest.ResponseRecorder
	}{
		{"no header", func(f *mcvShipFixture) *httptest.ResponseRecorder {
			return promptRequest(t, f.s, f.runID, f.impl.ID, f.priv, "")
		}},
		{"push-resume only", func(f *mcvShipFixture) *httptest.ResponseRecorder {
			return promptRequestWithCapabilities(t, f.s, f.impl.ID, f.priv, capabilityPushResume)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ig := newMCVStaleRunnerFixture(t)
			runBefore := f.runRow(t).State
			w := tc.do(f)
			refusal := mcvRefusal(t, w)
			if refusal.Details["settled"] != true {
				t.Errorf("settled = %v, want true", refusal.Details["settled"])
			}
			// What a stale runner decodes: no prompt to hand an agent.
			var stale struct {
				Prompt string `json:"prompt"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &stale)
			if stale.Prompt != "" {
				t.Errorf("a stale runner decoded a prompt from the refusal: %q", stale.Prompt)
			}
			rows := mcvVerdictRows(f.au)
			if len(rows) != 1 || rows[0].Result != mergeCandidateResultNotExecuted || rows[0].HeadSHA != mcvHead ||
				!strings.HasPrefix(rows[0].Reason, mergeCandidateReasonRunnerCapabilityMissing) {
				t.Fatalf("merge_candidate_verified rows = %+v, want one not_executed %s row", rows, mergeCandidateReasonRunnerCapabilityMissing)
			}
			if got := f.stageState(t, f.impl.ID); got != run.StageStateAwaitingApproval {
				t.Errorf("implement state = %q, want awaiting_approval (the pre-pass gate)", got)
			}
			if got := f.runRow(t).State; got != runBefore || got == run.StateFailed {
				t.Errorf("run state = %q, want unchanged %q", got, runBefore)
			}
			state, err := f.s.mergeCandidateVerifyState(t.Context(), f.runRow(t), mcvHead)
			if err != nil || state.State != mergeCandidateStateUnverified {
				t.Errorf("mergeCandidateVerifyState = %+v, %v; want unverified (re-triggerable)", state, err)
			}
			if n := len(auditEntries(f.au, CategoryStageFixupTriggered)); n != 0 {
				t.Errorf("stage_fixup_triggered rows = %d, want 0", n)
			}
			assertNoPreGateWork(t, f, ig)

			// The rebuilt runner: re-start the pass for the same head and fetch it
			// advertising the capability.
			if _, refusal := f.start(t, mcvHead, mergeCandidateCauseBaseAdvance, mcvOperator()); refusal != nil {
				t.Fatalf("re-trigger refused: %s", refusal.Reason)
			}
			f.repo.mu.Lock()
			f.impl.State = run.StageStateDispatched
			f.repo.mu.Unlock()
			cw := promptRequestWithCapabilities(t, f.s, f.impl.ID, f.priv, capableRunnerCapabilities)
			if cw.Code != http.StatusOK {
				t.Fatalf("capable fetch status = %d, want 200:\n%s", cw.Code, cw.Body.String())
			}
			var served promptResponse
			if err := json.Unmarshal(cw.Body.Bytes(), &served); err != nil {
				t.Fatal(err)
			}
			if !served.MergeCandidateVerify || served.MergeCandidateVerifyExpectedHeadSHA != mcvHead {
				t.Errorf("capable runner was not served the re-started pass: %+v", served)
			}
		})
	}
}

// TestGetStagePrompt_MergeCandidateStaleRunnerSettleRecordFails pins approval
// condition C1 (record-first): when the not_executed row cannot be appended,
// NOTHING else runs — no FailStage, no Advance. The stage is left exactly as
// it was, the trigger stays live for the D8 reap path, the run is not failed,
// and the runner is still answered 409 so it spawns no agent.
func TestGetStagePrompt_MergeCandidateStaleRunnerSettleRecordFails(t *testing.T) {
	f, ig := newMCVStaleRunnerFixture(t)
	runBefore := f.runRow(t).State
	f.au.appendErrCategory = CategoryMergeCandidateVerified
	refusal := mcvRefusal(t, promptRequest(t, f.s, f.runID, f.impl.ID, f.priv, ""))
	f.au.appendErrCategory = ""
	if refusal.Details["settled"] != false {
		t.Errorf("settled = %v, want false", refusal.Details["settled"])
	}
	if n := len(mcvVerdictRows(f.au)); n != 0 {
		t.Errorf("merge_candidate_verified rows = %d, want 0", n)
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateDispatched {
		t.Errorf("implement state = %q, want dispatched (a transient audit error must not transition the stage)", got)
	}
	if got := f.runRow(t).State; got != runBefore || got == run.StateFailed {
		t.Errorf("run state = %q, want unchanged %q (a transient audit error must not fail the run)", got, runBefore)
	}
	if live := f.s.resolveMergeCandidateVerifyTrigger(t.Context(), f.runID, f.impl.ID); live == nil || live.ExpectedHeadSHA != mcvHead {
		t.Errorf("live trigger = %+v, want still live for %s", live, mcvHead)
	}
	assertNoPreGateWork(t, f, ig)
}

// TestGetStagePrompt_MergeCandidateStaleRunnerFailStageFails: the row landed
// (the trigger is consumed) but failing the stage is refused. The refusal
// reports settled=false, never restores and never fails the run.
func TestGetStagePrompt_MergeCandidateStaleRunnerFailStageFails(t *testing.T) {
	f, _ := newMCVStaleRunnerFixture(t)
	runBefore := f.runRow(t).State
	f.repo.refuseTo = run.StageStateFailed
	refusal := mcvRefusal(t, promptRequest(t, f.s, f.runID, f.impl.ID, f.priv, ""))
	f.repo.refuseTo = ""
	if refusal.Details["settled"] != false {
		t.Errorf("settled = %v, want false (the stage was never failed or restored)", refusal.Details["settled"])
	}
	if rows := mcvVerdictRows(f.au); len(rows) != 1 || rows[0].Result != mergeCandidateResultNotExecuted {
		t.Fatalf("rows = %+v, want the one not_executed row written before FailStage", rows)
	}
	if got := f.stageState(t, f.impl.ID); got == run.StageStateFailed || got == run.StageStateAwaitingApproval {
		t.Errorf("implement state = %q, want neither failed nor restored", got)
	}
	if got := f.runRow(t).State; got != runBefore || got == run.StateFailed {
		t.Errorf("run state = %q, want unchanged %q", got, runBefore)
	}
}

// TestGetStagePrompt_MergeCandidateStaleRunnerRestoreFails: the row landed and
// the stage was failed, but the restore is refused. The failure path stays in
// force — Orchestrator.Advance fails the run, mirroring failPullRequestStage
// for the same pass — rather than stranding a failed stage under a live run.
func TestGetStagePrompt_MergeCandidateStaleRunnerRestoreFails(t *testing.T) {
	f, _ := newMCVStaleRunnerFixture(t)
	f.repo.refuseTo = run.StageStateAwaitingApproval
	refusal := mcvRefusal(t, promptRequest(t, f.s, f.runID, f.impl.ID, f.priv, ""))
	f.repo.refuseTo = ""
	if refusal.Details["settled"] != false {
		t.Errorf("settled = %v, want false", refusal.Details["settled"])
	}
	if got := f.stageState(t, f.impl.ID); got != run.StageStateFailed {
		t.Errorf("implement state = %q, want failed (the restore was refused)", got)
	}
	if got := f.runRow(t).State; got != run.StateFailed {
		t.Errorf("run state = %q, want failed (Advance keeps the failure path in force)", got)
	}
}

// TestRefuseMergeCandidatePassToIncapableRunner_NoLiveTrigger: a trigger
// consumed between the gate's resolve and the refusal (or never written)
// settles nothing — no row, no transition — and reports settled=false.
func TestRefuseMergeCandidatePassToIncapableRunner_NoLiveTrigger(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	f.impl.State = run.StageStateDispatched
	if f.s.refuseMergeCandidatePassToIncapableRunner(t.Context(), f.runID, f.impl.ID) {
		t.Error("settled = true, want false with no live trigger")
	}
	if n := len(mcvVerdictRows(f.au)); n != 0 {
		t.Errorf("merge_candidate_verified rows = %d, want 0", n)
	}
	if got := f.impl.State; got != run.StageStateDispatched {
		t.Errorf("implement state = %q, want dispatched (nothing to settle)", got)
	}
}

// --- E87.2 / #4293: transferred verdicts (approval condition C1) ----------

// TestNewestMergeCandidateVerdict_IncludesTransfers: in enforce the newest row
// by chain sequence wins ACROSS the recorded and transferred categories; in
// off/shadow a transferred row is ignored (the rollback contract).
func TestNewestMergeCandidateVerdict_IncludesTransfers(t *testing.T) {
	transfer := func(f *mcvFixture) {
		f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
			FromHead: mcvOtherHead, ToHead: mcvHead, RootHead: "", ReasonCode: transferReasonIndependent})
	}
	ctx := context.Background()

	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	f.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
	f.seedVerdict(mcvHead, mergeCandidateResultFailed)
	transfer(f)
	v, err := f.s.newestMergeCandidateVerdict(ctx, f.runID, mcvHead)
	if err != nil || v == nil || v.Result != mergeCandidateResultPassed || v.Source != mergeCandidateSourceTransferred ||
		v.TransferredFrom != mcvOtherHead || v.RootHead != mcvOtherHead {
		t.Fatalf("transfer newest: verdict = %+v err=%v, want passed/transferred from %s (root defaults to from_head)", v, err, mcvOtherHead)
	}
	f.seedVerdict(mcvHead, mergeCandidateResultFailed)
	if v, _ := f.s.newestMergeCandidateVerdict(ctx, f.runID, mcvHead); v == nil || v.Result != mergeCandidateResultFailed || v.Source != mergeCandidateSourceRecorded {
		t.Fatalf("recorded newest: verdict = %+v, want the later recorded failed", v)
	}
	// A malformed or other-head transfer row is skipped.
	f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, map[string]any{"to_head": mcvHead})
	f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{FromHead: mcvHead, ToHead: mcvOtherHead})
	if v, _ := f.s.newestMergeCandidateVerdict(ctx, f.runID, mcvHead); v == nil || v.Source != mergeCandidateSourceRecorded {
		t.Fatalf("malformed transfer rows changed the verdict: %+v", v)
	}

	for _, mode := range []MergeCandidateTransferMode{MergeCandidateTransferShadow, MergeCandidateTransferOff, ""} {
		g := newMCVFixture(t, mcvUndelegatedSpecYAML)
		g.s.cfg.MergeCandidateTransfer.Mode = mode
		transfer(g)
		g.au.listByCategoryErrCategory = CategoryMergeCandidateVerdictTransferred
		if v, err := g.s.newestMergeCandidateVerdict(ctx, g.runID, mcvHead); err != nil || v != nil {
			t.Errorf("mode %q: verdict = %+v err=%v, want nil (a transferred row is ignored and not even read)", mode, v, err)
		}
	}
}

func TestMergeCandidateVerifyState_TransferredSource(t *testing.T) {
	f := newMCVFixture(t, mcvUndelegatedSpecYAML)
	f.s.cfg.MergeCandidateTransfer.Mode = MergeCandidateTransferEnforce
	f.seedRebased(mcvHead, mcvHead, false)
	f.seedRebased(mcvOtherHead, mcvOtherHead, false)
	f.seedTrigger(mcvOtherHead)
	f.seedVerdict(mcvOtherHead, mergeCandidateResultPassed)
	f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
		FromHead: mcvOtherHead, ToHead: mcvHead, RootHead: "9999999999999999999999999999999999999999", ReasonCode: transferReasonIndependent})
	// #4749: the state carries the winning row's chain sequence, read back
	// from the chain (never assumed).
	lastSeq := func(category string) int64 {
		rows, err := f.au.ListForRunByCategory(context.Background(), f.runID, category)
		if err != nil || len(rows) != 1 || rows[0].Sequence == 0 {
			t.Fatalf("%s rows = %d (%v), want one sequenced row", category, len(rows), err)
		}
		return rows[0].Sequence
	}
	st, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvHead)
	want := mergeCandidateState{State: mergeCandidateStatePassed, Cause: mergeCandidateCauseBaseAdvance,
		Source: mergeCandidateSourceTransferred, TransferredFrom: mcvOtherHead, RootHead: "9999999999999999999999999999999999999999",
		VerdictSequence: lastSeq(CategoryMergeCandidateVerdictTransferred)}
	if err != nil || st != want {
		t.Errorf("state = %+v err=%v\nwant  %+v", st, err, want)
	}
	rec, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), mcvOtherHead)
	if err != nil || rec.State != mergeCandidateStatePassed || rec.Source != mergeCandidateSourceRecorded || rec.TransferredFrom != "" {
		t.Errorf("recorded state = %+v err=%v, want passed/recorded", rec, err)
	}
	if got, want := rec.VerdictSequence, lastSeq(CategoryMergeCandidateVerified); got != want {
		t.Errorf("recorded VerdictSequence = %d, want the merge_candidate_verified row's %d", got, want)
	}
}

// --- ADR-142 (#4874): scenario-corpus commits keep the recorded requirement --
//
// Every row reads the REAL mergeCandidateVerifyState. Recorded-obligation rows
// run on BOTH fixtures: mcvFixture wires no GitHub client (the ADR-090 D7
// cannot-probe deployment) and mcGateFixture is fully wired, so the
// uncertain arm (d) and the vouch ladder really probe there. The live head is
// mcvHead (scC) unless a row says otherwise; sequences are stamped in append
// order.

const (
	scS  = "5c00000000000000000000000000000000000001" // a runner-gated root
	scM  = "5c00000000000000000000000000000000000002" // a recorded producer's head
	scC1 = "5c00000000000000000000000000000000000003" // an intermediate scenario commit
	scF  = "5c00000000000000000000000000000000000004" // a gated fix-up / integrated head
	scF0 = "5c00000000000000000000000000000000000005" // an older integration head
	scV  = "5c00000000000000000000000000000000000006" // an operator-vouched head
	scG  = "5c00000000000000000000000000000000000007" // a newer gated head
	scX  = "5c00000000000000000000000000000000000008" // an unrelated SHA
	scB  = "5c00000000000000000000000000000000000009" // a base commit
	scV2 = "5c0000000000000000000000000000000000000a" // a second vouched head
	scC0 = "5c0000000000000000000000000000000000000b" // an earlier intermediate
	scC  = mcvHead                                    // the live scenario head
)

// scScenario appends an acceptance_scenarios_pushed row reporting head on
// base; a nil base omits the field, and any other non-string is written as is.
func scScenario(f *mcvFixture, head string, base any) {
	p := map[string]any{"head_sha": head, "branch": mcvBranch}
	if base != nil {
		p["base_sha"] = base
	}
	f.appendRow(nil, CategoryAcceptanceScenariosPushed, p)
}

func scHeadRow(f *mcvFixture, category, head string) {
	f.appendRow(nil, category, map[string]any{"head_sha": head})
}

func scVouch(f *mcvFixture, sha string) {
	f.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": sha})
}

func scRebase(f *mcvFixture, head string) { f.seedRebased(head, head, false) }

func scNoOp(f *mcvFixture) { f.seedRebased("", "", true) }

func scIntegration(f *mcvFixture, sha string) {
	f.appendRow(nil, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{"merge_sha": sha})
}

func scTransfer(f *mcvFixture, to string) {
	f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
		FromHead: scM, ToHead: to, ReasonCode: transferReasonIndependent})
}

// scBaseBringing is the compare pair for a terminal t that brought in base
// commit scB relative to reference ref: ref...t carries t and scB, main...t
// carries only t, so scB is a base commit t brought in.
func scBaseBringing(ref, t string) map[string][]string {
	return map[string][]string{ref + "..." + t: {t, scB}, mcvBaseRef + "..." + t: {t}}
}

// scEdit is the D8 operator-edit pair: ref...t carries only t, which main...t
// also carries, so t brings in no base commit.
func scEdit(ref, t string) map[string][]string {
	return map[string][]string{ref + "..." + t: {t}, mcvBaseRef + "..." + t: {ref, t}}
}

const (
	scD7WarnText    = "a scenario commit's reported chain is unresolved and the deployment cannot probe"
	scVouchWarnText = "cannot probe an operator-vouched head for base commits"
)

type scWant struct{ state, cause string }

var (
	scNotRequired  = scWant{mergeCandidateStateNotRequired, ""}
	scUnverBase    = scWant{mergeCandidateStateUnverified, mergeCandidateCauseBaseAdvance}
	scUnverConfl   = scWant{mergeCandidateStateUnverified, mergeCandidateCauseConflictResolution}
	scUnverFanIn   = scWant{mergeCandidateStateUnverified, mergeCandidateCauseFanIn}
	scPassedBase   = scWant{mergeCandidateStatePassed, mergeCandidateCauseBaseAdvance}
	scFailedBase   = scWant{mergeCandidateStateFailed, mergeCandidateCauseBaseAdvance}
	scInFlightBase = scWant{mergeCandidateStateInFlight, mergeCandidateCauseBaseAdvance}
)

// scRow is one classification row. want is the unprobeable (mcvFixture)
// answer; gate, when set, is the probeable (mcGateFixture) answer, else the
// same. only restricts a row to one fixture ("mcv" or "gate").
type scRow struct {
	name     string
	spec     string
	head     string
	mode     MergeCandidateTransferMode
	seed     func(f *mcvFixture)
	compares map[string][]string
	want     scWant
	gate     *scWant
	source   string
	// warn names a WARN the unprobeable run must log ("" asserts the (d)
	// residual WARN is absent); zeroCompares asserts the probeable run made
	// no compare.
	warn         string
	zeroCompares bool
	only         string
}

func scGate(w scWant) *scWant { return &w }

func runScenarioRows(t *testing.T, rows []scRow) {
	t.Helper()
	for _, tc := range rows {
		for _, fx := range []string{"mcv", "gate"} {
			if tc.only != "" && tc.only != fx {
				continue
			}
			tc, fx := tc, fx
			t.Run(tc.name+"/"+fx, func(t *testing.T) {
				spec := tc.spec
				if spec == "" {
					spec = mcvUndelegatedSpecYAML
				}
				var f *mcvFixture
				var g *mcGateFixture
				if fx == "gate" {
					g = newMCGateFixture(t, spec)
					g.gh.compares = tc.compares
					f = g.mcvFixture
				} else {
					f = newMCVFixture(t, spec)
				}
				f.s.cfg.MergeCandidateTransfer.Mode = tc.mode
				buf := captureLogger(f.s)
				tc.seed(f)
				head := tc.head
				if head == "" {
					head = scC
				}
				got, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), head)
				if err != nil {
					t.Fatalf("err = %v, want a state", err)
				}
				want := tc.want
				if fx == "gate" && tc.gate != nil {
					want = *tc.gate
				}
				if got.State != want.state || got.Cause != want.cause {
					t.Errorf("state = %q cause %q, want state %q cause %q (%+v)", got.State, got.Cause, want.state, want.cause, got)
				}
				if tc.source != "" && got.Source != tc.source {
					t.Errorf("source = %q, want %q", got.Source, tc.source)
				}
				logs := buf.String()
				if fx == "mcv" {
					if tc.warn != "" && !strings.Contains(logs, "level=WARN") || tc.warn != "" && !strings.Contains(logs, tc.warn) {
						t.Errorf("logs lack the WARN %q:\n%s", tc.warn, logs)
					}
					if tc.warn != scD7WarnText && strings.Contains(logs, scD7WarnText) {
						t.Errorf("logs carry the scenario D7 WARN on a row that must not reach (d):\n%s", logs)
					}
				}
				if fx == "gate" {
					if strings.Contains(logs, scD7WarnText) {
						t.Errorf("a probeable run logged the cannot-probe WARN:\n%s", logs)
					}
					if tc.zeroCompares && len(g.gh.compareKeys) != 0 {
						t.Errorf("compares = %v, want none", g.gh.compareKeys)
					}
				}
			})
		}
	}
}

// TestMergeCandidateVerifyState_ScenarioCommit pins ADR-142 D1, D3, D4 and D5
// on the real predicate, for every merge-gate, no-op, uncertain-chain and
// parent row in the plan's test strategy. The rows named "ordinary S + C on
// S" and "fix-up F after a rebase + C on F" are the positive proof that a
// genuine runner-gated root keeps today's answer.
func TestMergeCandidateVerifyState_ScenarioCommit(t *testing.T) {
	rebasedM := func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scRebase(f, scM) }
	rows := []scRow{
		// Merge gate: a recorded rebase M, then C on M.
		{name: "M unverified, C on M", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM) }, want: scUnverBase, zeroCompares: true},
		{name: "M passed, C on M", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "M failed, C on M", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultFailed)
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "M in flight, C on M", seed: func(f *mcvFixture) { rebasedM(f); f.seedTrigger(scM); scScenario(f, scC, scM) }, want: scUnverBase},
		{name: "conflict-resolution M, C on M", seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scHeadRow(f, CategoryConflictResolutionPushed, scM)
			scScenario(f, scC, scM)
		}, want: scUnverConfl},
		{name: "parent F passed, C on F", seed: func(f *mcvFixture) {
			scIntegration(f, scF)
			f.seedVerdict(scF, mergeCandidateResultPassed)
			scScenario(f, scC, scF)
		}, want: scUnverFanIn},
		{name: "C1 passed then C2", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC1, scM)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "C1 failed then C2", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC1, scM)
			f.seedVerdict(scC1, mergeCandidateResultFailed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "C1 in flight then C2", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC1, scM)
			f.seedTrigger(scC1)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "exact C2 passed, C1 and M failed", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultFailed)
			scScenario(f, scC1, scM)
			f.seedVerdict(scC1, mergeCandidateResultFailed)
			scScenario(f, scC, scC1)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scPassedBase},
		{name: "exact C2 failed, C1 and M passed", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scScenario(f, scC1, scM)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scFailedBase},
		{name: "exact C2 live, C1 and M passed", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scScenario(f, scC1, scM)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
			f.seedTrigger(scC)
		}, want: scInFlightBase},
		{name: "exact C passed, nothing at M", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC, scM)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scPassedBase},
		{name: "not_executed at C", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC, scM)
			f.seedTrigger(scC)
			f.seedVerdict(scC, mergeCandidateResultNotExecuted)
		}, want: scUnverBase},
		{name: "no declared verify", spec: mcvNoVerifySpecYAML, seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM) }, want: scNotRequired, zeroCompares: true},
		{name: "non-scenario ordinary head", head: scX, seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM) }, want: scNotRequired},
		{name: "ordinary S + C on S", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, scS) }, want: scNotRequired, zeroCompares: true},
		{name: "fix-up F after a rebase + C on F", seed: func(f *mcvFixture) {
			rebasedM(f)
			scHeadRow(f, "fixup_pushed", scF)
			scScenario(f, scC, scF)
		}, want: scNotRequired, zeroCompares: true},
		{name: "undecodable branch_rebased below C", seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			f.appendRow(nil, CategoryBranchRebased, map[string]any{"already_up_to_date": "yes"})
			scScenario(f, scC, scS)
		}, want: scUnverBase},
		{name: "reset after the rebase", seed: func(f *mcvFixture) {
			rebasedM(f)
			f.appendRow(nil, CategoryBranchReset, map[string]any{"target_sha": scS})
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "recorded rebase, malformed base", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, 42) }, want: scUnverBase, zeroCompares: true},
		{name: "recorded rebase, missing base", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, nil) }, want: scUnverBase},
		{name: "recorded rebase, self-referencing base", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scC) }, want: scUnverBase},
		{name: "recorded rebase, cyclic base", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC1, scC)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "recorded rebase, unrelated base", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scX) }, want: scUnverBase},
		{name: "recorded rebase, C alleging older gated S", seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scS) }, want: scUnverBase},
		// Uncertain chains with no recorded obligation (ADR-142 D3).
		{name: "gated run, malformed base", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, 42) }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "gated run, missing base", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, nil) }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "gated run, empty base", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, "") }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "gated run, self-referencing base", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, scC) }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "gated run, cyclic base", seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scScenario(f, scC1, scC)
			scScenario(f, scC, scC1)
		}, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "gated run, unrelated base", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scScenario(f, scC, scX) }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		// Transfer modes: a suggested transfer is not evidence.
		{name: "enforce transfer to C", mode: MergeCandidateTransferEnforce, seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM); scTransfer(f, scC) }, want: scPassedBase, source: mergeCandidateSourceTransferred},
		{name: "shadow transfer to C", mode: MergeCandidateTransferShadow, seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM); scTransfer(f, scC) }, want: scUnverBase},
		{name: "off transfer to C", mode: MergeCandidateTransferOff, seed: func(f *mcvFixture) { rebasedM(f); scScenario(f, scC, scM); scTransfer(f, scC) }, want: scUnverBase},
		{name: "enforce transfer to M, live C", mode: MergeCandidateTransferEnforce, seed: func(f *mcvFixture) { rebasedM(f); scTransfer(f, scM); scScenario(f, scC, scM) }, want: scUnverBase},
		// No-op rows (ADR-142 D1): an already_up_to_date row never masks a write.
		{name: "M, one no-op, C on M", seed: func(f *mcvFixture) { rebasedM(f); scNoOp(f); scScenario(f, scC, scM) }, want: scUnverBase},
		{name: "M, two no-ops, C on M", seed: func(f *mcvFixture) { rebasedM(f); scNoOp(f); scNoOp(f); scScenario(f, scC, scM) }, want: scUnverBase},
		{name: "M, no-op, passed at M, C on M", seed: func(f *mcvFixture) {
			rebasedM(f)
			scNoOp(f)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "conflict H, no-op, C on H", seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scHeadRow(f, CategoryConflictResolutionPushed, scM)
			scNoOp(f)
			scScenario(f, scC, scM)
		}, want: scUnverConfl},
		{name: "parent F, no-op, C on F", seed: func(f *mcvFixture) { scIntegration(f, scF); scNoOp(f); scScenario(f, scC, scF) }, want: scUnverFanIn},
		{name: "undecodable rebase, no-op, C", seed: func(f *mcvFixture) {
			f.appendRow(nil, CategoryBranchRebased, map[string]any{"already_up_to_date": "yes"})
			scNoOp(f)
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "undecodable rebase alone, C", seed: func(f *mcvFixture) {
			f.appendRow(nil, CategoryBranchRebased, map[string]any{"already_up_to_date": "yes"})
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "decodable rebase without the no-op key, C", seed: func(f *mcvFixture) {
			f.appendRow(nil, CategoryBranchRebased, map[string]any{"new_head_sha": scM})
			scScenario(f, scC, scM)
		}, want: scUnverBase},
		{name: "gated S, no-op, C on S", seed: func(f *mcvFixture) { scHeadRow(f, "pull_request_opened", scS); scNoOp(f); scScenario(f, scC, scS) }, want: scNotRequired},
		{name: "rebase M, fix-up F, no-op, C on F", seed: func(f *mcvFixture) {
			rebasedM(f)
			scHeadRow(f, "fixup_pushed", scF)
			scNoOp(f)
			scScenario(f, scC, scF)
		}, want: scNotRequired},
		{name: "no-op alone, C", seed: func(f *mcvFixture) { scNoOp(f); scScenario(f, scC, scS) }, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "operator vouch of exact C after a recorded rebase", only: "mcv", seed: func(f *mcvFixture) {
			rebasedM(f)
			scScenario(f, scC, scM)
			scVouch(f, scC)
		}, want: scNotRequired, warn: scVouchWarnText},
		// Older gated roots and parents.
		{name: "gated S, gated F, C alleging older S", seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scHeadRow(f, "fixup_pushed", scF)
			scScenario(f, scC, scS)
		}, want: scNotRequired, gate: scGate(scUnverBase), warn: scD7WarnText},
		{name: "parent F0, gated fix-up F, C on F", seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			scHeadRow(f, "fixup_pushed", scF)
			scScenario(f, scC, scF)
		}, want: scUnverFanIn},
		// Parent with no rebase row (criterion parent-scenario-commit-needs-exact-pass).
		{name: "parent: integration F, C on F", seed: func(f *mcvFixture) { scIntegration(f, scF); scScenario(f, scC, scF) }, want: scUnverFanIn},
		{name: "parent: passed then failed at F", seed: func(f *mcvFixture) {
			scIntegration(f, scF)
			f.seedVerdict(scF, mergeCandidateResultPassed)
			f.seedVerdict(scF, mergeCandidateResultFailed)
			scScenario(f, scC, scF)
		}, want: scUnverFanIn},
		{name: "parent: exact C passed, F failed", seed: func(f *mcvFixture) {
			scIntegration(f, scF)
			f.seedVerdict(scF, mergeCandidateResultFailed)
			scScenario(f, scC, scF)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scWant{mergeCandidateStatePassed, mergeCandidateCauseFanIn}},
		{name: "parent: exact C failed, F passed", seed: func(f *mcvFixture) {
			scIntegration(f, scF)
			f.seedVerdict(scF, mergeCandidateResultPassed)
			scScenario(f, scC, scF)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scWant{mergeCandidateStateFailed, mergeCandidateCauseFanIn}},
		{name: "parent: exact C live", seed: func(f *mcvFixture) {
			scIntegration(f, scF)
			f.seedVerdict(scF, mergeCandidateResultPassed)
			scScenario(f, scC, scF)
			f.seedTrigger(scC)
		}, want: scWant{mergeCandidateStateInFlight, mergeCandidateCauseFanIn}},
		{name: "parent: slices_integrated only, C", seed: func(f *mcvFixture) {
			f.appendRow(nil, auditcomplete.CategorySlicesIntegrated, map[string]any{})
			scScenario(f, scC, scF)
		}, want: scUnverFanIn},
		{name: "parent: no declared verify", spec: mcvNoVerifySpecYAML, seed: func(f *mcvFixture) { scIntegration(f, scF); scScenario(f, scC, scF) }, want: scNotRequired},
		// Vouched terminals (ADR-142 D2), ladder answers on the probeable fixture.
		{name: "vouched terminal: parent, F...V edit, C on V", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scUnverFanIn},
		{name: "vouched terminal: standalone, F...V edit, C on V", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scNotRequired},
		{name: "vouched terminal: standalone, base-bringing, C on V", only: "gate", compares: scBaseBringing(scF, scV), seed: func(f *mcvFixture) {
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "vouched terminal: parent, base-bringing, C on V", only: "gate", compares: scBaseBringing(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "vouched terminal: preempted by a recorded rebase", only: "gate", compares: scEdit(scM, scV), zeroCompares: true, seed: func(f *mcvFixture) {
			rebasedM(f)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "vouched terminal: newer gated F, empty ladder (characterization)", only: "gate",
			compares: map[string][]string{scF + "..." + scV: {}}, seed: func(f *mcvFixture) {
				scVouch(f, scV)
				scHeadRow(f, "fixup_pushed", scF)
				scScenario(f, scC, scV)
			}, want: scNotRequired},
		{name: "vouched terminal, unprobeable: standalone", only: "mcv", seed: func(f *mcvFixture) {
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scNotRequired, warn: scVouchWarnText},
		{name: "vouched terminal, unprobeable: parent", only: "mcv", seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, scV)
			scScenario(f, scC, scV)
		}, want: scUnverFanIn, warn: scVouchWarnText},
	}
	runScenarioRows(t, rows)
}

// TestMergeCandidateVerifyState_ScenarioTerminalVouch pins ADR-142 D2's
// isolated reference and D5 on the real predicate (step 19). Standalone
// gated F and vouch V unless a row says otherwise; compares follow git's
// (merge-base, head] semantics, keyed on the reference the ladder must use.
func TestMergeCandidateVerifyState_ScenarioTerminalVouch(t *testing.T) {
	gatedVouch := func(f *mcvFixture) { scHeadRow(f, "fixup_pushed", scF); scVouch(f, scV) }
	withC1 := func(f *mcvFixture, c1 string) {
		gatedVouch(f)
		scScenario(f, scC1, scV)
		if c1 != "" {
			f.seedVerdict(scC1, c1)
		}
		scScenario(f, scC, scC1)
	}
	bb := scBaseBringing(scF, scV)
	rows := []scRow{
		{name: "no intermediate, C absent", only: "gate", compares: bb, seed: func(f *mcvFixture) { gatedVouch(f); scScenario(f, scC, scV) }, want: scUnverBase},
		{name: "no intermediate, C passed", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC, scV)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scPassedBase},
		{name: "no intermediate, C failed", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC, scV)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scFailedBase},
		{name: "no intermediate, C live", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC, scV)
			f.seedTrigger(scC)
		}, want: scInFlightBase},
		{name: "passed C1, C2 absent", only: "gate", compares: bb, seed: func(f *mcvFixture) { withC1(f, mergeCandidateResultPassed) }, want: scUnverBase},
		{name: "passed C1, C2 passed", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scPassedBase},
		{name: "passed C1, C2 failed", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scFailedBase},
		{name: "passed C1, C2 live", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			f.seedTrigger(scC)
		}, want: scInFlightBase},
		{name: "any number: passed C0, passed C1, C absent", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC0, scV)
			f.seedVerdict(scC0, mergeCandidateResultPassed)
			scScenario(f, scC1, scC0)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "any number: failed C0, passed C1, C absent", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC0, scV)
			f.seedVerdict(scC0, mergeCandidateResultFailed)
			scScenario(f, scC1, scC0)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "fork: passed C1 on V, reset to V, C alleging V", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			f.appendRow(nil, CategoryBranchReset, map[string]any{"target_sha": scV})
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "out-of-order vouch after the scenario row", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			scHeadRow(f, "fixup_pushed", scF)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scVouch(f, scV)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "enforce transfer to C1, C absent", only: "gate", mode: MergeCandidateTransferEnforce, compares: bb, seed: func(f *mcvFixture) {
			withC1(f, "")
			scTransfer(f, scC1)
		}, want: scUnverBase},
		{name: "enforce transfer to exact C", only: "gate", mode: MergeCandidateTransferEnforce, compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			scTransfer(f, scC)
		}, want: scPassedBase, source: mergeCandidateSourceTransferred},
		{name: "shadow transfer to exact C", only: "gate", mode: MergeCandidateTransferShadow, compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			scTransfer(f, scC)
		}, want: scUnverBase},
		{name: "off transfer to exact C", only: "gate", mode: MergeCandidateTransferOff, compares: bb, seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			scTransfer(f, scC)
		}, want: scUnverBase},
		{name: "parent base-bringing, passed C1, C absent", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
		}, want: scUnverBase},
		{name: "parent edit, no intermediate, C absent", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			gatedVouch(f)
			scScenario(f, scC, scV)
		}, want: scUnverFanIn},
		{name: "parent edit, passed C1, C absent", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
		}, want: scUnverFanIn},
		{name: "parent edit, passed C1, C passed", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
			f.seedVerdict(scC, mergeCandidateResultPassed)
		}, want: scWant{mergeCandidateStatePassed, mergeCandidateCauseFanIn}},
		{name: "parent edit, passed C1, C failed", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scWant{mergeCandidateStateFailed, mergeCandidateCauseFanIn}},
		{name: "parent edit, passed C1, C live", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
			f.seedTrigger(scC)
		}, want: scWant{mergeCandidateStateInFlight, mergeCandidateCauseFanIn}},
		{name: "standalone edit, passed C1, C absent", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) { withC1(f, mergeCandidateResultPassed) }, want: scNotRequired},
		{name: "standalone edit, passed C1, C failed (characterization)", only: "gate", compares: scEdit(scF, scV), seed: func(f *mcvFixture) {
			withC1(f, mergeCandidateResultPassed)
			f.seedVerdict(scC, mergeCandidateResultFailed)
		}, want: scNotRequired},
		{name: "terminal's own pass", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			f.seedVerdict(scV, mergeCandidateResultPassed)
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "newest gated G, G...V empty", only: "gate", compares: map[string][]string{scG + "..." + scV: {}}, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scHeadRow(f, "fixup_pushed", scG)
			scScenario(f, scC, scV)
		}, want: scNotRequired},
		{name: "newest gated G, passed C1, G...V empty", only: "gate", compares: map[string][]string{scG + "..." + scV: {}}, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scHeadRow(f, "fixup_pushed", scG)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scNotRequired},
		{name: "newest gated G, G...V base-bringing", only: "gate", compares: scBaseBringing(scG, scV), seed: func(f *mcvFixture) {
			gatedVouch(f)
			scHeadRow(f, "fixup_pushed", scG)
			scScenario(f, scC, scV)
		}, want: scUnverBase},
		{name: "recorded priority: rebase M, passed M, vouch V, passed C1", only: "gate", compares: scEdit(scM, scV), zeroCompares: true, seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scRebase(f, scM)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scVouch(f, scV)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "recorded priority: conflict H", only: "gate", compares: scEdit(scM, scV), zeroCompares: true, seed: func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scHeadRow(f, CategoryConflictResolutionPushed, scM)
			f.seedVerdict(scM, mergeCandidateResultPassed)
			scVouch(f, scV)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverConfl},
		{name: "recorded priority: integration F0 newest", only: "gate", compares: scEdit(scF, scV), zeroCompares: true, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scIntegration(f, scF0)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverFanIn},
		{name: "non-scenario reference: passed V, vouch V2 on V", only: "gate",
			compares: map[string][]string{
				scV + "..." + scV2: {scV2}, scF + "..." + scV2: {scV, scV2, scB}, mcvBaseRef + "..." + scV2: {scF, scV, scV2},
			}, seed: func(f *mcvFixture) {
				scHeadRow(f, "fixup_pushed", scF)
				f.seedVerdict(scV, mergeCandidateResultPassed)
				scVouch(f, scV2)
				scScenario(f, scC, scV2)
			}, want: scNotRequired},
		{name: "only scenario evidence, no gated head", only: "gate", compares: bb, zeroCompares: true, seed: func(f *mcvFixture) {
			scVouch(f, scV)
			scScenario(f, scC1, scV)
			f.seedVerdict(scC1, mergeCandidateResultPassed)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "gated row at a scenario SHA", only: "gate", compares: bb, seed: func(f *mcvFixture) {
			gatedVouch(f)
			scScenario(f, scC1, scV)
			scHeadRow(f, "fixup_pushed", scC1)
			scScenario(f, scC, scC1)
		}, want: scUnverBase},
		{name: "unprobeable: standalone, passed C1", only: "mcv", seed: func(f *mcvFixture) { withC1(f, mergeCandidateResultPassed) }, want: scNotRequired, warn: scVouchWarnText},
		{name: "unprobeable: parent, passed C1", only: "mcv", seed: func(f *mcvFixture) {
			scIntegration(f, scF0)
			withC1(f, mergeCandidateResultPassed)
		}, want: scUnverFanIn, warn: scVouchWarnText},
		{name: "no declared verify", spec: mcvNoVerifySpecYAML, compares: bb, zeroCompares: true, seed: func(f *mcvFixture) { withC1(f, mergeCandidateResultPassed) }, want: scNotRequired},
		{name: "unresolved chain beside a malformed vouch row", only: "gate", compares: bb, zeroCompares: true, seed: func(f *mcvFixture) {
			scHeadRow(f, "fixup_pushed", scF)
			scVouch(f, "")
			scScenario(f, scC, "")
		}, want: scUnverBase},
	}
	runScenarioRows(t, rows)

	t.Run("pinned compares name F, never a scenario head", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = bb
		withC1(g.mcvFixture, mergeCandidateResultPassed)
		if _, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), scC); err != nil {
			t.Fatalf("err = %v", err)
		}
		want := []string{scF + "..." + scV, mcvBaseRef + "..." + scV}
		if !reflect.DeepEqual(g.gh.compareKeys, want) {
			t.Errorf("compare keys = %v, want %v (reference F, not C1)", g.gh.compareKeys, want)
		}
	})
	t.Run("a compare read error through the arm is returned", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = bb
		g.gh.compareStatusByKey = map[string]int{scF + "..." + scV: http.StatusInternalServerError}
		withC1(g.mcvFixture, mergeCandidateResultPassed)
		if got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), scC); err == nil {
			t.Fatalf("state = %+v, err = nil; want the compare error returned", got)
		}
	})
}

// scArmedReadErr fails exactly ONE ListForRunByCategory read of category
// fail: the first one made at or after the first read of category arm. Arming
// on acceptance_scenarios_pushed (rule 4a's first read, which rules 1 to 4 never
// make) means an earlier rule's read of the same category is never the one
// that fails, and failing it only once means a later read (the verdict read
// after classification) cannot re-surface an error the arm swallowed.
type scArmedReadErr struct {
	audit.Repository
	arm, fail    string
	armed, fired bool
}

func (a *scArmedReadErr) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.arm {
		a.armed = true
	}
	if a.armed && !a.fired && category == a.fail {
		a.fired = true
		return nil, errors.New(scArmedReadErrText + category)
	}
	return a.Repository.ListForRunByCategory(ctx, runID, category)
}

const scArmedReadErrText = "scArmedReadErr: injected scenario-arm read error for "

// TestMergeCandidateVerifyState_ScenarioReadErrorsAreReturned: ADR-142 D4 —
// every read the scenario arm adds returns its error, through a vouched
// terminal chain that reaches each one. Rules 1 to 4 read several of these
// categories BEFORE rule 4a, and the verdict read re-reads
// merge_candidate_verified after it, so a category-wide fault would surface
// from those reads even if the arm swallowed its own; scArmedReadErr fails only
// the arm's first read of the category, so a swallowed error leaves err nil.
func TestMergeCandidateVerifyState_ScenarioReadErrorsAreReturned(t *testing.T) {
	seed := func(t *testing.T) *mcGateFixture {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = scBaseBringing(scF, scV)
		scHeadRow(g.mcvFixture, "fixup_pushed", scF)
		scVouch(g.mcvFixture, scV)
		scScenario(g.mcvFixture, scC, scV)
		return g
	}
	t.Run("baseline: the chain reaches the terminal ladder and answers base_advance", func(t *testing.T) {
		g := seed(t)
		got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), scC)
		if err != nil || got.State != mergeCandidateStateUnverified || got.Cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("state = %+v err=%v, want unverified base_advance with no fault", got, err)
		}
	})
	for _, cat := range []string{
		CategoryAcceptanceScenariosPushed, CategoryBranchRebased, CategoryConflictResolutionPushed,
		auditcomplete.CategoryIntegrationCommitRecorded, auditcomplete.CategorySlicesIntegrated,
		"pull_request_opened", "fixup_pushed", "child_pushed", CategoryOperatorCommitVouched,
		CategoryMergeCandidateVerified,
	} {
		t.Run(cat, func(t *testing.T) {
			g := seed(t)
			fault := &scArmedReadErr{Repository: g.au, arm: CategoryAcceptanceScenariosPushed, fail: cat}
			g.s.cfg.AuditRepo = fault
			if g.au.listByCategoryErr != nil || g.au.listByCategoryErrCategory != "" {
				t.Fatal("the fake's own read errors must stay unset")
			}
			got, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), scC)
			if !fault.fired {
				t.Fatalf("the scenario arm never read %s (state %+v, err %v); the row does not reach it", cat, got, err)
			}
			if err == nil || !strings.Contains(err.Error(), scArmedReadErrText+cat) {
				t.Fatalf("state = %+v, err = %v; want the scenario arm's %s read error returned", got, err, cat)
			}
		})
	}
	t.Run("scenario rows read error on an otherwise not_required head", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		scHeadRow(f, "pull_request_opened", scS)
		f.au.listByCategoryErrCategory = CategoryAcceptanceScenariosPushed
		if got, err := f.s.mergeCandidateVerifyState(context.Background(), f.runRow(t), scC); err == nil {
			t.Fatalf("state = %+v, err = nil; want the scenario read error returned", got)
		}
	})
}

// TestMergeCandidateGate_CannotProbeStaysUngatedWithScenarioObligation is a
// characterization of ADR-090 D7, not a new control: with no GitHub client
// the merge gate proceeds with its WARN BEFORE reading state, while the
// state reader reports the recorded requirement. A state reader can require
// verification there; this change does not enforce it at merge.
func TestMergeCandidateGate_CannotProbeStaysUngatedWithScenarioObligation(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(f *mcvFixture)
		want scWant
	}{
		{"standalone rebase", func(f *mcvFixture) {
			scHeadRow(f, "pull_request_opened", scS)
			scRebase(f, scM)
			scScenario(f, scC, scM)
		}, scUnverBase},
		{"parent, no rebase", func(f *mcvFixture) { scIntegration(f, scF); scScenario(f, scC, scF) }, scUnverFanIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
			g.s.cfg.GitHub = nil
			buf := captureLogger(g.s)
			tc.seed(g.mcvFixture)
			res := g.s.mergeCandidateGate(context.Background(), g.runRow(t))
			if res.Code != "" {
				t.Errorf("gate code = %q, want ungated (ADR-090 D7)", res.Code)
			}
			if !strings.Contains(buf.String(), "proceeding ungated (ADR-090 D7)") {
				t.Errorf("logs lack the D7 gate WARN:\n%s", buf.String())
			}
			st, err := g.s.mergeCandidateVerifyState(context.Background(), g.runRow(t), scC)
			if err != nil || st.State != tc.want.state || st.Cause != tc.want.cause {
				t.Errorf("state = %+v err=%v, want %+v", st, err, tc.want)
			}
		})
	}
}

// TestMergeCandidateScenarioTerminalVouch_GateRefusesFinalHeadThenRebaseStartsPass
// drives the real merge handler at a final scenario head C2 on a vouched,
// base-bringing terminal with a passed intermediate C1, then the real rebase
// verb's up-to-date arm. It wires no queue, so it proves the refusal and the
// exact-head trigger, not admission (TestMergeCandidateScenarioCommit_
// QueueRecoveryNeedsCurrentApprovalAtC owns admission); a hold is never a
// started pass.
func TestMergeCandidateScenarioTerminalVouch_GateRefusesFinalHeadThenRebaseStartsPass(t *testing.T) {
	seed := func(f *mcvFixture) {
		scHeadRow(f, "fixup_pushed", scF)
		scVouch(f, scV)
		scScenario(f, scC1, scV)
		f.seedVerdict(scC1, mergeCandidateResultPassed)
		scScenario(f, scC, scC1)
	}
	t.Run("unverified at C2 refuses", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = scBaseBringing(scF, scV)
		seed(g.mcvFixture)
		env := g.assertRefused(t, g.post(t), http.StatusConflict, mergeCodeCandidateUnverified)
		if d := env.Error.Details; d["cause"] != mergeCandidateCauseBaseAdvance || d["head_sha"] != scC ||
			d["verify_state"] != mergeCandidateStateUnverified {
			t.Errorf("details = %v, want cause base_advance, head %s, unverified", d, scC)
		}
	})
	t.Run("failed at C2 refuses verify_failed", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = scBaseBringing(scF, scV)
		seed(g.mcvFixture)
		g.seedVerdict(scC, mergeCandidateResultFailed)
		g.assertRefused(t, g.post(t), http.StatusConflict, mergeCodeCandidateVerifyFailed)
	})
	t.Run("passed at C2 admits", func(t *testing.T) {
		g := newMCGateFixture(t, mcvUndelegatedSpecYAML)
		g.gh.compares = scBaseBringing(scF, scV)
		seed(g.mcvFixture)
		g.seedVerdict(scC, mergeCandidateResultPassed)
		g.assertAdmitted(t, g.post(t))
	})
	t.Run("rebase up-to-date arm triggers a pass naming C2", func(t *testing.T) {
		stub := upToDateRebaseStub()
		stub.headSHA = scC
		stub.compareByPair = map[string][]string{
			scF + "..." + scV:           {scV, scB},
			rebaseBaseRef + "..." + scV: {scV},
		}
		sd := seedMCVRebase(t, stub)
		impl := crImplementStage(t, sd)
		seedMCVRow(sd, nil, "fixup_pushed", map[string]any{"head_sha": scF})
		seedMCVRow(sd, nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": scV})
		seedMCVRow(sd, nil, CategoryAcceptanceScenariosPushed, map[string]any{"head_sha": scC1, "base_sha": scV})
		seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
			HeadSHA: scC1, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand, Result: mergeCandidateResultPassed})
		seedMCVRow(sd, nil, CategoryAcceptanceScenariosPushed, map[string]any{"head_sha": scC, "base_sha": scC1})

		resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true}, withRebaseOperator))
		if resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
			t.Errorf("state/triggered = %q/%v, want in_flight/true", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
		}
		triggers := mcvTriggerPayloads(t, sd.au)
		if len(triggers) != 1 || triggers[0].ExpectedHeadSHA != scC || triggers[0].Cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("triggers = %+v, want one naming expected_head_sha %s cause base_advance", triggers, scC)
		}
	})
}

// scReportScenarioCommit records C on base through the REAL
// recordAcceptanceScenariosPushed route, with the forge configured as a real
// push leaves it (ADR-141): the PR head is C and C's first parent is base.
func scReportScenarioCommit(t *testing.T, sd *rebaseSeed, stage *run.Stage, c, base string) {
	t.Helper()
	sd.stub.mu.Lock()
	sd.stub.headSHA = c
	if sd.stub.mergeParents == nil {
		sd.stub.mergeParents = map[string][]string{}
	}
	sd.stub.mergeParents[c] = []string{base}
	sd.stub.mu.Unlock()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+sd.runID.String()+"/stages/"+stage.ID.String()+"/pull-request", nil)
	subj := "runner"
	sd.s.recordAcceptanceScenariosPushed(rec, req, sd.runID, stage, &pullRequestBody{
		Outcome: outcomeAcceptanceScenariosPushed, Branch: rebaseBranchName, HeadSHA: c, BaseSHA: base,
	}, "run_token", audit.ActorSystem, &subj)
	if rec.Code != http.StatusOK {
		t.Fatalf("scenario push report = %d %s, want 200 (forge head %s, first parent %s)", rec.Code, rec.Body.String(), c, base)
	}
	if rows := auditEntries(sd.au, CategoryAcceptanceScenariosPushed); len(rows) != 1 {
		t.Fatalf("acceptance_scenarios_pushed rows = %d, want 1 written by the real route", len(rows))
	}
}

// TestMergeCandidateScenarioCommit_GateRefusesThenRebaseStartsExactHeadPass
// drives the merge-gate seam end to end: a clean rebase S -> M, C recorded
// through the REAL scenario report route, the real merge gate refusing at C,
// the real rebase verb's up-to-date arm starting a pass whose trigger names
// exactly C with cause base_advance, a passed verdict at M changing nothing
// and a passed verdict at C admitting. It wires no queue, so it proves the
// refusal and the exact-head trigger, not admission.
func TestMergeCandidateScenarioCommit_GateRefusesThenRebaseStartsExactHeadPass(t *testing.T) {
	ctx := context.Background()
	stub := upToDateRebaseStub()
	stub.headSHA = scM
	sd := seedMCVRebase(t, stub)
	impl := crImplementStage(t, sd)
	seedMCVRow(sd, nil, "pull_request_opened", map[string]any{"head_sha": scS})
	seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{
		"prior_head_sha": scS, "new_head_sha": scM, "merge_commit_sha": scM, "clean_verified": true})
	scReportScenarioCommit(t, sd, impl, scC, scM)

	runRow, err := sd.rr.GetRun(ctx, sd.runID)
	if err != nil {
		t.Fatal(err)
	}
	gate := func() mergeCandidateGateResult { return sd.s.mergeCandidateGate(ctx, runRow) }
	if res := gate(); res.Code != mergeCodeCandidateUnverified || res.Details["cause"] != mergeCandidateCauseBaseAdvance ||
		res.Details["head_sha"] != scC {
		t.Fatalf("gate at C = %+v, want merge_candidate_unverified cause base_advance at %s", res, scC)
	}

	resp := decodeRebase(t, postRebaseBranch(t, sd.s, sd.runID, rebaseBranchRequest{Confirm: true}, withRebaseOperator))
	if resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
		t.Errorf("rebase state/triggered = %q/%v, want in_flight/true", resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
	}
	triggers := mcvTriggerPayloads(t, sd.au)
	if len(triggers) != 1 || triggers[0].ExpectedHeadSHA != scC || triggers[0].Cause != mergeCandidateCauseBaseAdvance {
		t.Fatalf("triggers = %+v, want one naming expected_head_sha %s cause base_advance", triggers, scC)
	}

	seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: scM, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand, Result: mergeCandidateResultPassed})
	if res := gate(); res.Code != mergeCodeCandidateUnverified {
		t.Fatalf("gate after a pass at M = %+v, want still merge_candidate_unverified", res)
	}
	seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: scC, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand, Result: mergeCandidateResultPassed})
	if res := gate(); res.Code != "" || res.HeadSHA != scC {
		t.Fatalf("gate after a pass at C = %+v, want an admit at %s", res, scC)
	}
}

// scqSpecYAML is mqeSpecYAML (a declared verify command and a review approval
// gate listing github:alice) with an acceptance stage added, so the admission
// predicate's acceptance leg reads the recorded outcome.
const scqSpecYAML = mqeSpecYAML + `      - id: accept
        type: acceptance
        executor:
          agent: claude-code
`

// scqRun seeds one queue-recovery run on f's real Postgres stores: an
// acceptance stage row (succeeded) beside the seeded plan, implement and
// review, the chain rows seed writes, and C recorded through the REAL
// recordAcceptanceScenariosPushed route with the forge configured as a real
// push leaves it (ADR-141): PR head C and C's first parent base.
func scqRun(t *testing.T, f *mcqFixture, name, base string, seed func(r *mcqRun)) (*mcqRun, *run.Stage) {
	t.Helper()
	ctx := context.Background()
	r := f.seed(name, base, []byte(scqSpecYAML))
	acc, err := f.repo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.id, Sequence: 4, Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}
	f.setStage(acc.ID, run.StageStateSucceeded)
	seed(r)
	f.gh.set(func() {
		f.gh.prs[r.pr].head = scC
		f.gh.commitParents[scC] = []string{base}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+r.id.String()+"/stages/"+acc.ID.String()+"/pull-request", nil)
	subj := "runner"
	f.srv.recordAcceptanceScenariosPushed(rec, req, r.id, acc, &pullRequestBody{
		Outcome: outcomeAcceptanceScenariosPushed, Branch: r.branch, HeadSHA: scC, BaseSHA: base,
	}, "run_token", audit.ActorSystem, &subj)
	if rec.Code != http.StatusOK {
		t.Fatalf("scenario push report = %d %s, want 200 (forge head %s, first parent %s)", rec.Code, rec.Body.String(), scC, base)
	}
	if rows := f.rows(r, CategoryAcceptanceScenariosPushed); len(rows) != 1 {
		t.Fatalf("acceptance_scenarios_pushed rows = %d, want 1 written by the real route", len(rows))
	}
	return r, acc
}

// scqStandalone is the incident's standalone shape: pull_request_opened S, a
// clean branch_rebased S -> M, a passed pass at M and acceptance recorded at
// M with verdict.
func scqStandalone(f *mcqFixture, verdict string) func(r *mcqRun) {
	return func(r *mcqRun) {
		f.mqeAppend(r, "pull_request_opened", map[string]any{"head_sha": scS})
		f.mqeAppend(r, CategoryBranchRebased, map[string]any{
			"prior_head_sha": scS, "new_head_sha": scM, "merge_commit_sha": scM, "clean_verified": true})
		f.mqeAppend(r, CategoryMergeCandidateVerified, map[string]any{
			"head_sha": scM, "cause": mergeCandidateCauseBaseAdvance, "verify_command": mcvCommand, "result": mergeCandidateResultPassed})
		f.mqeAppend(r, CategoryAcceptanceOutcomeRecorded, map[string]any{"head_sha": scM, "verdict": verdict})
	}
}

func scqState(t *testing.T, f *mcqFixture, r *mcqRun) mergeCandidateState {
	t.Helper()
	st, err := f.srv.mergeCandidateVerifyState(context.Background(), f.runRow(r), scC)
	if err != nil {
		t.Fatalf("verify state at C: %v", err)
	}
	return st
}

// scqPassAt records a merge_candidate_verified verdict at head.
func scqPassAt(f *mcqFixture, r *mcqRun, head, cause, result string) {
	f.mqeAppend(r, CategoryMergeCandidateVerified, map[string]any{
		"head_sha": head, "cause": cause, "verify_command": mcvCommand, "result": result})
}

// TestMergeCandidateScenarioCommit_QueueRecoveryNeedsCurrentApprovalAtC
// decides queue ADMISSION for a scenario commit C (ADR-142, ADR-093 slice 1)
// on newMCQFixture's real Postgres queue store, the real rebase verb's
// up-to-date arm and the real scenario report route. It adds no permission,
// bypass, queue or eligibility change: an unverified C needs a pass, and the
// pass is queued only when the run is merge-queue eligible at C. A hold is
// reported as a hold, never as a started pass. Merge-on-pass's need for an
// approval covering exactly C (forge_approval.go) stays pinned by the existing
// queue tests and is not re-proved here.
func TestMergeCandidateScenarioCommit_QueueRecoveryNeedsCurrentApprovalAtC(t *testing.T) {
	const dismissed = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"alice"},"state":"DISMISSED"}]`
	requireNotQueued := func(t *testing.T, f *mcqFixture, r *mcqRun, resp rebaseBranchResponse, reasons ...string) {
		t.Helper()
		if resp.MergeCandidateVerifyState != mergeCandidateStateNotQueued || resp.MergeCandidateVerifyTriggered {
			t.Fatalf("rebase at C = state %q triggered=%v note %q, want not_queued, not triggered",
				resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyNote)
		}
		for _, want := range reasons {
			if !strings.Contains(resp.MergeCandidateVerifyNote, want) {
				t.Errorf("not_queued note %q lacks %q", resp.MergeCandidateVerifyNote, want)
			}
		}
		if trig := f.triggers(r); len(trig) != 0 {
			t.Errorf("triggers = %+v, want none at C", trig)
		}
		if es := f.entries(r); len(es) != 0 {
			t.Errorf("queue entries = %+v, want none", es)
		}
		if st := f.stageState(r.impl.ID); st != run.StageStateSucceeded {
			t.Errorf("implement = %s, want succeeded (not_queued re-opens nothing)", st)
		}
	}

	t.Run("(1) no live approval at C, the dismissed shape: not_queued not_approved", func(t *testing.T) {
		f := newMCQFixture(t)
		r, _ := scqRun(t, f, "q1", scM, scqStandalone(f, acceptanceVerdictPassed))
		f.gh.set(func() { f.gh.reviews[r.pr] = dismissed })
		if st := scqState(t, f, r); st.State != mergeCandidateStateUnverified || st.Cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("state at C = %+v, want unverified base_advance (the pass at M covers nothing at C)", st)
		}
		requireNotQueued(t, f, r, f.rebase(r), mergeQueueReasonNotApproved, "fishhawk_merge_run")
	})

	t.Run("(2) acceptance parked at triage as well: not_queued not_approved and acceptance_not_eligible", func(t *testing.T) {
		f := newMCQFixture(t)
		r, _ := scqRun(t, f, "q2", scM, scqStandalone(f, acceptanceVerdictFailed))
		f.gh.set(func() { f.gh.reviews[r.pr] = dismissed })
		requireNotQueued(t, f, r, f.rebase(r),
			mergeQueueReasonNotApproved, mergeQueueReasonAcceptanceNotEligible, acceptanceGateTriage)
	})

	t.Run("(3) live approval at C, acceptance passed at M: in_flight, a trigger naming C, entry live at C", func(t *testing.T) {
		f := newMCQFixture(t)
		r, _ := scqRun(t, f, "q3", scM, scqStandalone(f, acceptanceVerdictPassed))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
		if st := scqState(t, f, r); st.State != mergeCandidateStateUnverified || st.Cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("state at C = %+v, want unverified base_advance", st)
		}
		resp := f.rebase(r)
		if resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
			t.Fatalf("rebase at C = state %q triggered=%v note %q, want in_flight triggered",
				resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyNote)
		}
		trig := f.triggers(r)
		if len(trig) != 1 || trig[0].ExpectedHeadSHA != scC || trig[0].Cause != mergeCandidateCauseBaseAdvance {
			t.Fatalf("triggers = %+v, want one naming expected_head_sha %s cause base_advance", trig, scC)
		}
		if e := f.requireState(r, mergequeue.StateLive, ""); e.AnchoredHeadSHA != scC {
			t.Fatalf("entry anchored at %q, want %s", e.AnchoredHeadSHA, scC)
		}
		if st := scqState(t, f, r); st.State != mergeCandidateStateInFlight {
			t.Fatalf("state at C after the trigger = %+v, want in_flight (the pass at M does not make it passed)", st)
		}
		scqPassAt(f, r, scC, mergeCandidateCauseBaseAdvance, mergeCandidateResultPassed)
		if st := scqState(t, f, r); st.State != mergeCandidateStatePassed {
			t.Fatalf("state at C after a passed verdict at C = %+v, want passed", st)
		}
	})

	t.Run("(4) eligible behind another run's live pass: held, nothing started", func(t *testing.T) {
		f := newMCQFixture(t)
		other := f.seed("live", "l0", []byte(mqeSpecYAML))
		f.gh.set(func() { f.gh.reviews[other.pr] = mqeApprovedBy("alice") })
		f.behind(other, "l0", "l1")
		if resp := f.rebase(other); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
			t.Fatalf("fixture: other rebase = %q, want in_flight", resp.MergeCandidateVerifyState)
		}
		f.requireState(other, mergequeue.StateLive, "")

		r, _ := scqRun(t, f, "q4", scM, scqStandalone(f, acceptanceVerdictPassed))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
		resp := f.rebase(r)
		if resp.MergeCandidateVerifyState != mergeCandidateStateHeld || resp.MergeCandidateVerifyTriggered {
			t.Fatalf("rebase at C = state %q triggered=%v, want held, not triggered",
				resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered)
		}
		if trig := f.triggers(r); len(trig) != 0 {
			t.Fatalf("triggers = %+v, want none at C (a hold is not a started pass)", trig)
		}
		if e := f.requireState(r, mergequeue.StateHeld, ""); e.AnchoredHeadSHA != scC {
			t.Fatalf("held entry anchored at %q, want %s", e.AnchoredHeadSHA, scC)
		}
		if st := f.stageState(r.impl.ID); st != run.StageStateSucceeded {
			t.Fatalf("implement = %s, want succeeded (not re-opened while held)", st)
		}
	})

	t.Run("(5) parent, fan-in only, no rebase: unverified fan_in, gate refuses, direct route starts the pass", func(t *testing.T) {
		ctx := context.Background()
		f := newMCQFixture(t)
		r, _ := scqRun(t, f, "q5", scF, func(r *mcqRun) {
			f.mqeAppend(r, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{"merge_sha": scF})
			scqPassAt(f, r, scF, mergeCandidateCauseFanIn, mergeCandidateResultPassed)
			f.mqeAppend(r, CategoryAcceptanceOutcomeRecorded, map[string]any{"head_sha": scF, "verdict": acceptanceVerdictPassed})
		})
		if n := len(f.rows(r, CategoryBranchRebased)); n != 0 {
			t.Fatalf("fixture: branch_rebased rows = %d before the verb, want 0 (no rebase producer)", n)
		}
		if st := scqState(t, f, r); st.State != mergeCandidateStateUnverified || st.Cause != mergeCandidateCauseFanIn {
			t.Fatalf("state at C = %+v, want unverified fan_in (the pass at F changes nothing)", st)
		}
		if res := f.srv.mergeCandidateGate(ctx, f.runRow(r)); res.Code != mergeCodeCandidateUnverified ||
			res.Details["cause"] != mergeCandidateCauseFanIn || res.Details["head_sha"] != scC {
			t.Fatalf("gate at C = %+v, want merge_candidate_unverified cause fan_in at %s", res, scC)
		}

		resp := f.rebase(r)
		if resp.MergeCandidateVerifyState != mergeCandidateStateInFlight || !resp.MergeCandidateVerifyTriggered {
			t.Fatalf("rebase at C = state %q triggered=%v note %q, want in_flight triggered (fan_in is not serialized, ADR-092 D1)",
				resp.MergeCandidateVerifyState, resp.MergeCandidateVerifyTriggered, resp.MergeCandidateVerifyNote)
		}
		rebased := f.rows(r, CategoryBranchRebased)
		if len(rebased) != 1 {
			t.Fatalf("branch_rebased rows = %d, want only the verb's no-op row", len(rebased))
		}
		var noop map[string]any
		_ = json.Unmarshal(rebased[0].Payload, &noop)
		if noop["already_up_to_date"] != true {
			t.Fatalf("branch_rebased payload = %v, want the up-to-date no-op row", noop)
		}
		trig := f.triggers(r)
		if len(trig) != 1 || trig[0].ExpectedHeadSHA != scC || trig[0].Cause != mergeCandidateCauseFanIn {
			t.Fatalf("triggers = %+v, want one naming expected_head_sha %s cause fan_in", trig, scC)
		}
		if es := f.entries(r); len(es) != 0 {
			t.Fatalf("queue entries = %+v, want none (the direct route)", es)
		}
		scqPassAt(f, r, scC, mergeCandidateCauseFanIn, mergeCandidateResultPassed)
		if st := scqState(t, f, r); st.State != mergeCandidateStatePassed {
			t.Fatalf("state at C after a passed verdict at C = %+v, want passed", st)
		}
		if res := f.srv.mergeCandidateGate(ctx, f.runRow(r)); res.Code != "" || res.HeadSHA != scC {
			t.Fatalf("gate after a pass at C = %+v, want an admit at %s", res, scC)
		}
	})

	t.Run("(6) fishhawk_merge_run at C on an eligible run: 202 with merge intent, no merge dispatched", func(t *testing.T) {
		f := newMCQFixture(t)
		merger := &fakeMerger{}
		f.srv.cfg.GateMerger = merger
		r, _ := scqRun(t, f, "q6", scM, scqStandalone(f, acceptanceVerdictPassed))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
		w := f.mergeRun(r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("merge at C = %d, want 202:\n%s", w.Code, w.Body.String())
		}
		var resp mergeRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.Queued || resp.MergeQueued || resp.AnchoredHeadSHA != scC || resp.IntentSequence == 0 {
			t.Fatalf("response = %+v, want queued at %s with an intent sequence", resp, scC)
		}
		f.srv.bgMergeQueuePump.Wait()
		if got := f.intentAt(r); len(got) != 1 || !got[0] {
			t.Fatalf("entries merge_intent_at set = %v, want one entry with intent", got)
		}
		if merger.called != 0 {
			t.Fatalf("merger called %d times, want 0 (no merge dispatched before a pass at C)", merger.called)
		}
		if n := len(f.rows(r, CategoryMergeVerdictRecorded)); n != 0 {
			t.Fatalf("merge_verdict_recorded rows = %d, want 0 on the queued path", n)
		}
	})
}
