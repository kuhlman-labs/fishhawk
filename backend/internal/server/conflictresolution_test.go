package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// sequencedAuditFake is storingAuditFake with the ONE production property the
// consumption rule depends on: a MONOTONIC global Sequence across categories.
// The shared storing fake leaves Sequence at its zero value, which would make
// every trigger and every failure compare equal and mask both directions of the
// `failure.Sequence >= trigger.Sequence` comparison — the control this file
// exists to pin. Sequence is derived from the entry's index in the fake's own
// global append slice, which is exactly what the audit chain guarantees.
type sequencedAuditFake struct {
	*storingAuditFake
}

func newSequencedAuditFake() *sequencedAuditFake {
	return &sequencedAuditFake{storingAuditFake: newStoringAuditFake()}
}

func (a *sequencedAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for i, p := range a.appended {
		if p.RunID == runID && p.Category == category {
			rid := p.RunID
			out = append(out, &audit.Entry{
				Sequence: int64(i + 1),
				RunID:    &rid,
				StageID:  p.StageID,
				Category: p.Category,
				Payload:  p.Payload,
			})
		}
	}
	return out, nil
}

// seedConflictResolutionTriggered appends the durable trigger entry the
// resolver keys off.
func seedConflictResolutionTriggered(t *testing.T, au *sequencedAuditFake, runID, stageID uuid.UUID,
	branch, baseRef, head string, priorState run.StageState, reviewID *uuid.UUID) {
	t.Helper()
	fields := map[string]any{
		"branch":            branch,
		"base_ref":          baseRef,
		"expected_head_sha": head,
		"pass":              1,
		"prior_state":       string(priorState),
	}
	if reviewID != nil {
		fields["reparked_review_stage_id"] = reviewID.String()
	}
	payload, _ := json.Marshal(fields)
	kind := audit.ActorKind("user")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID,
		Category: CategoryStageConflictResolutionTriggered, ActorKind: &kind, Payload: payload,
	}); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}
}

func seedConflictResolutionFailed(t *testing.T, au *sequencedAuditFake, runID, stageID uuid.UUID, reason string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"stage_id": stageID.String(), "reason": reason})
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID,
		Category: CategoryStageConflictResolutionFailed, ActorKind: &kind, Payload: payload,
	}); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
}

// TestResolveConflictResolutionTrigger_ServesLiveTrigger is the success control.
func TestResolveConflictResolutionTrigger_ServesLiveTrigger(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	runRow, impl, review := seedFailedFixupRun(rr)
	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "head1", run.StageStateSucceeded, &review.ID)

	got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID)
	if got == nil {
		t.Fatal("live trigger was not served")
	}
	if got.Branch != "feature" || got.BaseRef != "main" || got.ExpectedHeadSHA != "head1" {
		t.Errorf("trigger = %+v, want the seeded anchors", got)
	}
}

// TestResolveConflictResolutionTrigger_FailedPassConsumesTrigger is the ROUND-2
// consumption requirement. Round 1 omitted the failed-entry comparison, so after
// a refused pass an ordinary later fix-up on the same stage was still served
// conflict_resolution=true with STALE anchors and got hijacked into a
// conflict-resolution pass against a repository that was not mid-merge.
//
// The counterfactual vehicle: deleting the `failure.Sequence >= trigger.Sequence`
// comparison makes this test go RED.
func TestResolveConflictResolutionTrigger_FailedPassConsumesTrigger(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	runRow, impl, review := seedFailedFixupRun(rr)

	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "head1", run.StageStateSucceeded, &review.ID)
	seedConflictResolutionFailed(t, au, runRow.ID, impl.ID, "conflict_resolution_residual_marker")
	// An ordinary fix-up follows on the SAME stage.
	seedFixupTriggered(t, au.storingAuditFake, runRow.ID, impl.ID, run.StageStateSucceeded, &review.ID)

	if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
		t.Fatalf("consumed trigger was served: %+v — the later fix-up would be hijacked into a pass", got)
	}
}

