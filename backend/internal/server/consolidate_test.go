package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// consolidateGitHub is consolidate_test's fake orchestrator.GitHubAPI. It
// records the fan-in's branch-create + merge calls so the cross-boundary test
// can assert the consolidated branch was created and each slice merged in
// ascending order, and lets a test inject a merge conflict or a non-conflict
// integration error to drive the failure modes.
type consolidateGitHub struct {
	mu sync.Mutex

	branchSHAs map[string]string // branch -> tip sha (present); absent => not exists

	createdRefs []string // consolidated branch CreateRef targets, in call order
	mergeHeads  []string // MergeBranch head branches, in call order

	conflictOnHeadSuffix string // a MergeBranch head with this suffix returns ErrMergeConflict
	mergeErr             error  // non-nil => every MergeBranch returns it (non-conflict error)
	prURL                string
}

func newConsolidateGitHub() *consolidateGitHub {
	return &consolidateGitHub{
		branchSHAs: map[string]string{"main": "basesha"}, // consolidated branch absent
		prURL:      "https://github.com/x/y/pull/7",
	}
}

func (g *consolidateGitHub) GetBranchSHA(_ context.Context, _ int64, _ githubclient.RepoRef, branch string) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sha, ok := g.branchSHAs[branch]
	return sha, ok, nil
}

func (g *consolidateGitHub) CreateRef(_ context.Context, _ int64, _ githubclient.RepoRef, branch, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createdRefs = append(g.createdRefs, branch)
	g.branchSHAs[branch] = sha
	return nil
}

func (g *consolidateGitHub) MergeBranch(_ context.Context, _ int64, _ githubclient.RepoRef, _, head, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeHeads = append(g.mergeHeads, head)
	if g.mergeErr != nil {
		return g.mergeErr
	}
	if g.conflictOnHeadSuffix != "" && strings.HasSuffix(head, g.conflictOnHeadSuffix) {
		return githubclient.ErrMergeConflict
	}
	return nil
}

func (g *consolidateGitHub) CreatePullRequest(_ context.Context, _ int64, _ githubclient.RepoRef, _, _, _, _ string) (*githubclient.PullRequest, error) {
	return &githubclient.PullRequest{HTMLURL: g.prURL}, nil
}

func (g *consolidateGitHub) ListOpenPullRequestsByHead(context.Context, int64, githubclient.RepoRef, string, string) ([]githubclient.PullRequest, error) {
	return nil, nil
}

func (g *consolidateGitHub) DispatchWorkflow(context.Context, int64, githubclient.RepoRef, string, string, githubclient.DispatchInputs) error {
	return nil
}

func (g *consolidateGitHub) EnableAutoMerge(context.Context, int64, githubclient.RepoRef, int, githubclient.MergeMethod) error {
	return nil
}

func (g *consolidateGitHub) mergeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.mergeHeads)
}

// consolidateFixture bundles the wired server + fakes + the seeded parent so
// each test drives the endpoint over the same harness.
type consolidateFixture struct {
	s      *Server
	rr     *orchestratorRepo
	au     *auditCompleteAuditFake
	gh     *consolidateGitHub
	parent *run.Run
	impl   *run.Stage
}

// childSpec describes a decomposed child to seed.
type childSpec struct {
	sliceIndex int
	state      run.State
}

