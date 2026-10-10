package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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

	// searchResults seeds SearchIssuesByTitle (default nil = empty tracker, so
	// epic-number discovery allocates 1). Seed a prior [E<n>] epic to force the
	// discovered ordinal ABOVE the issue-number counter, so the epic's title
	// ordinal and its issue number diverge (the #1644 regression condition).
	searchResults []githubclient.IssueTitleResult

	// Concurrency gate (nil unless the concurrent-filing test wires it): the
	// first CreateIssue closes createGateEntered then blocks on
	// createGateRelease, pinning the winning goroutine INSIDE the filing
	// critical section (holding the per-draft advisory lock) so a second
	// concurrent POST deterministically blocks on that lock instead of racing
	// the winner to completion first. Gating happens BEFORE f.mu is taken.
	createGateEntered chan struct{}
	createGateRelease chan struct{}
	gateOnce          sync.Once

	// blockLinkFor (#4153): AddSubIssue for the child whose issue number ==
	// this blocks until its context is done and returns the context error —
	// a sub-issue link wedged until the detached filing's budget expires.
	blockLinkFor int

	// release closes createGateRelease exactly once (set by releaseOnCleanup).
	release func()

	// linkUnblock releases a blockLinkFor-wedged AddSubIssue at test cleanup,
	// so a failing (or counterfactual) run never strands the detached filing
	// holding a pooled connection.
	linkUnblock chan struct{}
}

func newFakeGHAPI() *fakeGHAPI {
	return &fakeGHAPI{
		next:        1,
		byNumber:    map[int]*fakeIssue{},
		byNode:      map[string]*fakeIssue{},
		subIssues:   map[string][]string{},
		linkUnblock: make(chan struct{}),
	}
}

func (f *fakeGHAPI) CreateIssue(ctx context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	// Concurrency gate: hold the winner inside the filing critical section on its
	// FIRST create so a second concurrent POST provably blocks on the per-draft
	// advisory lock. Done before f.mu so it never blocks other API calls. The
	// wait respects ctx, so a budget-expired detached filing is released.
	if f.createGateRelease != nil {
		var gateErr error
		f.gateOnce.Do(func() {
			close(f.createGateEntered)
			select {
			case <-f.createGateRelease:
			case <-ctx.Done():
				gateErr = ctx.Err()
			}
		})
		if gateErr != nil {
			return nil, gateErr
		}
	}
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

func (f *fakeGHAPI) IssueNodeID(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, number int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.byNumber[number]
	if !ok {
		return "", fmt.Errorf("no issue #%d", number)
	}
	return iss.nodeID, nil
}

func (f *fakeGHAPI) ProjectFields(_ context.Context, _ forge.CredentialScope, _ githubclient.ProjectCoord, _ string) (*githubclient.ProjectMeta, error) {
	return &githubclient.ProjectMeta{ProjectID: "proj", FieldID: "field", StatusOptions: map[string]string{"Backlog": "opt-backlog"}}, nil
}

func (f *fakeGHAPI) ProjectItemStatus(_ context.Context, _ forge.CredentialScope, _, _, _ string) (*githubclient.ProjectItemStatus, error) {
	return &githubclient.ProjectItemStatus{OnBoard: false}, nil
}

func (f *fakeGHAPI) AddProjectItem(_ context.Context, _ forge.CredentialScope, _, contentID string) (string, error) {
	return "item-" + contentID, nil
}

func (f *fakeGHAPI) SetProjectItemSingleSelect(_ context.Context, _ forge.CredentialScope, _, _, _, _ string) error {
	return nil
}

func (f *fakeGHAPI) AddSubIssue(ctx context.Context, _ forge.CredentialScope, parentNodeID, childNodeID string) error {
	f.mu.Lock()
	child, known := f.byNode[childNodeID]
	block := known && f.blockLinkFor != 0 && child.number == f.blockLinkFor
	f.mu.Unlock()
	if block {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.linkUnblock:
			return errors.New("fake link released by test cleanup")
		}
	}
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

func (f *fakeGHAPI) ListSubIssues(_ context.Context, _ forge.CredentialScope, parentNodeID string) ([]githubclient.SubIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []githubclient.SubIssue
	for _, node := range f.subIssues[parentNodeID] {
		iss := f.byNode[node]
		out = append(out, githubclient.SubIssue{Number: iss.number, NodeID: iss.nodeID, Title: iss.title, Body: iss.body})
	}
	return out, nil
}

func (f *fakeGHAPI) SearchIssuesByTitle(_ context.Context, _ forge.CredentialScope, _ string) ([]githubclient.IssueTitleResult, error) {
	// Default (nil searchResults): empty tracker, so epic number discovery
	// allocates 1. A seeded searchResults forces a higher discovered ordinal.
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchResults, nil
}

// GetIssue satisfies the widened github.API (#2051): the no-epic
// ResolveDependencies path reads each named issue via GetIssue. Minimal stub —
// these tests do not exercise the no-epic path — serving the in-memory issue by
// number so an accidental call still returns coherent data rather than panicking.
func (f *fakeGHAPI) GetIssue(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, number int) (*githubclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.byNumber[number]
	if !ok {
		return nil, githubclient.ErrNotFound
	}
	return &githubclient.Issue{Number: iss.number, Title: iss.title, Body: iss.body, Labels: iss.labels}, nil
}

// issueTitle returns the stored title for an issue number under the API lock,
// so the GetIssue-serving integration stub can read titles concurrently.
func (f *fakeGHAPI) issueTitle(number int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.byNumber[number]
	if !ok {
		return "", false
	}
	return iss.title, true
}

func (f *fakeGHAPI) ProjectsTokenConfigured() bool { return true }

// creates / issueCount / linkedUnder / setLinkFaults read and steer the fake
// under its lock: the detached filing goroutine drives it concurrently with
// the test body.
func (f *fakeGHAPI) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

func (f *fakeGHAPI) issueCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byNumber)
}

