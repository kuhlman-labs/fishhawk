package server

// Cross-boundary end-to-end test for the forge-routed issue-comment channel
// (E45.52 / #3481, slice 2): server.New wires Deps.ForgeIssueOps to the
// shared issueOpsFor ladder, so a GitLab-triggered run driven through
// Server.notifyStatusUpdate lands its anchor on the resolver-supplied forge
// via forge.IssueOperations (+ the optional forge.IssueCommentEditor), while
// a github-family run stays on cfg.GitHub and never consults the resolver.
// Deleting the `ForgeIssueOps: s.issueOpsFor` line in server.go reddens the
// GitLab subtests (the family then hits the resolver-nil skip: zero forge
// calls, zero audit rows) and leaves every other test green — that is what
// proves the seam.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------
// Fakes.
// ---------------------------------------------------------------------

// icForgeCall records one forge.IssueOperations / IssueCommentEditor call.
type icForgeCall struct {
	method    string
	scope     forge.CredentialScope
	repo      forge.RepoRef
	number    int
	commentID int64
	body      string
}

// icAppendOnlyForge is a forge.Forge implementing forge.IssueOperations but
// NOT forge.IssueCommentEditor: the append-only degradation target. The
// embedded nil forge.Forge satisfies the interface for the resolver's type
// assertion; none of its methods are ever called.
type icAppendOnlyForge struct {
	forge.Forge
	mu    sync.Mutex
	calls []icForgeCall
}

var _ forge.IssueOperations = (*icAppendOnlyForge)(nil)

func (f *icAppendOnlyForge) record(c icForgeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *icAppendOnlyForge) callsFor(method string) []icForgeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []icForgeCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *icAppendOnlyForge) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *icAppendOnlyForge) FetchIssue(context.Context, forge.CredentialScope, forge.RepoRef, int) (*forge.Issue, error) {
	return nil, errors.New("fake forge: FetchIssue not expected")
}

func (f *icAppendOnlyForge) FetchIssueComments(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) ([]forge.IssueComment, error) {
	f.record(icForgeCall{method: "FetchIssueComments", scope: scope, repo: repo, number: number})
	return nil, nil
}

func (f *icAppendOnlyForge) PostIssueComment(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, body string) error {
	f.record(icForgeCall{method: "PostIssueComment", scope: scope, repo: repo, number: number, body: body})
	return nil
}

func (f *icAppendOnlyForge) SetIssueState(context.Context, forge.CredentialScope, forge.RepoRef, int, forge.IssueStateUpdate) error {
	return errors.New("fake forge: SetIssueState not expected")
}

// icEditorForge is the edit-capable forge: IssueOperations + the optional
// forge.IssueCommentEditor capability (what the real GitLab adapter ships).
type icEditorForge struct {
	icAppendOnlyForge
	nextID int64
}

var _ forge.IssueCommentEditor = (*icEditorForge)(nil)

func (f *icEditorForge) PostIssueCommentWithID(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, body string) (int64, error) {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	f.record(icForgeCall{method: "PostIssueCommentWithID", scope: scope, repo: repo, number: number, body: body, commentID: id})
	return id, nil
}

func (f *icEditorForge) EditIssueComment(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, commentID int64, body string) error {
	f.record(icForgeCall{method: "EditIssueComment", scope: scope, repo: repo, number: number, commentID: commentID, body: body})
	return nil
}

// icRunRepo is a one-run run.Repository.
type icRunRepo struct {
	run.BaseFake
	row *run.Run
}

func (r *icRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.row == nil || r.row.ID != id {
		return nil, run.ErrNotFound
	}
	return r.row, nil
}

// icAuditRepo records AppendChained entries and serves them back through
// BOTH list queries the notifier uses (ListForRun for the anchor render,
// ListForRunByCategory for the sticky comment-id lookup), so a second
// notifyStatusUpdate finds the id the first one audited and EDITS in place.
type icAuditRepo struct {
	audit.BaseFake
	mu      sync.Mutex
	entries []*audit.Entry
}

