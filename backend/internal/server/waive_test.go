package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// waiveServer wires the audit + concern fakes the waive handler needs
// (no RunRepo — the handler resolves everything from the concern row).
func waiveServer(t *testing.T) (*Server, *auditFake, *fakeConcernRepo) {
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

func postWaive(t *testing.T, s *Server, concernID string, body waiveConcernRequest) *httptest.ResponseRecorder {
	t.Helper()
	return postWaiveAs(t, s, concernID, body, withAuth)
}

// concernWaivedEntries returns the appended concern_waived params.
func auditEntriesByCategory(au *auditFake, category string) []int {
	au.mu.Lock()
	defer au.mu.Unlock()
	var idx []int
	for i, e := range au.appended {
		if e.Category == category {
			idx = append(idx, i)
		}
	}
	return idx
}

func TestWaiveConcern_HappyPath_RaisedToWaived(t *testing.T) {
	s, au, cr := waiveServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "out-of-scope edit")

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted trade-off"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp waiveConcernResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.State != string(concern.StateWaived) {
		t.Errorf("resp.State = %q, want waived", resp.State)
	}
	if resp.StateReason != "accepted trade-off" {
		t.Errorf("resp.StateReason = %q, want the operator reason", resp.StateReason)
	}
	if resp.ID != row.ID || resp.RunID != runID || resp.StageID != stageID {
		t.Errorf("resp identifiers = %s/%s/%s, want %s/%s/%s",
			resp.ID, resp.RunID, resp.StageID, row.ID, runID, stageID)
	}

	// The store transitioned.
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateWaived {
		t.Errorf("stored state = %q, want waived", rows[0].State)
	}

	// Exactly one concern_waived audit entry on the concern's run/stage,
	// payload carrying concern_id, prior_state, and the reason.
	waived := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(waived) != 1 {
		t.Fatalf("concern_waived entries = %d, want 1", len(waived))
	}
	au.mu.Lock()
	entry := au.appended[waived[0]]
	au.mu.Unlock()
	if entry.RunID != runID || entry.StageID == nil || *entry.StageID != stageID {
		t.Errorf("audit entry run/stage = %v/%v, want %s/%s", entry.RunID, entry.StageID, runID, stageID)
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["concern_id"] != row.ID.String() {
		t.Errorf("payload concern_id = %v, want %s", payload["concern_id"], row.ID)
	}
	if payload["prior_state"] != string(concern.StateRaised) {
		t.Errorf("payload prior_state = %v, want raised", payload["prior_state"])
	}
	if payload["reason"] != "accepted trade-off" {
		t.Errorf("payload reason = %v, want the operator reason", payload["reason"])
	}
	// No corrective entry on the happy path.
	if n := len(auditEntriesByCategory(au, CategoryConcernWaiveFailed)); n != 0 {
		t.Errorf("concern_waive_failed entries = %d, want 0", n)
	}
}

func TestWaiveConcern_AddressedPendingToWaived(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed"); err != nil {
		t.Fatalf("seed addressed_pending: %v", err)
	}

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "fix-up superfluous after all"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateWaived {
		t.Errorf("stored state = %q, want waived", rows[0].State)
	}
}

func TestWaiveConcern_ReopenedToWaived(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed"); err != nil {
		t.Fatalf("seed addressed_pending: %v", err)
	}
	if _, err := cr.ApplyResolution(context.Background(), row.ID, concern.StateReopened, "not fixed"); err != nil {
		t.Fatalf("seed reopened: %v", err)
	}

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "won't fix this round"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateWaived {
		t.Errorf("stored state = %q, want waived", rows[0].State)
	}
}

func TestWaiveConcern_EmptyReasonReturns400(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	for _, reason := range []string{"", "   "} {
		w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: reason})
		if w.Code != http.StatusBadRequest {
			t.Errorf("reason %q: status = %d, want 400:\n%s", reason, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "validation_failed") {
			t.Errorf("reason %q: body missing validation_failed: %s", reason, w.Body.String())
		}
	}
	// No state change, no audit entry.
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("stored state = %q, want raised (unchanged)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
}

