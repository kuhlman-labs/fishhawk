package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
)

// ---- fakes ----------------------------------------------------------------

// fakeRefinementDrafter returns a fixed draft (or error), standing in for the
// E34.1 agent so the gate can be exercised without a claude subprocess.
type fakeRefinementDrafter struct {
	draft refinement.EpicDraft
	model string
	err   error
	calls int

	// Recorded at the most recent Draft invocation (disconnect-survival tests):
	// the inbound context's cancellation error and its deadline. A detached,
	// budgeted context observes sawCtxErr==nil and sawHadDeadline==true.
	sawCtxErr      error
	sawHadDeadline bool
	sawDeadlineIn  time.Duration
}

func (f *fakeRefinementDrafter) Draft(ctx context.Context, _ uuid.UUID, _ string) (refinement.EpicDraft, string, error) {
	f.calls++
	f.sawCtxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		f.sawHadDeadline = true
		f.sawDeadlineIn = time.Until(dl)
	} else {
		f.sawHadDeadline = false
		f.sawDeadlineIn = 0
	}
	if f.err != nil {
		return refinement.EpicDraft{}, "", f.err
	}
	return f.draft, f.model, nil
}

// disconnectRefinementRepo is an in-memory refinement repo for the
// disconnect-survival tests. Its reads (ListForSession/ListDecisions) IGNORE
// context cancellation so a load always succeeds — modelling the real flow
// where the load completes before the client disconnects mid-drafting. Its
// CreateDraft RESPECTS cancellation, so a test can prove that only a DETACHED
// context reaches the persist: the amendment arm (detached) persists; the
// direct-edit arm (still bound to the cancelled request context) does not.
type disconnectRefinementRepo struct {
	refinement.Repository
	drafts []*refinement.StoredDraft
}

func (r *disconnectRefinementRepo) ListForSession(_ context.Context, _ uuid.UUID) ([]*refinement.StoredDraft, error) {
	return r.drafts, nil
}

func (r *disconnectRefinementRepo) ListDecisions(_ context.Context, _ uuid.UUID) ([]*refinement.Decision, error) {
	return nil, nil
}

func (r *disconnectRefinementRepo) CreateDraft(ctx context.Context, p refinement.CreateParams) (*refinement.StoredDraft, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sd := &refinement.StoredDraft{
		ID:        p.ID,
		SessionID: p.SessionID,
		Brief:     p.Brief,
		Draft:     p.Draft,
		Model:     p.Model,
		Origin:    p.Origin,
	}
	if sd.ID == uuid.Nil {
		sd.ID = uuid.New()
	}
	r.drafts = append(r.drafts, sd)
	return sd, nil
}

// erroringAuditRepo fails every AppendGlobalChained, to exercise the
// audit-append-failure-500 path (the audit-before-persist binding condition).
type erroringAuditRepo struct{ audit.BaseFake }

func (erroringAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("injected audit failure")
}

// okAuditRepo succeeds on AppendGlobalChained regardless of context — used by
// the disconnect-survival tests, which assert persistence detachment, not audit
// behavior (audit.BaseFake returns ErrNotFound, which would mask the persist).
type okAuditRepo struct{ audit.BaseFake }

func (okAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return &audit.Entry{}, nil
}

// seededRefinementRepo is a read-only fake serving a fixed drafts + decisions
// set, used to exercise the drift branch (a decision whose pinned hash does not
// match the recomputed hash) that the real adapter cannot naturally produce.
type seededRefinementRepo struct {
	refinement.Repository
	drafts    []*refinement.StoredDraft
	decisions []*refinement.Decision
}

func (r *seededRefinementRepo) ListForSession(context.Context, uuid.UUID) ([]*refinement.StoredDraft, error) {
	return r.drafts, nil
}

func (r *seededRefinementRepo) ListDecisions(context.Context, uuid.UUID) ([]*refinement.Decision, error) {
	return r.decisions, nil
}

// ---- helpers --------------------------------------------------------------

