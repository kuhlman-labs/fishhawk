package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// seedFixupTriggeredAudit appends a stage_fixup_triggered audit entry keyed
// to stageID — the durable fix-up-pass record reviewActionHintFor counts the
// remaining budget against.
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

// completeStatus builds a complete implement ReviewStatus carrying the given
// verdicts — the shape getRunStatus/run_stage feed reviewActionHintFor.
func completeStatus(reviews ...PlanReview) *ReviewStatus {
	return &ReviewStatus{Stage: "implement", Status: "complete", Reviews: reviews}
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
		// priorFixupStage, when non-nil, seeds one stage_fixup_triggered
		// entry against that stage id before the call.
		priorFixupStage func(stageID uuid.UUID) uuid.UUID
		wantNil         bool
		wantConcerns    int
		wantRemaining   int
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
			name:    "complete with only approve -> no hint",
			status:  completeStatus(PlanReview{ReviewerKind: "agent", Verdict: "approve"}),
			wantNil: true,
		},
		{
			name:          "complete with concerns, no prior fix-up -> hint",
			status:        completeStatus(withConcerns(2)),
			wantNil:       false,
			wantConcerns:  2,
			wantRemaining: 1,
		},
		{
			name:   "complete with concerns but budget spent -> no hint",
			status: completeStatus(withConcerns(1)),
			priorFixupStage: func(stageID uuid.UUID) uuid.UUID {
				return stageID // seed a prior pass against THIS stage
			},
			wantNil: true,
		},
		{
			name:          "concerns summed across reviewers -> hint",
			status:        completeStatus(withConcerns(1), PlanReview{Verdict: "approve"}, withConcerns(2)),
			wantNil:       false,
			wantConcerns:  3,
			wantRemaining: 1,
		},
		{
			name:   "fix-up on a different stage does not consume budget -> hint",
			status: completeStatus(withConcerns(1)),
			priorFixupStage: func(stageID uuid.UUID) uuid.UUID {
				return uuid.New() // a DIFFERENT stage's fix-up
			},
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			runID := uuid.New()
			implementStageID := uuid.New()
			if tc.priorFixupStage != nil {
				seedFixupTriggeredAudit(fb, runID, tc.priorFixupStage(implementStageID))
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
			if !strings.Contains(hint.Message, "fishhawk_fixup_stage") {
				t.Errorf("Message should reference fishhawk_fixup_stage; got %q", hint.Message)
			}
			if !strings.Contains(hint.Message, implementStageID.String()) {
				t.Errorf("Message should name the implement stage id; got %q", hint.Message)
			}
		})
	}
}
