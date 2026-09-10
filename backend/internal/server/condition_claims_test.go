package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve_with_concerns", nil)

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

func TestResolveConditionClaimedPlanConcerns_QualifiedWhenConfirmingReviewRaisedFreshConcerns(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "the gap is not met")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	// The confirming review minted two fresh implement concerns of its own.
	fresh := []uuid.UUID{uuid.New(), uuid.New()}
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve_with_concerns", fresh)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	// The claim STILL resolves — the operator's binding condition is the
	// authority; only the LABEL is qualified.
	if rows[0].State != concern.StateAddressedByCondition {
		t.Fatalf("state = %q, want addressed_by_condition (the claim still resolves when qualified)", rows[0].State)
	}
	if strings.Contains(rows[0].StateReason, "confirmed delivered") {
		t.Errorf("state_reason = %q, must NOT assert unqualified \"confirmed delivered\" when the confirming review raised fresh concerns", rows[0].StateReason)
	}
	if !strings.Contains(rows[0].StateReason, "2 fresh implement concern") {
		t.Errorf("state_reason = %q, want it to name the fresh-concern count (2)", rows[0].StateReason)
	}

	idx := auditEntriesByCategory(au, CategoryConcernAddressedByCondition)
	if len(idx) != 1 {
		t.Fatalf("concern_addressed_by_condition entries = %d, want 1", len(idx))
	}
	au.mu.Lock()
	entry := au.appended[idx[0]]
	au.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["confirming_review_qualified"] != true {
		t.Errorf("confirming_review_qualified = %v, want true", payload["confirming_review_qualified"])
	}
	raw, ok := payload["confirming_review_fresh_concern_ids"].([]any)
	if !ok {
		t.Fatalf("confirming_review_fresh_concern_ids = %v, want a []string", payload["confirming_review_fresh_concern_ids"])
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{fresh[0].String(), fresh[1].String()}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("confirming_review_fresh_concern_ids = %v, want %v", got, want)
	}
}

func TestResolveConditionClaimedPlanConcerns_UnqualifiedOnCleanApprove(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "the retry cap is not enforced")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	// A clean approve with zero fresh minted concerns keeps the verbatim wording.
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateAddressedByCondition {
		t.Fatalf("state = %q, want addressed_by_condition", rows[0].State)
	}
	wantReason := "binding approval condition (approval sequence 42) confirmed delivered by implement review sequence 200"
	if rows[0].StateReason != wantReason {
		t.Errorf("state_reason = %q, want the verbatim unqualified wording %q", rows[0].StateReason, wantReason)
	}

	idx := auditEntriesByCategory(au, CategoryConcernAddressedByCondition)
	if len(idx) != 1 {
		t.Fatalf("concern_addressed_by_condition entries = %d, want 1", len(idx))
	}
	au.mu.Lock()
	entry := au.appended[idx[0]]
	au.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["confirming_review_qualified"] != false {
		t.Errorf("confirming_review_qualified = %v, want false on a clean approve", payload["confirming_review_qualified"])
	}
	if _, present := payload["confirming_review_fresh_concern_ids"]; present {
		t.Errorf("confirming_review_fresh_concern_ids present = %v, want omitted when unqualified", payload["confirming_review_fresh_concern_ids"])
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

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

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

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

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

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (an implement-stage row is never resolved by a condition claim)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 0 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 0 for a skipped row", n)
	}
}

func TestResolveConditionClaimedPlanConcerns_ApplyResolutionFailureAfterAppend(t *testing.T) {
	au := newAuditFake()
	base := newFakeConcernRepo()
	// ApplyResolution fails AFTER the audit entry has appended — a state change
	// (e.g. a reopen) raced the hook between the IsOpen read and the transition.
	cr := &raceConcernRepo{
		fakeConcernRepo: base,
		applyErr:        concern.InvalidTransitionError{From: concern.StateRaised, To: concern.StateAddressedByCondition},
	}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})

	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, base, runID, stageID, concern.StageKindPlan, 5, "condition")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (ApplyResolution failed, the row stays open)", rows[0].State)
	}
	// The append-first ordering means the lineage entry already exists even
	// though the transition failed. A later confirming round re-fires the hook
	// (the row is still open) and appends a SECOND entry for the same concern —
	// pinning the documented duplicate-entry nuance so a downstream consumer of
	// the audit category is not surprised by it.
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 1 {
		t.Fatalf("concern_addressed_by_condition entries = %d, want 1 after the first failed transition", n)
	}
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 300, "gpt-5.5", "approve", nil)
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 2 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 2 (a still-open row re-appends on the next round)", n)
	}
}