func (f *fakeGHAPI) linkedUnder(epicNumber int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.byNumber[epicNumber]
	if !ok {
		return 0
	}
	return len(f.subIssues[iss.nodeID])
}

func (f *fakeGHAPI) setLinkFaults(drop, block int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLinkFor, f.blockLinkFor = drop, block
}

func (f *fakeGHAPI) setCreateErrOn(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErrOn = n
}

func (f *fakeGHAPI) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.byNumber))
	for _, iss := range f.byNumber {
		out = append(out, iss.title)
	}
	return out
}

// ListRepoIssues satisfies the work-item read capability's slice of the
// workmgmt/github API interface (#2230). This fake exercises the FILING and
// board-sync paths, which never enumerate issues, so it is a mechanical stub.
func (f *fakeGHAPI) ListRepoIssues(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ githubclient.ListRepoIssuesOptions) ([]githubclient.RepoIssue, error) {
	return nil, nil
}

// UpdateIssue satisfies the grooming-mutation capability's slice of the
// workmgmt/github API interface (E54.5 / #2237). This fake exercises the
// FILING and board-sync paths, which never edit an existing issue, so it is a
// mechanical stub.
func (f *fakeGHAPI) UpdateIssue(_ context.Context, _ forge.CredentialScope, _ githubclient.RepoRef, _ int, _ githubclient.UpdateIssueParams) (*githubclient.Issue, error) {
	return nil, nil
}

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
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })
}

// newRefinementGHClient builds a *githubclient.Client for the refinement filing
// path. Unlike newInstallationGitHubClient it serves BOTH
// GET /repos/{owner}/{repo}/installation (installation resolution) AND
// GET /repos/{owner}/{repo}/issues/{number} (GetIssue), the latter reading the
// parent epic's stored title from the SAME in-memory api the provider wrote to.
// The issue endpoint is required because the #1644 fix makes the executor leave
// {epic} unset, so deriveEpicTitleVar issues one GetIssue per child to derive
// the epic's discovered ordinal from its [E<n>] title.
func newRefinementGHClient(t *testing.T, api *fakeGHAPI, installID int64) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		// GET /repos/{owner}/{repo}/issues/{number} -> the stored issue title.
		if i := strings.Index(r.URL.Path, "/issues/"); i >= 0 {
			num, err := strconv.Atoi(r.URL.Path[i+len("/issues/"):])
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			title, ok := api.issueTitle(num)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{"number": num, "title": title})
			_, _ = w.Write(body)
			return
		}
		// GET /repos/{owner}/{repo}/installation -> the installation id.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d}`, installID)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
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

