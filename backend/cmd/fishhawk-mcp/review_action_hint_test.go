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

// seedFixupNoChangesAudit appends a fixup_no_changes audit entry keyed to
// stageID — the durable refund signal reviewActionHintFor counts to widen the
// normal fix-up budget, mirroring the backend's no-change refund (#967).
func seedFixupNoChangesAudit(fb *fakeBackend, runID, stageID uuid.UUID) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryFixupNoChanges,
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
		// runState is the run state fed to the hint; empty defaults to
		// "running". Terminal states suppress the hint entirely (#968).
		runState string
		// seedConcerns, when > 0, seeds one implement_reviewed entry with
		// that many concerns against the implement stage.
		seedConcerns int
		// priorPasses seeds that many stage_fixup_triggered entries against
		// the implement stage BEFORE the implement_reviewed entry, modelling a
		// prior fix-up round. The latest-round count then only includes the
		// implement_reviewed entry seeded after them.
		priorPasses int
		// refunds seeds that many fixup_no_changes entries against the
		// implement stage — no-change passes refunded against the normal
		// budget (#967/#1150). The hint widens the normal budget by the
		// refund count (clamped to priorPasses).
		refunds int
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
			// #1150 (a): a no-change pass is refunded against the normal
			// budget — one triggered + one refund => effectiveConsumed=0 < 1
			// => a normal route-back is restored (the core assertion the
			// backend's widened MaxPasses admits without force_additional_pass).
			name:          "refund restores normal budget -> route-back, no override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1150 (c): a refund count exceeding the triggered passes is
			// clamped to priorPasses, so remaining never widens past the
			// normal budget. Mirrors the backend's refundedPasses>priorPasses
			// clamp.
			name:          "refund clamped to prior passes -> remaining capped at budget",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   1,
			refunds:       2,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 1,
			wantOverride:  false,
		},
		{
			// #1150 (d): two triggered + one refund => effectiveConsumed=1,
			// which is NOT < maxFixupPasses(1), so the NORMAL arm does not
			// fire — the override arm does. raw priorPasses=2 is still < the
			// hard ceiling of 3, so an override pass is available. Proves the
			// override arm keys off RAW priorPasses, not effectiveConsumed.
			name:          "two passes, one refund -> override (keys off raw priorPasses)",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   2,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
		},
		{
			// #1150 (d) boundary: three triggered + one refund =>
			// effectiveConsumed=2. If the ceiling arm wrongly keyed off
			// effectiveConsumed (2 < 3) it would still offer an override; it
			// must key off RAW priorPasses=3, which is at the ceiling => no
			// override left. This is the case that truly distinguishes raw
			// from effective.
			name:          "ceiling keys off raw passes despite refund -> no override",
			status:        completeStatus(),
			seedConcerns:  1,
			priorPasses:   3,
			refunds:       1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  false,
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
		{
			// #968: a terminal run has no actionable fix-up — the server
			// refuses with fixup_not_applicable — so the hint must suppress
			// even when concerns remain and the ceiling has headroom (the
			// shape that advertised override_available on run 68e13183).
			name:         "run succeeded -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "succeeded",
			seedConcerns: 1,
			priorPasses:  1,
			wantNil:      true,
		},
		{
			name:         "run failed -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "failed",
			seedConcerns: 1,
			wantNil:      true,
		},
		{
			name:         "run cancelled -> no hint despite concerns",
			status:       completeStatus(),
			runState:     "cancelled",
			seedConcerns: 1,
			wantNil:      true,
		},
		{
			name:          "run running -> hint still surfaces",
			status:        completeStatus(),
			runState:      "running",
			seedConcerns:  1,
			priorPasses:   1,
			wantNil:       false,
			wantConcerns:  1,
			wantRemaining: 0,
			wantOverride:  true,
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
			for i := 0; i < tc.refunds; i++ {
				seedFixupNoChangesAudit(fb, runID, implementStageID)
			}
			if tc.seedConcerns > 0 {
				seedImplementReviewedAudit(fb, runID, implementStageID, tc.seedConcerns)
			}
			r := newResolver(srv, nil)

			runState := tc.runState
			if runState == "" {
				runState = "running"
			}
			hint, err := r.reviewActionHintFor(context.Background(), runID, implementStageID, runState, tc.status)
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
			// #964: when the hint points at a fix-up it must steer the
			// operator at stable concern_ids (the primary addressing
			// scheme), never the deprecated positional indices.
			if strings.Contains(hint.Message, "fishhawk_fixup_stage") {
				if !strings.Contains(hint.Message, "concern_ids") {
					t.Errorf("Message should point at concern_ids addressing; got %q", hint.Message)
				}
				if strings.Contains(hint.Message, "concern indices") {
					t.Errorf("Message still points at deprecated positional indices; got %q", hint.Message)
				}
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
	hint, err := r.reviewActionHintFor(context.Background(), runID, stageID, "running", completeStatus())
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

// TestImplementReviewMergeHint covers the #947 local-loop parity hint: a
// display-only merge-readiness warning surfaced ONLY while the implement-stage
// agent review is pending (dispatched, no verdict). It mirrors the backend's
// review-pending presence gate; once the review reaches any terminal status
// the hint is empty (the required fishhawk_audit_complete check flips green).
func TestImplementReviewMergeHint(t *testing.T) {
	tests := []struct {
		name     string
		status   *ReviewStatus
		wantHint bool
	}{
		{"nil status -> no hint", nil, false},
		{"none -> no hint", &ReviewStatus{Stage: "implement", Status: "none"}, false},
		{"pending -> hint", &ReviewStatus{Stage: "implement", Status: "pending"}, true},
		{"complete -> no hint", &ReviewStatus{Stage: "implement", Status: "complete"}, false},
		{"skipped -> no hint", &ReviewStatus{Stage: "implement", Status: "skipped"}, false},
		{"failed -> no hint", &ReviewStatus{Stage: "implement", Status: "failed"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := implementReviewMergeHint(tc.status)
			if tc.wantHint {
				if got == "" {
					t.Fatalf("expected a merge-readiness hint, got empty")
				}
				if !strings.Contains(got, "not") || !strings.Contains(got, "fishhawk_audit_complete") {
					t.Errorf("hint should warn the PR is not safe to merge and name the held check: %q", got)
				}
			} else if got != "" {
				t.Errorf("expected no hint for status %v, got %q", tc.status, got)
			}
		})
	}
}

// TestReviewActionHint_SuggestedActions pins the hint → next_actions
// translation (#1024): the concern-arm entries derive FROM the computed
// hint value, so each budget branch maps to a fixed action set and the
// two surfaces cannot disagree on the remaining budget.
func TestReviewActionHint_SuggestedActions(t *testing.T) {
	run := &Run{ID: uuid.NewString(), State: "running"}
	stageID := uuid.NewString()

	t.Run("below budget -> fixup first, consuming fixup_budget", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 2, RemainingFixupBudget: 1}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 2 || actions[0].Action != "fishhawk_fixup_stage" || actions[1].Action != "merge_and_file_follow_up" {
			t.Fatalf("actions = %+v, want [fishhawk_fixup_stage merge_and_file_follow_up]", actions)
		}
		if actions[0].Consumes != consumesFixupBudget {
			t.Errorf("fixup consumes = %q, want fixup_budget", actions[0].Consumes)
		}
		if actions[0].Params["stage_id"] != stageID {
			t.Errorf("fixup params.stage_id = %q, want %s", actions[0].Params["stage_id"], stageID)
		}
		// The remaining-budget number rides on the reason — the figure the
		// integration test cross-checks against the hint itself.
		if !strings.Contains(actions[0].Reason, "1 normal fix-up pass") {
			t.Errorf("fixup reason should carry the remaining budget; got %q", actions[0].Reason)
		}
		if _, forced := actions[0].Params["force_additional_pass"]; forced {
			t.Error("below-budget fixup action must not carry force_additional_pass")
		}
	})

	t.Run("budget spent, override available -> forced fixup offered", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: true}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 2 || actions[0].Action != "merge_and_file_follow_up" || actions[1].Action != "fishhawk_fixup_stage" {
			t.Fatalf("actions = %+v, want [merge_and_file_follow_up fishhawk_fixup_stage]", actions)
		}
		if actions[1].Params["force_additional_pass"] != "true" {
			t.Errorf("override fixup params = %v, want force_additional_pass=true", actions[1].Params)
		}
	})

	t.Run("ceiling reached -> merge-with-follow-up or fresh run", func(t *testing.T) {
		h := &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: false}
		actions := h.suggestedActions(run, stageID)
		if len(actions) != 2 || actions[0].Action != "merge_and_file_follow_up" || actions[1].Action != "fishhawk_start_run" {
			t.Fatalf("actions = %+v, want [merge_and_file_follow_up fishhawk_start_run]", actions)
		}
		if actions[1].Consumes != consumesNewRun {
			t.Errorf("fresh-run consumes = %q, want new_run", actions[1].Consumes)
		}
	})
}