// TestResolveConflictResolutionTrigger_NewTriggerAfterFailureIsLive pins the
// other side of the comparison: a trigger NEWER than the failure is live, so
// consumption cannot be implemented as "any failure kills every trigger".
func TestResolveConflictResolutionTrigger_NewTriggerAfterFailureIsLive(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	runRow, impl, review := seedFailedFixupRun(rr)

	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "old", run.StageStateSucceeded, &review.ID)
	seedConflictResolutionFailed(t, au, runRow.ID, impl.ID, "conflict_resolution_residual_marker")
	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "new", run.StageStateSucceeded, &review.ID)

	got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID)
	if got == nil || got.ExpectedHeadSHA != "new" {
		t.Fatalf("trigger = %+v, want the NEWER live trigger", got)
	}
}

// TestResolveConflictResolutionTrigger_FailsClosed covers every uncertainty
// branch: no trigger, a malformed payload, and each half-populated shape.
func TestResolveConflictResolutionTrigger_FailsClosed(t *testing.T) {
	t.Run("no_trigger", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, _ := seedFailedFixupRun(rr)
		if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("malformed_payload", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, _ := seedFailedFixupRun(rr)
		kind := audit.ActorKind("user")
		if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: runRow.ID, StageID: &impl.ID,
			Category: CategoryStageConflictResolutionTriggered, ActorKind: &kind,
			Payload: json.RawMessage(`{not json`),
		}); err != nil {
			t.Fatal(err)
		}
		if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	half := []struct{ name, branch, baseRef, head string }{
		{"no_branch", "", "main", "h"},
		{"no_base_ref", "feature", "", "h"},
		{"no_expected_head", "feature", "main", ""},
	}
	for _, tc := range half {
		t.Run(tc.name, func(t *testing.T) {
			rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
			au := newSequencedAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
			runRow, impl, _ := seedFailedFixupRun(rr)
			seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, tc.branch, tc.baseRef, tc.head, run.StageStateSucceeded, nil)
			if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
				t.Fatalf("half-populated trigger served: %+v", got)
			}
		})
	}
}

// TestMaybeRecoverConflictResolutionFailure_RestoresPrePassGate: a refused pass
// is an ASSIST that failed, never an escalation — the run must be left at its
// pre-pass review gate with the intact PR un-orphaned, and the trigger must be
// CONSUMED by a stage_conflict_resolution_failed entry carrying the named
// reason.
func TestMaybeRecoverConflictResolutionFailure_RestoresPrePassGate(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	ctx := context.Background()
	runRow, impl, review := seedFailedFixupRun(rr)
	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "head1", run.StageStateSucceeded, &review.ID)

	if !s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "conflict_resolution_residual_marker") {
		t.Fatal("maybeRecoverConflictResolutionFailure = false, want true")
	}
	curImpl, _ := rr.GetStage(ctx, impl.ID)
	if curImpl.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (pre-pass gate restored)", curImpl.State)
	}
	curReview, _ := rr.GetStage(ctx, review.ID)
	if curReview.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval", curReview.State)
	}

	var failedReason string
	au.mu.Lock()
	for _, e := range au.appended {
		if e.Category == CategoryStageConflictResolutionFailed {
			var p struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			failedReason = p.Reason
		}
	}
	au.mu.Unlock()
	if failedReason != "conflict_resolution_residual_marker" {
		t.Errorf("recorded reason = %q, want the runner's NAMED refusal reason", failedReason)
	}

	// The failure CONSUMED the trigger: a later dispatch is served no pass.
	if got := s.resolveConflictResolutionTrigger(ctx, runRow.ID, impl.ID); got != nil {
		t.Fatalf("trigger still live after the failure: %+v", got)
	}
}