// independentDraft is an n-child draft with no depends_on edges, so the wave
// order is ordinal order and child ordinal i files as issue #(i+1).
func independentDraft(n int) refinement.EpicDraft {
	children := make([]refinement.ChildDraft, n)
	for i := range children {
		children[i] = refinement.ChildDraft{
			Summary:            fmt.Sprintf("independent child %d", i+1),
			Proposal:           "do it",
			DoneMeans:          "done",
			AcceptanceCriteria: []string{"works"},
			Labels:             []string{"area:backend", "autonomy:medium"},
		}
	}
	return refinement.EpicDraft{
		Epic:     refinement.EpicSpec{Summary: "stand up Z", Scope: "the Z wiring", OutOfScope: "W"},
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

// postFile runs one POST /file and returns the recorder.
func postFile(s *Server, sessionID uuid.UUID, repo string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleFileRefinementSession(rec, fileReq(sessionID, repo))
	return rec
}

// postFileLaunched POSTs /file and asserts the 202 filing_in_progress launch.
func postFileLaunched(t *testing.T, s *Server, sessionID uuid.UUID, repo string) refinementFileResponse {
	t.Helper()
	rec := postFile(s, sessionID, repo)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /file status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeFileResp(t, rec)
	if resp.Status != refinementFilingStatusInProgress || resp.AlreadyInProgress {
		t.Fatalf("POST /file status=%q already_in_progress=%v, want %q/false (a launch)",
			resp.Status, resp.AlreadyInProgress, refinementFilingStatusInProgress)
	}
	return resp
}

// waitFilingBounded waits for every detached filing, FAILING after D(10s)
// instead of hanging the suite.
func waitFilingBounded(t *testing.T, s *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.waitRefinementFiling()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("the detached refinement filing did not finish within the bound")
	}
}

// getSessionView GETs the session and decodes the view.
func getSessionView(t *testing.T, s *Server, sessionID uuid.UUID) refinementSessionView {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGetRefinementSession(rec, refinementReq(http.MethodGet,
		"/v0/refinement/sessions/"+sessionID.String(), sessionID.String(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var view refinementSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode session view: %v (body=%s)", err, rec.Body.String())
	}
	return view
}

// globalPayload decodes the first global-chain entry of category.
func globalPayload(t *testing.T, repo audit.Repository, category string) map[string]any {
	t.Helper()
	entries, err := repo.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("ListGlobal: %v", err)
	}
	for _, e := range entries {
		if e.Category == category {
			var m map[string]any
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				t.Fatalf("decode %s payload: %v", category, err)
			}
			return m
		}
	}
	t.Fatalf("no %s entry on the global chain", category)
	return nil
}

// releaseOnCleanup closes the create gate at cleanup if the test did not, so a
// failing test never strands the detached goroutine.
func releaseOnCleanup(t *testing.T, api *fakeGHAPI, s *Server) {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(api.createGateRelease) }) }
	t.Cleanup(func() {
		release()
		s.waitRefinementFiling()
	})
	api.release = release
}

// ---- integration: happy path ----------------------------------------------