func refinementValidDraft() refinement.EpicDraft {
	return refinement.EpicDraft{
		Epic: refinement.EpicSpec{Summary: "stand up X", Scope: "X and its wiring", OutOfScope: "Y"},
		Children: []refinement.ChildDraft{
			{
				Summary: "child one", Proposal: "do one", DoneMeans: "one done",
				AcceptanceCriteria: []string{"one works"}, Labels: []string{"area:backend", "autonomy:medium"},
			},
			{
				Summary: "child two", Proposal: "do two", DoneMeans: "two done",
				AcceptanceCriteria: []string{"two works"}, Labels: []string{"area:backend", "autonomy:medium"},
				DependsOn: []int{1},
			},
		},
	}
}

// refinementReq builds an authed request carrying the write:approvals scope and
// the {session_id} path value (when non-empty).
func refinementReq(method, target, sessionID, bodyJSON string) *http.Request {
	var r *http.Request
	if bodyJSON != "" {
		r = httptest.NewRequest(method, target, bytes.NewReader([]byte(bodyJSON)))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if sessionID != "" {
		r.SetPathValue("session_id", sessionID)
	}
	id := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"write:approvals"}}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// countGlobalCategory counts global-chain audit entries of the given category.
func countGlobalCategory(t *testing.T, repo audit.Repository, category string) int {
	t.Helper()
	entries, err := repo.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("ListGlobal: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

// ---- route registration ---------------------------------------------------

func TestRefinementRoutesRegistered(t *testing.T) {
	s := New(Config{})
	cases := []struct {
		method, target string
	}{
		{http.MethodPost, "/v0/refinement/sessions"},
		{http.MethodGet, "/v0/refinement/sessions/" + uuid.NewString()},
		{http.MethodPatch, "/v0/refinement/sessions/" + uuid.NewString() + "/draft"},
		{http.MethodPost, "/v0/refinement/sessions/" + uuid.NewString() + "/decision"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, nil)
		s.Handler().ServeHTTP(rec, req)
		// The route reaches the handler's auth gate (anonymous → 401), proving
		// it is registered rather than 404/405 from the mux.
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401 (route registered, hits auth gate)", tc.method, tc.target, rec.Code)
		}
	}
}

// ---- unconfigured repo / drafter (the serve.go wiring coverage) -----------

func TestRefinement_NilRepo503OnAllRoutes(t *testing.T) {
	s := New(Config{}) // no RefinementRepo
	sid := uuid.NewString()
	cases := []struct {
		name string
		call func() *httptest.ResponseRecorder
	}{
		{"create", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"b"}`))
			return rec
		}},
		{"get", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", sid, ""))
			return rec
		}},
		{"patch", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sid, `{"brief_amendment":"more"}`))
			return rec
		}},
		{"decision", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sid, `{"decision":"approved","reason":"r"}`))
			return rec
		}},
	}
	for _, tc := range cases {
		rec := tc.call()
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 with nil RefinementRepo", tc.name, rec.Code)
		}
	}
}

func TestRefinement_NilDrafter503OnAgentArmsOnly(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	// Configured repo + audit, but NO drafter.
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	// Seed a session directly so GET / edit / decision have something to act on.
	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	// Create → 503 (agent-backed).
	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"b"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create with nil drafter: status = %d, want 503", rec.Code)
	}

	// Brief-amendment → 503 (agent-backed).
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), `{"brief_amendment":"more"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("brief-amendment with nil drafter: status = %d, want 503", rec.Code)
	}

	// GET → 200 (no drafter needed).
	rec = httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", sessionID.String(), ""))
	if rec.Code != http.StatusOK {
		t.Errorf("GET with nil drafter: status = %d, want 200", rec.Code)
	}

	// Direct edit → 200 (no drafter needed).
	edited := refinementValidDraft()
	edited.Epic.Summary = "edited summary"
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(),
		mustJSON(t, map[string]any{"draft": edited})))
	if rec.Code != http.StatusOK {
		t.Errorf("direct edit with nil drafter: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// Decision → 200 (no drafter needed).
	rec = httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"approved","reason":"ok"}`))
	if rec.Code != http.StatusOK {
		t.Errorf("decision with nil drafter: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// ---- drafting agent failure (502) -----------------------------------------

// TestRefinement_DraftingAgentFailure502 exercises the external-agent failure
// path on BOTH agent-backed arms: when the Drafter returns an error, the
// create-session and brief-amendment handlers must respond 502
// refinement_drafting_failed and persist nothing (the error is returned before
// any CreateDraft / audit append).
func TestRefinement_DraftingAgentFailure502(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{err: errors.New("agent boom")}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	// Create arm: the drafter errors before the persist → 502, nothing stored.
	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"build X"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("create under drafter failure: status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("refinement_drafting_failed")) {
		t.Errorf("create body = %s, want refinement_drafting_failed", rec.Body.String())
	}

	// Brief-amendment arm: seed an initial revision, then a failing amendment must
	// 502 and leave the session at its one initial revision with no edit audit
	// entry (the 502 returns before the audit append AND the persist).
	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), `{"brief_amendment":"more scope"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("amendment under drafter failure: status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("refinement_drafting_failed")) {
		t.Errorf("amendment body = %s, want refinement_drafting_failed", rec.Body.String())
	}
	drafts, _ := repo.ListForSession(context.Background(), sessionID)
	if len(drafts) != 1 {
		t.Errorf("revisions after failed amendment = %d, want 1 (nothing persisted)", len(drafts))
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_edited"); got != 0 {
		t.Errorf("edited audit entries after failed amendment = %d, want 0", got)
	}

	// Both arms reached and invoked the drafter — proving the 502 came from the
	// agent call, not an earlier gate.
	if drafter.calls != 2 {
		t.Errorf("drafter calls = %d, want 2 (create + amendment)", drafter.calls)
	}
}

// ---- disconnect survival (#1637) ------------------------------------------

// cancelledRefinementReq builds an authed refinement request whose context is
// ALREADY cancelled — simulating a client that disconnected. The identity value
// survives on the cancelled context (it is an ancestor value), so
// context.WithoutCancel in the handler still resolves the subject.
func cancelledRefinementReq(method, target, sessionID, bodyJSON string) *http.Request {
	r := refinementReq(method, target, sessionID, bodyJSON)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	return r.WithContext(ctx)
}

// TestRefinement_OpenArm_SurvivesClientDisconnect drives handleCreateRefinementSession
// with an already-cancelled inbound request context and asserts the drafter ran
// on a DETACHED, budgeted context and the session persisted+readable — proving a
// client disconnect neither kills the drafter nor strands the session.
func TestRefinement_OpenArm_SurvivesClientDisconnect(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft(), model: "claude-opus-4-8"}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, cancelledRefinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"build X"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create under disconnected client: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// The drafter saw a NON-cancelled (detached) context bearing a deadline
	// (budgeted), despite the cancelled inbound request context.
	if drafter.sawCtxErr != nil {
		t.Errorf("drafter ctx.Err() = %v, want nil (detached from the cancelled request)", drafter.sawCtxErr)
	}
	if !drafter.sawHadDeadline {
		t.Errorf("drafter ctx carried no deadline, want a refinementDraftBudget deadline (drafter is otherwise unbounded)")
	}
	// The deadline is ~= refinementDraftBudget (a couple of minutes' slack for
	// the value being consumed since the handler set it).
	if drafter.sawDeadlineIn < refinementDraftBudget-time.Minute || drafter.sawDeadlineIn > refinementDraftBudget+time.Minute {
		t.Errorf("drafter deadline-in = %v, want ~%v", drafter.sawDeadlineIn, refinementDraftBudget)
	}

	// The session persisted despite the disconnect: a fresh GET returns 200 with
	// the draft.
	var created refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	rec2 := httptest.NewRecorder()
	s.handleGetRefinementSession(rec2, refinementReq(http.MethodGet, "/x", created.SessionID.String(), ""))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get after disconnect: status = %d, want 200 (session persisted despite disconnect, body %s)", rec2.Code, rec2.Body.String())
	}
}

