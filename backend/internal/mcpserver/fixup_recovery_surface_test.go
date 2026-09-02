package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- the pure firing rule (latestFixupRecovery) ---

// TestLatestFixupRecovery_Branches drives ONE case per named branch of the
// #3081 firing rule. The two nil branches are the controls: no recovery entries
// at all (a fix-up that genuinely LANDED) and a recovery superseded by a later
// trigger (an earlier round's recovery, already answered by a later pass).
func TestLatestFixupRecovery_Branches(t *testing.T) {
	cases := []struct {
		name        string
		triggers    []int64
		recoveries  []fixupRecoverySignal
		wantMarker  bool
		wantDetails bool
		wantReason  string
		wantCat     string
		wantState   string
	}{
		{
			name:     "recovery after the latest trigger fires with decoded details",
			triggers: []int64{10},
			recoveries: []fixupRecoverySignal{{
				Sequence:              11,
				RestoredState:         "succeeded",
				RestoredReviewStageID: "11111111-1111-1111-1111-111111111111",
				SourceFailureReason:   "commit/push onto PR branch failed",
				SourceFailureCategory: "C",
				Parsed:                true,
			}},
			wantMarker:  true,
			wantDetails: true,
			wantReason:  "commit/push onto PR branch failed",
			wantCat:     "C",
			wantState:   "succeeded",
		},
		{
			// PRIMARY CONTROL: a fix-up that succeeded writes no recovery entry,
			// so the wait status must stay byte-identical to today.
			name:       "no recovery entries yields no marker",
			triggers:   []int64{10},
			recoveries: nil,
			wantMarker: false,
		},
		{
			// SECOND CONTROL: a later pass superseded the recovery, so the
			// marker self-clears rather than warning about a finished round.
			name:     "recovery sequenced before the latest trigger is superseded",
			triggers: []int64{10, 30},
			recoveries: []fixupRecoverySignal{{
				Sequence: 11, RestoredState: "succeeded", SourceFailureCategory: "A", Parsed: true,
			}},
			wantMarker: false,
		},
		{
			name:       "no triggers yields no marker",
			triggers:   nil,
			recoveries: []fixupRecoverySignal{{Sequence: 5, Parsed: true}},
			wantMarker: false,
		},
		{
			// An undecodable payload STILL fires: the recovery demonstrably
			// happened, and suppressing here restores the silence #3081 closes.
			name:        "undecodable payload fires with details_available false",
			triggers:    []int64{10},
			recoveries:  []fixupRecoverySignal{{Sequence: 11, Parsed: false}},
			wantMarker:  true,
			wantDetails: false,
		},
		{
			name:     "two post-trigger recoveries: the newest wins",
			triggers: []int64{10},
			recoveries: []fixupRecoverySignal{
				{Sequence: 11, SourceFailureReason: "older", SourceFailureCategory: "A", Parsed: true},
				{Sequence: 12, SourceFailureReason: "newer", SourceFailureCategory: "B", Parsed: true},
			},
			wantMarker:  true,
			wantDetails: true,
			wantReason:  "newer",
			wantCat:     "B",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := latestFixupRecovery(tc.triggers, tc.recoveries)
			if !tc.wantMarker {
				if got != nil {
					t.Fatalf("latestFixupRecovery = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("latestFixupRecovery = nil, want a marker")
			}
			if got.DetailsAvailable != tc.wantDetails {
				t.Errorf("DetailsAvailable = %v, want %v", got.DetailsAvailable, tc.wantDetails)
			}
			if got.SourceFailureReason != tc.wantReason {
				t.Errorf("SourceFailureReason = %q, want %q", got.SourceFailureReason, tc.wantReason)
			}
			if got.SourceFailureCategory != tc.wantCat {
				t.Errorf("SourceFailureCategory = %q, want %q", got.SourceFailureCategory, tc.wantCat)
			}
			if got.RestoredState != tc.wantState {
				t.Errorf("RestoredState = %q, want %q", got.RestoredState, tc.wantState)
			}
			if got.Message == "" {
				t.Error("Message is empty; the marker must always carry its advisory")
			}
		})
	}
}

// TestFixupRecoveryMessage_NamesTheConsequencesAndTheBudgetRule pins the four
// facts an operator acting on a bare `succeeded` would get wrong, including the
// CURRENT fix-up budget rule (#1957): category A/B consume a pass, only a
// delivered-nothing category C is refunded. Stating the rule is in scope;
// changing it is not.
func TestFixupRecoveryMessage_NamesTheConsequencesAndTheBudgetRule(t *testing.T) {
	msg := fixupRecoveryMessage(&FixupRecovery{
		SourceFailureReason:   "commit/push onto PR branch failed",
		SourceFailureCategory: "C",
		RestoredState:         "succeeded",
		DetailsAvailable:      true,
	})
	for _, want := range []string{
		"FAILED",
		"pushed no commit",
		"NOT addressed",
		"git log",
		"commit/push onto PR branch failed",
		"Source failure category: C",
		"CONSUMES a fix-up pass",
		"category-C",
		"refunded",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\nmessage: %s", want, msg)
		}
	}
}