// TestFileRefinementSession_Integration is the cross-boundary done-means test:
// HTTP handler -> ApprovedDraft gate -> detached executor -> applyAndFileWorkItem
// -> REAL github provider over a fake API -> pgtest persistence -> EpicChildren
// + campaign.Assemble round-trip -> GET session view. An approved 6-child draft
// files as epic + 6 conventions-complete children with real-number depends_on
// markers, sub-issue links, and board placement; the filed epic passes campaign
// assembly.
func TestFileRefinementSession_Integration(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	// Force the epic's DISCOVERED ordinal ABOVE its issue number: seed a prior
	// [E35] epic so discovery parses 35 -> the new epic's title ordinal is 36,
	// while its issue number stays #1 (the CreateIssue counter). Without this
	// divergence the two coincide and the #1644 bug hides.
	api.searchResults = []githubclient.IssueTitleResult{{Number: 35, Title: "[E35] prior epic"}}
	installGHProvider(t, api)

	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	// Serve GetIssue too: deriveEpicTitleVar fetches the parent epic to derive
	// its ordinal now that the executor no longer injects the {epic} title var.
	gh := newRefinementGHClient(t, api, 42)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})

	launch := postFileLaunched(t, s, sessionID, "kuhlman-labs/fishhawk")
	if launch.ChildCount != 6 || launch.BudgetSeconds != int(refinementFilingBudgetFor(6)/time.Second) {
		t.Errorf("launch child_count=%d budget_seconds=%d, want 6/%d", launch.ChildCount, launch.BudgetSeconds, int(refinementFilingBudgetFor(6)/time.Second))
	}
	waitFilingBounded(t, s)

	view := getSessionView(t, s, sessionID)
	if view.State != refinementFilingStateFiled {
		t.Errorf("session state = %q, want filed", view.State)
	}
	if view.Filing == nil || view.Filing.State != refinementFilingStateFiled || view.Filing.CompletedAt == nil {
		t.Fatalf("filing block = %+v, want state filed with completed_at", view.Filing)
	}
	if view.Filing.Epic == nil || view.Filing.Epic.Number != 1 {
		t.Errorf("filing epic = %+v, want #1", view.Filing.Epic)
	}
	if len(view.Filing.Children) != 6 || view.Filing.FiledCount != 7 {
		t.Fatalf("filing children=%d filed_count=%d, want 6/7", len(view.Filing.Children), view.Filing.FiledCount)
	}
	if p := globalPayload(t, auditRepo, "refinement_filing_completed"); p["verified"] != true {
		t.Errorf("completion audit verified = %v, want true (round-trip passed)", p["verified"])
	}
	// The detached context keeps the request's identity: the completion audit
	// names the filing caller, not an anonymous actor.
	entries, err := auditRepo.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("ListGlobal: %v", err)
	}
	for _, e := range entries {
		if e.Category == "refinement_filing_completed" && (e.ActorSubject == nil || *e.ActorSubject != "github:op") {
			t.Errorf("completion audit actor = %v, want github:op (the detached filing lost the caller identity)", e.ActorSubject)
		}
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
	// Child ordinal 1 (#2) is titled [E36.1] — keyed on the epic's DISCOVERED
	// ordinal (36), NOT its issue number (1). This is the #1644 shipped behavior.
	if title := api.byNumber[2].title; !strings.HasPrefix(title, "[E36.1]") {
		t.Errorf("child ordinal 1 (#2) title = %q, want [E36.1] prefix (discovered ordinal, not issue number)", title)
	}
	// Round-trip consistency: every child agrees on the discovered ordinal (36),
	// their n values cover 1..6, and NO child uses the epic ISSUE number.
	titleRE := regexp.MustCompile(`^\[E(\d+)(?:\.(\d+))?\]`)
	epicM := titleRE.FindStringSubmatch(api.byNumber[1].title)
	if epicM == nil {
		t.Fatalf("epic title %q did not match [E<n>]", api.byNumber[1].title)
	}
	epicOrd := epicM[1]
	if epicOrd != "36" {
		t.Errorf("epic discovered ordinal = %s, want 36 (issue number 1 — the two must diverge)", epicOrd)
	}
	gotN := map[string]bool{}
	for num := 2; num <= 7; num++ {
		m := titleRE.FindStringSubmatch(api.byNumber[num].title)
		if m == nil || m[2] == "" {
			t.Fatalf("child #%d title %q did not match [E<ord>.<n>]", num, api.byNumber[num].title)
		}
		if m[1] != epicOrd {
			t.Errorf("child #%d title %q uses epic ordinal %s, want %s (the epic's discovered ordinal)", num, api.byNumber[num].title, m[1], epicOrd)
		}
		if m[1] == "1" {
			t.Errorf("child #%d title %q uses the epic ISSUE number (1), not its discovered ordinal — #1644 regression", num, api.byNumber[num].title)
		}
		gotN[m[2]] = true
	}
	for _, n := range []string{"1", "2", "3", "4", "5", "6"} {
		if !gotN[n] {
			t.Errorf("child n=%s missing from filed titles (got %v)", n, gotN)
		}
	}
	// The completion audit landed exactly once and completed_at is set.
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("refinement_filing_completed entries = %d, want 1", got)
	}
	if _, tracked := s.refinementFiling.snapshot(mustDraftID(t, repo, sessionID)); tracked {
		t.Error("tracker entry survives a successful filing, want cleared")
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

// ---- C8: the POST returns before the forge --------------------------------

// TestFileRefinementSession_DetachedReturnsBeforeForge (#4153 AC 1): with the
// forge wedged on its first create (the gate stands in for "every forge call
// is slow" — an unbounded latency until release), the POST still answers 202
// within D(2s) with zero issues created; releasing the gate lets the detached
// filing finish all 8 children.
func TestFileRefinementSession_DetachedReturnsBeforeForge(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createGateEntered = make(chan struct{})
	api.createGateRelease = make(chan struct{})
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, independentDraft(8))
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), GitHub: newRefinementGHClient(t, api, 42)})
	releaseOnCleanup(t, api, s)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postFile(s, sessionID, "o/r") }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("POST /file did not return within D(2s) while the forge was wedged — the filing is not detached")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if resp := decodeFileResp(t, rec); resp.Status != refinementFilingStatusInProgress || resp.ChildCount != 8 {
		t.Errorf("response status=%q child_count=%d, want filing_in_progress/8", resp.Status, resp.ChildCount)
	}
	if n := api.issueCount(); n != 0 {
		t.Errorf("issues created at response time = %d, want 0", n)
	}
	// While wedged, the preview reports the filing in progress.
	<-api.createGateEntered
	if view := getSessionView(t, s, sessionID); view.Filing == nil || view.Filing.State != refinementFilingStateInProgress || !view.Filing.InFlight {
		t.Errorf("filing block while wedged = %+v, want in_progress/in_flight", view.Filing)
	}

	api.release()
	waitFilingBounded(t, s)
	view := getSessionView(t, s, sessionID)
	if view.State != refinementFilingStateFiled || view.Filing == nil || view.Filing.State != refinementFilingStateFiled {
		t.Fatalf("after release: state=%q filing=%+v, want filed/filed", view.State, view.Filing)
	}
	if len(view.Filing.Children) != 8 {
		t.Errorf("filed children = %d, want 8", len(view.Filing.Children))
	}
	if got := api.linkedUnder(1); got != 8 {
		t.Errorf("children linked under the epic = %d, want 8", got)
	}
}

// ---- C12 + C4: budget expiry mid-link, resume without duplicates -----------