// TestRefinement_AmendmentArm_SurvivesClientDisconnect drives handlePatchRefinementDraft's
// brief_amendment arm with an already-cancelled inbound context and asserts the
// re-draft ran detached+budgeted and the new revision persisted. The
// disconnectRefinementRepo lets the pre-detach load succeed (modelling a
// disconnect that happens mid-drafting, after the load) while making CreateDraft
// respect cancellation, so a persisted revision proves the detach reached it.
func TestRefinement_AmendmentArm_SurvivesClientDisconnect(t *testing.T) {
	sessionID := uuid.New()
	seed := &refinement.StoredDraft{ID: uuid.New(), SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief}
	repo := &disconnectRefinementRepo{drafts: []*refinement.StoredDraft{seed}}
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft(), model: "claude-opus-4-8"}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}, RefinementDrafter: drafter})

	rec := httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, cancelledRefinementReq(http.MethodPatch, "/x", sessionID.String(), `{"brief_amendment":"more scope"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("amendment under disconnected client: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if drafter.sawCtxErr != nil {
		t.Errorf("drafter ctx.Err() = %v, want nil (detached from the cancelled request)", drafter.sawCtxErr)
	}
	if !drafter.sawHadDeadline {
		t.Errorf("drafter ctx carried no deadline, want a refinementDraftBudget deadline")
	}
	// The new revision persisted (CreateDraft respects ctx, so it could only have
	// succeeded on the detached, non-cancelled context).
	if len(repo.drafts) != 2 {
		t.Errorf("revisions after disconnected amendment = %d, want 2 (new revision persisted despite disconnect)", len(repo.drafts))
	}
}

// TestRefinement_DirectEditArm_BoundToRequestContext proves the direct `draft`
// edit arm is NOT detached — it stays bound to the (cancellable) request context
// because it makes no agent call. Under a cancelled inbound context the persist
// aborts and no revision is added, the inverse of the amendment arm above.
func TestRefinement_DirectEditArm_BoundToRequestContext(t *testing.T) {
	sessionID := uuid.New()
	seed := &refinement.StoredDraft{ID: uuid.New(), SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief}
	repo := &disconnectRefinementRepo{drafts: []*refinement.StoredDraft{seed}}
	s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}})

	editBody := `{"draft":` + mustJSON(t, refinementValidDraft()) + `}`
	rec := httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, cancelledRefinementReq(http.MethodPatch, "/x", sessionID.String(), editBody))
	if rec.Code == http.StatusOK {
		t.Fatalf("direct edit under cancelled context = 200, want failure (the arm must stay bound to the request context)")
	}
	if len(repo.drafts) != 1 {
		t.Errorf("revisions after cancelled direct edit = %d, want 1 (nothing persisted — arm not detached)", len(repo.drafts))
	}
}

// ---- auth -----------------------------------------------------------------

func TestRefinement_AuthScopeEnforced(t *testing.T) {
	s := New(Config{RefinementRepo: &seededRefinementRepo{}})

	// Token lacking write:approvals → 403.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/refinement/sessions", bytes.NewReader([]byte(`{"brief":"b"}`)))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs"}}))
	s.handleCreateRefinementSession(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong-scope token: status = %d, want 403", rec.Code)
	}

	// Anonymous → 401.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v0/refinement/sessions", bytes.NewReader([]byte(`{"brief":"b"}`)))
	s.handleCreateRefinementSession(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
}

// ---- unknown session ------------------------------------------------------

func TestRefinement_UnknownSession404(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

	sid := uuid.NewString()
	for _, tc := range []struct {
		name string
		rec  func() *httptest.ResponseRecorder
	}{
		{"get", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", sid, ""))
			return rec
		}},
		{"patch", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sid, `{"brief_amendment":"m"}`))
			return rec
		}},
		{"decision", func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sid, `{"decision":"approved","reason":"r"}`))
			return rec
		}},
	} {
		rec := tc.rec()
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s unknown session: status = %d, want 404", tc.name, rec.Code)
		}
	}
}

// ---- integration flow -----------------------------------------------------

func TestRefinement_IntegrationFlow(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft(), model: "claude-opus-4-8"}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	// 1. Create a session.
	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"build X"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	sessionID := created.SessionID
	if created.State != string(refinement.StateAwaitingApproval) {
		t.Errorf("create state = %s, want awaiting_approval", created.State)
	}
	// Preview: epic + 2 children; wave DAG matches the depends_on edge.
	if len(created.Preview) != 3 {
		t.Errorf("preview length = %d, want 3 (epic + 2 children)", len(created.Preview))
	}
	wantWaves := [][]int{{1}, {2}}
	if !equalIntWaves(created.Waves, wantWaves) {
		t.Errorf("waves = %v, want %v (child 2 depends on child 1)", created.Waves, wantWaves)
	}

	// 2. Approve with a reason.
	rec = httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(),
		`{"decision":"approved","reason":"looks right"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// The decision row pins draft_id + hash; GET shows approved.
	rec = httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", sessionID.String(), ""))
	var afterApprove refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &afterApprove); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if afterApprove.State != string(refinement.StateApproved) {
		t.Errorf("state after approve = %s, want approved", afterApprove.State)
	}
	if len(afterApprove.Decisions) != 1 || afterApprove.Decisions[0].Reason != "looks right" {
		t.Errorf("decisions = %+v, want one with the verbatim reason", afterApprove.Decisions)
	}

	// The global audit chain carries a refinement_draft_approved entry with the
	// session_id/draft_id/content_hash and the verbatim reason.
	entries, err := auditRepo.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("ListGlobal: %v", err)
	}
	var approved *audit.Entry
	for _, e := range entries {
		if e.Category == "refinement_draft_approved" {
			approved = e
		}
		if e.RunID != nil {
			t.Errorf("refinement audit entry has non-nil run_id %v, want global chain", e.RunID)
		}
	}
	if approved == nil {
		t.Fatal("no refinement_draft_approved entry on the global chain")
	}
	var payload map[string]any
	if err := json.Unmarshal(approved.Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	for _, k := range []string{"session_id", "draft_id", "content_hash", "reason"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("audit payload missing %q: %v", k, payload)
		}
	}
	if payload["reason"] != "looks right" {
		t.Errorf("audit reason = %v, want verbatim 'looks right'", payload["reason"])
	}
	if payload["session_id"] != sessionID.String() {
		t.Errorf("audit session_id = %v, want %s", payload["session_id"], sessionID)
	}

	// 3. A direct field edit re-gates: refinement_draft_edited lands, GET shows
	// awaiting_approval again, and ApprovedDraft returns ErrNotApproved.
	edited := refinementValidDraft()
	edited.Epic.Summary = "stand up X (revised)"
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(),
		mustJSON(t, map[string]any{"draft": edited})))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_edited"); got != 1 {
		t.Errorf("refinement_draft_edited count = %d, want 1", got)
	}

	rec = httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", sessionID.String(), ""))
	var afterEdit refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &afterEdit); err != nil {
		t.Fatalf("decode GET after edit: %v", err)
	}
	if afterEdit.State != string(refinement.StateAwaitingApproval) {
		t.Errorf("state after edit = %s, want awaiting_approval (edit re-gates)", afterEdit.State)
	}
	if afterEdit.RevisionCount != 2 {
		t.Errorf("revision count after edit = %d, want 2", afterEdit.RevisionCount)
	}

	// The filing-executor seam: no usable approval after the edit.
	drafts, err := repo.ListForSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	decisions, err := repo.ListDecisions(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if _, err := refinement.ApprovedDraft(drafts, decisions); !errors.Is(err, refinement.ErrNotApproved) {
		t.Errorf("ApprovedDraft after edit = %v, want ErrNotApproved", err)
	}
}