func TestWaiveConcern_MalformedUUIDReturns400(t *testing.T) {
	s, _, _ := waiveServer(t)
	w := postWaive(t, s, "not-a-uuid", waiveConcernRequest{Reason: "r"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestWaiveConcern_UnknownIDReturns404(t *testing.T) {
	s, _, _ := waiveServer(t)
	w := postWaive(t, s, uuid.NewString(), waiveConcernRequest{Reason: "r"})
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_not_found") {
		t.Errorf("body missing concern_not_found: %s", w.Body.String())
	}
}

// TestWaiveConcern_DoubleWaiveReturns422WithCorrectiveEntry is the
// approval-condition interleaving test: the second waive's intent entry
// appends (durable-record-first), the transition then fails on the
// already-waived state, a corrective concern_waive_failed entry lands
// naming the actual state, and the request returns 422 — the chain
// shows intent + corrective outcome, never a silent mutation.
func TestWaiveConcern_DoubleWaiveReturns422WithCorrectiveEntry(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	if w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "first"}); w.Code != http.StatusOK {
		t.Fatalf("first waive status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "second"})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second waive status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_waive_conflict") {
		t.Errorf("body missing concern_waive_conflict: %s", w.Body.String())
	}
	// Two intent entries (both appended before their transitions) + one
	// corrective entry for the failed second transition.
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 2 {
		t.Errorf("concern_waived entries = %d, want 2", n)
	}
	failed := auditEntriesByCategory(au, CategoryConcernWaiveFailed)
	if len(failed) != 1 {
		t.Fatalf("concern_waive_failed entries = %d, want 1", len(failed))
	}
	au.mu.Lock()
	entry := au.appended[failed[0]]
	au.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode corrective payload: %v", err)
	}
	if payload["actual_state"] != string(concern.StateWaived) {
		t.Errorf("corrective actual_state = %v, want waived", payload["actual_state"])
	}
	// The reason on file stays the FIRST waive's.
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].StateReason != "first" {
		t.Errorf("state_reason = %q, want the first waive's reason", rows[0].StateReason)
	}
}

// TestWaiveConcern_AuditAppendFailureReturns500NoMutation is approval-
// condition test (a): when the concern_waived intent entry cannot be
// appended, the request fails 500 audit_append_failed and the concern
// state is UNCHANGED — a mutation can never exist without a durable
// audit record.
func TestWaiveConcern_AuditAppendFailureReturns500NoMutation(t *testing.T) {
	s, au, cr := waiveServer(t)
	au.appendErr = errors.New("db down")
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "r"})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "audit_append_failed") {
		t.Errorf("body missing audit_append_failed: %s", w.Body.String())
	}
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("stored state = %q, want raised (no mutation without the audit record)", rows[0].State)
	}
}

func TestWaiveConcern_UnauthenticatedReturns401(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r", Judgement: "no_defect"})
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", row.ID.String())
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, req) // no identity injected → anonymous

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

func TestWaiveConcern_MissingScopeReturns403(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r", Judgement: "no_defect"})
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", row.ID.String())
	id := Identity{Subject: "token:reader", TokenID: "tok-read", Scopes: []string{"read:runs"}}
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id)))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
}

func TestWaiveConcern_MCPTokenMismatchedRunReturns403(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")
	otherRunID := uuid.New() // does not match row.RunID

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r", Judgement: "no_defect"})
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", row.ID.String())
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, withMCPFixupAuth(req, otherRunID))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_waive") {
		t.Errorf("body missing cross_run_waive: %s", w.Body.String())
	}
	// Guarded before the audit append: no intent entry, no mutation.
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("stored state = %q, want raised (unchanged)", rows[0].State)
	}
}

func TestWaiveConcern_MCPTokenOwnRunSucceeds(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "own-run waive", Judgement: "no_defect"})
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", row.ID.String())
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, withMCPFixupAuth(req, row.RunID))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

func TestWaiveConcern_NilConcernRepoReturns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()}) // no ConcernRepo
	w := postWaive(t, s, uuid.NewString(), waiveConcernRequest{Reason: "r"})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_store_unconfigured") {
		t.Errorf("body missing concern_store_unconfigured: %s", w.Body.String())
	}
}

// --- Delegated waive (ADR-040 / #1026) --------------------------------------