// seedConsolidateFixture wires an orchestratorRepo + audit fake + an
// orchestrator backed by the consolidateGitHub, seeds a decomposed parent
// (plan succeeded, implement awaiting_children, review pending-human) plus the
// given children, and returns the fixture. implementAwaiting=false seeds the
// implement stage succeeded instead (the not_awaiting_children case).
func seedConsolidateFixture(t *testing.T, gh *consolidateGitHub, implementAwaiting bool, children []childSpec) *consolidateFixture {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditCompleteAuditFake()

	parent := rr.seedRun()
	inst := int64(42)
	parent.InstallationID = &inst

	plan := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan

	implState := run.StageStateAwaitingChildren
	if !implementAwaiting {
		implState = run.StageStateSucceeded
	}
	impl := rr.seedStage(parent.ID, 1, implState)
	impl.Type = run.StageTypeImplement

	review := rr.seedStage(parent.ID, 2, run.StageStatePending)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman // dispatch walks to awaiting_approval without GitHub

	for _, cs := range children {
		c := rr.seedRun()
		c.Repo = parent.Repo
		c.DecomposedFrom = &parent.ID
		idx := cs.sliceIndex
		c.SliceIndex = &idx
		c.State = cs.state
	}

	o := &orchestrator.Orchestrator{Runs: rr, GitHub: gh, Audit: au, DefaultRef: "main"}
	s := New(Config{RunRepo: rr, AuditRepo: au, Orchestrator: o})
	// Isolate the endpoint from the fire-and-forget consolidated implement
	// review New() wires (#1060) — it is out of scope for this endpoint's
	// behavior and would otherwise run during Advance.
	o.ConsolidatedReview = nil

	return &consolidateFixture{s: s, rr: rr, au: au, gh: gh, parent: parent, impl: impl}
}

// postConsolidate drives POST /v0/runs/{run_id}/consolidate with the given
// identity mutator.
func postConsolidate(t *testing.T, s *Server, runID uuid.UUID, withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/consolidate", nil)
	req.SetPathValue("run_id", runID.String())
	if withID != nil {
		req = withID(req)
	}
	w := httptest.NewRecorder()
	s.handleConsolidateRun(w, req)
	return w
}

func consolidateAuditCount(t *testing.T, au *auditCompleteAuditFake, runID uuid.UUID, category string) int {
	t.Helper()
	entries, err := au.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("ListForRunByCategory(%s): %v", category, err)
	}
	return len(entries)
}

func decodeConsolidate(t *testing.T, w *httptest.ResponseRecorder) consolidateResponse {
	t.Helper()
	var resp consolidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("unmarshal error: %v\nbody: %s", err, w.Body.String())
	}
	return e.Error.Code
}