// ---- E34.5 intake criteria pre-check (#1596) ------------------------------

// refinementFlaggedDraft returns a draft whose second child has NO acceptance
// criteria and whose epic carries an EMPTY out_of_scope — so the intake
// pre-check flags child 2 with no_blocking_criterion.
func refinementFlaggedDraft() refinement.EpicDraft {
	d := refinementValidDraft()
	d.Epic.OutOfScope = ""
	d.Children[1].AcceptanceCriteria = nil
	return d
}

// A drafted child with no blocking criterion surfaces the finding in the
// session view BEFORE any decision — the issue's core done-means.
func TestRefinement_CriteriaPrecheck_FlagsBeforeDecision(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{draft: refinementFlaggedDraft(), model: "claude-opus-4-8"}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"build X"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The precheck flags the draft and names child 2, all before a decision.
	if !view.CriteriaPrecheck.NeedsAttention {
		t.Fatalf("criteria_precheck.needs_attention must be true; got %+v", view.CriteriaPrecheck)
	}
	if len(view.CriteriaPrecheck.Children) != 2 {
		t.Fatalf("want 2 per-child checks; got %+v", view.CriteriaPrecheck.Children)
	}
	child2 := view.CriteriaPrecheck.Children[1]
	if child2.Ordinal != 2 || !child2.NeedsAttention {
		t.Fatalf("child 2 must be flagged by ordinal; got %+v", child2)
	}
	var found bool
	for _, f := range child2.Findings {
		if f.Rule == "no_blocking_criterion" {
			found = true
		}
	}
	if !found {
		t.Fatalf("child 2 must carry a no_blocking_criterion finding; got %+v", child2.Findings)
	}
	// State is still awaiting a verdict — the finding is in the PREVIEW.
	if view.State != string(refinement.StateAwaitingApproval) {
		t.Errorf("state = %s, want awaiting_approval", view.State)
	}
}

