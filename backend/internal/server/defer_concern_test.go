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

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// deferServer wires the run + audit + concern repos plus a GitHub client
// (for {epic} title derivation) and a registered fake work-item provider
// the defer handler needs. The fakeWorkProvider is returned so tests can
// assert whether provider.File was called (orphan-issue safety) and seed
// a filing failure.
func deferServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake, *fakeConcernRepo, *fakeWorkProvider) {
	t.Helper()
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	repo := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	gh := newEpicGitHubClient(t, 7788, "[E22] The parent epic")
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   au,
		ConcernRepo: cr,
		GitHub:      gh,
	})
	return s, repo, au, cr, fp
}

// seedDeferRun seeds a running run for the concern's run id with the
// installation + PR URL the defer body and provider need.
func seedDeferRun(repo *approvalRunRepo, runID uuid.UUID) {
	inst := int64(7788)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/1202"
	repo.seedRun(&run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/fishhawk",
		State:          run.StateRunning,
		InstallationID: &inst,
		PullRequestURL: &prURL,
	})
}

func postDefer(t *testing.T, s *Server, concernID string, body deferConcernRequest, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+concernID+"/defer", bytes.NewReader(raw))
	req.SetPathValue("concern_id", concernID)
	w := httptest.NewRecorder()
	s.handleDeferConcern(w, auth(req))
	return w
}

// TestDeferConcern_DoneMeans is the cross-boundary done-means test: an
// OPEN implement concern is deferred through handleDeferConcern, and the
// assertion spans the whole seam — (a) the rendered follow-up work item
// (title, the auto-drafted body containing the concern note + severity +
// category + reviewer model + evidence run id + source PR link, the
// category-defaulted type, merged labels, parent_epic/evidence relations)
// AND (b) the concern transitioned to deferred with a state_reason
// referencing the filed issue, the concern_deferred audit entry was
// appended, and the deferred concern is absent from ListOpenByRun.
func TestDeferConcern_DoneMeans(t *testing.T) {
	s, repo, au, cr, fp := deferServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "the retry loop can spin without a backoff")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{
		ParentEpic: "#389",
		N:          "3",
		Labels:     []string{"area:runner"},
		Note:       "split out as its own change after the gate",
	}, withAuth)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp deferConcernResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// (a) Rendered work item.
	if !fp.called {
		t.Fatal("provider.File was not called on the happy path")
	}
	if resp.Issue.Title != "[E22.3] the retry loop can spin without a backoff" {
		t.Errorf("issue title = %q, want the epic-derived [E22.3] title", resp.Issue.Title)
	}
	if resp.Issue.Type != "chore" {
		t.Errorf("issue type = %q, want chore (category 'scope' is not a defect)", resp.Issue.Type)
	}
	// The auto-drafted body reached the provider verbatim (FilingRequest.Body).
	body := fp.captured.Item.Body
	for _, want := range []string{
		"the retry loop can spin without a backoff", // concern note
		"medium",          // severity
		"scope",           // category
		"claude-opus-4-8", // reviewer model
		runID.String(),    // evidence run id
		"github.com/kuhlman-labs/fishhawk/pull/1202", // source PR link
		"split out as its own change after the gate", // operator note
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered body missing %q:\n%s", want, body)
		}
	}
	if got := fp.captured.Item.Relations.ParentEpic; got != "#389" {
		t.Errorf("provider Item.Relations.ParentEpic = %q, want #389", got)
	}
	if rel := fp.captured.Item.Relations.EvidenceRuns; len(rel) != 1 || rel[0] != runID.String() {
		t.Errorf("provider Item.Relations.EvidenceRuns = %v, want [%s]", rel, runID)
	}
	// Merged labels: the type default plus the operator-supplied one.
	if !containsStr2(fp.captured.Item.Classification.Labels, "type:chore") ||
		!containsStr2(fp.captured.Item.Classification.Labels, "area:runner") {
		t.Errorf("labels = %v, want both type:chore and area:runner", fp.captured.Item.Classification.Labels)
	}

	// (b) Concern transitioned to deferred, state_reason names the issue.
	if resp.Concern.State != string(concern.StateDeferred) {
		t.Errorf("concern state = %q, want deferred", resp.Concern.State)
	}
	if !strings.Contains(resp.Concern.StateReason, "#4242") {
		t.Errorf("state_reason = %q, want it to reference filed issue #4242", resp.Concern.StateReason)
	}
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateDeferred {
		t.Errorf("stored state = %q, want deferred", rows[0].State)
	}

	// concern_deferred audit entry on the concern's run/stage.
	deferred := auditEntriesByCategory(au, CategoryConcernDeferred)
	if len(deferred) != 1 {
		t.Fatalf("concern_deferred entries = %d, want 1", len(deferred))
	}
	au.mu.Lock()
	entry := au.appended[deferred[0]]
	au.mu.Unlock()
	if entry.RunID != runID || entry.StageID == nil || *entry.StageID != stageID {
		t.Errorf("audit run/stage = %v/%v, want %s/%s", entry.RunID, entry.StageID, runID, stageID)
	}
	var payload map[string]any
	_ = json.Unmarshal(entry.Payload, &payload)
	if payload["prior_state"] != string(concern.StateRaised) {
		t.Errorf("payload prior_state = %v, want raised", payload["prior_state"])
	}
	if payload["issue_url"] == "" || payload["issue_url"] == nil {
		t.Errorf("payload issue_url empty: %v", payload)
	}
	// No corrective entry on the happy path.
	if n := len(auditEntriesByCategory(au, CategoryConcernDeferFailed)); n != 0 {
		t.Errorf("concern_defer_failed entries = %d, want 0", n)
	}

	// The deferred concern drops off the open-concerns surface.
	open, _ := cr.ListOpenByRun(context.Background(), runID)
	for _, c := range open {
		if c.ID == row.ID {
			t.Errorf("deferred concern still listed by ListOpenByRun")
		}
	}
}