func (a *icAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := p.RunID
	e := &audit.Entry{
		ID: uuid.New(), Sequence: int64(len(a.entries) + 1), RunID: &r, StageID: p.StageID,
		Timestamp: p.Timestamp, Category: p.Category, ActorKind: p.ActorKind, Payload: p.Payload,
	}
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *icAuditRepo) list(runID uuid.UUID, category string) []*audit.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID && (category == "" || e.Category == category) {
			out = append(out, e)
		}
	}
	return out
}

func (a *icAuditRepo) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	return a.list(runID, ""), nil
}

func (a *icAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	return a.list(runID, category), nil
}

// rowsFor decodes every appended payload of one category.
func (a *icAuditRepo) rowsFor(t *testing.T, category string) []map[string]any {
	t.Helper()
	var out []map[string]any
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------
// Harness.
// ---------------------------------------------------------------------

// icGitLabRun is the canonical GitLab-triggered issue-anchored run: NO
// InstallationID, InstallationRef gitlab:5, a NESTED repo path, issue 7.
func icGitLabRun() *run.Run {
	return &run.Run{
		ID:              uuid.New(),
		Repo:            "group/sub/proj",
		WorkflowID:      "feature_change",
		TriggerSource:   run.TriggerGitHubIssue,
		TriggerRef:      strPtr("issue:7"),
		InstallationRef: strPtr("gitlab:5"),
		State:           run.StateRunning,
	}
}

// icGitHubRun is the github-family control: InstallationID 99, bare-decimal
// InstallationRef (no scheme).
func icGitHubRun() *run.Run {
	id := int64(99)
	return &run.Run{
		ID:              uuid.New(),
		Repo:            "x/y",
		WorkflowID:      "feature_change",
		TriggerSource:   run.TriggerGitHubIssue,
		TriggerRef:      strPtr("issue:42"),
		InstallationID:  &id,
		InstallationRef: strPtr("99"),
		State:           run.StateRunning,
	}
}

type icHarness struct {
	s     *Server
	au    *icAuditRepo
	gh    *httptest.Server
	ghMu  sync.Mutex
	ghHit []string // "METHOD path" of every request the GitHub fake served
	asked []string // forge ids the resolver was asked for
}

// newICHarness builds a Server through the REAL New (so the
// issuecomment.New wiring under test is the production one) over a fake
// GitHub API, the fake repos and a ForgeResolver returning glForge for the
// "gitlab" family. glForge nil models "no gitlab forge registered".
func newICHarness(t *testing.T, row *run.Run, glForge forge.Forge) *icHarness {
	t.Helper()
	h := &icHarness{au: &icAuditRepo{}}
	h.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ghMu.Lock()
		h.ghHit = append(h.ghHit, r.Method+" "+r.URL.Path)
		h.ghMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":4242,"body":"","html_url":"https://github.example/c/4242"}`))
	}))
	t.Cleanup(h.gh.Close)
	h.s = New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      &icRunRepo{row: row},
		AuditRepo:    h.au,
		ArtifactRepo: newFakeArtifactRepo(),
		GitHub: &githubclient.Client{
			BaseURL: h.gh.URL,
			Tokens:  splitParentTokens{},
			HTTP:    h.gh.Client(),
		},
		ForgeResolver: func(id string) (forge.Forge, error) {
			h.asked = append(h.asked, id)
			if id == "gitlab" && glForge != nil {
				return glForge, nil
			}
			return nil, errors.New("no forge registered for " + id + " in this test")
		},
	})
	if h.s.issueNotifier == nil {
		t.Fatal("server constructed no issue notifier; the test needs the production issuecomment.New wiring")
	}
	return h
}

func (h *icHarness) githubHits() []string {
	h.ghMu.Lock()
	defer h.ghMu.Unlock()
	return append([]string(nil), h.ghHit...)
}

func assertICGitLabTarget(t *testing.T, c icForgeCall) {
	t.Helper()
	if want := forge.FromRef("gitlab:5"); c.scope != want {
		t.Errorf("%s scope = %q; want gitlab:5 (the run's InstallationRef)", c.method, c.scope.Ref())
	}
	if want := (forge.RepoRef{Owner: "group/sub", Name: "proj"}); c.repo != want {
		t.Errorf("%s repo = %+v; want {group/sub proj}", c.method, c.repo)
	}
	if c.number != 7 {
		t.Errorf("%s issue number = %d; want 7", c.method, c.number)
	}
}

