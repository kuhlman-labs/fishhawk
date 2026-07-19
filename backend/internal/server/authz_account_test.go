package server

// authz_account_test.go — the cross-boundary integration suite for the
// account-ownership authorization middleware (ADR-057 / E44.5, #1829). It
// drives the require{Run,Stage,Concern}Account wrappers and the list/export
// account-scoping end-to-end over fake repositories, one assertion per mode.
//
// The wrapper cases invoke the wrapper directly with an injected Identity and
// a SetPathValue-populated request — the same seam the mux hands the wrapper
// after routing — so the run/stage/concern resolution + enforceAccount logic is
// exercised without standing up the full bearerAuth chain.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const (
	authzAcctA = "aaaaaaaa-0000-0000-0000-000000000001"
	authzAcctB = "bbbbbbbb-0000-0000-0000-000000000002"
)

// bearerID is a bearer/token identity (TokenID set → role-bounding skipped).
func bearerID(acct string) Identity {
	return Identity{Subject: "github:op", TokenID: "tok-1", AccountID: acct}
}

// mcpID is an mcp:run identity (TokenID set → role-bounding skipped).
func mcpID(acct string) Identity {
	return Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "mtok-1", AccountID: acct}
}

// cookieID is a resolved OAuth cookie identity (SessionID set, TokenID empty →
// role-bounding fires on write tiers).
func cookieID(acct string) Identity {
	return Identity{Subject: "github:op", UserID: "u-1", SessionID: "s-1", AccountID: acct}
}

func authzReqPath(t *testing.T, id Identity, key, val string) (*httptest.ResponseRecorder, *http.Request, *bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	req.SetPathValue(key, val)
	reached := false
	return rec, req, &reached
}

func nextReached(reached *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}
}

func authzServer(fr *fakeRepo, role string, concerns concern.Repository) *Server {
	return New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fr,
		AccountRoles: fakeAccountRoles{role: role},
		ConcernRepo:  concerns,
	})
}

// seedRuns returns a fakeRepo with one run per bucket: account A, account B,
// untenanted.
func seedAuthzRuns() (fr *fakeRepo, runA, runB, runU *run.Run) {
	fr = newFakeRepo()
	now := time.Now().UTC()
	runA = &run.Run{ID: uuid.New(), Repo: "acme/app", WorkflowID: "feature_change", AccountID: authzAcctA, State: run.StatePending, CreatedAt: now}
	runB = &run.Run{ID: uuid.New(), Repo: "acme/app", WorkflowID: "feature_change", AccountID: authzAcctB, State: run.StatePending, CreatedAt: now.Add(-time.Minute)}
	runU = &run.Run{ID: uuid.New(), Repo: "acme/app", WorkflowID: "feature_change", State: run.StatePending, CreatedAt: now.Add(-2 * time.Minute)}
	fr.runs[runA.ID] = runA
	fr.runs[runB.ID] = runB
	fr.runs[runU.ID] = runU
	return fr, runA, runB, runU
}

func assertForbidden(t *testing.T, rec *httptest.ResponseRecorder, reached *bool, code string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), code) {
		t.Errorf("body = %s, want %s", rec.Body.String(), code)
	}
	if *reached {
		t.Error("handler ran despite a 403 denial")
	}
}

func assertAllowed(t *testing.T, rec *httptest.ResponseRecorder, reached *bool) {
	t.Helper()
	if !*reached {
		t.Fatalf("handler did not run (denied); status %d body %s", rec.Code, rec.Body.String())
	}
}

// --- Run wrapper: ownership (all tiers) ---

func TestRequireRunAccount_BearerCrossAccount_Forbidden(t *testing.T) {
	fr, _, runB, _ := seedAuthzRuns()
	s := authzServer(fr, account.RoleAdmin, nil)
	rec, req, reached := authzReqPath(t, bearerID(authzAcctA), "run_id", runB.ID.String())
	s.requireRunAccount(readAccess, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "account_forbidden")
}