// TestFileRefinementSession_BudgetExpiryMidLink_ResumeNoDuplicates (#4153 AC
// 2/3, C4): child ordinal 4 (#5)'s sub-issue link wedges until the detached
// filing's budget expires. The first filing ends failed (preview: filing.state
// failed, last_error, #5 recorded); a re-invoke LAUNCHES a resume (202
// filing_in_progress, never already_in_progress) that creates only the
// remaining children, links #5 through the link pass, and completes — 9
// creates in all, no duplicate titles, all 8 linked.
func TestFileRefinementSession_BudgetExpiryMidLink_ResumeNoDuplicates(t *testing.T) {
	prevFloor, prevPer := refinementFilingBudget, refinementFilingPerItemBudget
	refinementFilingBudget, refinementFilingPerItemBudget = timescale.D(2*time.Second), 0
	t.Cleanup(func() { refinementFilingBudget, refinementFilingPerItemBudget = prevFloor, prevPer })

	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.setLinkFaults(0, 5) // child ordinal 4 is issue #5
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, independentDraft(8))
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: newRefinementGHClient(t, api, 42)})
	t.Cleanup(func() {
		close(api.linkUnblock)
		s.waitRefinementFiling()
	})

	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)

	view := getSessionView(t, s, sessionID)
	f := view.Filing
	if f == nil || f.State != refinementFilingStateFailed || f.InFlight {
		t.Fatalf("after budget expiry: filing = %+v, want state failed, not in flight", f)
	}
	if f.LastError == "" || f.Step != refinement.FilingStepCreate || f.FailedOrdinal == nil || *f.FailedOrdinal != 5 {
		t.Errorf("failure detail last_error=%q step=%q failed_ordinal=%v, want non-empty/create/5", f.LastError, f.Step, f.FailedOrdinal)
	}
	recordedFive := false
	for _, c := range f.Children {
		if c.Ordinal == 4 && c.Number == 5 {
			recordedFive = true
		}
	}
	if !recordedFive {
		t.Errorf("child ordinal 4 (#5) not recorded after its link was cut off: %+v", f.Children)
	}
	if view.State == refinementFilingStateFiled || f.CompletedAt != nil {
		t.Error("session reported filed after a budget-expired filing")
	}

	// C4: a re-invoke after a failure LAUNCHES a resume.
	api.setLinkFaults(0, 0)
	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)

	view = getSessionView(t, s, sessionID)
	if view.State != refinementFilingStateFiled || view.Filing == nil || view.Filing.State != refinementFilingStateFiled {
		t.Fatalf("after resume: state=%q filing=%+v, want filed", view.State, view.Filing)
	}
	if got := api.creates(); got != 9 {
		t.Errorf("CreateIssue calls = %d, want 9 (1 epic + 8 children, no duplicates)", got)
	}
	seen := map[string]bool{}
	for _, title := range api.titles() {
		if seen[title] {
			t.Errorf("duplicate filed title %q", title)
		}
		seen[title] = true
	}
	if got := api.linkedUnder(1); got != 8 {
		t.Errorf("children linked under the epic = %d, want 8 (#5 linked by the link pass)", got)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("refinement_filing_completed entries = %d, want 1", got)
	}
}

// ---- C9: in-process single-flight -----------------------------------------

// noLockRefinementRepo makes WithFilingLock a pass-through, removing the
// advisory lock that would otherwise MASK the in-process single-flight guard.
type noLockRefinementRepo struct{ refinement.Repository }

func (r noLockRefinementRepo) WithFilingLock(ctx context.Context, _ uuid.UUID, fn func(context.Context) error) error {
	return fn(ctx)
}

// TestFileRefinementSession_ConcurrentCalls_SingleFlight (#4153 AC 4): with
// the advisory lock removed, a second POST while the first filing is in flight
// returns 202 already_in_progress and launches nothing — exactly one epic + 8
// children are created.
func TestFileRefinementSession_ConcurrentCalls_SingleFlight(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createGateEntered = make(chan struct{})
	api.createGateRelease = make(chan struct{})
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, independentDraft(8))
	s := New(Config{RefinementRepo: noLockRefinementRepo{repo}, AuditRepo: audit.NewPostgresRepository(pool), GitHub: newRefinementGHClient(t, api, 42)})
	releaseOnCleanup(t, api, s)

	postFileLaunched(t, s, sessionID, "o/r")
	<-api.createGateEntered // the first filing is in flight, wedged on its epic create

	rec := postFile(s, sessionID, "o/r")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second POST status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if resp := decodeFileResp(t, rec); resp.Status != refinementFilingStatusAlreadyInProgress || !resp.AlreadyInProgress {
		t.Errorf("second POST status=%q already_in_progress=%v, want already_in_progress/true", resp.Status, resp.AlreadyInProgress)
	}

	api.release()
	waitFilingBounded(t, s)
	if got := api.creates(); got != 9 {
		t.Errorf("CreateIssue calls = %d, want 9 (one filing: no concurrent double-file)", got)
	}
	if got := api.issueCount(); got != 9 {
		t.Errorf("distinct issues = %d, want 9", got)
	}
}