// ---------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------

// TestIssueComment_Forge_GitLabEditInPlace drives a GitLab run through
// Server.notifyStatusUpdate → issuecomment.NotifyStatusUpdateForRun →
// resolveCommentTarget → issueOpsFor → the resolver-supplied edit-capable
// forge: exactly one PostIssueCommentWithID with scope gitlab:5 and a
// status_comment_posted row carrying forge=gitlab + comment_mode=edit_in_place;
// a second update EDITS that id in place instead of posting again. The GitHub
// fake serves zero requests.
func TestIssueComment_Forge_GitLabEditInPlace(t *testing.T) {
	row := icGitLabRun()
	gl := &icEditorForge{}
	h := newICHarness(t, row, gl)
	ctx := context.Background()

	h.s.notifyStatusUpdate(ctx, row.ID, "test")

	posts := gl.callsFor("PostIssueCommentWithID")
	if len(posts) != 1 {
		t.Fatalf("PostIssueCommentWithID calls = %d; want exactly 1 (all calls: %+v)", len(posts), gl.calls)
	}
	assertICGitLabTarget(t, posts[0])
	if posts[0].body == "" {
		t.Error("posted anchor body is empty")
	}
	if got := gl.callsFor("PostIssueComment"); len(got) != 0 {
		t.Errorf("an editor-capable forge must post via PostIssueCommentWithID, not PostIssueComment; got %d", len(got))
	}
	rows := h.au.rowsFor(t, issuecomment.CategoryStatusCommentPosted)
	if len(rows) != 1 {
		t.Fatalf("status_comment_posted rows = %d; want 1", len(rows))
	}
	if got := rows[0]["forge"]; got != "gitlab" {
		t.Errorf("forge = %v; want gitlab", got)
	}
	if got := rows[0]["comment_mode"]; got != "edit_in_place" {
		t.Errorf("comment_mode = %v; want edit_in_place", got)
	}
	if got := rows[0]["github_comment_id"]; got != float64(1) {
		t.Errorf("github_comment_id = %v; want 1 (the id the forge returned)", got)
	}
	if hits := h.githubHits(); len(hits) != 0 {
		t.Errorf("GitHub API served %d request(s) for a gitlab-family run: %v", len(hits), hits)
	}

	// Second update: the audited id is found through the server-wired audit
	// repo and edited in place — no second post.
	h.s.notifyStatusUpdate(ctx, row.ID, "test")
	edits := gl.callsFor("EditIssueComment")
	if len(edits) != 1 {
		t.Fatalf("EditIssueComment calls after second update = %d; want 1 (all calls: %+v)", len(edits), gl.calls)
	}
	assertICGitLabTarget(t, edits[0])
	if edits[0].commentID != 1 {
		t.Errorf("edited comment id = %d; want 1", edits[0].commentID)
	}
	if got := gl.callsFor("PostIssueCommentWithID"); len(got) != 1 {
		t.Errorf("second update must EDIT, not post again; PostIssueCommentWithID calls = %d", len(got))
	}
	if rows := h.au.rowsFor(t, issuecomment.CategoryStatusCommentPosted); len(rows) != 2 || rows[1]["comment_mode"] != "edit_in_place" {
		t.Errorf("second status_comment_posted row = %+v; want edit_in_place", rows)
	}
}