// TestFixupRecoveryMessage_UndecodableSaysSo: the details_available=false
// message must not silently omit the reason as if none existed — it names that
// the payload could not be decoded and where to read the raw entry.
func TestFixupRecoveryMessage_UndecodableSaysSo(t *testing.T) {
	msg := fixupRecoveryMessage(&FixupRecovery{DetailsAvailable: false})
	for _, want := range []string{"could not be decoded", "details_available=false", "fishhawk_list_audit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\nmessage: %s", want, msg)
		}
	}
}

// TestFixupRecoveryMessage_NilIsEmpty pins the nil guard.
func TestFixupRecoveryMessage_NilIsEmpty(t *testing.T) {
	if got := fixupRecoveryMessage(nil); got != "" {
		t.Errorf("fixupRecoveryMessage(nil) = %q, want empty", got)
	}
}

// TestLatestFixupRecovery_ReasonIsCapped proves a long runner error cannot
// perturb the get_run_status byte budget: the reason is truncated through
// capJSONString against fixupRecoveryReasonCap.
func TestLatestFixupRecovery_ReasonIsCapped(t *testing.T) {
	long := strings.Repeat("x", fixupRecoveryReasonCap*4)
	got := latestFixupRecovery([]int64{1}, []fixupRecoverySignal{{
		Sequence: 2, SourceFailureReason: long, Parsed: true,
	}})
	if got == nil {
		t.Fatal("latestFixupRecovery = nil, want a marker")
	}
	if len(got.SourceFailureReason) >= len(long) {
		t.Fatalf("SourceFailureReason len = %d, want truncated below %d", len(got.SourceFailureReason), len(long))
	}
	if len(got.SourceFailureReason) > fixupRecoveryReasonCap {
		t.Errorf("SourceFailureReason len = %d, want <= %d", len(got.SourceFailureReason), fixupRecoveryReasonCap)
	}
	if !strings.HasPrefix(long, got.SourceFailureReason) {
		t.Error("SourceFailureReason is not a prefix of the input")
	}
}

// --- the resolver probe (fixupRecoveryFor) ---

// seedFixupRecoveredPayload appends a stage_fixup_recovered audit entry keyed to
// stageID carrying the FULL #788 recovery payload the backend's
// writeFixupRecoveredAudit writes (restored_state, restored_review_stage_id,
// source_failure_category, source_failure_reason). The sibling
// seedStageFixupRecoveredAudit in review_action_hint_test.go carries only the
// category, which is all the refund mirror reads.
func seedFixupRecoveredPayload(fb *fakeBackend, runID, stageID uuid.UUID, category, reason string) {
	sid := stageID.String()
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		StageID:  &sid,
		Category: categoryStageFixupRecovered,
		Payload: map[string]any{
			"stage_id":                 sid,
			"restored_state":           "succeeded",
			"restored_review_stage_id": "22222222-2222-2222-2222-222222222222",
			"source_failure_category":  category,
			"source_failure_reason":    reason,
		},
	})
	fb.mu.Unlock()
}

