package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// ---- fake githubclient API for the REAL workmgmt/github provider ----------

type fakeIssue struct {
	number int
	nodeID string
	title  string
	body   string
	labels []string
}

// fakeGHAPI implements workmgmt/github.API in memory, so the integration test
// exercises the production github provider code (marker rendering, sub-issue
// linking, board placement) rather than a stub Provider. It is concurrency-safe
// (the concurrent-filing test drives two goroutines through it).
type fakeGHAPI struct {
	mu          sync.Mutex
	next        int
	byNumber    map[int]*fakeIssue
	byNode      map[string]*fakeIssue
	subIssues   map[string][]string // parentNodeID -> child nodeIDs
	createCalls int
	createErrOn int // fail the Nth CreateIssue (0 = never)
	dropLinkFor int // AddSubIssue no-op when the child's issue number == this
}

func newFakeGHAPI() *fakeGHAPI {
	return &fakeGHAPI{
		next:      1,
		byNumber:  map[int]*fakeIssue{},
		byNode:    map[string]*fakeIssue{},
		subIssues: map[string][]string{},
	}
}

func (f *fakeGHAPI) CreateIssue(_ context.Context, _ int64, _ githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErrOn != 0 && f.createCalls == f.createErrOn {
		return nil, fmt.Errorf("fake create failure on call %d", f.createCalls)
	}
	num := f.next
	f.next++
	node := fmt.Sprintf("node-%d", num)
	iss := &fakeIssue{number: num, nodeID: node, title: p.Title, body: p.Body, labels: p.Labels}
	f.byNumber[num] = iss
	f.byNode[node] = iss
	return &githubclient.CreatedIssue{Number: num, NodeID: node, HTMLURL: fmt.Sprintf("https://github.com/o/r/issues/%d", num)}, nil
}

func (f *fakeGHAPI) IssueNodeID(_ context.Context, _ int64, _ githubclient.RepoRef, number int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.byNumber[number]
	if !ok {
		return "", fmt.Errorf("no issue #%d", number)
	}
	return iss.nodeID, nil
}

func (f *fakeGHAPI) ProjectFields(_ context.Context, _ int64, _ githubclient.ProjectCoord, _ string) (*githubclient.ProjectMeta, error) {
	return &githubclient.ProjectMeta{ProjectID: "proj", FieldID: "field", StatusOptions: map[string]string{"Backlog": "opt-backlog"}}, nil
}

func (f *fakeGHAPI) ProjectItemStatus(_ context.Context, _ int64, _, _, _ string) (*githubclient.ProjectItemStatus, error) {
	return &githubclient.ProjectItemStatus{OnBoard: false}, nil
}

func (f *fakeGHAPI) AddProjectItem(_ context.Context, _ int64, _, contentID string) (string, error) {
	return "item-" + contentID, nil
}

func (f *fakeGHAPI) SetProjectItemSingleSelect(_ context.Context, _ int64, _, _, _, _ string) error {
	return nil
}

func (f *fakeGHAPI) AddSubIssue(_ context.Context, _ int64, parentNodeID, childNodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if child, ok := f.byNode[childNodeID]; ok && child.number == f.dropLinkFor {
		// Simulate a dropped sub-issue link: the child is created but never
		// attached, so EpicChildren later misses it.
		return nil
	}
	f.subIssues[parentNodeID] = append(f.subIssues[parentNodeID], childNodeID)
	return nil
}

func (f *fakeGHAPI) ListSubIssues(_ context.Context, _ int64, parentNodeID string) ([]githubclient.SubIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []githubclient.SubIssue
	for _, node := range f.subIssues[parentNodeID] {
		iss := f.byNode[node]
		out = append(out, githubclient.SubIssue{Number: iss.number, NodeID: iss.nodeID, Title: iss.title, Body: iss.body})
	}
	return out, nil
}

func (f *fakeGHAPI) SearchIssuesByTitle(_ context.Context, _ int64, _ string) ([]githubclient.IssueTitleResult, error) {
	// Empty tracker: epic number discovery allocates 1.
	return nil, nil
}