// TestMaybeRecoverConflictResolutionFailure_Misses covers every branch that
// must leave the normal failure path in force.
func TestMaybeRecoverConflictResolutionFailure_Misses(t *testing.T) {
	ctx := context.Background()

	t.Run("no_trigger", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, _ := seedFailedFixupRun(rr)
		if s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "r") {
			t.Fatal("recovered with no trigger")
		}
	})

	t.Run("already_consumed_trigger", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, review := seedFailedFixupRun(rr)
		seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "f", "m", "h", run.StageStateSucceeded, &review.ID)
		seedConflictResolutionFailed(t, au, runRow.ID, impl.ID, "already")
		if s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "r") {
			t.Fatal("recovered twice off one trigger")
		}
	})

	t.Run("unparseable_review_stage_id", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, _ := seedFailedFixupRun(rr)
		payload, _ := json.Marshal(map[string]any{
			"branch": "f", "base_ref": "m", "expected_head_sha": "h",
			"prior_state": string(run.StageStateSucceeded), "reparked_review_stage_id": "not-a-uuid",
		})
		kind := audit.ActorKind("user")
		if _, err := au.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runRow.ID, StageID: &impl.ID,
			Category: CategoryStageConflictResolutionTriggered, ActorKind: &kind, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
		if s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "r") {
			t.Fatal("recovered off an unparseable review stage id")
		}
	})

	t.Run("stage_not_failed", func(t *testing.T) {
		rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
		au := newSequencedAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		runRow, impl, review := seedFailedFixupRun(rr)
		rr.mu.Lock()
		impl.State = run.StageStateRunning
		rr.mu.Unlock()
		seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "f", "m", "h", run.StageStateSucceeded, &review.ID)
		if s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "r") {
			t.Fatal("recovered a stage that was not failed")
		}
	})
}

// seedConflictResolutionPushed appends the terminal SUCCESS marker
// succeedConflictResolutionPushStage writes, using the SAME exported category
// constant that handler uses — a literal here would let the writer and the
// consumption set drift apart silently.
func seedConflictResolutionPushed(t *testing.T, au *sequencedAuditFake, runID, stageID uuid.UUID, head string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"run_id": runID.String(), "stage_id": stageID.String(),
		"branch": "feature", "head_sha": head, "base_sha": "pre", "files_changed_count": 2,
	})
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &stageID,
		Category: CategoryConflictResolutionPushed, ActorKind: &kind, Payload: payload,
	}); err != nil {
		t.Fatalf("seed pushed: %v", err)
	}
}

// TestResolveConflictResolutionTrigger_SuccessfulPassConsumesTrigger is the
// ROUND-3 requirement. Round 2 keyed consumption on the FAILURE entry alone, so
// a pass that SUCCEEDED left its trigger live: the next ordinary fix-up on the
// same implement stage was served conflict_resolution=true with anchors naming
// a merge that had already been committed and pushed. Success settles the pass
// exactly as refusal does.
//
// The counterfactual vehicle: dropping CategoryConflictResolutionPushed from
// the resolver's consumption loop makes this test go RED.
func TestResolveConflictResolutionTrigger_SuccessfulPassConsumesTrigger(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	runRow, impl, review := seedFailedFixupRun(rr)

	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "head1", run.StageStateSucceeded, &review.ID)
	seedConflictResolutionPushed(t, au, runRow.ID, impl.ID, "merge-sha")
	// An ordinary fix-up follows on the SAME stage.
	seedFixupTriggered(t, au.storingAuditFake, runRow.ID, impl.ID, run.StageStateSucceeded, &review.ID)

	if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
		t.Fatalf("trigger still live after a SUCCESSFUL pass: %+v — the later fix-up would be hijacked", got)
	}
}