// delegatedWaiveServer adds the RunRepo the delegation evaluator
// needs on top of the waive handler's own audit + concern stores.
func delegatedWaiveServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   au,
		ConcernRepo: cr,
	})
	return s, repo, au, cr
}

// seedLowConcernRow inserts one open low-severity concern row — the
// solo_low shape (seedConcernRow hardcodes medium).
func seedLowConcernRow(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		ReviewerModel:        "claude-opus-4-8",
		OriginReviewSequence: 1,
		Concerns:             []concern.RaisedConcern{{Severity: "low", Category: "style", Note: "nit"}},
	})
	if err != nil {
		t.Fatalf("seed low concern: %v", err)
	}
	return rows[0]
}

// TestWaiveConcern_Delegated_SoloLowMet: with exactly one open concern
// of low severity, the delegated waive proceeds and the concern_waived
// payload records `delegated: "solo_low"`.
func TestWaiveConcern_Delegated_SoloLowMet(t *testing.T) {
	s, repo, au, cr := delegatedWaiveServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedLowConcernRow(t, cr, runID, stageID)
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "style nit, not blocking", Delegated: true})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if row.State != concern.StateWaived {
		t.Errorf("concern state = %q, want waived", row.State)
	}
	if rule := delegatedAuditRule(t, au, CategoryConcernWaived); rule != "solo_low" {
		t.Errorf("audit delegated = %q, want solo_low", rule)
	}
}

// TestWaiveConcern_Delegated_MediumUnmet: solo_low requires the single
// open concern to be LOW severity — a delegated waive of a medium
// concern is refused with the named predicate, appending no intent
// entry and mutating nothing.
func TestWaiveConcern_Delegated_MediumUnmet(t *testing.T) {
	s, repo, au, cr := delegatedWaiveServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 1, "out-of-scope edit")
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "want it gone", Delegated: true})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	errBody := decodeErrorEnvelope(t, w)
	reason, _ := errBody.Details["unmet_reason"].(string)
	if errBody.Code != "delegation_condition_unmet" ||
		!strings.Contains(reason, "severity is medium") {
		t.Errorf("error = %+v, want delegation_condition_unmet naming the severity", errBody)
	}
	if row.State != concern.StateRaised {
		t.Errorf("concern state = %q, want raised (no mutation on refusal)", row.State)
	}
	if idx := auditEntriesByCategory(au, CategoryConcernWaived); len(idx) != 0 {
		t.Errorf("concern_waived entries = %d after refusal, want 0 (refusal precedes the intent append)", len(idx))
	}
}

// TestWaiveConcern_Delegated_NotConfigured pins fail-closed: the plain
// waive server has no RunRepo wired, so a delegated waive cannot
// resolve any operator_agent block and refuses outright.
func TestWaiveConcern_Delegated_NotConfigured(t *testing.T) {
	s, _, cr := waiveServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedLowConcernRow(t, cr, runID, stageID)

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "nit", Delegated: true})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if errBody := decodeErrorEnvelope(t, w); errBody.Code != "delegation_not_configured" {
		t.Errorf("code = %q, want delegation_not_configured", errBody.Code)
	}
	if row.State != concern.StateRaised {
		t.Errorf("concern state = %q, want raised (no mutation on refusal)", row.State)
	}
}

// TestWaiveConcern_OperatorAgentActorAttribution: a waive recorded under
// an operator-agent token records actor_kind=agent with the full token
// subject on the concern_waived entry (ADR-040 D4, #1027).
func TestWaiveConcern_OperatorAgentActorAttribution(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "naming nit")

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "accepted trade-off", Judgement: "no_defect"})
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", row.ID.String())
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, withOperatorAgentAuth(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	waived := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(waived) != 1 {
		t.Fatalf("concern_waived entries = %d, want 1", len(waived))
	}
	au.mu.Lock()
	entry := au.appended[waived[0]]
	au.mu.Unlock()
	if entry.ActorKind == nil || *entry.ActorKind != audit.ActorAgent {
		t.Errorf("ActorKind = %v, want agent", entry.ActorKind)
	}
	if entry.ActorSubject == nil || *entry.ActorSubject != operatorAgentSubject {
		t.Errorf("ActorSubject = %v, want %q", entry.ActorSubject, operatorAgentSubject)
	}
}