func (f *fakeGHAPI) ProjectsTokenConfigured() bool { return true }

// namedGHProvider wraps the real github provider with a unique registry name so
// the integration test does not clobber the process-global github_projects
// registration used by other tests. The embedded *Provider promotes File,
// DiscoverNumbers, and EpicChildren, so the capability type-assertions still
// resolve.
type namedGHProvider struct {
	*workmgmtgithub.Provider
	name string
}

func (n *namedGHProvider) Name() string { return n.name }

// installConventions returns default conventions rerouted to a uniquely-named
// provider registered over api, plus the resolved conventions the handler will
// load. It overrides conventionsLoader for the test's duration.
func installGHProvider(t *testing.T, api *fakeGHAPI) {
	t.Helper()
	name := "github_it_" + uuid.NewString()
	workmgmt.Register(&namedGHProvider{Provider: workmgmtgithub.New(api), name: name})
	conv := workmgmt.Default()
	conv.Provider = name
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })
}

// ---- draft seeding --------------------------------------------------------

func sixChildDraft() refinement.EpicDraft {
	children := make([]refinement.ChildDraft, 6)
	for i := range children {
		children[i] = refinement.ChildDraft{
			Summary:            fmt.Sprintf("child %d", i+1),
			Proposal:           "do it",
			DoneMeans:          "done",
			AcceptanceCriteria: []string{"works"},
			Labels:             []string{"area:backend", "autonomy:medium"},
		}
	}
	children[1].DependsOn = []int{1} // child 2 depends on child 1
	children[5].DependsOn = []int{3} // child 6 depends on child 3
	return refinement.EpicDraft{
		Epic:     refinement.EpicSpec{Summary: "stand up X", Scope: "the X wiring", OutOfScope: "Y"},
		Children: children,
	}
}

// seedApprovedDraft persists an approved, hash-pinned draft revision and returns
// its session id.
func seedApprovedDraft(t *testing.T, repo refinement.Repository, d refinement.EpicDraft) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	stored, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: d,
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	hash, err := refinement.ContentHash(d)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if _, err := repo.RecordDecision(context.Background(), refinement.DecisionParams{
		SessionID: sessionID, DraftID: stored.ID, Decision: refinement.DecisionApproved,
		Reason: "ok", DraftContentHash: hash,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	return sessionID
}

// fileReq builds an authed POST /file request for the session.
func fileReq(sessionID uuid.UUID, repo string) *http.Request {
	return refinementReq(http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/file",
		sessionID.String(), fmt.Sprintf(`{"repo":%q}`, repo))
}

func decodeFileResp(t *testing.T, rec *httptest.ResponseRecorder) refinementFileResponse {
	t.Helper()
	var resp refinementFileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// ---- integration: happy path ----------------------------------------------

// TestFileRefinementSession_Integration is the cross-boundary done-means test:
// HTTP handler -> ApprovedDraft gate -> executor -> applyAndFileWorkItem -> REAL
// github provider over a fake API -> pgtest persistence -> EpicChildren +
// campaign.Assemble round-trip. An approved 6-child draft files as epic + 6
// conventions-complete children with real-number depends_on markers, sub-issue
// links, and board placement; the filed epic passes campaign assembly.
func TestFileRefinementSession_Integration(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)

	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "kuhlman-labs/fishhawk"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeFileResp(t, rec)

	if resp.Epic.Number != 1 {
		t.Errorf("epic number = %d, want 1", resp.Epic.Number)
	}
	if len(resp.Children) != 6 {
		t.Fatalf("children = %d, want 6", len(resp.Children))
	}
	if !resp.Verified {
		t.Error("verified = false, want true (round-trip passed)")
	}
	if resp.Resumed || resp.AlreadyCompleted {
		t.Errorf("fresh fill: resumed=%v already_completed=%v, want both false", resp.Resumed, resp.AlreadyCompleted)
	}

	// Provider created exactly epic + 6 children.
	if api.createCalls != 7 {
		t.Errorf("CreateIssue calls = %d, want 7", api.createCalls)
	}
	// Epic sub-issue attachment: all 6 children linked under the epic node.
	epicNode := api.byNumber[1].nodeID
	if got := len(api.subIssues[epicNode]); got != 6 {
		t.Errorf("epic children linked = %d, want 6", got)
	}
	// Wave order: {1,3,4,5} then {2,6}; child 2 (ord2) => #6 depends on child 1
	// (ord1) => #2; child 6 (ord6) => #7 depends on child 3 (ord3) => #3. The
	// markers carry the REAL filed numbers.
	if body := api.byNumber[6].body; !strings.Contains(body, "Depends on: #2") {
		t.Errorf("child 2 (#6) body missing real-number marker 'Depends on: #2':\n%s", body)
	}
	if body := api.byNumber[7].body; !strings.Contains(body, "Depends on: #3") {
		t.Errorf("child 6 (#7) body missing real-number marker 'Depends on: #3':\n%s", body)
	}
	// [E1.n] titles keyed on the epic issue number.
	if title := api.byNumber[2].title; !strings.HasPrefix(title, "[E1.1]") {
		t.Errorf("child ordinal 1 (#2) title = %q, want [E1.1] prefix", title)
	}
	// The completion audit landed exactly once and completed_at is set.
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("refinement_filing_completed entries = %d, want 1", got)
	}
	sess, err := repo.GetFilingSession(context.Background(), mustDraftID(t, repo, sessionID))
	if err != nil {
		t.Fatalf("GetFilingSession: %v", err)
	}
	if sess.CompletedAt == nil {
		t.Error("completed_at is nil after a full fill, want set")
	}
}