func TestResolveConditionClaimedPlanConcerns_CrossRunRowSkipped(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID := uuid.New()
	otherRunID, stageID := uuid.New(), uuid.New()
	// Defense-in-depth: a plan-stage concern from ANOTHER run (which the
	// approve-time gate would have rejected) must never be resolved by this
	// run's condition claim — the RunID side of the cross-run/kind guard.
	row := seedConcernRow(t, cr, otherRunID, stageID, concern.StageKindPlan, 5, "another run's plan concern")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (a cross-run row is never resolved by a condition claim)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 0 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 0 for a cross-run row", n)
	}
}

func TestResolveConditionClaimedPlanConcerns_SecondRoundIdempotent(t *testing.T) {
	s, au, cr := conditionClaimsServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindPlan, 5, "condition")
	seedApprovalEntry(au, runID, 42, "approve", "brett", []string{row.ID.String()})

	// Round 1 resolves it.
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 200, "claude-opus-4-8", "approve", nil)
	// Round 2 (a post-fixup re-review) must be a no-op on the already-terminal row.
	s.resolveConditionClaimedPlanConcerns(context.Background(), runID, 300, "gpt-5.5", "approve", nil)

	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateAddressedByCondition {
		t.Errorf("state = %q, want addressed_by_condition", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernAddressedByCondition)); n != 1 {
		t.Errorf("concern_addressed_by_condition entries = %d, want 1 (round 2 is idempotent)", n)
	}
}

// ---------------------------------------------------------------------------
// claims_all_open_plan_concerns — the #3318 approve-time expansion shorthand.
// ---------------------------------------------------------------------------

// claimsErrorEnvelope decodes {"error":{"code","message","details"}} out of a
// refusal body. Error-code IDENTITY (rather than "some 400 happened") is what
// makes each per-mode test below a working counterfactual: deleting one branch
// lets a NEIGHBOURING guard refuse instead, with a different code, which this
// catches and a bare status assertion would not.
func claimsErrorEnvelope(t *testing.T, body []byte) (string, map[string]any) {
	t.Helper()
	var wrapper map[string]any
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, body)
	}
	env, _ := wrapper["error"].(map[string]any)
	if env == nil {
		t.Fatalf("body carries no error envelope: %s", body)
	}
	code, _ := env["code"].(string)
	details, _ := env["details"].(map[string]any)
	return code, details
}