// TestResolveConflictResolutionTrigger_NewTriggerAfterPushIsLive pins the other
// side of the success comparison, so consumption cannot be implemented as "any
// conflict_resolution_pushed entry kills every trigger".
func TestResolveConflictResolutionTrigger_NewTriggerAfterPushIsLive(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newSequencedAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	runRow, impl, review := seedFailedFixupRun(rr)

	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "old", run.StageStateSucceeded, &review.ID)
	seedConflictResolutionPushed(t, au, runRow.ID, impl.ID, "merge-sha")
	seedConflictResolutionTriggered(t, au, runRow.ID, impl.ID, "feature", "main", "new", run.StageStateSucceeded, &review.ID)

	got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID)
	if got == nil || got.ExpectedHeadSHA != "new" {
		t.Fatalf("trigger = %+v, want the NEWER live trigger", got)
	}
}

// appendFailingAuditFake injects an AppendChained failure for ONE category,
// leaving every read and every other append intact — the fault the recovery's
// consumption guarantee has to survive.
type appendFailingAuditFake struct {
	*sequencedAuditFake
	failCategory string
}

func (a *appendFailingAuditFake) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.failCategory {
		return nil, errors.New("audit chain append failed")
	}
	return a.sequencedAuditFake.AppendChained(ctx, p)
}

// TestMaybeRecoverConflictResolutionFailure_MarkerPersistenceFailureRefusesRecovery
// is the ROUND-3 fault-injection requirement. Consumption lives ONLY in the
// audit chain, so a recovery that restores the pre-pass gate while the
// stage_conflict_resolution_failed append fails would acknowledge the recovery
// AND leave the stale trigger dispatchable — the next ordinary fix-up on this
// stage served conflict_resolution=true with anchors naming an aborted merge.
//
// The assertions read COMMITTED STATE back after the call, not the returned
// error: the control's effect is the restore that must NOT have happened, and
// the function returns the same bare false on every miss branch.
//
// The counterfactual vehicle: reverting writeConflictResolutionFailedAudit to
// discard its append error (or moving the append back after the restore) makes
// this test go RED on the restored implement/review states.
func TestMaybeRecoverConflictResolutionFailure_MarkerPersistenceFailureRefusesRecovery(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := &appendFailingAuditFake{
		sequencedAuditFake: newSequencedAuditFake(),
		failCategory:       CategoryStageConflictResolutionFailed,
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	ctx := context.Background()
	runRow, impl, review := seedFailedFixupRun(rr)
	seedConflictResolutionTriggered(t, au.sequencedAuditFake, runRow.ID, impl.ID,
		"feature", "main", "head1", run.StageStateSucceeded, &review.ID)

	if s.maybeRecoverConflictResolutionFailure(ctx, runRow.ID, impl.ID, "conflict_resolution_residual_marker") {
		t.Fatal("recovery ACKNOWLEDGED while its consumption marker was not persisted")
	}

	// COMMITTED STATE: nothing was restored, so no ordinary fix-up can be
	// dispatched off the stale instruction.
	curImpl, _ := rr.GetStage(ctx, impl.ID)
	if curImpl.State != run.StageStateFailed {
		t.Errorf("implement state = %q, want failed — the gate must NOT be restored when consumption cannot be recorded", curImpl.State)
	}
	curReview, _ := rr.GetStage(ctx, review.ID)
	if curReview.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (un-restored)", curReview.State)
	}

	// And the marker genuinely did not land, which is what made the trigger
	// still resolvable — the exact pairing the refusal exists to prevent.
	if entry, ok := s.newestStageEntry(ctx, runRow.ID, impl.ID, CategoryStageConflictResolutionFailed); !ok || entry != nil {
		t.Errorf("fault injection did not land: failure entry = %+v (ok=%v)", entry, ok)
	}
}

// listFailingAuditFake injects a ListForRunByCategory failure for ONE category,
// leaving every other read intact.
type listFailingAuditFake struct {
	*sequencedAuditFake
	failCategory string
}

func (a *listFailingAuditFake) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.failCategory {
		return nil, errors.New("audit chain read failed")
	}
	return a.sequencedAuditFake.ListForRunByCategory(ctx, runID, category)
}