// A clean multi-child draft shows checked-and-clean per child: findings [] (not
// null) and needs_attention false.
func TestRefinement_CriteriaPrecheck_CleanIsCheckedAndClean(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft(), model: "claude-opus-4-8"}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	rec := httptest.NewRecorder()
	s.handleCreateRefinementSession(rec, refinementReq(http.MethodPost, "/v0/refinement/sessions", "", `{"brief":"build X"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.CriteriaPrecheck.NeedsAttention {
		t.Fatalf("a clean draft must not need attention; got %+v", view.CriteriaPrecheck)
	}
	for _, c := range view.CriteriaPrecheck.Children {
		if c.NeedsAttention || len(c.Findings) != 0 {
			t.Errorf("child %d must be clean; got %+v", c.Ordinal, c)
		}
	}
	// findings serialize as [] (not null) so a reader can distinguish
	// checked-and-clean from never-checked.
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"findings":[]`)) {
		t.Errorf("clean child findings must marshal as [] (not null): %s", rec.Body.String())
	}
}

// A direct-edit PATCH with a zero-criteria child returns 200 with an advisory
// finding where it returned 422 before — the behavioral done-means for the
// Validate relaxation (a comment-only touch cannot fake this).
func TestRefinement_CriteriaPrecheck_DirectEditZeroCriteria200(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(),
		mustJSON(t, map[string]any{"draft": refinementFlaggedDraft()})))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: status = %d, want 200 (a criteria-less child is now advisory, not a 422); body %s", rec.Code, rec.Body.String())
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.CriteriaPrecheck.NeedsAttention {
		t.Fatalf("the edited draft must carry the advisory finding; got %+v", view.CriteriaPrecheck)
	}
}