func TestRequireRunAccount_UntenantedRun_Allowed(t *testing.T) {
	fr, _, _, runU := seedAuthzRuns()
	s := authzServer(fr, account.RoleAdmin, nil)
	rec, req, reached := authzReqPath(t, bearerID(authzAcctA), "run_id", runU.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

func TestRequireRunAccount_MCPTokenOwnAccount(t *testing.T) {
	fr, runA, runB, _ := seedAuthzRuns()
	s := authzServer(fr, account.RoleAdmin, nil)

	// mcp token bound to account A: allowed on A's run.
	rec, req, reached := authzReqPath(t, mcpID(authzAcctA), "run_id", runA.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)

	// Same token: forbidden on B's run.
	rec, req, reached = authzReqPath(t, mcpID(authzAcctA), "run_id", runB.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "account_forbidden")
}

// --- Run wrapper: cookie role-bounding (write tiers only) ---

func TestRequireRunAccount_CookieEmptyAccount_WriteUnresolved(t *testing.T) {
	fr, _, _, runU := seedAuthzRuns()
	s := authzServer(fr, account.RoleAdmin, nil)
	// Empty-AccountID resolved cookie on a write tier → account_unresolved,
	// even on an untenanted run (ownership passes; role-bounding trips).
	rec, req, reached := authzReqPath(t, cookieID(""), "run_id", runU.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "account_unresolved")
}

func TestRequireRunAccount_CookieMemberOnAdminWrite_InsufficientRole(t *testing.T) {
	fr, runA, _, _ := seedAuthzRuns()
	s := authzServer(fr, account.RoleMember, nil)
	rec, req, reached := authzReqPath(t, cookieID(authzAcctA), "run_id", runA.ID.String())
	s.requireRunAccount(adminWrite, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "insufficient_role")
}

func TestRequireRunAccount_CookieAdminOnAdminWrite_Allowed(t *testing.T) {
	fr, runA, _, _ := seedAuthzRuns()
	s := authzServer(fr, account.RoleAdmin, nil)
	rec, req, reached := authzReqPath(t, cookieID(authzAcctA), "run_id", runA.ID.String())
	s.requireRunAccount(adminWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

func TestRequireRunAccount_CookieMemberOnMemberWrite_Allowed(t *testing.T) {
	fr, runA, _, _ := seedAuthzRuns()
	s := authzServer(fr, account.RoleMember, nil)
	rec, req, reached := authzReqPath(t, cookieID(authzAcctA), "run_id", runA.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

func TestRequireRunAccount_RoleResolutionError_503(t *testing.T) {
	fr, runA, _, _ := seedAuthzRuns()
	// AccountRoles returns an error → the write-tier role resolution fails
	// closed with 503 rather than silently granting or denying.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: fr,
		AccountRoles: fakeAccountRoles{err: context.DeadlineExceeded}})
	rec, req, reached := authzReqPath(t, cookieID(authzAcctA), "run_id", runA.ID.String())
	s.requireRunAccount(memberWrite, nextReached(reached))(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "service_unavailable") {
		t.Errorf("body = %s, want service_unavailable", rec.Body.String())
	}
	if *reached {
		t.Error("handler ran despite a 503 role-resolution failure")
	}
}

func TestRequireRunAccount_CookieReadUntenanted_Allowed(t *testing.T) {
	fr, _, _, runU := seedAuthzRuns()
	// Member role, empty account cookie, READ tier: role-bounding never fires
	// on reads, ownership passes on an untenanted run → allowed.
	s := authzServer(fr, account.RoleMember, nil)
	rec, req, reached := authzReqPath(t, cookieID(""), "run_id", runU.ID.String())
	s.requireRunAccount(readAccess, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

// --- Stage wrapper: stage_id -> stage.RunID -> run -> account ---

func TestRequireStageAccount_ResolvesCorrectRun(t *testing.T) {
	fr, _, runB, _ := seedAuthzRuns()
	stageID := uuid.New()
	fr.stagesByRun[runB.ID] = []*run.Stage{{ID: stageID, RunID: runB.ID}}
	s := authzServer(fr, account.RoleAdmin, nil)

	// Caller A on a stage of B's run → forbidden (stage wrapper resolved B).
	rec, req, reached := authzReqPath(t, bearerID(authzAcctA), "stage_id", stageID.String())
	s.requireStageAccount(memberWrite, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "account_forbidden")

	// Caller B on the same stage → allowed.
	rec, req, reached = authzReqPath(t, bearerID(authzAcctB), "stage_id", stageID.String())
	s.requireStageAccount(memberWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

// --- Concern wrapper: concern_id -> concern.RunID -> run -> account ---

func TestRequireConcernAccount_ResolvesCorrectRun(t *testing.T) {
	fr, _, runB, _ := seedAuthzRuns()
	concernID := uuid.New()
	cr := newFakeConcernRepo()
	cr.rows = []*concern.Concern{{ID: concernID, RunID: runB.ID}}
	s := authzServer(fr, account.RoleAdmin, cr)

	// Caller A on a concern of B's run → forbidden (concern wrapper resolved B).
	rec, req, reached := authzReqPath(t, bearerID(authzAcctA), "concern_id", concernID.String())
	s.requireConcernAccount(memberWrite, nextReached(reached))(rec, req)
	assertForbidden(t, rec, reached, "account_forbidden")

	// Caller B on the same concern → allowed.
	rec, req, reached = authzReqPath(t, bearerID(authzAcctB), "concern_id", concernID.String())
	s.requireConcernAccount(memberWrite, nextReached(reached))(rec, req)
	assertAllowed(t, rec, reached)
}

// TestHandler_WrapsRunRoute_EnforcesAccount proves the route-REGISTRATION
// wiring: a request routed through the full mux (Handler()) — not a direct
// wrapper call — is subject to the account wrapper. A bearer token bound to
// account A hitting a run owned by B is 403 account_forbidden.
func TestHandler_WrapsRunRoute_EnforcesAccount(t *testing.T) {
	fr, _, runB, _ := seedAuthzRuns()
	repo := &stubAPITokenRepo{tok: &apitoken.Token{
		ID:        uuid.New(),
		Subject:   "github:op",
		AccountID: authzAcctA,
		PlainText: "fhk_account_a_token",
	}}
	s := New(Config{RunRepo: fr, APITokenRepo: repo})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runB.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+repo.tok.PlainText)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "account_forbidden") {
		t.Errorf("body = %s, want account_forbidden", rec.Body.String())
	}
}

// --- List filtering (handleListRuns) ---

func TestHandleListRuns_ExcludesOtherAccount(t *testing.T) {
	fr, runA, runB, runU := seedAuthzRuns()
	s := New(Config{RunRepo: fr})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, bearerID(authzAcctA)))
	s.handleListRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, it := range body.Items {
		got[it.ID] = true
	}
	if !got[runA.ID] || !got[runU.ID] {
		t.Errorf("account A listing must include runA + untenanted; got %v", got)
	}
	if got[runB.ID] {
		t.Errorf("account A listing must EXCLUDE account B's run")
	}
}

// --- Export filtering (handleAuditExport) ---

func authzExportServer(fr *fakeRepo, af *exportAuditFake) *Server {
	return New(Config{RunRepo: fr, AuditRepo: af, SigningRepo: &exportSigningFake{}})
}

func prAuditEntry(runID uuid.UUID) *audit.Entry {
	rid := runID
	return &audit.Entry{
		ID:        uuid.New(),
		Sequence:  1,
		RunID:     &rid,
		Timestamp: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		Category:  "pull_request_opened",
		Payload:   json.RawMessage(`{"pr_url":"https://github.com/acme/app/pull/1","pr_number":1}`),
		EntryHash: "hash-" + runID.String(),
	}
}

func authzExportReq(t *testing.T, path string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	id := Identity{Subject: "github:op", TokenID: "tok-export", Scopes: []string{scopeAuditExport}, AccountID: authzAcctA}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	return rec, req
}

func TestHandleAuditExport_ExcludesOtherAccount(t *testing.T) {
	fr, runA, runB, runU := seedAuthzRuns()
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		runA.ID: {prAuditEntry(runA.ID)},
		runB.ID: {prAuditEntry(runB.ID)},
		runU.ID: {prAuditEntry(runU.ID)},
	}}
	s := authzExportServer(fr, af)

	rec, req := authzExportReq(t, "/v0/audit/export?include_global=false")
	s.handleAuditExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Runs map[string]json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body.Runs[runA.ID.String()]; !ok {
		t.Errorf("export must include account A's run")
	}
	if _, ok := body.Runs[runU.ID.String()]; !ok {
		t.Errorf("export must keep untenanted run visible")
	}
	if _, ok := body.Runs[runB.ID.String()]; ok {
		t.Errorf("export must EXCLUDE account B's run")
	}
}

// --- Report filtering (handleAgentChangesReport JSON + .md) — binding condition ---

func TestHandleAgentChangesReport_ExcludesOtherAccount(t *testing.T) {
	fr, runA, runB, runU := seedAuthzRuns()
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		runA.ID: {prAuditEntry(runA.ID)},
		runB.ID: {prAuditEntry(runB.ID)},
		runU.ID: {prAuditEntry(runU.ID)},
	}}
	s := authzExportServer(fr, af)

	// JSON report.
	rec, req := authzExportReq(t, "/v0/reports/agent-changes")
	s.handleAgentChangesReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("json status = %d; body %s", rec.Code, rec.Body.String())
	}
	jsonBody := rec.Body.String()
	if !strings.Contains(jsonBody, runA.ID.String()) || !strings.Contains(jsonBody, runU.ID.String()) {
		t.Errorf("json report must include account A + untenanted runs")
	}
	if strings.Contains(jsonBody, runB.ID.String()) {
		t.Errorf("json report must EXCLUDE account B's run")
	}

	// Markdown report.
	recMD, reqMD := authzExportReq(t, "/v0/reports/agent-changes.md")
	s.handleAgentChangesReportMarkdown(recMD, reqMD)
	if recMD.Code != http.StatusOK {
		t.Fatalf("md status = %d; body %s", recMD.Code, recMD.Body.String())
	}
	mdBody := recMD.Body.String()
	if !strings.Contains(mdBody, runA.ID.String()) || !strings.Contains(mdBody, runU.ID.String()) {
		t.Errorf("md report must include account A + untenanted runs")
	}
	if strings.Contains(mdBody, runB.ID.String()) {
		t.Errorf("md report must EXCLUDE account B's run")
	}
}