// TestResolveConflictResolutionTrigger_UnreadableConsumptionChainFailsClosed:
// an unreadable consumption category must never read as "no consumption". Both
// settlement categories are checked, because a read failure on EITHER leaves the
// resolver unable to tell a live trigger from a spent one — and serving a spent
// one is the hijack this consumption rule exists to prevent.
func TestResolveConflictResolutionTrigger_UnreadableConsumptionChainFailsClosed(t *testing.T) {
	for _, category := range []string{CategoryStageConflictResolutionFailed, CategoryConflictResolutionPushed} {
		t.Run(category, func(t *testing.T) {
			rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
			au := &listFailingAuditFake{sequencedAuditFake: newSequencedAuditFake(), failCategory: category}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
			runRow, impl, review := seedFailedFixupRun(rr)
			seedConflictResolutionTriggered(t, au.sequencedAuditFake, runRow.ID, impl.ID,
				"feature", "main", "head1", run.StageStateSucceeded, &review.ID)

			if got := s.resolveConflictResolutionTrigger(context.Background(), runRow.ID, impl.ID); got != nil {
				t.Fatalf("served a pass with the %s chain unreadable: %+v", category, got)
			}
		})
	}
}

// TestMaybeRecoverConflictResolutionFailure_NoRepositories pins the unconfigured
// guard: with no run or audit repository there is nothing to restore and nothing
// to record consumption in, so the normal failure path must stay in force.
func TestMaybeRecoverConflictResolutionFailure_NoRepositories(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.maybeRecoverConflictResolutionFailure(context.Background(), uuid.New(), uuid.New(), "r") {
		t.Fatal("recovered with no repositories configured")
	}
}

// --- #3673: a SUCCESSFUL pass's merge commit is not foreign to the product ---

const (
	// crAgentHeadSHA is the head the implement agent's PR-open artifact
	// recorded — the sha the run pushed BEFORE the conflict-resolution pass.
	crAgentHeadSHA = "0a9e170000000000000000000000000000000000"
	// crMergeSHA is the merge commit the SUCCESSFUL pass authored and pushed.
	// It appears in NO other head-report category, carries no
	// operator_commit_vouched entry, and the fixture has no decomposition
	// children — so it is known to rule 5 ONLY through the
	// conflict_resolution_pushed union, which is what makes the counterfactual
	// observable rather than fixture-masked.
	crMergeSHA = "c0ff1c700000000000000000000000000000ab12"
)

// crSeed is the world the #3673 end-to-end test drives.
type crSeed struct {
	s       *Server
	stub    *rebaseGitHub
	au      *auditFake
	rr      *promptRunRepo
	creator *vouchCheckCreator
	sf      *signingFake
	runID   uuid.UUID
	impl    *run.Stage
}