// A decision on a flagged draft still succeeds — the flag is advisory, so the
// operator can approve anyway.
func TestRefinement_CriteriaPrecheck_ApproveFlaggedSucceeds(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementFlaggedDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(),
		`{"decision":"approved","reason":"the missing criterion is acceptable for this slice"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve flagged draft: status = %d, want 200 (advisory); body %s", rec.Code, rec.Body.String())
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.State != string(refinement.StateApproved) {
		t.Errorf("state after approving a flagged draft = %s, want approved", view.State)
	}
	// The advisory finding is STILL present on the approved view.
	if !view.CriteriaPrecheck.NeedsAttention {
		t.Errorf("the advisory finding must persist through approval; got %+v", view.CriteriaPrecheck)
	}
}

// ---- decision reason required ---------------------------------------------

func TestRefinement_DecisionMissingReason422(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, body := range []string{`{"decision":"approved","reason":""}`, `{"decision":"approved","reason":"   "}`} {
		rec := httptest.NewRecorder()
		s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s: status = %d, want 422", body, rec.Code)
		}
	}

	// No decision row and no audit entry landed.
	decisions, _ := repo.ListDecisions(context.Background(), sessionID)
	if len(decisions) != 0 {
		t.Errorf("decisions = %d, want 0 after rejected-for-missing-reason", len(decisions))
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_approved"); got != 0 {
		t.Errorf("approved audit entries = %d, want 0", got)
	}
}

// ---- double decision -------------------------------------------------------

func TestRefinement_SecondDecision409(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"approved","reason":"ok"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("first decision: status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"rejected","reason":"changed mind"}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("second decision on same revision: status = %d, want 409", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("decision_already_recorded")) {
		t.Errorf("body = %s, want decision_already_recorded", rec.Body.String())
	}
}

// ---- amendment budget ------------------------------------------------------

func TestRefinement_AmendmentBudget409(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	drafter := &fakeRefinementDrafter{draft: refinementValidDraft()}
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: drafter})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// defaultMaxBriefAmendments successful amendments, then a 409.
	for i := 0; i < defaultMaxBriefAmendments; i++ {
		rec := httptest.NewRecorder()
		s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), `{"brief_amendment":"more scope"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("amendment %d: status = %d, want 200 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), `{"brief_amendment":"one too many"}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("over-budget amendment: status = %d, want 409", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("amendment_budget_exhausted")) {
		t.Errorf("body = %s, want amendment_budget_exhausted", rec.Body.String())
	}
	// No new revision beyond the initial + the budgeted amendments.
	drafts, _ := repo.ListForSession(context.Background(), sessionID)
	if len(drafts) != 1+defaultMaxBriefAmendments {
		t.Errorf("revisions = %d, want %d (initial + %d amendments)", len(drafts), 1+defaultMaxBriefAmendments, defaultMaxBriefAmendments)
	}
}

// ---- drift fails closed ----------------------------------------------------

func TestRefinement_DriftFailsClosed(t *testing.T) {
	draft := refinementValidDraft()
	rev := &refinement.StoredDraft{
		ID: uuid.New(), SessionID: uuid.New(), Draft: draft, Origin: refinement.OriginBrief, CreatedAt: time.Unix(1, 0),
	}
	// A decision on the latest revision whose pinned hash does not match.
	dec := &refinement.Decision{
		ID: uuid.New(), DraftID: rev.ID, Decision: refinement.DecisionApproved,
		Reason: "approved before drift", DraftContentHash: "not-the-real-hash",
	}
	seeded := &seededRefinementRepo{drafts: []*refinement.StoredDraft{rev}, decisions: []*refinement.Decision{dec}}
	s := New(Config{RefinementRepo: seeded, AuditRepo: audit.BaseFake{}})

	rec := httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet, "/x", rev.SessionID.String(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET drifted session: status = %d, want 200", rec.Code)
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.State != string(refinement.StateAwaitingApproval) || !view.Drifted {
		t.Errorf("drifted GET: state=%s drifted=%v, want awaiting_approval + drifted", view.State, view.Drifted)
	}
	// The filing-executor seam refuses a drifted draft.
	if _, err := refinement.ApprovedDraft(seeded.drafts, seeded.decisions); !errors.Is(err, refinement.ErrDraftDrifted) {
		t.Errorf("ApprovedDraft on drift = %v, want ErrDraftDrifted", err)
	}
}

// ---- PATCH both/neither arm -----------------------------------------------

func TestRefinement_PatchBothOrNeither422(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	both := mustJSON(t, map[string]any{"brief_amendment": "x", "draft": refinementValidDraft()})
	for _, body := range []string{both, `{}`} {
		rec := httptest.NewRecorder()
		s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s: status = %d, want 422 (exactly one arm)", body, rec.Code)
		}
	}
}

// ---- edited draft with a cyclic edge --------------------------------------

func TestRefinement_EditCyclicDepends422(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cyclic := refinementValidDraft()
	cyclic.Children[0].DependsOn = []int{2}
	cyclic.Children[1].DependsOn = []int{1}
	rec := httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(),
		mustJSON(t, map[string]any{"draft": cyclic})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("cyclic edit: status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	// No revision persisted.
	drafts, _ := repo.ListForSession(context.Background(), sessionID)
	if len(drafts) != 1 {
		t.Errorf("revisions after rejected edit = %d, want 1", len(drafts))
	}
	// And no edit audit entry.
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_edited"); got != 0 {
		t.Errorf("edited audit entries = %d, want 0", got)
	}
}

// ---- audit-append failure fails closed (binding condition) ----------------

func TestRefinement_AuditFailureFailsClosed(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	// Real refinement repo + an audit repo that ALWAYS errors on append.
	s := New(Config{RefinementRepo: repo, AuditRepo: erroringAuditRepo{}, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Decision path: 500, and NO decision persisted (session state unchanged).
	rec := httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"approved","reason":"ok"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("decision under audit failure: status = %d, want 500", rec.Code)
	}
	decisions, _ := repo.ListDecisions(context.Background(), sessionID)
	if len(decisions) != 0 {
		t.Errorf("decisions after audit-failed approve = %d, want 0 (nothing persisted)", len(decisions))
	}
	drafts, _ := repo.ListForSession(context.Background(), sessionID)
	if len(drafts) != 1 {
		t.Errorf("revisions after audit-failed approve = %d, want 1 (unchanged)", len(drafts))
	}
	// ApprovedDraft must NOT see a usable approval.
	if _, err := refinement.ApprovedDraft(drafts, decisions); !errors.Is(err, refinement.ErrNotApproved) {
		t.Errorf("ApprovedDraft after audit-failed approve = %v, want ErrNotApproved", err)
	}

	// Edit path: 500, and NO new revision persisted.
	edited := refinementValidDraft()
	edited.Epic.Summary = "edited"
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(),
		mustJSON(t, map[string]any{"draft": edited})))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("edit under audit failure: status = %d, want 500", rec.Code)
	}
	drafts, _ = repo.ListForSession(context.Background(), sessionID)
	if len(drafts) != 1 {
		t.Errorf("revisions after audit-failed edit = %d, want 1 (nothing persisted)", len(drafts))
	}
}

