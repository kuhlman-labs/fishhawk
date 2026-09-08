package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// crSeed wires the minimal world the conflict-resolution trigger + recovery
// need: a run, an implement stage, an open review gate, an audit fake.
type crSeed struct {
	s      *Server
	rr     *promptRunRepo
	au     *auditFake
	runID  uuid.UUID
	impl   *run.Stage
	review *run.Stage
}

func seedConflictResolution(t *testing.T, implState run.StageState) *crSeed {
	t.Helper()
	runID := uuid.New()
	rr := newPromptRunRepo()
	au := newAuditFake()
	prURL := rebasePRURL
	rr.getRuns[runID] = &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	impl := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement,
		State: implState, Sequence: 1}
	review := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeReview,
		State: run.StageStateAwaitingApproval, Sequence: 2}
	rr.getStages[impl.ID] = impl
	rr.getStages[review.ID] = review
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {impl, review}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	return &crSeed{s: s, rr: rr, au: au, runID: runID, impl: impl, review: review}
}

func (sd *crSeed) appendTrigger(t *testing.T, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := sd.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: sd.runID, StageID: &sd.impl.ID,
		Category: CategoryStageConflictResolutionTriggered, Payload: raw,
	}); err != nil {
		t.Fatalf("append trigger: %v", err)
	}
}

// TestTriggerConflictResolutionPass_ReopensAndAudits: the happy path. The
// implement stage re-opens to pending, the review gate re-parks, and the
// durable authorization entry carries the merge the runner must perform plus
// the recovery anchors the failure arm reads back.
func TestTriggerConflictResolutionPass_ReopensAndAudits(t *testing.T) {
	sd := seedConflictResolution(t, run.StageStateSucceeded)

	dec, err := sd.s.triggerConflictResolutionPass(context.Background(), Identity{Subject: "op"},
		conflictResolutionTriggerParams{
			StageID: sd.impl.ID, Branch: "fishhawk/run/x", BaseRef: "main",
			ExpectedHeadSHA: "abc123", Reason: "advance",
		})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement stage state = %q, want pending", dec.Stage.State)
	}
	entries := auditEntries(sd.au, CategoryStageConflictResolutionTriggered)
	if len(entries) != 1 {
		t.Fatalf("trigger entries = %d, want 1", len(entries))
	}
	var got conflictResolutionTriggerAudit
	if err := json.Unmarshal(entries[0].Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.Branch != "fishhawk/run/x" || got.BaseRef != "main" || got.ExpectedHeadSHA != "abc123" {
		t.Errorf("payload merge fields = %+v, want branch/base/head populated", got)
	}
	if got.Pass != 1 {
		t.Errorf("pass = %d, want 1", got.Pass)
	}
	if got.PriorState != string(run.StageStateSucceeded) {
		t.Errorf("prior_state = %q, want succeeded", got.PriorState)
	}
	if got.ReparkedReviewStageID != sd.review.ID.String() {
		t.Errorf("reparked_review_stage_id = %q, want %q", got.ReparkedReviewStageID, sd.review.ID)
	}
	// Fix-up isolation, asserted at the unit level too: no fix-up entry.
	if n := len(auditEntries(sd.au, CategoryStageFixupTriggered)); n != 0 {
		t.Errorf("stage_fixup_triggered entries = %d, want 0", n)
	}
}