// TestFixupRecoveryFor_DecodesTheRecoveryPayload drives the probe against the
// package's fakeBackend: a trigger followed by a recovery must produce the
// marker with the payload's category + reason decoded.
func TestFixupRecoveryFor_DecodesTheRecoveryPayload(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "A", "the agent exited non-zero")

	got := newResolver(srv, nil).fixupRecoveryFor(context.Background(), runID, stageID)
	if got == nil {
		t.Fatal("fixupRecoveryFor = nil, want a marker")
	}
	if got.SourceFailureCategory != "A" {
		t.Errorf("SourceFailureCategory = %q, want A", got.SourceFailureCategory)
	}
	if got.SourceFailureReason != "the agent exited non-zero" {
		t.Errorf("SourceFailureReason = %q", got.SourceFailureReason)
	}
	if got.RestoredState != "succeeded" {
		t.Errorf("RestoredState = %q, want succeeded", got.RestoredState)
	}
	if !got.DetailsAvailable {
		t.Error("DetailsAvailable = false, want true for a well-formed payload")
	}
}

// TestFixupRecoveryFor_RejectsARecoveryOnADifferentStage is the wrong-stage
// control. The fake's audit endpoint filters by CATEGORY only (as a lenient
// audit backend might), so the sibling stage's recovery entry IS returned to the
// probe; only the per-entry StageID double-check keeps it from being attributed
// here. Deleting that check makes this test RED.
func TestFixupRecoveryFor_RejectsARecoveryOnADifferentStage(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID, otherStageID := uuid.New(), uuid.New(), uuid.New()
	seedFixupTriggeredAudit(fb, runID, stageID)
	// The recovery belongs to a DIFFERENT stage of the same run, and is
	// sequenced AFTER our stage's trigger — so only the stage-id check can
	// reject it.
	seedFixupRecoveredPayload(fb, runID, otherStageID, "C", "someone else's failure")

	if got := newResolver(srv, nil).fixupRecoveryFor(context.Background(), runID, stageID); got != nil {
		t.Fatalf("fixupRecoveryFor = %+v, want nil (the recovery belongs to stage %s, not %s)", got, otherStageID, stageID)
	}
}

// TestFixupRecoveryFor_AuditErrorDegradesToNil pins the best-effort contract at
// the resolver layer: an audit read failure loses the advisory rather than
// producing an error a caller could fail a wait on.
func TestFixupRecoveryFor_AuditErrorDegradesToNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedFixupRecoveredPayload(fb, runID, stageID, "C", "boom")
	fb.mu.Lock()
	fb.perRunAuditStatus = 500
	fb.mu.Unlock()

	if got := newResolver(srv, nil).fixupRecoveryFor(context.Background(), runID, stageID); got != nil {
		t.Fatalf("fixupRecoveryFor = %+v, want nil on an audit read error", got)
	}
}

// TestFixupRecoveryFor_NoTriggersYieldsNil covers the early return before the
// second audit read — a stage that never had a fix-up pays one read, not two.
func TestFixupRecoveryFor_NoTriggersYieldsNil(t *testing.T) {
	_, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	if got := newResolver(srv, nil).fixupRecoveryFor(context.Background(), runID, stageID); got != nil {
		t.Fatalf("fixupRecoveryFor = %+v, want nil with no fix-up trigger", got)
	}
}

// TestFixupRecoveryFor_UndecodablePayloadStillFires drives the undecodable
// branch through the REAL probe (not just the pure rule): a recovery entry whose
// payload is a JSON array decodes to nothing, and the marker must still fire
// with details_available=false.
func TestFixupRecoveryFor_UndecodablePayloadStillFires(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	seedFixupTriggeredAudit(fb, runID, stageID)
	seedRecoveredSignalUnparseable(fb, runID, stageID)

	got := newResolver(srv, nil).fixupRecoveryFor(context.Background(), runID, stageID)
	if got == nil {
		t.Fatal("fixupRecoveryFor = nil, want a marker even on an undecodable payload")
	}
	if got.DetailsAvailable {
		t.Error("DetailsAvailable = true, want false for an undecodable payload")
	}
	if got.SourceFailureCategory != "" || got.SourceFailureReason != "" {
		t.Errorf("detail fields = %q/%q, want empty", got.SourceFailureCategory, got.SourceFailureReason)
	}
}