// TestWaiveConcern_SinglePath_OmitsBulkWaiveMarker pins the byte-identical half
// of the #3318 applyConcernWaive extraction: the SINGLE waive path passes
// bulk=false, so its concern_waived payload must carry NO bulk_waive key. The
// ordering assertions above (audit-append-before-mutation, the corrective
// entry, the delegated rule) are the extraction's regression control and pass
// unchanged; this is the one NEW fact the shared helper introduces.
func TestWaiveConcern_SinglePath_OmitsBulkWaiveMarker(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "a nit")

	w := postWaive(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted trade-off"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	idx := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(idx) != 1 {
		t.Fatalf("concern_waived entries = %d, want 1", len(idx))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[idx[0]].Payload, &payload); err != nil {
		t.Fatalf("decode concern_waived payload: %v", err)
	}
	if _, present := payload["bulk_waive"]; present {
		t.Errorf("bulk_waive present on a SINGLE waive payload, want the key absent: %v", payload)
	}
	if payload["concern_id"] != row.ID.String() || payload["reason"] != "accepted trade-off" {
		t.Errorf("payload = %v, want the concern id + operator reason unchanged by the extraction", payload)
	}
}

// --- Human judgement (ADR-117 D1 phase 2, E94.7 / #4840) --------------------

// auditPayloadsByCategory decodes the RAW payload bytes of every appended entry
// of category, so a test can tell an absent key from an empty one.
func auditPayloadsByCategory(t *testing.T, au *auditFake, category string) []map[string]any {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []map[string]any
	for _, e := range au.appended {
		if e.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v\n%s", category, err, e.Payload)
		}
		out = append(out, m)
	}
	return out
}

// assertJudgementPayload asserts one decoded audit payload's judgement keys:
// `judgement` equals judgement, and `tradeoff_cost` equals cost when cost is
// non-empty and is ABSENT (not an empty string) otherwise.
func assertJudgementPayload(t *testing.T, p map[string]any, judgement concern.Judgement, cost string) {
	t.Helper()
	if p["judgement"] != string(judgement) {
		t.Errorf("payload judgement = %v, want %q (payload %v)", p["judgement"], judgement, p)
	}
	got, has := p["tradeoff_cost"]
	switch {
	case cost == "" && has:
		t.Errorf("payload carries tradeoff_cost = %v; want the key ABSENT for judgement %q", got, judgement)
	case cost != "" && got != cost:
		t.Errorf("payload tradeoff_cost = %v, want %q", got, cost)
	}
}

// assertJudgementRefused asserts the 400 validation_failed a refused judgement
// returns, with details naming field, rule and the allowed set, and returns the
// details. A wrong status is an Errorf so the caller's committed-state reads
// still run and a deleted guard reports what it let through.
func assertJudgementRefused(t *testing.T, w *httptest.ResponseRecorder, field, rule string) map[string]any {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 validation_failed field=%s rule=%s:\n%s", w.Code, field, rule, w.Body.String())
		return nil
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", code)
	}
	if details["field"] != field || details["rule"] != rule {
		t.Errorf("details = %v, want field=%s rule=%s", details, field, rule)
	}
	if allowed, _ := details["allowed"].([]any); len(allowed) != len(concern.HumanJudgements()) {
		t.Errorf("details.allowed = %v, want the %d human judgements", details["allowed"], len(concern.HumanJudgements()))
	}
	return details
}

// assertUnjudged reads each concern back AFTER the call returned and asserts it
// is still in state with no judgement, actor kind or outcome recorded.
func assertUnjudged(t *testing.T, cr *fakeConcernRepo, state concern.State, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		rows, err := cr.GetByIDs(context.Background(), []uuid.UUID{id})
		if err != nil {
			t.Fatalf("read back concern %s: %v", id, err)
		}
		row := rows[0]
		if row.State != state || concern.NormalizeJudgement(string(row.Judgement)) != concern.JudgementUnlabelled ||
			row.JudgementActorKind != "" || row.Outcome != "" || row.TradeoffCost != "" {
			t.Errorf("concern %s committed state %s judgement %q actor %q outcome %q cost %q; want %s and unjudged",
				id, row.State, row.Judgement, row.JudgementActorKind, row.Outcome, row.TradeoffCost, state)
		}
	}
}

