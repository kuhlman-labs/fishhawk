package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// fakeScopeAmendmentRepo is an in-memory scopeamendment.Repository
// for handler tests. Mirrors the production postgres semantics the
// handlers rely on: status-blind CountByStage, pending-only Decide.
type fakeScopeAmendmentRepo struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]*scopeamendment.Amendment
	order []uuid.UUID
}

func newFakeScopeAmendmentRepo() *fakeScopeAmendmentRepo {
	return &fakeScopeAmendmentRepo{rows: map[uuid.UUID]*scopeamendment.Amendment{}}
}

func (f *fakeScopeAmendmentRepo) Create(_ context.Context, p scopeamendment.CreateParams) (*scopeamendment.Amendment, error) {
	paths, err := scopeamendment.ValidatePaths(p.Paths)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &scopeamendment.Amendment{
		ID:          uuid.New(),
		RunID:       p.RunID,
		StageID:     p.StageID,
		Paths:       paths,
		Reason:      p.Reason,
		Status:      scopeamendment.StatusPending,
		RequestedAt: time.Now().UTC(),
	}
	f.rows[a.ID] = a
	f.order = append(f.order, a.ID)
	return a, nil
}

func (f *fakeScopeAmendmentRepo) GetByID(_ context.Context, id uuid.UUID) (*scopeamendment.Amendment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.rows[id]
	if !ok {
		return nil, scopeamendment.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeScopeAmendmentRepo) ListByRun(_ context.Context, runID uuid.UUID) ([]*scopeamendment.Amendment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*scopeamendment.Amendment
	for _, id := range f.order {
		if a := f.rows[id]; a.RunID == runID {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeScopeAmendmentRepo) CountByStage(_ context.Context, stageID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.rows {
		if a.StageID == stageID {
			n++
		}
	}
	return n, nil
}

func (f *fakeScopeAmendmentRepo) Decide(_ context.Context, p scopeamendment.DecideParams) (*scopeamendment.Amendment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.rows[p.ID]
	if !ok {
		return nil, scopeamendment.ErrNotFound
	}
	if a.Status != scopeamendment.StatusPending {
		return nil, scopeamendment.ErrAlreadyDecided
	}
	now := time.Now().UTC()
	reason := p.Reason
	decidedBy := p.DecidedBy
	a.Status = p.Status
	a.DecisionReason = &reason
	a.DecidedBy = &decidedBy
	a.DecidedAt = &now
	cp := *a
	return &cp, nil
}

var _ scopeamendment.Repository = (*fakeScopeAmendmentRepo)(nil)

// scopeAmendmentServer wires a server with a run + an executing
// implement stage and the fakes the three handlers need.
func scopeAmendmentServer(t *testing.T) (*Server, *orchestratorRepo, *fakeScopeAmendmentRepo, *auditCapture, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 2, run.StageStateRunning)
	stage.Type = run.StageTypeImplement
	sa := newFakeScopeAmendmentRepo()
	au := &auditCapture{}
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: sa,
		AuditRepo:          au,
	})
	return s, rr, sa, au, runRow, stage
}