// seedConflictResolutionE2E wires the ONE world both halves of the seam run in:
// the runner's signed pull-request report handler (the WRITER of
// conflict_resolution_pushed) and fishhawk_rebase_run_branch's already-up-to-date
// arm (whose republish drives auditcomplete.ComputeResult → the READER). It is
// modelled on rebase_branch_test.go's seedRebaseRun, adapted rather than reused
// because this test additionally needs the signing fake (to sign the runner
// report) and the implement stage's PR-open artifact at crAgentHeadSHA (which is
// what ACTIVATES rule 5 against the forge's live head).
//
// The forge stub serves crMergeSHA as the live PR head with an EMPTY
// behind-probe, which is the already-up-to-date arm: no merge is attempted and
// the shared tail republishes at the live head.
func seedConflictResolutionE2E(t *testing.T) *crSeed {
	t.Helper()
	stub := &rebaseGitHub{
		baseRef:       rebaseBaseRef,
		headRef:       rebaseBranchName,
		headSHA:       crMergeSHA,
		behindCommits: nil,
	}
	gh := newRebaseGitHubClient(t, stub)

	runID := uuid.New()
	prURL := rebasePRURL // .../pull/77
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	// The pass's report is the runner's terminal report for a RUNNING implement
	// stage, so the stage starts running and the handler settles it.
	impl := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	review := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview,
		State: run.StageStateAwaitingApproval, Sequence: 2}

	sf := newSigningFake()
	au := newAuditFake()
	ar := newFakeArtifactRepo()
	rr := newPromptRunRepo()
	rr.getRuns[runID] = runRow
	rr.getStages[impl.ID] = impl
	rr.getStages[review.ID] = review
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {impl, review}}

	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
		GitHub:       gh,
	})

	// The PR-open artifact records the AGENT head, not the merge commit: the
	// stale-artifact shape every #1682/#3415/#3429 instance of this seam had.
	body, _ := json.Marshal(map[string]any{"head_sha": crAgentHeadSHA, "pr_number": 77})
	if _, err := ar.Create(context.Background(), artifact.CreateParams{
		StageID: impl.ID, Kind: artifact.KindPullRequest, Content: body,
	}); err != nil {
		t.Fatalf("seed pull_request artifact: %v", err)
	}

	creator := &vouchCheckCreator{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        rr,
		Artifacts:   ar,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	s.auditCheckPublisher = pub

	return &crSeed{s: s, stub: stub, au: au, rr: rr, creator: creator, sf: sf, runID: runID, impl: impl}
}