// judgedRow is the judgement record a human verb commits on a concern row.
type judgedRow struct {
	State        concern.State
	Outcome      concern.Outcome
	Judgement    concern.Judgement
	ActorKind    string
	Delegated    bool
	Bulk         bool
	TradeoffCost string
}

// assertJudgedRow reads the concern back AFTER the call returned and compares
// its committed judgement record; judgement_backfill must read false.
func assertJudgedRow(t *testing.T, cr concern.Repository, id uuid.UUID, want judgedRow) {
	t.Helper()
	rows, err := cr.GetByIDs(context.Background(), []uuid.UUID{id})
	if err != nil {
		t.Fatalf("read back concern %s: %v", id, err)
	}
	row := rows[0]
	got := judgedRow{
		State: row.State, Outcome: row.Outcome, Judgement: row.Judgement, ActorKind: row.JudgementActorKind,
		Delegated: row.JudgementDelegated, Bulk: row.JudgementBulk, TradeoffCost: row.TradeoffCost,
	}
	if got != want {
		t.Errorf("concern %s committed %+v, want %+v", id, got, want)
	}
	if row.JudgementBackfill {
		t.Errorf("concern %s judgement_backfill = true, want false on a human judgement", id)
	}
}

// assertNoAuditRows asserts no entry of any of the categories was appended.
func assertNoAuditRows(t *testing.T, au *auditFake, categories ...string) {
	t.Helper()
	for _, c := range categories {
		if n := len(auditEntriesByCategory(au, c)); n != 0 {
			t.Errorf("%s entries = %d, want 0 (a refused judgement appends nothing)", c, n)
		}
	}
}

// TestWaiveConcern_RefusesMissingJudgement (CF1): a HUMAN waive with an
// unprefixed reason and NO judgement is refused 400 validation_failed
// field=judgement BEFORE the intent entry. With the guard deleted the handler
// appends the intent row and the store refuses the transition (500 plus a
// corrective row), so the 400, the untouched row and the zero rows all fail.
func TestWaiveConcern_RefusesMissingJudgement(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "naming nit")

	w := postWaiveRaw(t, s, row.ID.String(), `{"reason":"not blocking this merge"}`, withAuth)
	assertJudgementRefused(t, w, "judgement", concern.JudgementRuleRequired)
	assertUnjudged(t, cr, concern.StateRaised, row.ID)
	assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
}

