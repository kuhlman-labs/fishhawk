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
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+concernID+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", concernID)
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, withAuth(req))
	return w
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

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r"})
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

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r"})
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

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "r"})
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

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "own-run waive"})
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

	raw, _ := json.Marshal(waiveConcernRequest{Reason: "accepted trade-off"})
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
