package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// postVouchCommit posts a vouch-commit request with the given identity
// mutator and JSON body.
func postVouchCommit(t *testing.T, s *Server, runID uuid.UUID, body vouchCommitRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return postVouchCommitRaw(t, s, runID, raw, withID)
}

// postVouchCommitRaw posts an arbitrary (possibly malformed) body so the
// decode-error path can be exercised.
func postVouchCommitRaw(t *testing.T, s *Server, runID uuid.UUID, raw []byte,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/vouch-commit", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleVouchCommit(w, withID(req))
	return w
}

// withVouchOperator injects an operator fhk_ token identity carrying
// write:stages — the only credential vouch accepts. The shared withAuth
// helper is a scope-less cookie session, which vouch rejects (the binding
// approval condition enforces write:stages UNCONDITIONALLY, with no
// cookie-session bypass), so vouch tests use this scoped injector instead.
func withVouchOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:stages"},
	}))
}

// seedVouchServer wires a server with a run + audit/run repos for the
// vouch handler, returning the server, audit fake, and the run ID.
func seedVouchServer(t *testing.T) (*Server, *auditFake, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement}
	s, _, au, _ := newLineageServer(t, nil, runRow, stage)
	return s, au, runID
}

// vouchAudit finds the operator_commit_vouched audit entry, if any.
func vouchAudit(au *auditFake) *audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		if au.appended[i].Category == CategoryOperatorCommitVouched {
			return &au.appended[i]
		}
	}
	return nil
}

const vouchedSHA = "abc1230000000000000000000000000000000000"

func TestVouchCommit_HappyPath(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.VouchedSHA != vouchedSHA {
		t.Errorf("vouched_sha = %q, want %q", resp.VouchedSHA, vouchedSHA)
	}
	if resp.RunID != runID.String() {
		t.Errorf("run_id = %q, want %q", resp.RunID, runID)
	}

	a := vouchAudit(au)
	if a == nil {
		t.Fatal("no operator_commit_vouched audit entry written")
	}
	if a.ActorKind == nil || *a.ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", a.ActorKind)
	}
	var payload struct {
		RunID      string `json:"run_id"`
		VouchedSHA string `json:"vouched_sha"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.VouchedSHA != vouchedSHA {
		t.Errorf("payload vouched_sha = %q, want %q", payload.VouchedSHA, vouchedSHA)
	}
	if payload.Reason != "sync-schemas remediation commit" {
		t.Errorf("payload reason = %q", payload.Reason)
	}
}

func TestVouchCommit_MissingScope(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withScopeless)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite missing scope")
	}
}

// TestVouchCommit_RunBoundTokenForbidden is the BINDING amendment guard: a
// run-bound mcp:run token is rejected outright (403 run_token_forbidden),
// even for its OWN run — vouching git lineage is an operator action, and an
// agent self-declaring lineage for a commit on its own branch would defeat
// the ADR-035 sole-writer invariant.
func TestVouchCommit_RunBoundTokenForbidden(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	// The run-bound token's subject IS this run's id (its own run), and it
	// even carries write:stages — it must STILL be rejected.
	withOwnRunToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + runID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:stages"},
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withOwnRunToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_token_forbidden")) {
		t.Errorf("body missing run_token_forbidden: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite run-bound token rejection")
	}
}

func TestVouchCommit_EmptySHA(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: "   ", Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty sha")
	}
}

func TestVouchCommit_EmptyReason(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "  "}, withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty reason")
	}
}

func TestVouchCommit_RunNotFound(t *testing.T) {
	s, au, _ := seedVouchServer(t)

	unknown := uuid.New()
	w := postVouchCommit(t, s, unknown, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_found")) {
		t.Errorf("body missing run_not_found: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written for an unknown run")
	}
}

// TestVouchCommit_EmptyTokenIDNoScope locks the high-severity fix: an
// authenticated identity with an EMPTY TokenID (the cookie-session shape)
// and no write:stages scope is rejected 403, NOT waved past the scope gate.
// Vouch enforces write:stages unconditionally — there is no cookie-session
// bypass, unlike the sibling reset-branch/waive/retry handlers.
func TestVouchCommit_EmptyTokenIDNoScope(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	withSessionNoScope := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", UserID: "u-1", SessionID: "s-1", // empty TokenID, empty Scopes
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withSessionNoScope)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty-TokenID identity without scope")
	}
}

// TestVouchCommit_Unconfigured covers the 503 branch: nil RunRepo/AuditRepo.
func TestVouchCommit_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no RunRepo / AuditRepo

	w := postVouchCommit(t, s, uuid.New(), vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("vouch_unconfigured")) {
		t.Errorf("body missing vouch_unconfigured: %s", w.Body.String())
	}
}

// TestVouchCommit_MalformedBody covers the decode-error branch: a body that
// is not valid JSON returns 400 validation_failed and writes no audit entry.
func TestVouchCommit_MalformedBody(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommitRaw(t, s, runID, []byte("{not json"), withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite malformed body")
	}
}