// TestDeferConcern_NotFound_NoFiling: an unknown concern id returns 404
// and never calls the provider (no issue is filed for a phantom concern).
func TestDeferConcern_NotFound_NoFiling(t *testing.T) {
	s, _, _, _, fp := deferServer(t)
	w := postDefer(t, s, uuid.NewString(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_not_found") {
		t.Errorf("body missing concern_not_found: %s", w.Body.String())
	}
	if fp.called {
		t.Error("provider.File called for a non-existent concern")
	}
}

// TestDeferConcern_AlreadyResolved_PreCheckNoFiling pins orphan-issue
// safety: a closed (already-waived) concern is rejected 422 by the
// open-state pre-check and provider.File is NEVER called — no durable
// issue is created for a concern that cannot transition.
func TestDeferConcern_AlreadyResolved_PreCheckNoFiling(t *testing.T) {
	s, repo, au, cr, fp := deferServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "n")
	if _, err := cr.ApplyResolution(context.Background(), row.ID, concern.StateWaived, "already waived"); err != nil {
		t.Fatalf("seed waived: %v", err)
	}

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_defer_conflict") {
		t.Errorf("body missing concern_defer_conflict: %s", w.Body.String())
	}
	if fp.called {
		t.Error("provider.File called for an already-resolved concern (orphan-issue risk)")
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernDeferred)); n != 0 {
		t.Errorf("concern_deferred entries = %d, want 0", n)
	}
	// State unchanged (still waived from the seed).
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateWaived {
		t.Errorf("state = %q, want waived (unchanged)", rows[0].State)
	}
}

// TestDeferConcern_CrossRun_Forbidden: a run-bound token reaching another
// run's concern is rejected 403 cross_run_defer before any filing.
func TestDeferConcern_CrossRun_Forbidden(t *testing.T) {
	s, repo, _, cr, fp := deferServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "n")
	otherRunID := uuid.New()

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "1"},
		func(req *http.Request) *http.Request { return withMCPFixupAuth(req, otherRunID) })

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_defer") {
		t.Errorf("body missing cross_run_defer: %s", w.Body.String())
	}
	if fp.called {
		t.Error("provider.File called despite cross-run rejection")
	}
}

// TestDeferConcern_FilingFailure_LeavesConcernOpen: when provider.File
// fails, the request returns 502 and the concern is NOT transitioned —
// it stays open so the operator can retry, and no audit entry is written.
func TestDeferConcern_FilingFailure_LeavesConcernOpen(t *testing.T) {
	s, repo, au, cr, fp := deferServer(t)
	fp.fileErr = errors.New("github said no")
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "n")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "work_item_filing_failed") {
		t.Errorf("body missing work_item_filing_failed: %s", w.Body.String())
	}
	// Concern stays OPEN — re-read the row.
	rows, _ := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if rows[0].State != concern.StateRaised {
		t.Errorf("state = %q, want raised (no transition on filing failure)", rows[0].State)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernDeferred)); n != 0 {
		t.Errorf("concern_deferred entries = %d, want 0 on filing failure", n)
	}
}

// raceConcernRepo wraps fakeConcernRepo to force ApplyResolution to fail
// AFTER a successful File — simulating a concurrent writer that raced the
// defer's transition.
type raceConcernRepo struct {
	*fakeConcernRepo
	applyErr error
}