// withRunBoundIdentity injects an MCP run-bound identity with the
// given scopes into req's context.
func withRunBoundIdentity(req *http.Request, runID uuid.UUID, scopes ...string) *http.Request {
	id := Identity{
		Subject: "mcp:run:" + runID.String(),
		TokenID: "tok-mcp-test",
		Scopes:  scopes,
	}
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

// withOperatorIdentity injects an operator (non-run-bound) token
// identity with the given scopes.
func withOperatorIdentity(req *http.Request, scopes ...string) *http.Request {
	id := Identity{
		Subject: "github:operator",
		TokenID: "tok-operator-test",
		Scopes:  scopes,
	}
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

func postAmendment(t *testing.T, s *Server, pathRunID uuid.UUID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID.String()+"/scope-amendments", strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleRequestScopeAmendment(w, req)
	return w
}

func getAmendments(t *testing.T, s *Server, pathRunID uuid.UUID, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+pathRunID.String()+"/scope-amendments", nil)
	req.SetPathValue("run_id", pathRunID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleListScopeAmendments(w, req)
	return w
}

func postDecision(t *testing.T, s *Server, pathRunID, amendmentID uuid.UUID, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+pathRunID.String()+"/scope-amendments/"+amendmentID.String()+"/decision",
		strings.NewReader(body))
	req.SetPathValue("run_id", pathRunID.String())
	req.SetPathValue("amendment_id", amendmentID.String())
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	s.handleDecideScopeAmendment(w, req)
	return w
}

const validAmendmentBody = `{"paths":[{"path":"backend/internal/server/extra.go","operation":"modify"},{"path":"docs/new.md","operation":"create"}],"reason":"the seam needs these"}`

// --- POST /scope-amendments ---

func TestRequestScopeAmendment_HappyPath(t *testing.T) {
	s, _, sa, au, runRow, stage := scopeAmendmentServer(t)

	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "pending" || resp.StageID != stage.ID || len(resp.Paths) != 2 {
		t.Errorf("response = %+v", resp)
	}
	if n, _ := sa.CountByStage(context.Background(), stage.ID); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryScopeAmendmentRequested {
		t.Fatalf("audit = %+v", au.appended)
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if payload["amendment_id"] != resp.ID.String() {
		t.Errorf("audit amendment_id = %v", payload["amendment_id"])
	}
	if payload["remaining_budget"] != float64(1) {
		t.Errorf("remaining_budget = %v, want 1", payload["remaining_budget"])
	}
	if au.appended[0].StageID == nil || *au.appended[0].StageID != stage.ID {
		t.Errorf("audit stage_id = %v, want %s", au.appended[0].StageID, stage.ID)
	}
}

func TestRequestScopeAmendment_BudgetExhausted422(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	agent := func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	}
	for i := 0; i < maxScopeAmendmentsPerStage; i++ {
		if w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent); w.Code != http.StatusCreated {
			t.Fatalf("request %d status = %d; body = %s", i+1, w.Code, w.Body.String())
		}
	}
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amendment_budget_exhausted") {
		t.Errorf("body missing amendment_budget_exhausted: %s", w.Body.String())
	}
}

func TestRequestScopeAmendment_BudgetCountsDeniedRows(t *testing.T) {
	s, _, sa, _, runRow, _ := scopeAmendmentServer(t)
	agent := func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	}
	for i := 0; i < maxScopeAmendmentsPerStage; i++ {
		if w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent); w.Code != http.StatusCreated {
			t.Fatalf("request %d status = %d", i+1, w.Code)
		}
	}
	// Deny every prior request — the third POST must still be 422:
	// the cap bounds operator interruptions, not approvals.
	items, _ := sa.ListByRun(context.Background(), runRow.ID)
	for _, a := range items {
		if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
			ID: a.ID, Status: scopeamendment.StatusDenied, Reason: "no", DecidedBy: "github:operator",
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 regardless of prior decisions", w.Code)
	}
}

func TestRequestScopeAmendment_CrossRun403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, uuid.New(), "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_scope_amendment") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestRequestScopeAmendment_OperatorToken403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "write:stages")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent_token_required") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestRequestScopeAmendment_MissingScope403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "write:scope-amendments") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestRequestScopeAmendment_Anonymous401(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequestScopeAmendment_NoExecutingImplementStage409(t *testing.T) {
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	// Plan stage running; no implement stage executing.
	rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: newFakeScopeAmendmentRepo(),
		AuditRepo:          &auditCapture{},
	})
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_implement") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestRequestScopeAmendment_InvalidPaths400(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	agent := func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	}
	cases := []string{
		`{"paths":[{"path":"../escape.go","operation":"modify"}],"reason":"r"}`,
		`{"paths":[{"path":"/abs.go","operation":"modify"}],"reason":"r"}`,
		`{"paths":[{"path":"ok.go","operation":"delete"}],"reason":"r"}`,
		`{"paths":[],"reason":"r"}`,
		`{"paths":[{"path":"ok.go","operation":"modify"}],"reason":"  "}`,
	}
	for _, body := range cases {
		if w := postAmendment(t, s, runRow.ID, body, agent); w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestRequestScopeAmendment_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postAmendment(t, s, uuid.New(), validAmendmentBody, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- GET /scope-amendments ---

func TestListScopeAmendments_RunBoundToken200(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	agent := func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	}
	if w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent); w.Code != http.StatusCreated {
		t.Fatalf("seed POST failed: %d", w.Code)
	}
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "pending" {
		t.Errorf("items = %+v", resp.Items)
	}
}