// TestTriggerConflictResolutionPass_BudgetSpent: the ceiling-1 check refuses
// BEFORE any stage is touched, with the sentinel the rebase handler's 422 arm
// is bound to. Seeded BY CONSTRUCTION with a prior trigger entry.
func TestTriggerConflictResolutionPass_BudgetSpent(t *testing.T) {
	sd := seedConflictResolution(t, run.StageStateSucceeded)
	sd.appendTrigger(t, conflictResolutionTriggerAudit{
		conflictResolutionTrigger: conflictResolutionTrigger{Branch: "b", BaseRef: "main", Pass: 1},
		RunID:                     sd.runID.String(), StageID: sd.impl.ID.String(),
		PriorState: string(run.StageStateSucceeded),
	})

	_, err := sd.s.triggerConflictResolutionPass(context.Background(), Identity{Subject: "op"},
		conflictResolutionTriggerParams{StageID: sd.impl.ID, Branch: "b", BaseRef: "main"})
	if !errors.Is(err, errConflictResolutionBudgetSpent) {
		t.Fatalf("err = %v, want errConflictResolutionBudgetSpent", err)
	}
	// NOTHING was touched: no second trigger entry, and the stage did not move.
	if n := len(auditEntries(sd.au, CategoryStageConflictResolutionTriggered)); n != 1 {
		t.Errorf("trigger entries = %d, want 1 (the refusal wrote nothing)", n)
	}
	if sd.impl.State != run.StageStateSucceeded {
		t.Errorf("implement stage state = %q, want succeeded (untouched)", sd.impl.State)
	}
}

// TestCountConflictResolutionPasses_CountsOnlyThisStagesTriggers: the counter
// is stage-scoped and category-scoped — a sibling stage's trigger and a
// stage_fixup_triggered entry both count zero.
func TestCountConflictResolutionPasses_CountsOnlyThisStagesTriggers(t *testing.T) {
	sd := seedConflictResolution(t, run.StageStateSucceeded)
	other := uuid.New()
	if _, err := sd.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: sd.runID, StageID: &other,
		Category: CategoryStageConflictResolutionTriggered, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := sd.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: sd.runID, StageID: &sd.impl.ID,
		Category: CategoryStageFixupTriggered, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	n, err := sd.s.countConflictResolutionPasses(context.Background(), sd.runID, sd.impl.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 (a sibling stage's trigger and a fix-up entry must not count)", n)
	}
}

// TestNewestConflictResolutionFailureReason covers the reader the budget-spent
// 422 uses to NAME the failed pass, plus its two degrade branches.
func TestNewestConflictResolutionFailureReason(t *testing.T) {
	sd := seedConflictResolution(t, run.StageStateSucceeded)

	if _, ok := sd.s.newestConflictResolutionFailureReason(context.Background(), sd.runID, sd.impl.ID); ok {
		t.Error("found a reason with no failure entry recorded")
	}

	appendFailure := func(reason string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"source_failure_reason": reason})
		if _, err := sd.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: sd.runID, StageID: &sd.impl.ID,
			Category: CategoryStageConflictResolutionFailed, Payload: raw,
		}); err != nil {
			t.Fatalf("append failure: %v", err)
		}
	}
	appendFailure("head_moved")
	appendFailure("residual_conflict_marker")

	got, ok := sd.s.newestConflictResolutionFailureReason(context.Background(), sd.runID, sd.impl.ID)
	if !ok || got != "residual_conflict_marker" {
		t.Errorf("reason = %q ok=%v, want the NEWEST entry's reason", got, ok)
	}

	// A different stage's failure is not this stage's.
	other := uuid.New()
	if _, ok := sd.s.newestConflictResolutionFailureReason(context.Background(), sd.runID, other); ok {
		t.Error("a sibling stage's failure entry was attributed to another stage")
	}
}

// --- RECOVERY -------------------------------------------------------------