// TestFileRefinementSession_InFlightOtherRepo409: a POST naming a different
// repo than the filing already in flight for the draft is the pinned-repo
// mismatch (409), not an already_in_progress that would silently ignore it.
func TestFileRefinementSession_InFlightOtherRepo409(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	s := New(Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), GitHub: newRefinementGHClient(t, api, 42)})
	draftID := mustDraftID(t, repo, sessionID)
	if _, started := s.refinementFiling.tryStart(draftID, "other/repo", 6, time.Minute); !started {
		t.Fatal("seed tryStart did not start")
	}

	rec := postFile(s, sessionID, "o/r")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_filing_repo_mismatch" {
		t.Errorf("code = %q, want refinement_filing_repo_mismatch", env.Error.Code)
	}
	waitFilingBounded(t, s)
	if got := api.creates(); got != 0 {
		t.Errorf("CreateIssue calls = %d, want 0", got)
	}
}

// ---- binding condition: concurrent filing serializes across processes -----

// TestFileRefinementSession_ConcurrentFilesOnce: two SERVERS (two replicas,
// each with its own in-process tracker) file the same approved draft
// concurrently; the per-draft advisory lock guarantees exactly ONE epic + N
// children are provider-created, zero duplicate records and one completion
// audit.
//
// The overlap is FORCED deterministically: the winner's detached filing is
// pinned inside the critical section by a provider-side gate on its first
// CreateIssue (barrier 1), the loser's filing is then launched and its arrival
// confirmed by a real non-granted advisory-lock waiter in pg_locks (barrier
// 2), and only THEN is the winner released.
func TestFileRefinementSession_ConcurrentFilesOnce(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createGateEntered = make(chan struct{})
	api.createGateRelease = make(chan struct{})
	installGHProvider(t, api)

	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	gh := newRefinementGHClient(t, api, 42)
	winner := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})
	loser := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: gh})
	releaseOnCleanup(t, api, winner)
	t.Cleanup(loser.waitRefinementFiling)

	postFileLaunched(t, winner, sessionID, "o/r")
	select {
	case <-api.createGateEntered:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("winner never reached its first CreateIssue under the filing lock")
	}

	postFileLaunched(t, loser, sessionID, "o/r")
	waitForAdvisoryLockWaiter(t, pool)
	api.release()

	waitFilingBounded(t, winner)
	waitFilingBounded(t, loser)

	if got := api.creates(); got != 7 {
		t.Errorf("CreateIssue calls = %d, want 7 (serialized: no concurrent double-file)", got)
	}
	final, _ := repo.ListFiledItems(context.Background(), mustDraftID(t, repo, sessionID))
	if len(final) != 7 {
		t.Errorf("recorded items = %d, want 7 (zero duplicate records)", len(final))
	}
	// The completion side effects run under the SAME per-draft lock as filing,
	// so the loser sees completed_at set and appends NO second completion audit.
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