func TestListScopeAmendments_CrossRun403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, uuid.New(), "mcp:read")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_scope_amendment") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestListScopeAmendments_RunBoundMissingScope403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "write:scope-amendments")
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestListScopeAmendments_Operator200(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "read:runs")
	})
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for operator token; body = %s", w.Code, w.Body.String())
	}
}

func TestListScopeAmendments_Anonymous401(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	w := getAmendments(t, s, runRow.ID, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// --- POST /scope-amendments/{id}/decision ---

// seedPendingAmendment files one amendment via the handler so the
// audit + row plumbing matches production.
func seedPendingAmendment(t *testing.T, s *Server, runID uuid.UUID) uuid.UUID {
	t.Helper()
	w := postAmendment(t, s, runID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("seed POST status = %d; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.ID
}

func TestDecideScopeAmendment_ApproveHappyPath(t *testing.T) {
	s, _, sa, au, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)

	w := postDecision(t, s, runRow.ID, amendmentID,
		`{"decision":"approve","reason":"the seam is real"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") })
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "approved" || resp.DecidedBy == nil || *resp.DecidedBy != "github:operator" {
		t.Errorf("response = %+v", resp)
	}
	got, _ := sa.GetByID(context.Background(), amendmentID)
	if got.Status != scopeamendment.StatusApproved {
		t.Errorf("persisted status = %q", got.Status)
	}
	// Second audit entry is the decision.
	if len(au.appended) != 2 || au.appended[1].Category != CategoryScopeAmendmentDecided {
		t.Fatalf("audit = %+v", au.appended)
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[1].Payload, &payload)
	if payload["decision"] != "approve" || payload["decided_by"] != "github:operator" {
		t.Errorf("decided payload = %v", payload)
	}
}

func TestDecideScopeAmendment_RunBoundToken403SelfDecision(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)

	// Even the requesting run's own token — holding every agent
	// scope — may not decide.
	w := postDecision(t, s, runRow.ID, amendmentID, `{"decision":"approve","reason":"r"}`,
		func(r *http.Request) *http.Request {
			return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments", "write:retries")
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "self_decision") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestDecideScopeAmendment_MissingScope403(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)
	w := postDecision(t, s, runRow.ID, amendmentID, `{"decision":"approve","reason":"r"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "read:runs") })
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestDecideScopeAmendment_AlreadyDecided409(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)
	operator := func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") }

	if w := postDecision(t, s, runRow.ID, amendmentID, `{"decision":"deny","reason":"r"}`, operator); w.Code != http.StatusOK {
		t.Fatalf("first decision status = %d", w.Code)
	}
	w := postDecision(t, s, runRow.ID, amendmentID, `{"decision":"approve","reason":"flip"}`, operator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amendment_already_decided") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestDecideScopeAmendment_WrongRun404(t *testing.T) {
	s, rr, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)
	otherRun := rr.seedRun()

	w := postDecision(t, s, otherRun.ID, amendmentID, `{"decision":"approve","reason":"r"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") })
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

func TestDecideScopeAmendment_BadDecision400(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)
	w := postDecision(t, s, runRow.ID, amendmentID, `{"decision":"maybe","reason":"r"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") })
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Cross-boundary backend half (#961 activation path) ---
//
// Drives operator decision → persistence → GET list response and pins
// the canonical wire JSON the runner's mid-stage refresh decodes. The
// runner half (main_test.go) serves this same shape from an httptest
// backend; together they pin the seam (#618 cross-boundary test rule).
func TestScopeAmendment_EndToEnd_DecisionThenList(t *testing.T) {
	s, _, _, _, runRow, stage := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)

	if w := postDecision(t, s, runRow.ID, amendmentID,
		`{"decision":"approve","reason":"folding the seam file"}`,
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") }); w.Code != http.StatusOK {
		t.Fatalf("decision status = %d", w.Code)
	}

	// The agent's poll loop reads the decision back with the same
	// run-bound token it used to request.
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	item := resp.Items[0]
	if item.Status != "approved" || item.StageID != stage.ID || item.DecidedAt == nil {
		t.Errorf("item = %+v", item)
	}
	if item.DecisionReason == nil || *item.DecisionReason != "folding the seam file" {
		t.Errorf("decision_reason = %v", item.DecisionReason)
	}
	if len(item.Paths) != 2 || item.Paths[1].Operation != "create" {
		t.Errorf("paths = %+v", item.Paths)
	}
}