// TestIssueComment_Forge_GitLabAppendOnly: a resolved forge implementing
// IssueOperations but NOT IssueCommentEditor degrades to append-only —
// PostIssueComment lands with the gitlab scope, the marker rediscovery list
// is skipped, and the audit row names comment_mode=append_only.
func TestIssueComment_Forge_GitLabAppendOnly(t *testing.T) {
	row := icGitLabRun()
	gl := &icAppendOnlyForge{}
	h := newICHarness(t, row, gl)

	h.s.notifyStatusUpdate(context.Background(), row.ID, "test")

	posts := gl.callsFor("PostIssueComment")
	if len(posts) != 1 {
		t.Fatalf("PostIssueComment calls = %d; want exactly 1 (all calls: %+v)", len(posts), gl.calls)
	}
	assertICGitLabTarget(t, posts[0])
	if got := gl.callsFor("FetchIssueComments"); len(got) != 0 {
		t.Errorf("append-only forge must not rediscover a marker it cannot edit; FetchIssueComments calls = %d", len(got))
	}
	rows := h.au.rowsFor(t, issuecomment.CategoryStatusCommentPosted)
	if len(rows) != 1 {
		t.Fatalf("status_comment_posted rows = %d; want 1", len(rows))
	}
	if got := rows[0]["forge"]; got != "gitlab" {
		t.Errorf("forge = %v; want gitlab", got)
	}
	if got := rows[0]["comment_mode"]; got != "append_only" {
		t.Errorf("comment_mode = %v; want append_only", got)
	}
	if hits := h.githubHits(); len(hits) != 0 {
		t.Errorf("GitHub API served %d request(s) for a gitlab-family run: %v", len(hits), hits)
	}
}

// TestIssueComment_Forge_GitLabResolverNilIsSkip: with no gitlab forge
// resolvable the ladder yields nil and the run takes the defined skip — zero
// forge calls, zero status_comment_posted rows, no panic, no GitHub fallback.
// This is ALSO the observable shape of the counterfactual for the server.go
// wiring line: without `ForgeIssueOps: s.issueOpsFor` every gitlab subtest
// above collapses to exactly this outcome.
func TestIssueComment_Forge_GitLabResolverNilIsSkip(t *testing.T) {
	row := icGitLabRun()
	h := newICHarness(t, row, nil)

	h.s.notifyStatusUpdate(context.Background(), row.ID, "test")

	if len(h.asked) == 0 || h.asked[0] != "gitlab" {
		t.Errorf("resolver asked for %v; want the gitlab family first", h.asked)
	}
	if rows := h.au.rowsFor(t, issuecomment.CategoryStatusCommentPosted); len(rows) != 0 {
		t.Errorf("status_comment_posted rows = %d; want 0 (a misconfiguration is not a fact about the run)", len(rows))
	}
	if hits := h.githubHits(); len(hits) != 0 {
		t.Errorf("a gitlab-family run with no resolvable forge fell through to GitHub: %v", hits)
	}
}

// TestIssueComment_Forge_GitHubNeverConsultsResolver is the byte-identical
// GitHub control: a github-family run posts its anchor through cfg.GitHub (the
// App client → the fake GitHub API) and the ForgeResolver is NEVER invoked.
func TestIssueComment_Forge_GitHubNeverConsultsResolver(t *testing.T) {
	row := icGitHubRun()
	gl := &icEditorForge{}
	h := newICHarness(t, row, gl)

	h.s.notifyStatusUpdate(context.Background(), row.ID, "test")

	if len(h.asked) != 0 {
		t.Errorf("ForgeResolver consulted for a github-family run (asked for %v); the github family must stay on cfg.GitHub", h.asked)
	}
	if n := gl.total(); n != 0 {
		t.Errorf("gitlab forge received %d call(s) for a github-family run: %+v", n, gl.calls)
	}
	hits := h.githubHits()
	var posted bool
	for _, hit := range hits {
		if hit == "POST /repos/x/y/issues/42/comments" {
			posted = true
		}
	}
	if !posted {
		t.Errorf("GitHub API never received the anchor create for a github-family run; hits = %v", hits)
	}
	rows := h.au.rowsFor(t, issuecomment.CategoryStatusCommentPosted)
	if len(rows) != 1 {
		t.Fatalf("status_comment_posted rows = %d; want 1", len(rows))
	}
	if _, has := rows[0]["forge"]; has {
		t.Errorf("github-family audit payload carries a forge key (%v); the GitHub payload must stay byte-identical", rows[0])
	}
	if _, has := rows[0]["comment_mode"]; has {
		t.Errorf("github-family audit payload carries a comment_mode key (%v); the GitHub payload must stay byte-identical", rows[0])
	}
}