// waitForAdvisoryLockWaiter blocks until a session is waiting on (blocked
// acquiring) a Postgres advisory lock — the signal that the loser has reached
// WithFilingLock and is contending for the per-draft lock the winner holds.
func waitForAdvisoryLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for {
		var waiters int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`,
		).Scan(&waiters); err != nil {
			t.Fatalf("poll pg_locks for advisory waiter: %v", err)
		}
		if waiters > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("loser never blocked on the per-draft advisory lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: newRefinementGHClient(t, api, 42)})

	// First filing fails mid-sequence: observed on the preview, partial rows durable.
	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	f := getSessionView(t, s, sessionID).Filing
	if f == nil || f.State != refinementFilingStateFailed || f.FiledCount != 4 || f.Step != refinement.FilingStepCreate {
		t.Fatalf("after kill: filing = %+v, want failed with 4 filed at step create", f)
	}
	if !strings.Contains(f.LastError, "provider could not file the work item") || f.FailedOrdinal == nil || *f.FailedOrdinal != 5 {
		t.Errorf("last_error=%q failed_ordinal=%v, want the provider failure at ordinal 5", f.LastError, f.FailedOrdinal)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 0 {
		t.Errorf("completion audit after kill = %d, want 0", got)
	}

	// Re-invoke: exactly the remaining children created (no duplicate creates).
	api.setCreateErrOn(0)
	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	f = getSessionView(t, s, sessionID).Filing
	if f == nil || f.State != refinementFilingStateFiled || f.FiledCount != 7 {
		t.Fatalf("after resume: filing = %+v, want filed with 7", f)
	}
	if got := api.issueCount(); got != 7 {
		t.Errorf("distinct issues created = %d, want 7 (no duplicates)", got)
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

// TestFileRefinementSession_RepoMismatch409IsSynchronous (C10): a filing
// session pinning a different repo is refused 409 SYNCHRONOUSLY — no goroutine
// launched, no tracker entry, zero creates.
func TestFileRefinementSession_RepoMismatch409IsSynchronous(t *testing.T) {
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
	rec := postFile(s, sessionID, "o/r")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "refinement_filing_repo_mismatch" {
		t.Errorf("code = %q, want refinement_filing_repo_mismatch", env.Error.Code)
	}
	waitFilingBounded(t, s)
	if _, tracked := s.refinementFiling.snapshot(draftID); tracked {
		t.Error("a repo-mismatch POST left a tracker entry (a filing was launched)")
	}
	if got := api.creates(); got != 0 {
		t.Errorf("CreateIssue calls = %d, want 0 on repo mismatch", got)
	}
}

// TestFileRefinementSession_VerificationFailure_SurfacesOnPreview (C13): a
// child whose sub-issue link never sticks fails the round-trip verification;
// the preview reports filing.state failed at step verify, completed_at stays
// NULL and no completion audit lands. A later re-invoke (link healthy) links
// the child through the link pass and completes.
func TestFileRefinementSession_VerificationFailure_SurfacesOnPreview(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.setLinkFaults(4, 0) // drop child #4's sub-issue link -> EpicChildren misses it
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: newRefinementGHClient(t, api, 42)})

	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	f := getSessionView(t, s, sessionID).Filing
	if f == nil || f.State != refinementFilingStateFailed || f.Step != refinementFilingStepVerify {
		t.Fatalf("filing = %+v, want failed at step verify", f)
	}
	if !strings.Contains(f.LastError, "verification") || f.CompletedAt != nil {
		t.Errorf("last_error=%q completed_at=%v, want a verification error and NULL", f.LastError, f.CompletedAt)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 0 {
		t.Errorf("completion audit on verification failure = %d, want 0", got)
	}

	api.setLinkFaults(0, 0)
	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	if f := getSessionView(t, s, sessionID).Filing; f == nil || f.State != refinementFilingStateFiled {
		t.Fatalf("after re-invoke: filing = %+v, want filed", f)
	}
	if got := api.creates(); got != 7 {
		t.Errorf("CreateIssue calls = %d, want 7 (the re-invoke re-verifies, files nothing)", got)
	}
}

// toggleAuditRepo fails AppendGlobalChained while fail is set.
type toggleAuditRepo struct {
	audit.Repository
	fail atomic.Bool
}

func (r *toggleAuditRepo) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if r.fail.Load() {
		return nil, errors.New("injected audit failure")
	}
	return r.Repository.AppendGlobalChained(ctx, p)
}

// TestFileRefinementSession_AuditFailure_SurfacesOnPreview (C13): a failed
// completion-audit append leaves the session open (completed_at NULL) and the
// preview reports filing.state failed at step audit; a re-invoke with a
// healthy audit chain completes.
func TestFileRefinementSession_AuditFailure_SurfacesOnPreview(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := &toggleAuditRepo{Repository: audit.NewPostgresRepository(pool)}
	auditRepo.fail.Store(true)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: newRefinementGHClient(t, api, 42)})

	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	f := getSessionView(t, s, sessionID).Filing
	if f == nil || f.State != refinementFilingStateFailed || f.Step != refinementFilingStepAudit {
		t.Fatalf("filing = %+v, want failed at step audit", f)
	}
	if !strings.Contains(f.LastError, "refinement_filing_completed audit") || f.CompletedAt != nil {
		t.Errorf("last_error=%q completed_at=%v, want the audit append error and NULL", f.LastError, f.CompletedAt)
	}

	auditRepo.fail.Store(false)
	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)
	if f := getSessionView(t, s, sessionID).Filing; f == nil || f.State != refinementFilingStateFiled {
		t.Fatalf("after re-invoke: filing = %+v, want filed", f)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("completion audit entries = %d, want 1", got)
	}
}

// TestFileRefinementSession_CompletedReplay200NoForgeCalls (C11): a completed
// session replays 200 status filed / already_completed with the recorded
// items and zero forge calls.
func TestFileRefinementSession_CompletedReplay200NoForgeCalls(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{RefinementRepo: repo, AuditRepo: auditRepo, GitHub: newRefinementGHClient(t, api, 42)})

	postFileLaunched(t, s, sessionID, "o/r")
	waitFilingBounded(t, s)

	rec := postFile(s, sessionID, "o/r")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeFileResp(t, rec)
	if !resp.AlreadyCompleted || resp.Status != refinementFilingStatusFiled {
		t.Errorf("replay already_completed=%v status=%q, want true/filed", resp.AlreadyCompleted, resp.Status)
	}
	if resp.Epic == nil || resp.Epic.Number != 1 || len(resp.Children) != 6 {
		t.Errorf("replay epic=%+v children=%d, want #1 and 6", resp.Epic, len(resp.Children))
	}
	waitFilingBounded(t, s)
	if got := api.creates(); got != 7 {
		t.Errorf("CreateIssue calls after replay = %d, want 7 (no new creates)", got)
	}
	if got := countGlobalCategory(t, auditRepo, "refinement_filing_completed"); got != 1 {
		t.Errorf("completion audit entries = %d, want 1 (replay appends none)", got)
	}
}

// filingFaultRepo injects a filing-ledger read fault into an otherwise
// approved seeded session.
type filingFaultRepo struct {
	*seededRefinementRepo
	sess    *refinement.FilingSession
	getErr  error
	listErr error
}

func (r *filingFaultRepo) GetFilingSession(context.Context, uuid.UUID) (*refinement.FilingSession, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.sess == nil {
		return nil, refinement.ErrNotFound
	}
	return r.sess, nil
}

func (r *filingFaultRepo) ListFiledItems(context.Context, uuid.UUID) ([]*refinement.FiledItem, error) {
	return nil, r.listErr
}

// approvedSeeded is an approved, un-drifted in-memory session.
func approvedSeeded(t *testing.T) *seededRefinementRepo {
	t.Helper()
	draft := sixChildDraft()
	rev := &refinement.StoredDraft{ID: uuid.New(), SessionID: uuid.New(), Draft: draft}
	hash, err := refinement.ContentHash(draft)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	dec := &refinement.Decision{DraftID: rev.ID, Decision: refinement.DecisionApproved, DraftContentHash: hash}
	return &seededRefinementRepo{drafts: []*refinement.StoredDraft{rev}, decisions: []*refinement.Decision{dec}}
}

// TestFileRefinementSession_LedgerReadFaults500: a fault reading the filing
// session or its items in the synchronous gate is a 500, never a launch.
func TestFileRefinementSession_LedgerReadFaults500(t *testing.T) {
	installGHProvider(t, newFakeGHAPI())
	cases := map[string]*filingFaultRepo{
		"get filing session": {getErr: errors.New("filing_sessions down")},
		"list filed items":   {sess: &refinement.FilingSession{Repo: "o/r"}, listErr: errors.New("filed_items down")},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			repo.seededRefinementRepo = approvedSeeded(t)
			s := New(Config{RefinementRepo: repo, AuditRepo: okAuditRepo{}})
			rec := postFile(s, uuid.New(), "o/r")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
			}
			waitFilingBounded(t, s)
			if _, tracked := s.refinementFiling.snapshot(repo.drafts[0].ID); tracked {
				t.Error("a ledger read fault launched a filing")
			}
		})
	}
}

// ---- C14: Shutdown drains the detached filing -----------------------------

// TestShutdown_DrainsDetachedRefinementFiling: Shutdown WAITS for an in-flight
// detached filing (it has not returned while the forge is wedged) and returns
// once the released filing completes.
func TestShutdown_DrainsDetachedRefinementFiling(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createGateEntered = make(chan struct{})
	api.createGateRelease = make(chan struct{})
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: timescale.D(30 * time.Second),
		RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), GitHub: newRefinementGHClient(t, api, 42)})
	releaseOnCleanup(t, api, s)

	postFileLaunched(t, s, sessionID, "o/r")
	<-api.createGateEntered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the detached refinement filing was still in flight — it did not drain")
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	api.release()
	select {
	case <-shutdownDone:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("Shutdown did not return after the detached filing finished")
	}
	sess, err := repo.GetFilingSession(context.Background(), mustDraftID(t, repo, sessionID))
	if err != nil {
		t.Fatalf("GetFilingSession: %v", err)
	}
	if sess.CompletedAt == nil {
		t.Error("Shutdown returned before the drained filing completed")
	}
}

// TestShutdown_RefinementFilingDrainBoundedByDeadline: a wedged detached
// filing cannot hold Shutdown past its deadline; once released, the goroutine
// still exits (the WaitGroup reaches zero).
func TestShutdown_RefinementFilingDrainBoundedByDeadline(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)
	api := newFakeGHAPI()
	api.createGateEntered = make(chan struct{})
	api.createGateRelease = make(chan struct{})
	installGHProvider(t, api)
	sessionID := seedApprovedDraft(t, repo, sixChildDraft())
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: timescale.D(200 * time.Millisecond),
		RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), GitHub: newRefinementGHClient(t, api, 42)})
	releaseOnCleanup(t, api, s)

	postFileLaunched(t, s, sessionID, "o/r")
	<-api.createGateEntered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown was held past its deadline by a wedged detached filing")
	}
	api.release()
	waitFilingBounded(t, s)
}
