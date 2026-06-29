package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gateaction_extract_test.go is owned solely by E25.6 slice 1. It proves the
// gate-action extraction (approveStageAs / fixupStageAs / retryStageAs) is
// behaviour-preserving by driving the SAME gate action through (a) the HTTP
// handler and (b) the extracted identity-driven service method and asserting
// identical audit entries — AND that the in-process campaign operator Identity
// passes the handler scope-check identically to an HTTP token
// (scope-acceptance parity, binding approval condition 2). The per-handler
// tests (approvals_test.go / fixup_test.go / retry_test.go) stay byte-identical
// and still pass; this file adds the cross-path equivalence those tests cannot
// express on their own.

// normalizeGateAuditPayload unmarshals a captured audit payload and drops the
// stage-instance identifier so two entries written for different stages can be
// compared field-for-field. The stage_id is the only payload field that
// legitimately differs between the handler stage and the service stage.
func normalizeGateAuditPayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	delete(m, "stage_id")
	return m
}

// assertAuditEntriesEquivalent asserts the handler-written and service-written
// audit captures are the same single entry modulo the per-stage identifiers:
// same category, same actor kind/subject, same payload (minus stage_id).
func assertAuditEntriesEquivalent(t *testing.T, handler, service *approvalAuditFake) {
	t.Helper()
	if len(handler.appended) != 1 {
		t.Fatalf("handler path wrote %d audit entries, want exactly 1", len(handler.appended))
	}
	if len(service.appended) != 1 {
		t.Fatalf("service path wrote %d audit entries, want exactly 1", len(service.appended))
	}
	h, s := handler.appended[0], service.appended[0]

	if h.Category != s.Category {
		t.Errorf("audit category mismatch: handler %q vs service %q", h.Category, s.Category)
	}
	switch {
	case h.ActorKind == nil || s.ActorKind == nil:
		t.Errorf("actor kind nil: handler=%v service=%v", h.ActorKind, s.ActorKind)
	case *h.ActorKind != *s.ActorKind:
		t.Errorf("actor kind mismatch: handler %q vs service %q", *h.ActorKind, *s.ActorKind)
	}
	switch {
	case h.ActorSubject == nil || s.ActorSubject == nil:
		t.Errorf("actor subject nil: handler=%v service=%v", h.ActorSubject, s.ActorSubject)
	case *h.ActorSubject != *s.ActorSubject:
		t.Errorf("actor subject mismatch: handler %q vs service %q", *h.ActorSubject, *s.ActorSubject)
	}
	hp := normalizeGateAuditPayload(t, h.Payload)
	sp := normalizeGateAuditPayload(t, s.Payload)
	if !reflect.DeepEqual(hp, sp) {
		t.Errorf("audit payload mismatch:\n handler = %#v\n service = %#v", hp, sp)
	}
}

// TestGateActionExtract_Approve_HandlerVsService drives a plan-stage approve
// through the HTTP handler and through approveStageAs under the SAME campaign
// operator identity, and asserts the resulting approval_submitted audit entry
// is identical (modulo stage id). It ALSO asserts the handler accepted the
// in-process operator identity at its scope gate (200, not 401/403) — the
// scope-acceptance half of binding condition 2 exercised at the real entry.
func TestGateActionExtract_Approve_HandlerVsService(t *testing.T) {
	op := campaignOperatorIdentity()

	// (a) HTTP handler path.
	sh, _, rrh, auh := newApprovalServer(t)
	stageA := rrh.seedStage(run.StageStateAwaitingApproval)
	w := submitApprovalWithIdentity(t, sh, stageA.ID, &op, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("handler approve under operator identity: status = %d, want 200 (scope accepted):\n%s", w.Code, w.Body.String())
	}

	// (b) Extracted service-method path.
	ss, _, rrs, aus := newApprovalServer(t)
	stageB := rrs.seedStage(run.StageStateAwaitingApproval)
	res, err := ss.approveStageAs(context.Background(), op, approveActionParams{
		Stage:    stageB,
		Decision: "approve",
	})
	if err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if res.Duplicate != nil {
		t.Fatalf("approveStageAs returned a duplicate result on a fresh stage")
	}
	if res.Stage == nil || res.Stage.State != run.StageStateSucceeded {
		t.Fatalf("approveStageAs advanced to %v, want succeeded", res.Stage)
	}

	assertAuditEntriesEquivalent(t, auh, aus)

	// Both paths recorded exactly one approval and one stage transition.
	if len(rrh.transitions) != 1 || len(rrs.transitions) != 1 {
		t.Errorf("transitions: handler=%d service=%d, want 1 each", len(rrh.transitions), len(rrs.transitions))
	}
	if rrh.transitions[0].To != rrs.transitions[0].To {
		t.Errorf("transition target mismatch: handler %q vs service %q", rrh.transitions[0].To, rrs.transitions[0].To)
	}
}