// TestExpandAllOpenPlanConcernClaims_ClaimsExactlyOpenPlanConcerns is the
// DONE-MEANS for the shorthand. Correctness is not structurally enforced by
// compilation — an expansion that silently claimed the WRONG set would still
// compile and still record a claims_concern_ids key — so this seeds a run
// carrying two open PLAN concerns, one open IMPLEMENT concern and one
// already-waived PLAN concern, approves with the shorthand, and asserts the
// recorded claims_concern_ids equals EXACTLY the two open plan ids. A no-op or
// over-broad expansion fails here where a mere file-touch would pass.
func TestExpandAllOpenPlanConcernClaims_ClaimsExactlyOpenPlanConcerns(t *testing.T) {
	s, _, rr, au, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	planStageID := uuid.New()

	planA := seedConcernRow(t, cr, stage.RunID, planStageID, concern.StageKindPlan, 5, "the retry cap is not enforced")
	planB := seedConcernRow(t, cr, stage.RunID, planStageID, concern.StageKindPlan, 6, "the fallback is untested")
	implA := seedConcernRow(t, cr, stage.RunID, uuid.New(), concern.StageKindImplement, 7, "an out-of-scope edit")
	waived := seedConcernRow(t, cr, stage.RunID, planStageID, concern.StageKindPlan, 4, "already settled")
	if _, err := cr.ApplyResolution(context.Background(), waived.ID, concern.StateWaived, "not blocking"); err != nil {
		t.Fatalf("seed waived concern: %v", err)
	}
	// A concern in ANOTHER run, to pin the run scoping of the expansion.
	other := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindPlan, 9, "another run's concern")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := findApprovalSubmittedPayload(t, au.appended)
	raw, ok := payload["claims_concern_ids"].([]any)
	if !ok {
		t.Fatalf("claims_concern_ids = %v, want the expanded open plan ids", payload["claims_concern_ids"])
	}
	got := make(map[string]bool, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		got[s] = true
	}
	want := map[string]bool{planA.ID.String(): true, planB.ID.String(): true}
	if len(got) != len(want) {
		t.Fatalf("claims_concern_ids = %v (%d ids), want exactly the two open plan ids %v", raw, len(got), want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("expanded claim set is missing open plan concern %s: %v", id, raw)
		}
	}
	if got[implA.ID.String()] {
		t.Errorf("expansion claimed an IMPLEMENT-stage concern %s — the StageKindPlan filter is not holding: %v", implA.ID, raw)
	}
	if got[waived.ID.String()] {
		t.Errorf("expansion claimed an already-waived concern %s — ListOpenByRun should exclude it: %v", waived.ID, raw)
	}
	if got[other.ID.String()] {
		t.Errorf("expansion claimed another run's concern %s: %v", other.ID, raw)
	}
	// The INTENT keys ride alongside the expanded ids.
	if payload["claims_all_open_plan_concerns"] != true {
		t.Errorf("claims_all_open_plan_concerns = %v, want true", payload["claims_all_open_plan_concerns"])
	}
	if n, _ := payload["claims_all_open_plan_concerns_expanded"].(float64); int(n) != 2 {
		t.Errorf("claims_all_open_plan_concerns_expanded = %v, want 2", payload["claims_all_open_plan_concerns_expanded"])
	}
}

// TestExpandAllOpenPlanConcernClaims_EmptyExpansionIsLegal: no open plan
// concerns at approval time is NOT an error — nothing to claim is a legal
// outcome — but the operator's declared INTENT is still recorded, with
// _expanded:0 distinguishing it from an approve that never used the channel.
func TestExpandAllOpenPlanConcernClaims_EmptyExpansionIsLegal(t *testing.T) {
	s, _, rr, au, _ := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on an empty expansion:\n%s", w.Code, w.Body.String())
	}
	payload := findApprovalSubmittedPayload(t, au.appended)
	if _, ok := payload["claims_concern_ids"]; ok {
		t.Errorf("claims_concern_ids should be absent for an empty expansion: %v", payload)
	}
	if payload["claims_all_open_plan_concerns"] != true {
		t.Errorf("claims_all_open_plan_concerns = %v, want true (the intent survives an empty expansion)", payload["claims_all_open_plan_concerns"])
	}
	if n, _ := payload["claims_all_open_plan_concerns_expanded"].(float64); int(n) != 0 {
		t.Errorf("claims_all_open_plan_concerns_expanded = %v, want 0", payload["claims_all_open_plan_concerns_expanded"])
	}
}

// TestApprove_ClaimsAllOpenPlanConcerns_NoShorthand_OmitsIntentKeys pins the
// byte-identical no-shorthand path: an approve that does not use the channel
// carries NEITHER intent key.
func TestApprove_ClaimsAllOpenPlanConcerns_NoShorthand_OmitsIntentKeys(t *testing.T) {
	s, _, rr, au, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	row := seedConcernRow(t, cr, stage.RunID, uuid.New(), concern.StageKindPlan, 5, "note")

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","claims_concern_ids":["`+row.ID.String()+`"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := findApprovalSubmittedPayload(t, au.appended)
	if _, ok := payload["claims_all_open_plan_concerns"]; ok {
		t.Errorf("claims_all_open_plan_concerns present on an explicit-id approve: %v", payload)
	}
	if _, ok := payload["claims_all_open_plan_concerns_expanded"]; ok {
		t.Errorf("claims_all_open_plan_concerns_expanded present on an explicit-id approve: %v", payload)
	}
}

