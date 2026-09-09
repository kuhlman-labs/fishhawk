package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
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