// mustDraftID resolves the session's latest draft id (the filing-session key).
func mustDraftID(t *testing.T, repo refinement.Repository, sessionID uuid.UUID) uuid.UUID {
	t.Helper()
	drafts, err := repo.ListForSession(context.Background(), sessionID)
	if err != nil || len(drafts) == 0 {
		t.Fatalf("ListForSession: %v (n=%d)", err, len(drafts))
	}
	return drafts[len(drafts)-1].ID
}

// ---- integration: kill-and-resume -----------------------------------------

func TestFileRefinementSession_KillAndResume(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createErrOn = 5 // fail the 5th CreateIssue (epic + 3 children ok, 4th fails)
	installGHProvider(t, api)

	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	// First POST: fails mid-sequence -> 502, partial rows durable.
	rec1 := httptest.NewRecorder()
	s.handleFileRefinementSession(rec1, fileReq(sessionID, "o/r"))
	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502 (body=%s)", rec1.Code, rec1.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec1.Body.Bytes(), &env)
	if env.Error.Code != "refinement_filing_failed" {
		t.Errorf("first code = %q, want refinement_filing_failed", env.Error.Code)
	}
	draftID := mustDraftID(t, repo, sessionID)
	recorded, _ := repo.ListFiledItems(context.Background(), draftID)
	if len(recorded) != 4 { // epic + 3 children
		t.Fatalf("recorded after kill = %d, want 4 (epic + 3 children)", len(recorded))
	}
	// No completion audit, completed_at NULL.
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 0 {
		t.Errorf("completion audit after kill = %d, want 0", got)
	}

	// Re-invoke: exactly the remaining 3 children created (no duplicate creates).
	api.createErrOn = 0
	rec2 := httptest.NewRecorder()
	s.handleFileRefinementSession(rec2, fileReq(sessionID, "o/r"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}
	resp := decodeFileResp(t, rec2)
	if !resp.Resumed {
		t.Error("resume: resumed = false, want true")
	}
	if !resp.Verified {
		t.Error("resume: verified = false, want true")
	}
	// createCalls: 5 on the first pass (1 epic + 3 ok + 1 failed) + 3 on resume
	// (the remaining children) = 8; but only 7 issues were actually created
	// (the failed call created nothing). Assert distinct issues == 7.
	if len(api.byNumber) != 7 {
		t.Errorf("distinct issues created = %d, want 7 (no duplicates)", len(api.byNumber))
	}
	final, _ := repo.ListFiledItems(context.Background(), draftID)
	if len(final) != 7 {
		t.Errorf("final recorded = %d, want 7", len(final))
	}
}

// ---- binding condition: concurrent filing serializes ----------------------

// TestFileRefinementSession_ConcurrentFilesOnce is the operator binding
// condition: two goroutines POST /file for the same approved draft
// simultaneously; the per-draft advisory lock guarantees exactly ONE epic + N
// children are provider-created and zero duplicate records.
func TestFileRefinementSession_ConcurrentFilesOnce(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)

	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.handleFileRefinementSession(rec, fileReq(sessionID, "o/r"))
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("goroutine %d status = %d, want 200", i, c)
		}
	}
	// Exactly one epic + 6 children created, zero duplicates.
	if api.createCalls != 7 {
		t.Errorf("CreateIssue calls = %d, want 7 (serialized: no concurrent double-file)", api.createCalls)
	}
	if len(api.byNumber) != 7 {
		t.Errorf("distinct issues = %d, want 7", len(api.byNumber))
	}
	final, _ := repo.ListFiledItems(context.Background(), mustDraftID(t, repo, sessionID))
	if len(final) != 7 {
		t.Errorf("recorded items = %d, want 7 (zero duplicate records)", len(final))
	}
	// The completion side effects run under the SAME per-draft lock as filing, so
	// the loser sees completed_at set and appends NO second completion audit: the
	// refinement_filing_completed entry lands EXACTLY once (high/concurrency fix —
	// without it the loser enters after all rows are recorded but before
	// completed_at is set, observes AlreadyCompleted=false, and double-audits).
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("refinement_filing_completed entries = %d, want 1 (no duplicate completion under concurrency)", got)
	}
	sess, err := repo.GetFilingSession(context.Background(), mustDraftID(t, repo, sessionID))
	if err != nil {
		t.Fatalf("GetFilingSession: %v", err)
	}
	if sess.CompletedAt == nil {
		t.Error("completed_at is nil after a concurrent full fill, want set")
	}
}

