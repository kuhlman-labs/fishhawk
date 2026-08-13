package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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
	// listCalls counts ListByRun invocations; when failListOn > 0 the
	// call whose 1-based index equals it returns a transient error, so a
	// test can fail a SPECIFIC poll re-list (e.g. the first ?wait ticker)
	// while the handler's initial list succeeds.
	listCalls  int
	failListOn int
	// createErr, when set, makes Create return it (after path validation), so a
	// test can drive the createApprovedScopeAmendment DB-failure branch in
	// recover.go (E67.15 / #2587). Zero value nil leaves Create unchanged, so
	// every existing test is unaffected.
	createErr error
}

func newFakeScopeAmendmentRepo() *fakeScopeAmendmentRepo {
	return &fakeScopeAmendmentRepo{rows: map[uuid.UUID]*scopeamendment.Amendment{}}
}

func (f *fakeScopeAmendmentRepo) Create(_ context.Context, p scopeamendment.CreateParams) (*scopeamendment.Amendment, error) {
	paths, err := scopeamendment.ValidatePaths(p.Paths)
	if err != nil {
		return nil, err
	}
	if f.createErr != nil {
		// A post-validation failure (a DB error), distinct from the path-validation
		// error above — the branch a recover.go DB-cause test drives.
		return nil, f.createErr
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
	f.listCalls++
	if f.failListOn > 0 && f.listCalls == f.failListOn {
		return nil, errors.New("transient list failure")
	}
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

// getAmendmentsWait drives the list handler with a ?wait=<seconds>
// query param so the #1035 long-poll path is exercised.
func getAmendmentsWait(t *testing.T, s *Server, pathRunID uuid.UUID, waitSeconds int, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+pathRunID.String()+"/scope-amendments?wait="+strconv.Itoa(waitSeconds), nil)
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

// implementSpec3600 is a feature_change workflow whose implement stage declares
// an explicit 60m executor timeout, so resolveAgentTimeout(implement) resolves a
// known 3600s budget (no plan artifact / calibration widens it) — the
// deadline-derivation handler tests seed the stage's started_at relative to it.
const implementSpec3600 = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          timeout: 60m
        produces:
          - artifact: pull_request
`

// scopeAmendmentServerWithBudget is scopeAmendmentServer with the run given a
// spec whose implement stage resolves a positive agent_timeout_seconds, so the
// #2540 deadline derivation runs instead of failing open on an absent spec.
func scopeAmendmentServerWithBudget(t *testing.T) (*Server, *fakeScopeAmendmentRepo, *auditCapture, *run.Run, *run.Stage) {
	t.Helper()
	s, _, sa, au, runRow, stage := scopeAmendmentServer(t)
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = []byte(implementSpec3600)
	return s, sa, au, runRow, stage
}

// deadlineFieldsAbsent asserts the response and audit payload carry NONE of the
// three #2540 deadline fields — the fail-open wire shape, byte-identical to
// pre-#2540. Returns the decoded response for further assertions.
func requireDeadlineFieldsAbsent(t *testing.T, w *httptest.ResponseRecorder, au *auditCapture) {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UndecidableBeforeDeadline {
		t.Errorf("undecidable_before_deadline = true, want false (fail open)")
	}
	if resp.StageDeadlineSecondsRemaining != nil {
		t.Errorf("stage_deadline_seconds_remaining = %v, want absent (fail open)", *resp.StageDeadlineSecondsRemaining)
	}
	if resp.AmendmentPollWindowSeconds != nil {
		t.Errorf("amendment_poll_window_seconds = %v, want absent (fail open)", *resp.AmendmentPollWindowSeconds)
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	// The base key set must be byte-identical to pre-#2540: the fishhawk_await_audit
	// anchor and page-class ping depend on it.
	wantKeys := map[string]bool{"amendment_id": true, "paths": true, "reason": true, "remaining_budget": true}
	for k := range payload {
		if !wantKeys[k] {
			t.Errorf("audit payload carries unexpected key %q (fail open must add none): %v", k, payload)
		}
	}
	for k := range wantKeys {
		if _, ok := payload[k]; !ok {
			t.Errorf("audit payload missing base key %q: %v", k, payload)
		}
	}
}

// TestAmendmentDeadlineRemaining is the pure-function table pinning the deadline
// derivation and — decisively (#2540 approval condition 1) — the FAIL-OPEN
// direction of every uncertainty case: an uncertain input must PARK (ok=false),
// never manufacture a refusal. It is the counterfactual vehicle for the
// selfRetryCount and non-positive-remainder clauses: deleting either fail-open
// clause makes its row return a positive remainder with ok=true and reddens.
func TestAmendmentDeadlineRemaining(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	ago := func(sec int) *time.Time { tt := now.Add(-time.Duration(sec) * time.Second); return &tt }
	for _, tc := range []struct {
		name          string
		budget        int
		startedAt     *time.Time
		selfRetry     int
		wantOK        bool
		wantRemaining int // only checked when wantOK
	}{
		// Decidable / undecidable both resolve ok=true — the flag is the caller's job.
		{name: "decidable remainder", budget: 3600, startedAt: ago(60), wantOK: true, wantRemaining: 3540},
		{name: "undecidable remainder", budget: 3600, startedAt: ago(3540), wantOK: true, wantRemaining: 60},
		// (a) unresolvable spec budget → fail open.
		{name: "fail-open (a) zero budget", budget: 0, startedAt: ago(60), wantOK: false},
		{name: "fail-open (a) negative budget", budget: -1, startedAt: ago(60), wantOK: false},
		// (b) never-started stage → fail open.
		{name: "fail-open (b) nil startedAt", budget: 3600, startedAt: nil, wantOK: false},
		// (c) non-positive remainder → fail open (a live agent posting proves the
		// deadline has not literally passed, so this is a bad derivation).
		{name: "fail-open (c) elapsed exceeds budget", budget: 3600, startedAt: ago(3700), wantOK: false},
		{name: "fail-open (c) elapsed equals budget", budget: 3600, startedAt: ago(3600), wantOK: false},
		// (d) self-retried stage → fail open EVEN with a remainder that would
		// otherwise trip the window; proves the clause is load-bearing.
		{name: "fail-open (d) self-retried undecidable-looking", budget: 3600, startedAt: ago(3540), selfRetry: 1, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotRemaining, gotOK := amendmentDeadlineRemaining(tc.budget, tc.startedAt, tc.selfRetry, now)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (remaining=%d)", gotOK, tc.wantOK, gotRemaining)
			}
			if tc.wantOK && gotRemaining != tc.wantRemaining {
				t.Errorf("remaining = %d, want %d", gotRemaining, tc.wantRemaining)
			}
		})
	}
}

// TestRequestScopeAmendment_UndecidableBeforeDeadline is the end-to-end
// counterfactual for the undecidability control (#2540): an implement stage
// whose started_at leaves ~60s of a 3600s budget — well inside the 900s poll
// window — files an amendment, and the REQUEST response + audit must carry
// undecidable_before_deadline:true with the two numbers. Deleting the
// `remaining < AmendmentPollWindowSeconds` branch turns the flag false and
// reddens this. The bad state is seeded BY CONSTRUCTION (a started_at literal
// computed from the resolved budget), never by calling the control in setup.
func TestRequestScopeAmendment_UndecidableBeforeDeadline(t *testing.T) {
	s, _, au, runRow, stage := scopeAmendmentServerWithBudget(t)
	budget := s.resolveAgentTimeout(context.Background(), runRow, run.StageTypeImplement)
	if budget <= AmendmentPollWindowSeconds {
		t.Fatalf("test precondition: resolved budget %d must exceed window %d", budget, AmendmentPollWindowSeconds)
	}
	started := time.Now().UTC().Add(-time.Duration(budget-60) * time.Second)
	stage.StartedAt = &started

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
	if !resp.UndecidableBeforeDeadline {
		t.Fatalf("undecidable_before_deadline = false, want true (remaining ~60 < window 900)")
	}
	if resp.AmendmentPollWindowSeconds == nil || *resp.AmendmentPollWindowSeconds != AmendmentPollWindowSeconds {
		t.Errorf("amendment_poll_window_seconds = %v, want %d", resp.AmendmentPollWindowSeconds, AmendmentPollWindowSeconds)
	}
	if resp.StageDeadlineSecondsRemaining == nil {
		t.Fatal("stage_deadline_seconds_remaining absent, want ~60")
	}
	if rem := *resp.StageDeadlineSecondsRemaining; rem < 30 || rem >= AmendmentPollWindowSeconds {
		t.Errorf("stage_deadline_seconds_remaining = %d, want ~60 (in [30,900))", rem)
	}
	// The same three keys ride the scope_amendment_requested audit payload.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	if payload["undecidable_before_deadline"] != true {
		t.Errorf("audit undecidable_before_deadline = %v, want true", payload["undecidable_before_deadline"])
	}
	if payload["amendment_poll_window_seconds"] != float64(AmendmentPollWindowSeconds) {
		t.Errorf("audit amendment_poll_window_seconds = %v, want %d", payload["amendment_poll_window_seconds"], AmendmentPollWindowSeconds)
	}
	if _, ok := payload["stage_deadline_seconds_remaining"]; !ok {
		t.Errorf("audit missing stage_deadline_seconds_remaining: %v", payload)
	}
}

// TestRequestScopeAmendment_DeadlineDecidable pins the decidable path (NOT a
// fail-open): a stage with ample budget left files an amendment, so the flag is
// false BUT the two numbers are still present, so the agent and operator can see
// the headroom on a fresh request.
func TestRequestScopeAmendment_DeadlineDecidable(t *testing.T) {
	s, _, _, runRow, stage := scopeAmendmentServerWithBudget(t)
	budget := s.resolveAgentTimeout(context.Background(), runRow, run.StageTypeImplement)
	if budget <= AmendmentPollWindowSeconds {
		t.Fatalf("test precondition: resolved budget %d must exceed window %d", budget, AmendmentPollWindowSeconds)
	}
	started := time.Now().UTC().Add(-60 * time.Second) // remaining ~ budget-60 >> window
	stage.StartedAt = &started

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
	if resp.UndecidableBeforeDeadline {
		t.Errorf("undecidable_before_deadline = true, want false (remaining >> window)")
	}
	if resp.StageDeadlineSecondsRemaining == nil || *resp.StageDeadlineSecondsRemaining < AmendmentPollWindowSeconds {
		t.Errorf("stage_deadline_seconds_remaining = %v, want present and >= window", resp.StageDeadlineSecondsRemaining)
	}
	if resp.AmendmentPollWindowSeconds == nil || *resp.AmendmentPollWindowSeconds != AmendmentPollWindowSeconds {
		t.Errorf("amendment_poll_window_seconds = %v, want %d", resp.AmendmentPollWindowSeconds, AmendmentPollWindowSeconds)
	}
}

// TestRequestScopeAmendment_DeadlineFailOpen drives each of the FOUR fail-open
// modes through the real handler and asserts the response omits all three
// deadline fields AND the audit key set stays exactly today's. Mode (d) is the
// end-to-end counterfactual for the selfRetryCount clause: with a remainder that
// WOULD trip the window, only the clause keeps it fail-open — delete the clause
// and this row reddens by carrying undecidable_before_deadline:true.
func TestRequestScopeAmendment_DeadlineFailOpen(t *testing.T) {
	agent := func(runID uuid.UUID) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request {
			return withRunBoundIdentity(r, runID, "mcp:read", "write:scope-amendments")
		}
	}
	t.Run("(a) unresolvable spec budget", func(t *testing.T) {
		s, _, au, runRow, stage := scopeAmendmentServerWithBudget(t)
		runRow.WorkflowSpec = []byte("this: is: not: valid: yaml: {[") // parse error → resolveAgentTimeout 0
		started := time.Now().UTC().Add(-60 * time.Second)
		stage.StartedAt = &started
		requireDeadlineFieldsAbsent(t, postAmendment(t, s, runRow.ID, validAmendmentBody, agent(runRow.ID)), au)
	})
	t.Run("(b) never-started stage", func(t *testing.T) {
		s, _, au, runRow, stage := scopeAmendmentServerWithBudget(t)
		stage.StartedAt = nil
		requireDeadlineFieldsAbsent(t, postAmendment(t, s, runRow.ID, validAmendmentBody, agent(runRow.ID)), au)
	})
	t.Run("(c) non-positive remainder", func(t *testing.T) {
		s, _, au, runRow, stage := scopeAmendmentServerWithBudget(t)
		budget := s.resolveAgentTimeout(context.Background(), runRow, run.StageTypeImplement)
		started := time.Now().UTC().Add(-time.Duration(budget+120) * time.Second) // elapsed > budget
		stage.StartedAt = &started
		requireDeadlineFieldsAbsent(t, postAmendment(t, s, runRow.ID, validAmendmentBody, agent(runRow.ID)), au)
	})
	t.Run("(d) self-retried stage with undecidable-looking remainder", func(t *testing.T) {
		s, _, au, runRow, stage := scopeAmendmentServerWithBudget(t)
		budget := s.resolveAgentTimeout(context.Background(), runRow, run.StageTypeImplement)
		started := time.Now().UTC().Add(-time.Duration(budget-60) * time.Second) // would be undecidable
		stage.StartedAt = &started
		stage.SelfRetryCount = 1 // but the self-retry clause fails open
		requireDeadlineFieldsAbsent(t, postAmendment(t, s, runRow.ID, validAmendmentBody, agent(runRow.ID)), au)
	})
}

// TestRequestScopeAmendment_FiresPageClassHook is one of the four
// binding-condition-#2 site assertions (#1786): the scope-amendment request
// handler invokes the pings-only immediate hook at its append site so the
// must_page_human request pages the operator within the request window
// rather than at the next transition.
func TestRequestScopeAmendment_FiresPageClassHook(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	rec := &pageClassRecorder{}
	s.issueNotifier = rec

	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if !rec.pagedRun(runRow.ID) {
		t.Errorf("scope-amendment site did not invoke the page-class hook; paged=%v", rec.pageClass)
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

// TestRequestScopeAmendment_PendingImplementChildRun_EndToEnd is the
// #1030 cross-boundary check: issue a REAL signed MCP token for a
// run whose ONLY stage is a PENDING implement stage (decomposition-
// child shape under a local runner), then POST the amendment through
// the full s.Handler() chain with that bearer. This crosses token
// issuance → bearer-auth middleware → amendment handler → repo — the
// seam the live 403 (run 8b0282a2) broke, where fixing only the
// token grant would still 409.
func TestRequestScopeAmendment_PendingImplementChildRun_EndToEnd(t *testing.T) {
	sf := newSigningFake()
	mt := newFakeMCPTokenRepo()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 1, run.StageStatePending)
	stage.Type = run.StageTypeImplement
	sa := newFakeScopeAmendmentRepo()
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		MCPTokenRepo:       mt,
		AuditRepo:          &auditCapture{},
		ScopeAmendmentRepo: sa,
	})
	priv, _ := sf.issue(t, runRow.ID)

	resp := signedMCPTokenRequest(t, s, runRow.ID, priv, []byte{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("issue status = %d; body = %s", resp.Code, resp.Body.String())
	}
	var issued mcpTokenResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runRow.ID.String()+"/scope-amendments",
		strings.NewReader(validAmendmentBody))
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var amResp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &amResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if amResp.StageID != stage.ID {
		t.Errorf("amendment stage_id = %s, want the pending implement stage %s", amResp.StageID, stage.ID)
	}
	if n, _ := sa.CountByStage(context.Background(), stage.ID); n != 1 {
		t.Errorf("rows on pending implement stage = %d, want 1", n)
	}
}

func TestRequestScopeAmendment_PlanFirstPendingRun409(t *testing.T) {
	// The #1030 fallback must not loosen the plan-stage gate: when the
	// run's first non-terminal stage is a pending PLAN stage (with the
	// implement stage pending behind it), the POST still 409s.
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	rr.seedStage(runRow.ID, 1, run.StageStatePending) // plan (seedStage default type)
	impl := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	impl.Type = run.StageTypeImplement
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

// --- GET ?wait long-poll (#1035) ---

// shortenScopeAmendmentPoll lowers the long-poll re-list interval for
// the duration of a test so the ?wait loop reacts in milliseconds, and
// restores it on cleanup.
func shortenScopeAmendmentPoll(t *testing.T) {
	t.Helper()
	prev := scopeAmendmentPollInterval
	scopeAmendmentPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { scopeAmendmentPollInterval = prev })
}

// TestListScopeAmendments_WaitReturnsOnDecision asserts the ?wait
// long-poll returns PROMPTLY (well before the cap) the moment a
// snapshotted-pending amendment is decided from another session.
func TestListScopeAmendments_WaitReturnsOnDecision(t *testing.T) {
	shortenScopeAmendmentPoll(t)
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	amendmentID := seedPendingAmendment(t, s, runRow.ID)

	// A second session decides ~30ms into the agent's wait.
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = postDecision(t, s, runRow.ID, amendmentID,
			`{"decision":"approve","reason":"in-window decision"}`,
			func(r *http.Request) *http.Request { return withOperatorIdentity(r, "write:stages") })
	}()

	start := time.Now()
	w := getAmendmentsWait(t, s, runRow.ID, 10, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Errorf("wait returned after %s — expected prompt return on decision, not at cap", elapsed)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "approved" {
		t.Errorf("items = %+v, want one approved", resp.Items)
	}
}

// TestListScopeAmendments_WaitReturnsAtCap asserts a ?wait elapses and
// returns the still-pending list when no decision lands in the window.
func TestListScopeAmendments_WaitReturnsAtCap(t *testing.T) {
	shortenScopeAmendmentPoll(t)
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	seedPendingAmendment(t, s, runRow.ID)

	start := time.Now()
	w := getAmendmentsWait(t, s, runRow.ID, 1, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("wait returned after %s — expected to hold ~1s to the cap", elapsed)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "pending" {
		t.Errorf("items = %+v, want one still-pending at the cap", resp.Items)
	}
}

// TestListScopeAmendments_NoWaitBackCompat asserts the absence of ?wait
// returns the single list immediately (unchanged behavior) even with a
// pending amendment outstanding.
func TestListScopeAmendments_NoWaitBackCompat(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	seedPendingAmendment(t, s, runRow.ID)

	start := time.Now()
	w := getAmendments(t, s, runRow.ID, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("no-wait GET took %s — must return immediately", elapsed)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "pending" {
		t.Errorf("items = %+v", resp.Items)
	}
}

// TestListScopeAmendments_WaitNoPendingImmediate asserts ?wait returns
// immediately when nothing is currently pending — there is nothing to
// await.
func TestListScopeAmendments_WaitNoPendingImmediate(t *testing.T) {
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)

	start := time.Now()
	w := getAmendmentsWait(t, s, runRow.ID, 30, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("wait with no pending took %s — must return immediately", elapsed)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("items = %+v, want empty", resp.Items)
	}
}

// TestParseScopeAmendmentWaitSeconds covers the additive edge semantics
// of the ?wait parser directly (the handler tests only exercise happy
// values): absent/empty/non-positive/non-integer all read as 0 (no wait,
// back-compat), a value above the cap clamps to maxScopeAmendmentWaitSeconds,
// and surrounding whitespace is trimmed. A regression here would silently
// change the documented back-compat or cap behavior.
func TestParseScopeAmendmentWaitSeconds(t *testing.T) {
	cases := []struct {
		name  string
		query string // raw query string; "" means the param is absent
		want  int
	}{
		{"absent", "", 0},
		{"empty-value", "wait=", 0},
		{"zero", "wait=0", 0},
		{"negative", "wait=-5", 0},
		{"non-integer", "wait=abc", 0},
		{"trimmed", "wait=%2010%20", 10}, // " 10 " → trimmed → 10
		{"happy", "wait=10", 10},
		{"at-cap", "wait=30", maxScopeAmendmentWaitSeconds},
		{"over-cap-clamped", "wait=999", maxScopeAmendmentWaitSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "/v0/runs/x/scope-amendments"
			if tc.query != "" {
				target += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if got := parseScopeAmendmentWaitSeconds(req); got != tc.want {
				t.Errorf("parseScopeAmendmentWaitSeconds(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

// TestListScopeAmendments_WaitClientDisconnect asserts the ?wait loop
// honors r.Context().Done() (binding condition 1): a canceled request
// context (client disconnect) releases the poll promptly — well before
// the cap — returning the last-good still-pending list.
func TestListScopeAmendments_WaitClientDisconnect(t *testing.T) {
	shortenScopeAmendmentPoll(t)
	s, _, _, _, runRow, _ := scopeAmendmentServer(t)
	seedPendingAmendment(t, s, runRow.ID)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+runRow.ID.String()+"/scope-amendments?wait=30", nil).WithContext(ctx)
	req.SetPathValue("run_id", runRow.ID.String())
	req = withRunBoundIdentity(req, runRow.ID, "mcp:read")
	w := httptest.NewRecorder()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.handleListScopeAmendments(w, req)
		close(done)
	}()
	// Simulate the client disconnecting mid-wait.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %s — expected prompt release on disconnect, not at cap", elapsed)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "pending" {
		t.Errorf("items = %+v, want one still-pending (last-good)", resp.Items)
	}
}

// TestListScopeAmendments_WaitTransientListError asserts the deliberate
// divergence from the non-wait GET: when a re-list inside the ?wait loop
// fails transiently, the loop returns the last-good list (200) rather than
// regressing to the non-wait 500. The handler's initial list succeeds and
// finds the pending amendment; the first ticker re-list errors.
func TestListScopeAmendments_WaitTransientListError(t *testing.T) {
	shortenScopeAmendmentPoll(t)
	s, _, sa, _, runRow, _ := scopeAmendmentServer(t)
	seedPendingAmendment(t, s, runRow.ID)
	sa.failListOn = 2 // call 1 = handler initial list; call 2 = first ticker

	start := time.Now()
	w := getAmendmentsWait(t, s, runRow.ID, 30, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read")
	})
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (transient re-list error returns last-good, not 500); body = %s",
			w.Code, w.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned after %s — expected prompt return on transient list error", elapsed)
	}
	var resp scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Status != "pending" {
		t.Errorf("items = %+v, want one still-pending (last-good)", resp.Items)
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

// --- Scope-cap headroom on amendment surfaces (#983) ---

// scopeAmendmentHeadroomServer mirrors scopeAmendmentServer but wires
// the ArtifactRepo + a cached workflow spec (implement-stage
// max_files_changed: 3 via specImplementPathConstraints) + a plan
// artifact with two scope files, so effectiveScopeHeadroom resolves
// instead of failing open.
func scopeAmendmentHeadroomServer(t *testing.T) (*Server, *fakeScopeAmendmentRepo, *auditCapture, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specImplementPathConstraints
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded)
	stage := rr.seedStage(runRow.ID, 2, run.StageStateRunning)
	stage.Type = run.StageTypeImplement
	sa := newFakeScopeAmendmentRepo()
	au := &auditCapture{}
	art := newFakeArtifactRepo()
	seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
		PlanVersion: "standard_v1",
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/a.go", Operation: plan.FileOpModify},
			{Path: "backend/b.go", Operation: plan.FileOpModify},
		}},
	})
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ScopeAmendmentRepo: sa,
		AuditRepo:          au,
		ArtifactRepo:       art,
	})
	return s, sa, au, runRow, stage
}

// TestScopeAmendment_HeadroomFields_RequestDecideFlow is the #983
// cross-boundary seam test: a request→decide flow through both HTTP
// handlers against a real spec snapshot must carry the headroom fields
// in both wire responses AND both audit payloads, with the same count
// the prompt builder's foldScopePaths produces for identical inputs.
func TestScopeAmendment_HeadroomFields_RequestDecideFlow(t *testing.T) {
	s, _, au, runRow, _ := scopeAmendmentHeadroomServer(t)

	// validAmendmentBody adds 2 new paths to the 2-file plan scope:
	// effective-after-approval = 4 against cap 3.
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var reqResp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &reqResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reqResp.EffectiveScopeFilesAfterApproval == nil || *reqResp.EffectiveScopeFilesAfterApproval != 4 {
		t.Errorf("request effective_scope_files_after_approval = %v, want 4", reqResp.EffectiveScopeFilesAfterApproval)
	}
	if reqResp.MaxFilesChanged == nil || *reqResp.MaxFilesChanged != 3 {
		t.Errorf("request max_files_changed = %v, want 3", reqResp.MaxFilesChanged)
	}

	// Dedupe-parity seam: the reported count must equal what the prompt
	// builder folds for the same plan scope + amendment paths.
	folded := s.foldScopePaths(context.Background(),
		[]scopeFile{{Path: "backend/a.go", Operation: "modify"}, {Path: "backend/b.go", Operation: "modify"}},
		[]string{"backend/internal/server/extra.go", "docs/new.md"}, "test")
	if len(folded) != *reqResp.EffectiveScopeFilesAfterApproval {
		t.Errorf("foldScopePaths produced %d, response reports %d — dedupe semantics diverged",
			len(folded), *reqResp.EffectiveScopeFilesAfterApproval)
	}

	var reqPayload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &reqPayload)
	if reqPayload["effective_scope_files_after_approval"] != float64(4) {
		t.Errorf("requested audit effective_scope_files_after_approval = %v, want 4", reqPayload["effective_scope_files_after_approval"])
	}
	if reqPayload["max_files_changed"] != float64(3) {
		t.Errorf("requested audit max_files_changed = %v, want 3", reqPayload["max_files_changed"])
	}

	// Decide (approve) — over-cap approve still succeeds (warn-only),
	// and the decision response + audit carry the same numbers.
	w = postDecision(t, s, runRow.ID, reqResp.ID, `{"decision":"approve","reason":"forced"}`, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "write:stages")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("decision status = %d, want 200 (over-cap approve is warn-only); body = %s", w.Code, w.Body.String())
	}
	var decResp scopeAmendmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &decResp); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if decResp.EffectiveScopeFilesAfterApproval == nil || *decResp.EffectiveScopeFilesAfterApproval != 4 {
		t.Errorf("decision effective_scope_files_after_approval = %v, want 4", decResp.EffectiveScopeFilesAfterApproval)
	}
	if decResp.MaxFilesChanged == nil || *decResp.MaxFilesChanged != 3 {
		t.Errorf("decision max_files_changed = %v, want 3", decResp.MaxFilesChanged)
	}

	var decPayload map[string]any
	found := false
	for _, e := range au.appended {
		if e.Category != CategoryScopeAmendmentDecided {
			continue
		}
		found = true
		_ = json.Unmarshal(e.Payload, &decPayload)
	}
	if !found {
		t.Fatalf("no scope_amendment_decided audit entry; audit = %+v", au.appended)
	}
	if decPayload["effective_scope_files_after_approval"] != float64(4) {
		t.Errorf("decided audit effective_scope_files_after_approval = %v, want 4", decPayload["effective_scope_files_after_approval"])
	}
	if decPayload["max_files_changed"] != float64(3) {
		t.Errorf("decided audit max_files_changed = %v, want 3", decPayload["max_files_changed"])
	}
}

// TestScopeAmendment_HeadroomFields_AbsentWithoutCap asserts the
// fields stay absent (omitempty, fail-open) when no plan artifact /
// spec resolution is available — the scopeAmendmentServer harness has
// no ArtifactRepo.
func TestScopeAmendment_HeadroomFields_AbsentWithoutCap(t *testing.T) {
	s, _, _, au, runRow, _ := scopeAmendmentServer(t)

	w := postAmendment(t, s, runRow.ID, validAmendmentBody, func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "effective_scope_files_after_approval") ||
		strings.Contains(w.Body.String(), "max_files_changed") {
		t.Errorf("headroom fields must be absent on fail-open; body = %s", w.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(au.appended[0].Payload, &payload)
	if _, ok := payload["effective_scope_files_after_approval"]; ok {
		t.Errorf("audit payload must omit headroom fields on fail-open; payload = %v", payload)
	}
}

// TestScopeAmendment_HeadroomFields_ListPendingOnly asserts the list
// handler populates the fields for PENDING items only — decided rows
// carry their decision-time numbers in the audit log.
func TestScopeAmendment_HeadroomFields_ListPendingOnly(t *testing.T) {
	s, _, _, runRow, _ := scopeAmendmentHeadroomServer(t)
	agent := func(r *http.Request) *http.Request {
		return withRunBoundIdentity(r, runRow.ID, "mcp:read", "write:scope-amendments")
	}

	// First amendment: decided (deny). Second: left pending.
	w := postAmendment(t, s, runRow.ID, validAmendmentBody, agent)
	if w.Code != http.StatusCreated {
		t.Fatalf("first request: %d; %s", w.Code, w.Body.String())
	}
	var first scopeAmendmentResponse
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	if w := postDecision(t, s, runRow.ID, first.ID, `{"decision":"deny","reason":"no"}`, func(r *http.Request) *http.Request {
		return withOperatorIdentity(r, "write:stages")
	}); w.Code != http.StatusOK {
		t.Fatalf("decision: %d; %s", w.Code, w.Body.String())
	}
	if w := postAmendment(t, s, runRow.ID,
		`{"paths":[{"path":"backend/second.go","operation":"modify"}],"reason":"second"}`,
		agent); w.Code != http.StatusCreated {
		t.Fatalf("second request: %d", w.Code)
	}

	w = getAmendments(t, s, runRow.ID, agent)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d; %s", w.Code, w.Body.String())
	}
	var list scopeAmendmentListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(list.Items))
	}
	for _, item := range list.Items {
		switch item.Status {
		case "denied":
			if item.EffectiveScopeFilesAfterApproval != nil || item.MaxFilesChanged != nil {
				t.Errorf("decided item must omit headroom fields; got %+v", item)
			}
		case "pending":
			if item.EffectiveScopeFilesAfterApproval == nil || item.MaxFilesChanged == nil {
				t.Errorf("pending item must carry headroom fields; got %+v", item)
			} else if *item.EffectiveScopeFilesAfterApproval != 3 || *item.MaxFilesChanged != 3 {
				// 2 plan files + backend/second.go (the denied
				// amendment's paths confer nothing).
				t.Errorf("pending headroom = %d/%d, want 3/3",
					*item.EffectiveScopeFilesAfterApproval, *item.MaxFilesChanged)
			}
		default:
			t.Errorf("unexpected status %q", item.Status)
		}
	}
}