// TestGateActionExtract_Retry_HandlerVsService drives a category-A retry
// through the HTTP handler and through retryStageAs under the same campaign
// operator identity and asserts the stage_retried audit entries match (modulo
// stage id). This covers the retry leg of the extraction.
func TestGateActionExtract_Retry_HandlerVsService(t *testing.T) {
	op := campaignOperatorIdentity()

	// (a) HTTP handler path.
	sh, rrh, auh := retryServer(t)
	stageA := seedFailedStage(rrh, run.FailureA, "agent crashed mid-run")
	reqA := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stageA.ID.String()+"/retry", nil)
	reqA.SetPathValue("stage_id", stageA.ID.String())
	reqA = reqA.WithContext(context.WithValue(reqA.Context(), ctxKeyIdentity, op))
	wA := httptest.NewRecorder()
	sh.handleRetryStage(wA, reqA)
	if wA.Code != http.StatusOK {
		t.Fatalf("handler retry under operator identity: status = %d, want 200:\n%s", wA.Code, wA.Body.String())
	}

	// (b) Extracted service-method path.
	ss, rrs, aus := retryServer(t)
	stageB := seedFailedStage(rrs, run.FailureA, "agent crashed mid-run")
	stageOut, err := ss.retryStageAs(context.Background(), op, retryActionParams{StageID: stageB.ID})
	if err != nil {
		t.Fatalf("retryStageAs: %v", err)
	}
	if stageOut == nil || stageOut.State != run.StageStatePending {
		t.Fatalf("retryStageAs re-opened to %v, want pending", stageOut)
	}

	assertAuditEntriesEquivalent(t, auh, aus)
}

// TestGateActionExtract_ScopeAcceptanceParity is binding approval condition 2:
// the in-process operator Identity passes the handler scope-check IDENTICALLY
// to an HTTP token carrying the same scopes — not only audit parity. It also
// proves the check is genuinely RUN for the operator identity (TokenID
// non-empty, so the cookie-session bypass does not apply): an operator-shaped
// identity missing the scope is rejected.
func TestGateActionExtract_ScopeAcceptanceParity(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	op := campaignOperatorIdentity()
	httpToken := Identity{
		Subject: "github:operator",
		TokenID: "tok-http-equivalent",
		Scopes:  operatorrole.CampaignActorScopes(),
	}

	// requireWriteScope is the approve gate's scope-check. The operator
	// identity and an equivalently-scoped HTTP token must both be accepted.
	for _, scope := range operatorrole.CampaignActorScopes() {
		if !scopeCheckAccepts(s, op, scope) {
			t.Errorf("requireWriteScope rejected the in-process operator identity for %q; want accepted (parity)", scope)
		}
		if !scopeCheckAccepts(s, httpToken, scope) {
			t.Errorf("requireWriteScope rejected the HTTP token for %q; want accepted", scope)
		}
	}

	// The fixup/retry handlers gate on hasScope inline rather than
	// requireWriteScope; assert the operator identity satisfies those too.
	for _, scope := range []string{"write:stages", "write:fixups", "write:retries"} {
		if !hasScope(op, scope) {
			t.Errorf("operator identity is missing %q; the fixup/retry inline scope checks would reject it", scope)
		}
	}

	// The check actually runs (not bypassed): an operator-shaped identity
	// without the scope is rejected with a 403.
	missing := Identity{Subject: op.Subject, TokenID: op.TokenID, Scopes: []string{"read:runs"}}
	if scopeCheckAccepts(s, missing, "write:approvals") {
		t.Error("requireWriteScope accepted an operator-shaped identity missing write:approvals; the scope check is being bypassed")
	}
}

// TestGateActionExtract_ScopeEnforcedInServiceMethods is the authz half of
// the extraction (#1445): the in-process service methods MUST enforce the same
// write scope their HTTP handlers do, because the campaign auto-driver reaches
// them WITHOUT passing through requireWriteScope / the fixup-retry inline
// check. An identity missing the gate's write scope is rejected with a
// gateActionScopeError before any repository mutation; the campaign operator
// identity (which carries every gate scope) clears all three guards.
func TestGateActionExtract_ScopeEnforcedInServiceMethods(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	underScoped := Identity{Subject: "github:x", TokenID: "tok", Scopes: []string{"read:runs"}}

	var scopeErr *gateActionScopeError
	if _, err := s.approveStageAs(context.Background(), underScoped, approveActionParams{}); !errors.As(err, &scopeErr) {
		t.Errorf("approveStageAs under an identity missing write:approvals: err = %v, want gateActionScopeError", err)
	}
	if _, err := s.fixupStageAs(context.Background(), underScoped, fixupActionParams{}); !errors.As(err, &scopeErr) {
		t.Errorf("fixupStageAs under an identity missing write:stages/write:fixups: err = %v, want gateActionScopeError", err)
	}
	if _, err := s.retryStageAs(context.Background(), underScoped, retryActionParams{}); !errors.As(err, &scopeErr) {
		t.Errorf("retryStageAs under an identity missing write:stages/write:retries: err = %v, want gateActionScopeError", err)
	}

	// The campaign operator identity carries all gate scopes, so none of the
	// guards reject it (parity with an equivalently-scoped HTTP token).
	op := campaignOperatorIdentity()
	if !identityHasGateScope(op, "write:approvals") ||
		!identityHasGateScope(op, "write:stages", "write:fixups") ||
		!identityHasGateScope(op, "write:stages", "write:retries") {
		t.Error("campaign operator identity fails a gate scope guard; the auto-driver would be wrongly rejected")
	}
}

// scopeCheckAccepts reports whether requireWriteScope admits id for scope,
// driving it through a request context exactly as the handlers do.
func scopeCheckAccepts(s *Server, id Identity, scope string) bool {
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+uuid.New().String()+"/approvals", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	return s.requireWriteScope(w, req, scope)
}