// TestConsolidateRun_CleanFanIn is the CROSS-BOUNDARY end-to-end: the endpoint
// drives IntegrateSlices -> stage transition -> Advance -> consolidated PR. It
// asserts the fake GitHub recorded the consolidated branch create + both slice
// merges in ascending order, a slices_integrated AND a children_settled audit
// entry landed, the parent implement stage resolved succeeded, and the
// response carries the consolidated branch + PR URL. It then re-invokes to
// assert idempotency: a second call is a 409 no-op that records no new merges.
func TestConsolidateRun_CleanFanIn(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
		{sliceIndex: 1, state: run.StateSucceeded},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeConsolidate(t, w)
	if resp.Outcome != "integrated" {
		t.Errorf("outcome = %q, want integrated", resp.Outcome)
	}
	if resp.ResolvedToState != string(run.StageStateSucceeded) {
		t.Errorf("resolved_to_state = %q, want succeeded", resp.ResolvedToState)
	}

	// The consolidated branch was created from the base sha.
	gh.mu.Lock()
	createdRefs := append([]string(nil), gh.createdRefs...)
	mergeHeads := append([]string(nil), gh.mergeHeads...)
	gh.mu.Unlock()
	if len(createdRefs) != 1 {
		t.Fatalf("createdRefs = %v, want exactly 1 consolidated branch create", createdRefs)
	}
	if !strings.HasPrefix(createdRefs[0], "fishhawk/run-") {
		t.Errorf("created ref = %q, want a fishhawk/run- consolidated branch", createdRefs[0])
	}
	// Both slices merged in ascending slice order.
	if len(mergeHeads) != 2 {
		t.Fatalf("mergeHeads = %v, want 2 merges", mergeHeads)
	}
	if !strings.HasSuffix(mergeHeads[0], "/slice-0") || !strings.HasSuffix(mergeHeads[1], "/slice-1") {
		t.Errorf("merge order = %v, want slice-0 then slice-1 (ascending)", mergeHeads)
	}

	// Both audit entries landed.
	if n := consolidateAuditCount(t, f.au, f.parent.ID, "slices_integrated"); n != 1 {
		t.Errorf("slices_integrated entries = %d, want 1", n)
	}
	if n := consolidateAuditCount(t, f.au, f.parent.ID, "children_settled"); n != 1 {
		t.Errorf("children_settled entries = %d, want 1", n)
	}

	// Parent implement stage resolved succeeded.
	if f.impl.State != run.StageStateSucceeded {
		t.Errorf("parent implement state = %q, want succeeded", f.impl.State)
	}

	// Response + run carry the consolidated PR + branch.
	if resp.PullRequestURL != gh.prURL {
		t.Errorf("response pull_request_url = %q, want %q", resp.PullRequestURL, gh.prURL)
	}
	if !strings.HasPrefix(resp.ConsolidatedBranch, "fishhawk/run-") {
		t.Errorf("response consolidated_branch = %q, want a fishhawk/run- branch", resp.ConsolidatedBranch)
	}
	updated, _ := f.rr.GetRun(context.Background(), f.parent.ID)
	if updated.PullRequestURL == nil || *updated.PullRequestURL != gh.prURL {
		t.Errorf("run pull_request_url = %v, want %q", updated.PullRequestURL, gh.prURL)
	}

	// Idempotent re-invocation: the stage already resolved, so a second call is
	// a 409 not_awaiting_children no-op and records NO new merges.
	w2 := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w2.Code != http.StatusConflict {
		t.Fatalf("re-invoke status = %d, want 409:\n%s", w2.Code, w2.Body.String())
	}
	if code := decodeError(t, w2); code != "not_awaiting_children" {
		t.Errorf("re-invoke error = %q, want not_awaiting_children", code)
	}
	if got := gh.mergeCount(); got != 2 {
		t.Errorf("merge calls after re-invoke = %d, want 2 (no new merges)", got)
	}
}

