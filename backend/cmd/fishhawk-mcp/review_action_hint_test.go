package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedFixupTriggeredAudit appends a stage_fixup_triggered audit entry keyed
// to stageID — the durable fix-up-pass record reviewActionHintFor counts the
// prior passes (and reads the latest-round boundary sequence) against.
func seedFixupTriggeredAudit(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryStageFixupTriggered,
	})
	fb.mu.Unlock()
}

// seedImplementReviewedAudit appends an implement_reviewed audit entry keyed
// to stageID carrying an approve_with_concerns verdict with n concerns — the
// round-scoped source reviewActionHintFor counts concerns from (#860).
func seedImplementReviewedAudit(fb *fakeBackend, runID, stageID uuid.UUID, n int) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: "implement_reviewed",
		Payload:  withConcerns(n),
	})
	fb.mu.Unlock()
}

// completeStatus builds a complete implement ReviewStatus — the shape
// getRunStatus/run_stage feed reviewActionHintFor for the complete/none gate.
// The concern COUNT is now read from the seeded implement_reviewed audit
// entries (round-scoped), not from this struct's Reviews.
func completeStatus() *ReviewStatus {
	return &ReviewStatus{Stage: "implement", Status: "complete"}
}

// withConcerns is an approve_with_concerns verdict carrying n concerns.
func withConcerns(n int) PlanReview {
	concerns := make([]PlanReviewConcern, n)
	for i := range concerns {
		concerns[i] = PlanReviewConcern{Severity: "medium", Category: "scope", Note: "fix it"}
	}
	return PlanReview{ReviewerKind: "agent", Verdict: "approve_with_concerns", Concerns: concerns}
}

func TestReviewActionHintFor(t *testing.T) {
	tests := []struct {
		name   string
		status *ReviewStatus
		// seedConcerns, when > 0, seeds one implement_reviewed entry with
		// that many concerns against the implement stage.
		seedConcerns int
		// priorPasses seeds that many stage_fixup_triggered entries against
		// the implement stage BEFORE the implement_reviewed entry, modelling a
		// prior fix-up round. The latest-round count then only includes the
		// implement_reviewed entry seeded after them.
		priorPasses int
		// fixupOnOtherStage, when true, seeds the prior passes against a
		// DIFFERENT stage so they do not count against this one.
		fixupOnOtherStage bool
		wantNil           bool
		wantConcerns      int
		wantRemaining     int
		wantOverride      bool
	}{
		{
			name:    "nil status -> no hint",
			status:  nil,
			wantNil: true,
		},
		{
			name:    "status none -> no hint",
			status:  &ReviewStatus{Stage: "implement", Status: "none"},
			wantNil: true,
		},
		{
			name:    "status pending -> no hint",
			status:  &ReviewStatus{Stage: "implement", Status: "pending"},
			wantNil: true,
		},
		{
			name:         "complete with no concerns -> no hint",
			status:       completeStatus(),
			seedConcerns: 0,
			wantNil:      true,
		},
		{
			name:          "complete with concerns, no prior fix-up -> hint",
			status:        completeStatus(),
			seedConcerns:  2,
			wantNil:       false,
			wantConcerns:  2,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			name:          "budget spent, below ceiling -> exhaustion hint with override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
		},
		{
			name:          "ceiling reached -> hard-stop hint, no override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   3,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  false,
		},
		{
			name:              "fix-up on a different stage does not consume budget -> below-budget hint",
			status:            completeStatus(),
			seedConcerns:      1,
			priorPasses:       1,
			fixupOnOtherStage: true,
			wantNil:           false,
			wantConcerns:      1,
			wantRemaining:     1,
			wantOverride:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID := uuid.New()
			implementStageID := uuid.New()
			// Seed prior fix-up passes first so the latest-round concern
			// count only includes the implement_reviewed entry seeded after.
			for i := 0; i < tc.priorPasses; i++ {
				if tc.fixupOnOtherStage {
					seedFixupTriggeredAudit(fb, runID, uuid.New())
				} else {
					seedFixupTriggeredAudit(fb, runID, implementStageID)
				}
			}
			if tc.seedConcerns > 0 {
				seedImplementReviewedAudit(fb, runID, implementStageID, tc.seedConcerns)
			}
			r := newResolver(srv, nil)

			hint, err := r.reviewActionHintFor(context.Background(), runID, implementStageID, tc.status)
			if err != nil {
				t.Fatalf("reviewActionHintFor: %v", err)
			}
			if tc.wantNil {
				if hint != nil {
					t.Fatalf("hint = %+v, want nil", hint)
				}
				return
			}
			if hint == nil {
				t.Fatalf("hint = nil, want a populated hint")
			}
			if hint.Concerns != tc.wantConcerns {
				t.Errorf("Concerns = %d, want %d", hint.Concerns, tc.wantConcerns)
			}
			if hint.RemainingFixupBudget != tc.wantRemaining {
				t.Errorf("RemainingFixupBudget = %d, want %d", hint.RemainingFixupBudget, tc.wantRemaining)
			}
			if hint.OverrideAvailable != tc.wantOverride {
				t.Errorf("OverrideAvailable = %v, want %v", hint.OverrideAvailable, tc.wantOverride)
			}
			if !strings.Contains(hint.Message, "fishhawk_fixup_stage") && !strings.Contains(hint.Message, "fresh run") {
				t.Errorf("Message should reference fishhawk_fixup_stage or a fresh run; got %q", hint.Message)
			}
		})
	}
}

// TestReviewActionHintFor_LatestRoundOnly proves the concern count is scoped
// to the latest review round: a first round with 2 concerns, then a fix-up,
// then a second round with 1 concern must surface 1 — not 3 (#860).
func TestReviewActionHintFor_LatestRoundOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// Round 1: 2 concerns. Then a fix-up pass. Then round 2: 1 concern.
	seedImplementReviewedAudit(fb, runID, stageID, 2)
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedImplementReviewedAudit(fb, runID, stageID, 1)

	r := newResolver(srv, nil)
	hint, err := r.reviewActionHintFor(context.Background(), runID, stageID, completeStatus())
	if err != nil {
		t.Fatalf("reviewActionHintFor: %v", err)
	}
	if hint == nil {
		t.Fatal("hint = nil, want a populated hint")
	}
	if hint.Concerns != 1 {
		t.Errorf("Concerns = %d, want 1 (latest round only, not summed across rounds)", hint.Concerns)
	}
	// One fix-up pass spent the normal budget; below the ceiling -> override.
	if !hint.OverrideAvailable {
		t.Errorf("OverrideAvailable = false, want true (budget spent, below ceiling)")
	}
}