func (r *raceConcernRepo) ApplyResolution(ctx context.Context, id uuid.UUID, to concern.State, reason string) (*concern.Concern, error) {
	if r.applyErr != nil {
		return nil, r.applyErr
	}
	return r.fakeConcernRepo.ApplyResolution(ctx, id, to, reason)
}

// TestDeferConcern_PostFilingRace_CorrectiveAudit pins the binding
// audit-ordering invariant: the issue files successfully but the state
// transition then fails (InvalidTransitionError). The ONLY audit entry is
// the corrective concern_defer_failed (naming the actual state + the
// orphaned issue url) — never a success concern_deferred for a transition
// that did not happen — and the request returns 422.
func TestDeferConcern_PostFilingRace_CorrectiveAudit(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	repo := newApprovalRunRepo()
	au := newAuditFake()
	base := newFakeConcernRepo()
	cr := &raceConcernRepo{
		fakeConcernRepo: base,
		applyErr:        concern.InvalidTransitionError{From: concern.StateWaived, To: concern.StateDeferred},
	}
	gh := newEpicGitHubClient(t, 7788, "[E22] The parent epic")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr, GitHub: gh})

	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, base, runID, stageID, concern.StageKindImplement, 100, "n")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_defer_conflict") {
		t.Errorf("body missing concern_defer_conflict: %s", w.Body.String())
	}
	// The issue WAS filed (the race is post-filing).
	if !fp.called {
		t.Error("provider.File should have been called before the transition race")
	}
	// ONLY the corrective entry — never a success concern_deferred.
	if n := len(auditEntriesByCategory(au, CategoryConcernDeferred)); n != 0 {
		t.Errorf("concern_deferred entries = %d, want 0 (transition did not happen)", n)
	}
	failed := auditEntriesByCategory(au, CategoryConcernDeferFailed)
	if len(failed) != 1 {
		t.Fatalf("concern_defer_failed entries = %d, want 1", len(failed))
	}
	au.mu.Lock()
	entry := au.appended[failed[0]]
	au.mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(entry.Payload, &payload)
	if payload["actual_state"] != string(concern.StateWaived) {
		t.Errorf("corrective actual_state = %v, want waived", payload["actual_state"])
	}
	if payload["issue_url"] == "" || payload["issue_url"] == nil {
		t.Errorf("corrective entry must name the orphaned issue url: %v", payload)
	}
}

// TestDeferConcern_StoreUnconfigured_503: no concern/audit/run repos
// wired returns 503.
func TestDeferConcern_StoreUnconfigured_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()}) // no ConcernRepo / RunRepo
	w := postDefer(t, s, uuid.NewString(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "concern_store_unconfigured") {
		t.Errorf("body missing concern_store_unconfigured: %s", w.Body.String())
	}
}

// TestDeferConcern_RunResolutionFailure_500: the concern resolves but its
// run does not (a data-integrity failure) — the handler returns 500 and
// does not file an issue.
func TestDeferConcern_RunResolutionFailure_500(t *testing.T) {
	s, _, _, cr, fp := deferServer(t)
	// Seed a concern whose run id is NOT seeded into the run repo.
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "n")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "1"}, withAuth)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if fp.called {
		t.Error("provider.File called despite an unresolvable run")
	}
}

// TestDeferConcern_Unauthenticated_401 / _MissingScope_403 pin the auth
// parity with the waive handler.
func TestDeferConcern_Unauthenticated_401(t *testing.T) {
	s, repo, _, cr, _ := deferServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "n")

	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/defer", bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("concern_id", row.ID.String())
	w := httptest.NewRecorder()
	s.handleDeferConcern(w, req) // no identity -> anonymous
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

func TestDeferConcern_MissingScope_403(t *testing.T) {
	s, repo, _, cr, _ := deferServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedDeferRun(repo, runID)
	row := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "n")

	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/defer", bytes.NewReader([]byte(`{}`)))
	req.SetPathValue("concern_id", row.ID.String())
	id := Identity{Subject: "token:reader", TokenID: "tok-read", Scopes: []string{"read:runs"}}
	w := httptest.NewRecorder()
	s.handleDeferConcern(w, req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id)))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
}

// TestDeferTypeForCategory pins the category -> default type mapping.
func TestDeferTypeForCategory(t *testing.T) {
	cases := map[string]string{
		"correctness": "bug",
		"security":    "bug",
		"BUG":         "bug", // case-insensitive
		"scope":       "chore",
		"style":       "chore",
		"":            "chore",
	}
	for in, want := range cases {
		if got := deferTypeForCategory(in); got != want {
			t.Errorf("deferTypeForCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

// containsStr2 reports whether xs contains want.
func containsStr2(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