// TestConsolidateRun_SliceConflict asserts the recoverable category-B path: a
// slice that fails to merge fails the parent implement stage category-B, emits
// a slice_integration_conflict audit carrying the conflicting slice index +
// child run id, and returns a 200 slice_conflict body (the E24.2 contract).
func TestConsolidateRun_SliceConflict(t *testing.T) {
	gh := newConsolidateGitHub()
	gh.conflictOnHeadSuffix = "/slice-1"
	f := seedConsolidateFixture(t, gh, true, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
		{sliceIndex: 1, state: run.StateSucceeded},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeConsolidate(t, w)
	if resp.Outcome != "slice_conflict" {
		t.Errorf("outcome = %q, want slice_conflict", resp.Outcome)
	}
	if resp.ConflictingSliceIndex == nil || *resp.ConflictingSliceIndex != 1 {
		t.Errorf("conflicting_slice_index = %v, want 1", resp.ConflictingSliceIndex)
	}
	if resp.ConflictingChildRunID == "" {
		t.Error("conflicting_child_run_id is empty, want the conflicting child's run id")
	}

	// Parent implement failed recoverable category-B.
	if f.impl.State != run.StageStateFailed {
		t.Errorf("parent implement state = %q, want failed", f.impl.State)
	}
	if f.impl.FailureCategory == nil || *f.impl.FailureCategory != run.FailureB {
		t.Errorf("parent implement failure category = %v, want B", f.impl.FailureCategory)
	}

	// The conflict audit landed; the success audit did NOT.
	if n := consolidateAuditCount(t, f.au, f.parent.ID, "slice_integration_conflict"); n != 1 {
		t.Errorf("slice_integration_conflict entries = %d, want 1", n)
	}
	if n := consolidateAuditCount(t, f.au, f.parent.ID, "children_settled"); n != 0 {
		t.Errorf("children_settled entries = %d, want 0 on a conflict", n)
	}
}

// TestConsolidateRun_IntegrationError asserts the diagnosability fix: a
// non-conflict IntegrateSlices error returns 502 with the error surfaced, the
// parent stage is left UNCHANGED (still awaiting_children), and NO
// children_settled entry is appended.
func TestConsolidateRun_IntegrationError(t *testing.T) {
	gh := newConsolidateGitHub()
	gh.mergeErr = githubclient.ErrNotFound // a non-conflict integration error
	f := seedConsolidateFixture(t, gh, true, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "slice_integration_error" {
		t.Errorf("error = %q, want slice_integration_error", code)
	}
	// Stage left untouched for retry.
	if f.impl.State != run.StageStateAwaitingChildren {
		t.Errorf("parent implement state = %q, want awaiting_children (unchanged)", f.impl.State)
	}
	if n := consolidateAuditCount(t, f.au, f.parent.ID, "children_settled"); n != 0 {
		t.Errorf("children_settled entries = %d, want 0 on an integration error", n)
	}
}

// TestConsolidateRun_ChildrenInFlight asserts the 409 when a child is still
// non-terminal, naming the in-flight child.
func TestConsolidateRun_ChildrenInFlight(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
		{sliceIndex: 1, state: run.StateRunning},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "children_in_flight" {
		t.Errorf("error = %q, want children_in_flight", code)
	}
	if gh.mergeCount() != 0 {
		t.Errorf("merges = %d, want 0 (no fan-in while a child is in flight)", gh.mergeCount())
	}
}

// TestConsolidateRun_ChildFailed asserts the 409 when a child failed — the
// operator must resolve the failed child rather than consolidate a partial set.
func TestConsolidateRun_ChildFailed(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
		{sliceIndex: 1, state: run.StateFailed},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "children_failed" {
		t.Errorf("error = %q, want children_failed", code)
	}
}

// TestConsolidateRun_NotAParent asserts the 400 when the run is itself a
// decomposed child (decomposed_from set).
func TestConsolidateRun_NotAParent_Child(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, []childSpec{{sliceIndex: 0, state: run.StateSucceeded}})
	other := uuid.New()
	f.parent.DecomposedFrom = &other // make the "parent" itself a child

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "not_a_decomposed_parent" {
		t.Errorf("error = %q, want not_a_decomposed_parent", code)
	}
}

// TestConsolidateRun_NotAParent_NoChildren asserts the 400 when the run has no
// decomposed children.
func TestConsolidateRun_NotAParent_NoChildren(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, nil)

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "not_a_decomposed_parent" {
		t.Errorf("error = %q, want not_a_decomposed_parent", code)
	}
}

// TestConsolidateRun_NotAwaitingChildren asserts the 409 when the parent has no
// implement stage parked in awaiting_children (already resolved).
func TestConsolidateRun_NotAwaitingChildren(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, false /* implement already succeeded */, []childSpec{
		{sliceIndex: 0, state: run.StateSucceeded},
	})

	w := postConsolidate(t, f.s, f.parent.ID, withAuth)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "not_awaiting_children" {
		t.Errorf("error = %q, want not_awaiting_children", code)
	}
}

// TestConsolidateRun_RunBoundTokenForbidden asserts a run-bound fhm_ agent
// token (subject mcp:run:<uuid>) is rejected 403 — consolidation is an
// operator action.
func TestConsolidateRun_RunBoundTokenForbidden(t *testing.T) {
	gh := newConsolidateGitHub()
	f := seedConsolidateFixture(t, gh, true, []childSpec{{sliceIndex: 0, state: run.StateSucceeded}})

	withRunBound := func(req *http.Request) *http.Request {
		id := Identity{
			Subject: "mcp:run:" + f.parent.ID.String(),
			TokenID: "fhm-token",
			Scopes:  []string{"write:runs"},
		}
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}

	w := postConsolidate(t, f.s, f.parent.ID, withRunBound)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code := decodeError(t, w); code != "agent_token_forbidden" {
		t.Errorf("error = %q, want agent_token_forbidden", code)
	}
}
