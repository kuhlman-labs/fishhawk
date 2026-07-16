package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// conditionClaimsServer wires the audit + concern fakes the condition-claim
// loader and resolver need.
func conditionClaimsServer(t *testing.T) (*Server, *auditFake, *fakeConcernRepo) {
	t.Helper()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		AuditRepo:   au,
		ConcernRepo: cr,
	})
	return s, au, cr
}

// seedApprovalEntry seeds an approval_submitted audit entry into the fake's
// pre-existing history with an explicit sequence, decision, approver, and
// claims_concern_ids payload.
func seedApprovalEntry(au *auditFake, runID uuid.UUID, seq int64, decision, approver string, claims []string) {
	fields := map[string]any{
		"decision": decision,
		"approver": approver,
	}
	if len(claims) > 0 {
		fields["claims_concern_ids"] = claims
	}
	payload, _ := json.Marshal(fields)
	rid := runID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		Sequence: seq,
		Category: "approval_submitted",
		Payload:  payload,
	})
	au.mu.Unlock()
}

// seedRawApprovalEntry seeds an approval_submitted entry with a raw
// (possibly malformed) payload.
func seedRawApprovalEntry(au *auditFake, runID uuid.UUID, seq int64, payload []byte) {
	rid := runID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		Sequence: seq,
		Category: "approval_submitted",
		Payload:  payload,
	})
	au.mu.Unlock()
}

func TestLoadApprovalConcernClaims_NoneFoundReturnsNil(t *testing.T) {
	s, _, _ := conditionClaimsServer(t)
	if got := s.loadApprovalConcernClaims(context.Background(), uuid.New()); got != nil {
		t.Errorf("loadApprovalConcernClaims = %+v, want nil when no approval entries", got)
	}
}

func TestLoadApprovalConcernClaims_ApproveWithoutClaimsReturnsNil(t *testing.T) {
	s, au, _ := conditionClaimsServer(t)
	runID := uuid.New()
	seedApprovalEntry(au, runID, 10, "approve", "brett", nil)
	if got := s.loadApprovalConcernClaims(context.Background(), runID); got != nil {
		t.Errorf("loadApprovalConcernClaims = %+v, want nil when approve carries no claims", got)
	}
}

func TestLoadApprovalConcernClaims_MalformedPayloadSkipped(t *testing.T) {
	s, au, _ := conditionClaimsServer(t)
	runID := uuid.New()
	cid := uuid.New().String()
	// A valid approve-with-claims entry earlier, a malformed entry later:
	// the newest-first scan hits the malformed one (skipped) then the valid.
	seedApprovalEntry(au, runID, 10, "approve", "brett", []string{cid})
	seedRawApprovalEntry(au, runID, 11, []byte(`{not json`))

	got := s.loadApprovalConcernClaims(context.Background(), runID)
	if got == nil {
		t.Fatal("loadApprovalConcernClaims = nil, want the valid entry (malformed skipped)")
	}
	if len(got.ConcernIDs) != 1 || got.ConcernIDs[0] != cid {
		t.Errorf("ConcernIDs = %v, want [%s]", got.ConcernIDs, cid)
	}
	if got.ApprovalSeq != 10 {
		t.Errorf("ApprovalSeq = %d, want 10", got.ApprovalSeq)
	}
}

func TestLoadApprovalConcernClaims_NewestApproveWins(t *testing.T) {
	s, au, _ := conditionClaimsServer(t)
	runID := uuid.New()
	oldCID := uuid.New().String()
	newCID := uuid.New().String()
	seedApprovalEntry(au, runID, 10, "approve", "brett", []string{oldCID})
	seedApprovalEntry(au, runID, 20, "approve", "casey", []string{newCID})

	got := s.loadApprovalConcernClaims(context.Background(), runID)
	if got == nil {
		t.Fatal("loadApprovalConcernClaims = nil, want the newest approve entry")
	}
	if len(got.ConcernIDs) != 1 || got.ConcernIDs[0] != newCID {
		t.Errorf("ConcernIDs = %v, want the newest [%s]", got.ConcernIDs, newCID)
	}
	if got.ApprovalSeq != 20 || got.ApproverSubject != "casey" {
		t.Errorf("lineage = seq %d approver %q, want seq 20 approver casey", got.ApprovalSeq, got.ApproverSubject)
	}
}