// TestConflictResolutionPushed_RepublishedCheckDoesNotFlagMergeForeign is the
// #3673 Done-means, cross-boundary test: the entry is WRITTEN in
// backend/internal/server and READ in backend/internal/auditcomplete, and both
// #3415 and #3429 are instances of exactly that seam breaking while each side's
// unit tests stayed green.
//
// It spans the runner's signed pull-request report handler (outcome
// conflict_resolution_pushed at crMergeSHA) → the audit chain →
// fishhawk_rebase_run_branch's already-up-to-date arm →
// recomputeAndPublishAuditCompleteAtHead → auditcomplete.ComputeResult (rule 5)
// → auditcheckpublisher → the CreateCheckRun fake, and asserts the re-posted
// Check Run is at the merge commit and carries NO foreign_commit item — so the
// product's own sanctioned recovery path no longer needs a manual
// fishhawk_vouch_commit.
//
// Counterfactual: replacing addConflictResolutionHeads' body with `return nil`
// turns this RED on the foreign_commit assertion.
func TestConflictResolutionPushed_RepublishedCheckDoesNotFlagMergeForeign(t *testing.T) {
	sd := seedConflictResolutionE2E(t)

	// --- pass 1: the runner reports the pushed merge commit ---
	priv, _ := sd.sf.issue(t, sd.runID)
	report := []byte(`{"outcome":"conflict_resolution_pushed","branch":"` + rebaseBranchName +
		`","head_sha":"` + crMergeSHA + `","base_sha":"` + crAgentHeadSHA + `","files_changed_count":2}`)
	if w := shipPRRequest(t, sd.s, sd.runID, sd.impl.ID, priv, report, ""); w.Code != http.StatusOK {
		t.Fatalf("pull-request report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := auditEntries(sd.au, CategoryConflictResolutionPushed); len(got) != 1 {
		t.Fatalf("conflict_resolution_pushed entries = %d, want 1 (the writer half of the seam)", len(got))
	}

	// The operator approves the re-parked review gate, terminalizing the
	// implement stage. Rule 5 only evaluates once every non-review stage is
	// TERMINAL (run.StageState.IsTerminal), so without this the recompute would
	// short-circuit at stage_not_terminal and the foreign_commit assertion below
	// would be vacuous — which the explicit stage_not_terminal assertion at the
	// end also pins.
	sd.impl.State = run.StageStateSucceeded

	// --- the operator re-invokes the rebase verb; the branch already contains
	// the base, so the already-up-to-date arm republishes at the live head ---
	w := postRebaseBranch(t, sd.s, sd.runID,
		rebaseBranchRequest{Reason: "recheck after the conflict-resolution pass", Confirm: true},
		withRebaseOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("rebase status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp rebaseBranchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyUpToDate {
		t.Errorf("already_up_to_date = false on a zero-commit behind-probe")
	}
	if !resp.AuditCheckRepublished {
		t.Fatalf("audit_check_republished = false, want true; warning=%q", resp.AuditCheckRepublishWarning)
	}

	sd.creator.mu.Lock()
	calls := append([]forge.CreateCheckRunParams(nil), sd.creator.calls...)
	sd.creator.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("check run creations = %d, want exactly 1", len(calls))
	}
	if got := calls[0].HeadSHA; got != crMergeSHA {
		t.Errorf("check run head_sha = %q, want the pushed merge commit %q (not the stale artifact head %q)",
			got, crMergeSHA, crAgentHeadSHA)
	}
	for _, field := range []struct{ name, text string }{
		{"OutputSummary", calls[0].OutputSummary},
		{"OutputText", calls[0].OutputText},
	} {
		if strings.Contains(field.text, string(auditcomplete.MissingForeignCommit)) {
			t.Errorf("re-posted check %s flags the runner's OWN conflict-resolution merge as foreign_commit (#3673):\n%s",
				field.name, field.text)
		}
		// ANTI-VACUITY: rule 5 must actually have run. A mid-flight run
		// short-circuits at stage_not_terminal and reports no foreign_commit for
		// a reason that has nothing to do with the union under test.
		if strings.Contains(field.text, string(auditcomplete.MissingStageNotTerminal)) {
			t.Errorf("recompute short-circuited at stage_not_terminal, so the foreign_commit assertion is vacuous:\n%s",
				field.text)
		}
	}
}

// TestConflictResolutionPushed_RepublishedCheckStillFlagsUnrecordedHead is the
// fail-closed ANTI-VACUITY control for the end-to-end above (not a counterfactual
// vehicle): the exemption is keyed to the RECORDED merge sha, not to "a pass
// succeeded". With the forge serving a live head no category records, the same
// recompute still reports foreign_commit and lists the recorded merge sha among
// the known set. This arm stays GREEN both before and after the change.
func TestConflictResolutionPushed_RepublishedCheckStillFlagsUnrecordedHead(t *testing.T) {
	const unrecorded = "dddd444444444444444444444444444444444444"
	sd := seedConflictResolutionE2E(t)

	priv, _ := sd.sf.issue(t, sd.runID)
	report := []byte(`{"outcome":"conflict_resolution_pushed","branch":"` + rebaseBranchName +
		`","head_sha":"` + crMergeSHA + `","base_sha":"` + crAgentHeadSHA + `","files_changed_count":2}`)
	if w := shipPRRequest(t, sd.s, sd.runID, sd.impl.ID, priv, report, ""); w.Code != http.StatusOK {
		t.Fatalf("pull-request report status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	sd.impl.State = run.StageStateSucceeded

	// The live head is a commit NOTHING recorded — neither the artifact nor the
	// pass's own entry.
	sd.stub.mu.Lock()
	sd.stub.headSHA = unrecorded
	sd.stub.mu.Unlock()

	res, err := auditcomplete.ComputeResult(context.Background(), sd.runID, sd.s.auditCompleteDeps())
	if err != nil {
		t.Fatalf("ComputeResult: %v", err)
	}
	var foreign *auditcomplete.MissingItem
	for i := range res.Missing {
		if res.Missing[i].Kind == auditcomplete.MissingForeignCommit {
			foreign = &res.Missing[i]
		}
	}
	if foreign == nil {
		t.Fatalf("live head %s is recorded by nothing and must still be foreign_commit; missing=%+v",
			unrecorded[:7], res.Missing)
	}
	if !strings.Contains(foreign.Detail, crMergeSHA[:7]) {
		t.Errorf("foreign_commit detail should list the recorded merge sha %s as known: %s",
			crMergeSHA[:7], foreign.Detail)
	}
}