// TestApprove_ClaimsAllAndExplicitIDs_Rejected pins the MUTUAL EXCLUSION
// branch: the expansion is always a superset of any valid explicit list, so
// accepting both would make the recorded claim ambiguous about operator intent.
func TestApprove_ClaimsAllAndExplicitIDs_Rejected(t *testing.T) {
	s, ar, rr, au, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	row := seedConcernRow(t, cr, stage.RunID, uuid.New(), concern.StageKindPlan, 5, "note")

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","claims_all_open_plan_concerns":true,"claims_concern_ids":["`+row.ID.String()+`"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when both claim channels are set:\n%s", w.Code, w.Body.String())
	}
	code, details := claimsErrorEnvelope(t, w.Body.Bytes())
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", code)
	}
	if details["rule"] != "claims_all_and_explicit_ids" {
		t.Errorf("details.rule = %v, want claims_all_and_explicit_ids", details["rule"])
	}
	assertNoApprovalRecorded(t, ar, au)
}

// TestApprove_ClaimsAllOpenPlanConcerns_OnRejectRejected: the shorthand is
// approve-only — a claim only makes sense on the approval carrying the binding
// condition.
func TestApprove_ClaimsAllOpenPlanConcerns_OnRejectRejected(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"reject","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a shorthand claim with reject:\n%s", w.Code, w.Body.String())
	}
	code, details := claimsErrorEnvelope(t, w.Body.Bytes())
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", code)
	}
	if details["field"] != "claims_all_open_plan_concerns" {
		t.Errorf("details.field = %v, want claims_all_open_plan_concerns", details["field"])
	}
	assertNoApprovalRecorded(t, ar, au)
}

// TestApprove_ClaimsAllOpenPlanConcerns_OnDeployStageRejected: plan-stage only
// — the concerns a condition answers are plan-stage concerns.
func TestApprove_ClaimsAllOpenPlanConcerns_OnDeployStageRejected(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.mu.Lock()
	stage.Type = run.StageTypeDeploy
	rr.mu.Unlock()

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a deploy-stage shorthand claim:\n%s", w.Code, w.Body.String())
	}
	code, details := claimsErrorEnvelope(t, w.Body.Bytes())
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", code)
	}
	if details["stage_type"] != string(run.StageTypeDeploy) {
		t.Errorf("details.stage_type = %v, want deploy", details["stage_type"])
	}
	assertNoApprovalRecorded(t, ar, au)
}

// TestApprove_ClaimsAllOpenPlanConcerns_NilConcernRepoReturns503: the
// expansion cannot be computed without a concern store, so it fails CLOSED
// rather than silently claiming nothing.
func TestApprove_ClaimsAllOpenPlanConcerns_NilConcernRepoReturns503(t *testing.T) {
	// newApprovalServer wires NO ConcernRepo.
	s, ar, rr, au := newApprovalServer(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no concern store:\n%s", w.Code, w.Body.String())
	}
	code, _ := claimsErrorEnvelope(t, w.Body.Bytes())
	if code != "concern_store_unconfigured" {
		t.Errorf("error code = %q, want concern_store_unconfigured", code)
	}
	assertNoApprovalRecorded(t, ar, au)
}

// TestApprove_ClaimsAllOpenPlanConcerns_ListErrorReturns500: a store OUTAGE is
// retryable, not an operator-corrected input, so it is a 500 internal_error —
// the same posture validateClaimsConcernIDs takes for a non-ErrNotFound
// GetByIDs failure. Collapsing it into a 400 would point the operator at
// re-reading concern ids instead of retrying.
func TestApprove_ClaimsAllOpenPlanConcerns_ListErrorReturns500(t *testing.T) {
	s, ar, rr, au, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	cr.listErr = errors.New("concern store down")

	w := submitApproval(t, s, stage.ID, `{"decision":"approve","claims_all_open_plan_concerns":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on a concern-store read failure:\n%s", w.Code, w.Body.String())
	}
	code, _ := claimsErrorEnvelope(t, w.Body.Bytes())
	if code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", code)
	}
	assertNoApprovalRecorded(t, ar, au)
}