// TestWaiveConcern_RefusesTradeoffWithoutCost (CF5): a tradeoff judgement with
// a blank or whitespace-only tradeoff_cost is refused 400 field=tradeoff_cost.
func TestWaiveConcern_RefusesTradeoffWithoutCost(t *testing.T) {
	for name, body := range map[string]string{
		"absent cost":     `{"reason":"slower cold start accepted","judgement":"tradeoff"}`,
		"whitespace cost": `{"reason":"slower cold start accepted","judgement":"tradeoff","tradeoff_cost":" \t "}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, au, cr := waiveServer(t)
			row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "cold start")
			w := postWaiveRaw(t, s, row.ID.String(), body, withAuth)
			assertJudgementRefused(t, w, "tradeoff_cost", concern.JudgementRuleTradeoffCostRequired)
			assertUnjudged(t, cr, concern.StateRaised, row.ID)
			assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
		})
	}
}

// TestWaiveConcern_RefusesTradeoffCostWithoutTradeoff (approval condition T1,
// an INFERRED criterion): a tradeoff_cost sent with any judgement other than
// tradeoff is refused, never accepted and dropped.
func TestWaiveConcern_RefusesTradeoffCostWithoutTradeoff(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "naming nit")
	w := postWaiveRaw(t, s, row.ID.String(), `{"reason":"not a defect","judgement":"no_defect","tradeoff_cost":"none really"}`, withAuth)
	assertJudgementRefused(t, w, "tradeoff_cost", concern.JudgementRuleTradeoffCostWithoutTradeoff)
	assertUnjudged(t, cr, concern.StateRaised, row.ID)
	assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
}

// TestWaiveConcern_RefusesUnlabelledAndUnknownJudgement: unlabelled (the read
// of an unjudged row) and any value outside the closed set (matched exactly, so
// `No_Defect` is unknown) are refused for THAT reason — also beside a
// recognised reason prefix, which is checked only after the value is valid.
func TestWaiveConcern_RefusesUnlabelledAndUnknownJudgement(t *testing.T) {
	for _, tc := range []struct{ name, body, rule string }{
		{"unlabelled", `{"reason":"r","judgement":"unlabelled"}`, concern.JudgementRuleUnlabelled},
		{"unknown", `{"reason":"r","judgement":"bogus"}`, concern.JudgementRuleUnknown},
		{"wrong case", `{"reason":"r","judgement":"No_Defect"}`, concern.JudgementRuleUnknown},
		{"unknown beside a prefix", `{"reason":"false_positive: upstream guard","judgement":"bogus"}`, concern.JudgementRuleUnknown},
		{"unrecognised prefix case", `{"reason":"False_Positive: upstream guard"}`, concern.JudgementRuleRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := waiveServer(t)
			row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "nit")
			w := postWaiveRaw(t, s, row.ID.String(), tc.body, withAuth)
			assertJudgementRefused(t, w, "judgement", tc.rule)
			assertUnjudged(t, cr, concern.StateRaised, row.ID)
			assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
		})
	}
}

// TestWaiveConcern_ReasonPrefixJudgementRules pins FO ruling D1's accepting
// rules on the committed row: (a) an explicit judgement with a MATCHING prefix,
// (b) an explicit judgement with no prefix, (c) a prefix alone supplying the
// judgement. A prefix-derived tradeoff still needs a stated cost: none is
// inferred from the reason.
func TestWaiveConcern_ReasonPrefixJudgementRules(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       concern.Judgement
		cost       string
		refuseRule string // non-empty: the body is refused with this rule
	}{
		{name: "(a) matching prefix", body: `{"reason":"false_positive: the guard is upstream","judgement":"no_defect"}`, want: concern.JudgementNoDefect},
		{name: "(a) matching tradeoff prefix", body: `{"reason":"accepted_tradeoff: cold start","judgement":"tradeoff","tradeoff_cost":"200ms"}`, want: concern.JudgementTradeoff, cost: "200ms"},
		{name: "(b) no prefix", body: `{"reason":"real but out of scope","judgement":"defect"}`, want: concern.JudgementDefect},
		{name: "(c) false_positive:", body: `{"reason":"false_positive: the guard is upstream"}`, want: concern.JudgementNoDefect},
		{name: "(c) leading whitespace", body: `{"reason":"  false_positive: the guard is upstream"}`, want: concern.JudgementNoDefect},
		{name: "(c) accepted_tradeoff:", body: `{"reason":"accepted_tradeoff: cold start","tradeoff_cost":"200ms"}`, want: concern.JudgementTradeoff, cost: "200ms"},
		{name: "(c) preference:", body: `{"reason":"preference: naming"}`, want: concern.JudgementPreference},
		{name: "(c) accepted_tradeoff: without a cost", body: `{"reason":"accepted_tradeoff: cold start"}`, refuseRule: concern.JudgementRuleTradeoffCostRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := waiveServer(t)
			row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "nit")
			w := postWaiveRaw(t, s, row.ID.String(), tc.body, withAuth)
			if tc.refuseRule != "" {
				assertJudgementRefused(t, w, "tradeoff_cost", tc.refuseRule)
				assertUnjudged(t, cr, concern.StateRaised, row.ID)
				assertNoAuditRows(t, au, CategoryConcernWaived)
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			assertJudgedRow(t, cr, row.ID, judgedRow{
				State: concern.StateWaived, Outcome: concern.OutcomeWaived, Judgement: tc.want,
				ActorKind: string(audit.ActorUser), TradeoffCost: tc.cost,
			})
			payloads := auditPayloadsByCategory(t, au, CategoryConcernWaived)
			if len(payloads) != 1 {
				t.Fatalf("concern_waived entries = %d, want 1", len(payloads))
			}
			assertJudgementPayload(t, payloads[0], tc.want, tc.cost)
		})
	}
}

// TestWaiveConcern_RefusesContradictingReasonPrefix (CF10, FO ruling D1 rule
// (d), an INFERRED criterion): an explicit judgement contradicting the reason
// prefix is refused 400 judgement_conflicts_reason_prefix, naming BOTH values,
// with nothing written. HumanDisposition.Validate takes no reason, so no
// downstream guard masks a deleted conflict check: the waive would commit.
func TestWaiveConcern_RefusesContradictingReasonPrefix(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "nil deref")
	w := postWaiveRaw(t, s, row.ID.String(), `{"reason":"false_positive: the guard is upstream","judgement":"defect"}`, withAuth)
	details := assertJudgementRefused(t, w, "judgement", concern.JudgementRuleConflictsReasonPrefix)
	if details != nil {
		if details["reason_prefix"] != "false_positive:" {
			t.Errorf("details.reason_prefix = %v, want false_positive:", details["reason_prefix"])
		}
		env := decodeErrorEnvelope(t, w)
		if !strings.Contains(env.Message, `"defect"`) || !strings.Contains(env.Message, "false_positive:") {
			t.Errorf("message = %q, want it to name both the judgement \"defect\" and the prefix false_positive:", env.Message)
		}
	}
	assertUnjudged(t, cr, concern.StateRaised, row.ID)
	assertNoAuditRows(t, au, CategoryConcernWaived, CategoryConcernWaiveFailed)
}

// TestWaiveConcern_AuditCarriesJudgement: the concern_waived payload and the
// committed row carry the judgement, and tradeoff_cost only for a tradeoff (the
// key is ABSENT otherwise, decoded from the raw payload bytes).
func TestWaiveConcern_AuditCarriesJudgement(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       concern.Judgement
		cost       string
	}{
		{"defect", `{"reason":"real, tracked separately","judgement":"defect_pre_existing"}`, concern.JudgementDefectPreExisting, ""},
		{"tradeoff", `{"reason":"cold start accepted","judgement":"tradeoff","tradeoff_cost":"200ms p99"}`, concern.JudgementTradeoff, "200ms p99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := waiveServer(t)
			row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "nit")
			if w := postWaiveRaw(t, s, row.ID.String(), tc.body, withAuth); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			payloads := auditPayloadsByCategory(t, au, CategoryConcernWaived)
			if len(payloads) != 1 {
				t.Fatalf("concern_waived entries = %d, want 1", len(payloads))
			}
			assertJudgementPayload(t, payloads[0], tc.want, tc.cost)
			assertJudgedRow(t, cr, row.ID, judgedRow{
				State: concern.StateWaived, Outcome: concern.OutcomeWaived, Judgement: tc.want,
				ActorKind: string(audit.ActorUser), TradeoffCost: tc.cost,
			})
		})
	}
}

// TestWaiveConcern_RecordsJudgementActorKind: the committed row's actor kind is
// the user|agent kind the concern_waived row carries — user for a human token,
// agent for an operator-agent token — and a single waive records
// judgement_bulk false.
func TestWaiveConcern_RecordsJudgementActorKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth func(*http.Request) *http.Request
		want audit.ActorKind
	}{
		{"human token", withAuth, audit.ActorUser},
		{"operator-agent token", withOperatorAgentAuth, audit.ActorAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, cr := waiveServer(t)
			row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 1, "nit")
			if w := postWaiveRaw(t, s, row.ID.String(), `{"reason":"style only","judgement":"preference"}`, tc.auth); w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			assertJudgedRow(t, cr, row.ID, judgedRow{
				State: concern.StateWaived, Outcome: concern.OutcomeWaived, Judgement: concern.JudgementPreference,
				ActorKind: string(tc.want),
			})
		})
	}
}

// TestWaiveConcern_Delegated_RecordsJudgementDelegated: a waive the solo_low
// delegation admitted records judgement_delegated true on the committed row.
func TestWaiveConcern_Delegated_RecordsJudgementDelegated(t *testing.T) {
	s, repo, _, cr := delegatedWaiveServer(t)
	runID, stageID := uuid.New(), uuid.New()
	row := seedLowConcernRow(t, cr, runID, stageID)
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})
	w := postWaiveRaw(t, s, row.ID.String(), `{"reason":"style nit","judgement":"preference","delegated":true}`, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertJudgedRow(t, cr, row.ID, judgedRow{
		State: concern.StateWaived, Outcome: concern.OutcomeWaived, Judgement: concern.JudgementPreference,
		ActorKind: string(audit.ActorUser), Delegated: true,
	})
}
