package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// scopeTestServer returns a Server with nil repos — sufficient for
// scope guard paths, which fire before any repo access.
func scopeTestServer() *Server {
	return New(Config{Addr: "127.0.0.1:0"})
}

func injectIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
}

func mcpReadIdentity() Identity {
	return Identity{
		Subject: "mcp:run:" + uuid.New().String(),
		TokenID: "tok-test",
		Scopes:  []string{"mcp:read"},
	}
}

func anonIdentity() Identity {
	return Identity{}
}

// assertScopeError checks the response code and that the body
// contains the expected error code string inside the error envelope.
func assertScopeError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Errorf("status: got %d, want %d (body: %s)", w.Code, wantStatus, w.Body.String())
		return
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Errorf("parse body: %v", err)
		return
	}
	if env.Error.Code != wantCode {
		t.Errorf("error code: got %q, want %q", env.Error.Code, wantCode)
	}
}

func TestCreateRun_InsufficientScope(t *testing.T) {
	s := scopeTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{}"))
	req = injectIdentity(req, mcpReadIdentity())
	w := httptest.NewRecorder()
	s.handleCreateRun(w, req)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestCreateRun_Unauthenticated(t *testing.T) {
	s := scopeTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader("{}"))
	req = injectIdentity(req, anonIdentity())
	w := httptest.NewRecorder()
	s.handleCreateRun(w, req)
	assertScopeError(t, w, http.StatusUnauthorized, "authentication_required")
}

func TestCancelRun_InsufficientScope(t *testing.T) {
	s := scopeTestServer()
	runID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/cancel", nil)
	req.SetPathValue("run_id", runID.String())
	req = injectIdentity(req, mcpReadIdentity())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, req)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestCancelRun_Unauthenticated(t *testing.T) {
	s := scopeTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/cancel", nil)
	req = injectIdentity(req, anonIdentity())
	w := httptest.NewRecorder()
	s.handleCancelRun(w, req)
	assertScopeError(t, w, http.StatusUnauthorized, "authentication_required")
}

func TestRetryStage_InsufficientScope(t *testing.T) {
	s := scopeTestServer()
	stageID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stageID.String()+"/retry", nil)
	req.SetPathValue("stage_id", stageID.String())
	req = injectIdentity(req, mcpReadIdentity())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, req)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestRetryStage_Unauthenticated(t *testing.T) {
	s := scopeTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/retry", nil)
	req = injectIdentity(req, anonIdentity())
	w := httptest.NewRecorder()
	s.handleRetryStage(w, req)
	assertScopeError(t, w, http.StatusUnauthorized, "authentication_required")
}

func TestSubmitApproval_InsufficientScope(t *testing.T) {
	s := scopeTestServer()
	stageID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", strings.NewReader(`{"decision":"approve"}`))
	req.SetPathValue("stage_id", stageID.String())
	req = injectIdentity(req, mcpReadIdentity())
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, req)
	assertScopeError(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestSubmitApproval_Unauthenticated(t *testing.T) {
	s := scopeTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/approvals", strings.NewReader(`{"decision":"approve"}`))
	req = injectIdentity(req, anonIdentity())
	w := httptest.NewRecorder()
	s.handleSubmitApproval(w, req)
	assertScopeError(t, w, http.StatusUnauthorized, "authentication_required")
}