// ---- one audit entry per gate action --------------------------------------

func TestRefinement_OneAuditEntryPerGateAction(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, RefinementDrafter: &fakeRefinementDrafter{draft: refinementValidDraft()}})

	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: refinementValidDraft(), Origin: refinement.OriginBrief,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Reject the initial revision.
	rec := httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"rejected","reason":"no"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: status = %d, want 200", rec.Code)
	}
	// Edit to a fresh revision (re-gates), then approve it.
	edited := refinementValidDraft()
	edited.Epic.Summary = "v2"
	rec = httptest.NewRecorder()
	s.handlePatchRefinementDraft(rec, refinementReq(http.MethodPatch, "/x", sessionID.String(), mustJSON(t, map[string]any{"draft": edited})))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleDecideRefinementSession(rec, refinementReq(http.MethodPost, "/x", sessionID.String(), `{"decision":"approved","reason":"yes"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d, want 200", rec.Code)
	}

	if got := countGlobalCategory(t, auditRepo, "refinement_draft_rejected"); got != 1 {
		t.Errorf("rejected entries = %d, want 1", got)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_edited"); got != 1 {
		t.Errorf("edited entries = %d, want 1", got)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_draft_approved"); got != 1 {
		t.Errorf("approved entries = %d, want 1", got)
	}
}

// equalIntWaves compares two wave slices for exact equality.
func equalIntWaves(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}