// ---- failure modes --------------------------------------------------------

func TestFileRefinementSession_MissingScope403(t *testing.T) {
	s := New(Config{RefinementRepo: &seededRefinementRepo{}})
	// Authenticated but WITHOUT write:approvals.
	sessionID := uuid.New()
	r := httptest.NewRequest(http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/file",
		strings.NewReader(`{"repo":"o/r"}`))
	r.SetPathValue("session_id", sessionID.String())
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs"}}))
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing scope)", rec.Code)
	}
}

func TestFileRefinementSession_Anonymous401(t *testing.T) {
	s := New(Config{RefinementRepo: &seededRefinementRepo{}})
	sessionID := uuid.New()
	r := httptest.NewRequest(http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/file",
		strings.NewReader(`{"repo":"o/r"}`))
	r.SetPathValue("session_id", sessionID.String())
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (anonymous)", rec.Code)
	}
}

func TestFileRefinementSession_UnknownSession404(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool)})
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(uuid.New(), "o/r"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestFileRefinementSession_NotApproved409(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	// Persist a draft with NO decision -> not approved.
	sessionID := uuid.New()
	if _, err := repo.CreateDraft(context.Background(), refinement.CreateParams{
		SessionID: sessionID, Brief: "b", Draft: sixChildDraft(),
	}); err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool)})
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "o/r"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_not_approved" {
		t.Errorf("code = %q, want refinement_not_approved", env.Error.Code)
	}
}