func TestResolveConditionClaimedPlanConcerns_HappyPath(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "the retry cap is not enforced")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve_with_concerns")

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateAddressedByCondition {
		t.Fatalf("state = %q, want addressed_by_condition", rows[0].State)
	}
	if rows[0].StateReason == "" {
		t.Error("state_reason is empty, want a lineage reason naming the approval + review")
	}

	idx := auditEntriesByCategory(au, CategoryConcernAddressedByCondition)
	if len(idx) != 1 {
		t.Fatalf("concern_addressed_by_condition entries = %d, want 1", len(idx))
	}
	au.mu.Lock()
	entry := au.appended[idx[0]]
	au.mu.Unlock()
	if entry.RunID != runID || entry.StageID == nil || *entry.StageID != stageID {
		t.Errorf("audit run/stage = %v/%v, want %s/%s", entry.RunID, entry.StageID, runID, stageID)
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["concern_id"] != row.ID.String() {
		t.Errorf("concern_id = %v, want %s", payload["concern_id"], row.ID)
	}
	if payload["prior_state"] != string(concern.StateRaised) {
		t.Errorf("prior_state = %v, want raised", payload["prior_state"])
	}
	if payload["approval_sequence"] != float64(42) {
		t.Errorf("approval_sequence = %v, want 42", payload["approval_sequence"])
	}
	if payload["confirming_review_sequence"] != float64(200) {
		t.Errorf("confirming_review_sequence = %v, want 200", payload["confirming_review_sequence"])
	}
	if payload["reviewer_model"] != "claude-opus-4-8" {
		t.Errorf("reviewer_model = %v, want claude-opus-4-8", payload["reviewer_model"])
	}
	if payload["approver_subject"] != "brett" {
		t.Errorf("approver_subject = %v, want brett", payload["approver_subject"])
	}
}

func TestResolveConditionClaimedPlanConcerns_AppendFailureNoTransition(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "condition unmet")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})
	// The concern_addressed_by_condition append fails; the transition must
	// NOT run (durable-record-first).
	au.appendErrCategory = CategoryConcernAddressedByCondition

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve")

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (unchanged when the audit append failed)", rows[0].State)
	}
}

func TestResolveConditionClaimedPlanConcerns_AlreadyWaivedSilentSkip(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "operator waived this")
	// The operator's waive landed first (arbitration wins).
	if _, err := cr.ApplyResolution(context.Background(), row.ID, concern.StateWaived, "not blocking"); err != nil {
		t.Fatalf("pre-waive: %v", err)
	}
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve")

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateWaived {
		t.Errorf("state = %q, want waived (an already-terminal claim is skipped, waive wins)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 0 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 0 for an already-terminal claim", n)
	}
}

func TestResolveConditionClaimedPlanConcerns_ImplementStageRowSkipped(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	// Defense-in-depth: a claim referencing an implement-stage row (which the
	// approve-time gate would have rejected) must never resolve here.
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 5, "implement concern")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve")

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (an implement-stage row is never resolved by a condition claim)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 0 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 0 for a skipped row", n)
	}
}

func TestResolveConditionClaimedPlanConcerns_SecondRoundIdempotent(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "condition")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	// Round 1 resolves it.
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve")
	// Round 2 (a post-fixup re-review) must be a no-op on the already-terminal row.
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 300, "gpt-5.5", "approve")

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateAddressedByCondition {
		t.Errorf("state = %q, want addressed_by_condition", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 1 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 1 (round 2 is idempotent)", n)
	}
}