// TestMaybeRecoverConflictResolutionFailure_RestoresPrePassGate: a pass that
// REFUSED must leave the run exactly where it was — implement restored to its
// pre-pass state, review gate restored — and record the named reason.
func TestMaybeRecoverConflictResolutionFailure_RestoresPrePassGate(t *testing.T) {
	sd := seedConflictResolution(t, run.StageStateFailed)
	sd.impl.FailureReason = strPtr("mergehead_changed")
	cat := run.FailureA
	sd.impl.FailureCategory = &cat
	sd.review.State = run.StageStatePending
	sd.appendTrigger(t, conflictResolutionTriggerAudit{
		conflictResolutionTrigger: conflictResolutionTrigger{Branch: "b", BaseRef: "main", Pass: 1},
		RunID:                     sd.runID.String(), StageID: sd.impl.ID.String(),
		PriorState:            string(run.StageStateSucceeded),
		ReparkedReviewStageID: sd.review.ID.String(),
	})

	if !sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.impl.ID) {
		t.Fatal("recovery returned false; the run would be left terminal-failed")
	}
	if sd.impl.State != run.StageStateSucceeded {
		t.Errorf("implement stage state = %q, want succeeded (the pre-pass state)", sd.impl.State)
	}
	if sd.review.State != run.StageStateAwaitingApproval {
		t.Errorf("review stage state = %q, want awaiting_approval (the pre-pass gate)", sd.review.State)
	}
	failures := auditEntries(sd.au, CategoryStageConflictResolutionFailed)
	if len(failures) != 1 {
		t.Fatalf("stage_conflict_resolution_failed entries = %d, want 1", len(failures))
	}
	var payload struct {
		SourceFailureReason string `json:"source_failure_reason"`
		RestoredState       string `json:"restored_state"`
	}
	if err := json.Unmarshal(failures[0].Payload, &payload); err != nil {
		t.Fatalf("decode failure payload: %v", err)
	}
	if payload.SourceFailureReason != "mergehead_changed" {
		t.Errorf("source_failure_reason = %q, want the runner's named confinement-gate reason",
			payload.SourceFailureReason)
	}
	if payload.RestoredState != string(run.StageStateSucceeded) {
		t.Errorf("restored_state = %q, want succeeded", payload.RestoredState)
	}
	// The FIX-UP recovery category must NOT have been written.
	if n := len(auditEntries(sd.au, CategoryStageFixupRecovered)); n != 0 {
		t.Errorf("stage_fixup_recovered entries = %d, want 0", n)
	}
}

// TestMaybeRecoverConflictResolutionFailure_Degrades covers every branch that
// must return false and leave the caller's normal failure path in force. Each
// is a distinct guard, so each gets its own case rather than a subset.
func TestMaybeRecoverConflictResolutionFailure_Degrades(t *testing.T) {
	t.Run("no trigger entry", func(t *testing.T) {
		sd := seedConflictResolution(t, run.StageStateFailed)
		if sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.impl.ID) {
			t.Error("recovered a stage with no conflict-resolution trigger")
		}
	})
	t.Run("not an implement stage", func(t *testing.T) {
		sd := seedConflictResolution(t, run.StageStateFailed)
		if sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.review.ID) {
			t.Error("recovered a non-implement stage")
		}
	})
	t.Run("malformed trigger payload", func(t *testing.T) {
		sd := seedConflictResolution(t, run.StageStateFailed)
		if _, err := sd.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: sd.runID, StageID: &sd.impl.ID,
			Category: CategoryStageConflictResolutionTriggered, Payload: []byte(`{not json`),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.impl.ID) {
			t.Error("recovered from a malformed trigger payload")
		}
	})
	t.Run("unparseable reparked review stage id", func(t *testing.T) {
		sd := seedConflictResolution(t, run.StageStateFailed)
		sd.appendTrigger(t, map[string]any{
			"prior_state": string(run.StageStateSucceeded), "reparked_review_stage_id": "not-a-uuid",
		})
		if sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.impl.ID) {
			t.Error("recovered with an unparseable reparked_review_stage_id")
		}
	})
	t.Run("stage not failed (restore not applicable)", func(t *testing.T) {
		sd := seedConflictResolution(t, run.StageStateRunning)
		sd.appendTrigger(t, map[string]any{"prior_state": string(run.StageStateSucceeded)})
		if sd.s.maybeRecoverConflictResolutionFailure(context.Background(), sd.runID, sd.impl.ID) {
			t.Error("recovered a stage that is not failed")
		}
		if n := len(auditEntries(sd.au, CategoryStageConflictResolutionFailed)); n != 0 {
			t.Errorf("stage_conflict_resolution_failed entries = %d, want 0 on a no-op recovery", n)
		}
	})
	t.Run("unconfigured repositories", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if s.maybeRecoverConflictResolutionFailure(context.Background(), uuid.New(), uuid.New()) {
			t.Error("recovered with no run/audit repositories configured")
		}
	})
}