func TestFileRefinementSession_Drifted409(t *testing.T) {
	// A decision whose pinned hash no longer matches the recomputed hash — the
	// drift branch the real adapter cannot naturally produce, via seededRefinementRepo.
	draft := sixChildDraft()
	rev := &refinement.StoredDraft{ID: uuid.New(), SessionID: uuid.New(), Draft: draft}
	dec := &refinement.Decision{
		DraftID: rev.ID, Decision: refinement.DecisionApproved, DraftContentHash: "stale-hash",
	}
	seeded := &seededRefinementRepo{drafts: []*refinement.StoredDraft{rev}, decisions: []*refinement.Decision{dec}}
	s := New(Config{RefinementRepo: seeded, AuditRepo: audit.BaseFake{}})
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(uuid.New(), "o/r"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_draft_drifted" {
		t.Errorf("code = %q, want refinement_draft_drifted", env.Error.Code)
	}
}

func TestFileRefinementSession_MalformedRepo400(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool)})
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "not-a-repo"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestFileRefinementSession_RepoMismatchOnResume409(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	// Pre-open a filing session pinning a DIFFERENT repo.
	draftID := mustDraftID(t, repo, sessionID)
	if _, err := repo.CreateFilingSession(context.Background(), refinement.FilingSessionParams{
		DraftID: draftID, SessionID: sessionID, Repo: "other/repo",
	}); err != nil {
		t.Fatalf("CreateFilingSession: %v", err)
	}
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), GitHub: gh})
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "o/r"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_filing_repo_mismatch" {
		t.Errorf("code = %q, want refinement_filing_repo_mismatch", env.Error.Code)
	}
	// Nothing was filed.
	if api.createCalls != 0 {
		t.Errorf("CreateIssue calls = %d, want 0 on repo mismatch", api.createCalls)
	}
}

func TestFileRefinementSession_VerificationFailure502(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.dropLinkFor = 4 // drop one child's sub-issue link -> EpicChildren misses it
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "o/r"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_filing_verification_failed" {
		t.Errorf("code = %q, want refinement_filing_verification_failed", env.Error.Code)
	}
	// NO completion audit; completed_at NULL (items are durable, re-invoke re-verifies).
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 0 {
		t.Errorf("completion audit on verification failure = %d, want 0", got)
	}
	sess, err := repo.GetFilingSession(context.Background(), mustDraftID(t, repo, sessionID))
	if err != nil {
		t.Fatalf("GetFilingSession: %v", err)
	}
	if sess.CompletedAt != nil {
		t.Error("completed_at set despite verification failure, want NULL")
	}
}

func TestFileRefinementSession_AuditFailure500(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	gh := newInstallationGitHubClient(t, 42, false)
	// erroringAuditRepo fails every AppendGlobalChained -> the completion audit fails.
	s := New(Config{RefinementRepo: repo, AuditRepo: erroringAuditRepo{}, GitHub: gh})

	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, "o/r"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	// Items are durable but the session stays open (completed_at NULL) so a
	// re-invoke retries the close.
	sess, err := repo.GetFilingSession(context.Background(), mustDraftID(t, repo, sessionID))
	if err != nil {
		t.Fatalf("GetFilingSession: %v", err)
	}
	if sess.CompletedAt != nil {
		t.Error("completed_at set despite an audit-append failure, want NULL")
	}
}

func TestFileRefinementSession_AlreadyCompletedReplay(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newInstallationGitHubClient(t, 42, false)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	// First POST completes the filing.
	rec1 := httptest.NewRecorder()
	s.handleFileRefinementSession(rec1, fileReq(sessionID, "o/r"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (body=%s)", rec1.Code, rec1.Body.String())
	}

	// Second POST replays: 200, already_completed=true, NO new creates, NO
	// second completion audit.
	rec2 := httptest.NewRecorder()
	s.handleFileRefinementSession(rec2, fileReq(sessionID, "o/r"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (body=%s)", rec2.Code, rec2.Body.String())
	}
	resp := decodeFileResp(t, rec2)
	if !resp.AlreadyCompleted {
		t.Error("replay already_completed = false, want true")
	}
	if api.createCalls != 7 {
		t.Errorf("CreateIssue calls after replay = %d, want 7 (no new creates)", api.createCalls)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("completion audit entries = %d, want 1 (replay appends none)", got)
	}
}
