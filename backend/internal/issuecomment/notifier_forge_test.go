package issuecomment

// Forge-routed issue-locus comment tests (E45.52 / #3481): a run whose
// InstallationRef carries a non-GitHub scheme (`gitlab:5`) posts its anchor,
// pings, CI-retry, budget-alert and generic comments through
// forge.IssueOperations, with edit-in-place negotiated via the optional
// forge.IssueCommentEditor. notifier_test.go / ping_test.go are the
// byte-identical GitHub control and are NOT edited (approval condition 4).
//
// This file is package-INTERNAL (not issuecomment_test) because the generic
// contextFor → post path has NO exported caller in the tree (see
// TestNotify_IssueAnchoredSuppression's note in notifier_test.go), and
// approval condition 2 requires that path pinned on the forge fake
// (TestContextFor_GitLab_GenericPost). The external fakes in notifier_test.go
// live in a different package, so this file carries its own minimal fakes;
// every other test here still drives an EXPORTED Notify* entry point.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------
// Fakes.
// ---------------------------------------------------------------------

// forgeCall records one forge.IssueOperations / IssueCommentEditor call.
type forgeCall struct {
	method    string // FetchIssueComments | PostIssueComment | PostIssueCommentWithID | EditIssueComment
	scope     forge.CredentialScope
	repo      forge.RepoRef
	number    int
	commentID int64
	body      string
}

// fakeForgeOps implements forge.IssueOperations WITHOUT the editor: the
// append-only forge. Embed it in fakeForgeEditor for the edit-capable one.
type fakeForgeOps struct {
	mu       sync.Mutex
	calls    []forgeCall
	comments []forge.IssueComment
	postErr  error
	fetchErr error
}

var _ forge.IssueOperations = (*fakeForgeOps)(nil)

func (f *fakeForgeOps) record(c forgeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeForgeOps) callsFor(method string) []forgeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgeCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeForgeOps) FetchIssue(context.Context, forge.CredentialScope, forge.RepoRef, int) (*forge.Issue, error) {
	return nil, errors.New("fake forge: FetchIssue not expected")
}

func (f *fakeForgeOps) FetchIssueComments(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) ([]forge.IssueComment, error) {
	f.record(forgeCall{method: "FetchIssueComments", scope: scope, repo: repo, number: number})
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.comments, nil
}

func (f *fakeForgeOps) PostIssueComment(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, body string) error {
	f.record(forgeCall{method: "PostIssueComment", scope: scope, repo: repo, number: number, body: body})
	return f.postErr
}

func (f *fakeForgeOps) SetIssueState(context.Context, forge.CredentialScope, forge.RepoRef, int, forge.IssueStateUpdate) error {
	return errors.New("fake forge: SetIssueState not expected")
}

// fakeForgeEditor is the edit-capable forge: IssueOperations + the optional
// forge.IssueCommentEditor capability.
type fakeForgeEditor struct {
	fakeForgeOps
	nextID  int64
	editErr error
}

var _ forge.IssueCommentEditor = (*fakeForgeEditor)(nil)

func (f *fakeForgeEditor) PostIssueCommentWithID(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, body string) (int64, error) {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	f.record(forgeCall{method: "PostIssueCommentWithID", scope: scope, repo: repo, number: number, body: body, commentID: id})
	if f.postErr != nil {
		return 0, f.postErr
	}
	return id, nil
}

func (f *fakeForgeEditor) EditIssueComment(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, commentID int64, body string) error {
	f.record(forgeCall{method: "EditIssueComment", scope: scope, repo: repo, number: number, commentID: commentID, body: body})
	return f.editErr
}

// forgeTestGitHub is a minimal IssueCommenter whose every call is recorded,
// so a forge-routed test can assert ZERO GitHub calls and the github control
// can assert the create landed here.
type forgeTestGitHub struct {
	mu      sync.Mutex
	creates []forgeCall
	updates []forgeCall
	lists   []forgeCall
}

func (g *forgeTestGitHub) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.creates) + len(g.updates) + len(g.lists)
}

func (g *forgeTestGitHub) CreateIssueComment(_ context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, issueNumber int, body string) (*githubclient.IssueComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.creates = append(g.creates, forgeCall{method: "CreateIssueComment", scope: scope, repo: repo, number: issueNumber, body: body})
	return &githubclient.IssueComment{ID: int64(len(g.creates)), Body: body}, nil
}

func (g *forgeTestGitHub) UpdateIssueComment(_ context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, commentID int64, body string) (*githubclient.IssueComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.updates = append(g.updates, forgeCall{method: "UpdateIssueComment", scope: scope, repo: repo, commentID: commentID, body: body})
	return &githubclient.IssueComment{ID: commentID, Body: body}, nil
}

func (g *forgeTestGitHub) CreateReview(context.Context, forge.CredentialScope, githubclient.RepoRef, int, githubclient.CreateReviewParams) (*githubclient.CreateReviewResult, error) {
	return nil, errors.New("fake github: CreateReview not expected")
}

func (g *forgeTestGitHub) ListIssueComments(_ context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, number int) ([]githubclient.FetchedIssueComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lists = append(g.lists, forgeCall{method: "ListIssueComments", scope: scope, repo: repo, number: number})
	return nil, nil
}

// forgeTestRuns is a one-run run.Repository.
type forgeTestRuns struct {
	run.Repository
	row *run.Run
}

func (r *forgeTestRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.row == nil || r.row.ID != id {
		return nil, run.ErrNotFound
	}
	return r.row, nil
}

func (r *forgeTestRuns) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, nil
}

func (r *forgeTestRuns) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, nil
}

// forgeTestAudit records appends and serves them (plus pre-seeded rows) back
// through the two list queries the notifier uses.
type forgeTestAudit struct {
	audit.Repository
	mu       sync.Mutex
	seeded   []*audit.Entry
	appended []audit.ChainAppendParams
}

func (a *forgeTestAudit) seed(runID uuid.UUID, category string, payload map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	a.seeded = append(a.seeded, &audit.Entry{
		ID: uuid.New(), Sequence: int64(len(a.seeded) + 1), RunID: &r, Category: category, Payload: body,
	})
}

func (a *forgeTestAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appended = append(a.appended, p)
	r := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &r}, nil
}

func (a *forgeTestAudit) entries(runID uuid.UUID, category string) []*audit.Entry {
	out := []*audit.Entry{}
	for _, e := range a.seeded {
		if e.RunID != nil && *e.RunID == runID && (category == "" || e.Category == category) {
			out = append(out, e)
		}
	}
	for i, p := range a.appended {
		if p.RunID == runID && (category == "" || p.Category == category) {
			r := p.RunID
			out = append(out, &audit.Entry{
				ID: uuid.New(), Sequence: int64(len(a.seeded) + i + 1), RunID: &r,
				Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp,
			})
		}
	}
	return out
}

func (a *forgeTestAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entries(runID, category), nil
}

func (a *forgeTestAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entries(runID, ""), nil
}

// rowsFor returns the appended rows of one category, decoded.
func (a *forgeTestAudit) rowsFor(category string) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, p := range a.appended {
		if p.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p.Payload, &m); err != nil {
			m = map[string]any{"_undecodable": string(p.Payload)}
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// gitlabRun is the canonical GitLab-triggered issue-anchored run: NO
// InstallationID, InstallationRef gitlab:5, a NESTED repo path, issue 7.
func gitlabRun() *run.Run {
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

// githubRun is the GitHub control: InstallationID 99, bare-decimal ref.
func githubRun() *run.Run {
	return &run.Run{
		ID:              uuid.New(),
		Repo:            "x/y",
		WorkflowID:      "feature_change",
		TriggerSource:   run.TriggerGitHubIssue,
		TriggerRef:      strPtr("issue:42"),
		InstallationID:  i64Ptr(99),
		InstallationRef: strPtr("99"),
		State:           run.StateRunning,
	}
}

type forgeHarness struct {
	row   *run.Run
	gh    *forgeTestGitHub
	au    *forgeTestAudit
	n     *Notifier
	asked []string // families the resolver was asked for
}

// newForgeHarness wires a Notifier whose ForgeIssueOps returns ops for the
// "gitlab" family (nil for any other) and records which families it was asked
// for. ops may be nil to model "resolver returns nil".
func newForgeHarness(t *testing.T, row *run.Run, ops forge.IssueOperations) *forgeHarness {
	t.Helper()
	h := &forgeHarness{row: row, gh: &forgeTestGitHub{}, au: &forgeTestAudit{}}
	h.n = New(Deps{
		GitHub: h.gh,
		ForgeIssueOps: func(forgeID string) forge.IssueOperations {
			h.asked = append(h.asked, forgeID)
			if forgeID != commentFamilyGitLab {
				return nil
			}
			return ops
		},
		Runs:        &forgeTestRuns{row: row},
		Audit:       h.au,
		ExternalURL: "https://app.fishhawk.example.com",
		Now:         func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
	})
	if h.n == nil {
		t.Fatal("notifier nil")
	}
	return h
}

var (
	wantGitLabScope = forge.FromRef("gitlab:5")
	wantGitLabRepo  = forge.RepoRef{Owner: "group/sub", Name: "proj"}
)

func assertGitLabTarget(t *testing.T, c forgeCall) {
	t.Helper()
	if c.scope != wantGitLabScope {
		t.Errorf("%s scope = %q; want gitlab:5", c.method, c.scope.Ref())
	}
	if c.repo != wantGitLabRepo {
		t.Errorf("%s repo = %+v; want {group/sub proj} (LAST-slash split)", c.method, c.repo)
	}
	if c.number != 7 {
		t.Errorf("%s issue number = %d; want 7", c.method, c.number)
	}
}

func assertNoGitHub(t *testing.T, gh *forgeTestGitHub) {
	t.Helper()
	if n := gh.total(); n != 0 {
		t.Errorf("fakeGitHub received %d call(s); a forge-routed run must never touch the GitHub client", n)
	}
}

// ---------------------------------------------------------------------
// (a) Family derivation.
// ---------------------------------------------------------------------

func TestCommentForgeFamily(t *testing.T) {
	cases := []struct {
		name string
		row  *run.Run
		want string
	}{
		{"nil run", nil, "github"},
		{"nil ref", &run.Run{}, "github"},
		{"empty ref", &run.Run{InstallationRef: strPtr("")}, "github"},
		{"bare decimal (GitHub App installation id)", &run.Run{InstallationRef: strPtr("123")}, "github"},
		{"gitlab scheme", &run.Run{InstallationRef: strPtr("gitlab:5")}, "gitlab"},
		{"gitlab_ci kind with nil ref", &run.Run{RunnerKind: run.RunnerKindGitLabCI}, "gitlab"},
		{"gitlab_ci kind wins over a github ref", &run.Run{RunnerKind: run.RunnerKindGitLabCI, InstallationRef: strPtr("123")}, "gitlab"},
		{"empty scheme returned verbatim, not laundered to github", &run.Run{InstallationRef: strPtr(":x")}, ""},
		{"other scheme", &run.Run{InstallationRef: strPtr("bitbucket:ws")}, "bitbucket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentForgeFamily(tc.row); got != tc.want {
				t.Errorf("commentForgeFamily = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestSplitRepoLastSlash(t *testing.T) {
	cases := []struct {
		in   string
		want forge.RepoRef
		ok   bool
	}{
		{"group/sub/proj", forge.RepoRef{Owner: "group/sub", Name: "proj"}, true},
		{"x/y", forge.RepoRef{Owner: "x", Name: "y"}, true},
		{"no-slash", forge.RepoRef{}, false},
		{"/leading", forge.RepoRef{}, false},
		{"trailing/", forge.RepoRef{}, false},
		{"", forge.RepoRef{}, false},
	}
	for _, tc := range cases {
		got, ok := splitRepoLastSlash(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("splitRepoLastSlash(%q) = (%+v, %v); want (%+v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// ---------------------------------------------------------------------
// (b) Edit-in-place on a forge implementing IssueCommentEditor.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitLab_EditInPlace(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeEditor{}
	h := newForgeHarness(t, row, ops)
	ctx := context.Background()

	if err := h.n.NotifyStatusUpdate(ctx, row.ID, "status v1"); err != nil {
		t.Fatalf("first NotifyStatusUpdate: %v", err)
	}
	posts := ops.callsFor("PostIssueCommentWithID")
	if len(posts) != 1 {
		t.Fatalf("first call: PostIssueCommentWithID calls = %d; want 1 (calls: %+v)", len(posts), ops.calls)
	}
	assertGitLabTarget(t, posts[0])
	if posts[0].body != "status v1" {
		t.Errorf("posted body = %q", posts[0].body)
	}
	if got := ops.callsFor("PostIssueComment"); len(got) != 0 {
		t.Errorf("editor-capable forge must post via PostIssueCommentWithID, not PostIssueComment; got %d", len(got))
	}
	rows := h.au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 1 {
		t.Fatalf("status_comment_posted rows = %d; want 1", len(rows))
	}
	if got := rows[0]["github_comment_id"]; got != float64(1) {
		t.Errorf("github_comment_id = %v; want 1 (the id the forge returned)", got)
	}
	if got := rows[0]["forge"]; got != "gitlab" {
		t.Errorf("forge = %v; want gitlab", got)
	}
	if got := rows[0]["comment_mode"]; got != commentModeEditInPlace {
		t.Errorf("comment_mode = %v; want %s", got, commentModeEditInPlace)
	}
	if got := rows[0]["repo"]; got != "group/sub/proj" {
		t.Errorf("repo = %v; want group/sub/proj", got)
	}
	// The FIRST call legitimately lists the thread once (empty audit chain →
	// orphan rediscovery before creating); the second must not list again.
	listsAfterFirst := len(ops.callsFor("FetchIssueComments"))
	if listsAfterFirst != 1 {
		t.Errorf("first call FetchIssueComments calls = %d; want exactly 1 (rediscovery on an empty chain)", listsAfterFirst)
	}

	if err := h.n.NotifyStatusUpdate(ctx, row.ID, "status v2"); err != nil {
		t.Fatalf("second NotifyStatusUpdate: %v", err)
	}
	edits := ops.callsFor("EditIssueComment")
	if len(edits) != 1 {
		t.Fatalf("second call: EditIssueComment calls = %d; want 1 (calls: %+v)", len(edits), ops.calls)
	}
	assertGitLabTarget(t, edits[0])
	if edits[0].commentID != 1 || edits[0].body != "status v2" {
		t.Errorf("edit = %+v; want id 1 body 'status v2'", edits[0])
	}
	if got := ops.callsFor("PostIssueCommentWithID"); len(got) != 1 {
		t.Errorf("second call must EDIT, not post again; PostIssueCommentWithID calls = %d", len(got))
	}
	if got := ops.callsFor("FetchIssueComments"); len(got) != listsAfterFirst {
		t.Errorf("an audited id must not trigger marker rediscovery; FetchIssueComments calls went %d → %d", listsAfterFirst, len(got))
	}
	rows = h.au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 2 || rows[1]["github_comment_id"] != float64(1) || rows[1]["comment_mode"] != commentModeEditInPlace {
		t.Errorf("second audit row = %+v; want id 1, edit_in_place", rows)
	}
	assertNoGitHub(t, h.gh)
}

// ---------------------------------------------------------------------
// (c) Append-only degradation on a forge WITHOUT the editor.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitLab_AppendOnlyDegradation(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{}
	h := newForgeHarness(t, row, ops)
	ctx := context.Background()

	for _, body := range []string{"status v1", "status v2"} {
		if err := h.n.NotifyStatusUpdate(ctx, row.ID, body); err != nil {
			t.Fatalf("NotifyStatusUpdate(%q): %v", body, err)
		}
	}
	posts := ops.callsFor("PostIssueComment")
	if len(posts) != 2 {
		t.Fatalf("PostIssueComment calls = %d; want 2 (one fresh comment per update; calls: %+v)", len(posts), ops.calls)
	}
	for _, p := range posts {
		assertGitLabTarget(t, p)
	}
	if posts[0].body != "status v1" || posts[1].body != "status v2" {
		t.Errorf("bodies = %q, %q", posts[0].body, posts[1].body)
	}
	if got := ops.callsFor("FetchIssueComments"); len(got) != 0 {
		t.Errorf("append-only forge must skip marker rediscovery (nothing could act on a match); FetchIssueComments calls = %d", len(got))
	}
	rows := h.au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 2 {
		t.Fatalf("status_comment_posted rows = %d; want 2", len(rows))
	}
	for i, r := range rows {
		if r["comment_mode"] != commentModeAppendOnly {
			t.Errorf("row %d comment_mode = %v; want %s", i, r["comment_mode"], commentModeAppendOnly)
		}
		if r["forge"] != "gitlab" {
			t.Errorf("row %d forge = %v; want gitlab", i, r["forge"])
		}
		if r["github_comment_id"] != float64(0) {
			t.Errorf("row %d github_comment_id = %v; want 0 (the forge returned no id)", i, r["github_comment_id"])
		}
	}
	assertNoGitHub(t, h.gh)
}

// ---------------------------------------------------------------------
// (d) Deleted-comment fallback keys on forge.ErrNotFound.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitLab_DeletedCommentFallsBackToCreate(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeEditor{nextID: 40, editErr: forge.ErrNotFound}
	h := newForgeHarness(t, row, ops)
	h.au.seed(row.ID, CategoryStatusCommentPosted, map[string]any{
		"kind": "status_update", "github_comment_id": 40, "forge": "gitlab", "comment_mode": "edit_in_place",
	})

	if err := h.n.NotifyStatusUpdate(context.Background(), row.ID, "status v2"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	edits := ops.callsFor("EditIssueComment")
	if len(edits) != 1 || edits[0].commentID != 40 {
		t.Fatalf("EditIssueComment calls = %+v; want exactly one on id 40", edits)
	}
	posts := ops.callsFor("PostIssueCommentWithID")
	if len(posts) != 1 {
		t.Fatalf("PostIssueCommentWithID calls = %d; want 1 fresh create after ErrNotFound", len(posts))
	}
	assertGitLabTarget(t, posts[0])
	rows := h.au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 1 || rows[0]["github_comment_id"] != float64(41) {
		t.Errorf("audit rows = %+v; want one row carrying the NEW id 41", rows)
	}
	assertNoGitHub(t, h.gh)
}

// A NON-ErrNotFound edit failure must NOT fall back to create (that would
// stack duplicate anchors on a transient forge error) — it surfaces.
func TestNotifyStatusUpdate_GitLab_OtherEditErrorSurfaces(t *testing.T) {
	row := gitlabRun()
	boom := errors.New("gitlab: 502")
	ops := &fakeForgeEditor{nextID: 40, editErr: boom}
	h := newForgeHarness(t, row, ops)
	h.au.seed(row.ID, CategoryStatusCommentPosted, map[string]any{"kind": "status_update", "github_comment_id": 40})

	err := h.n.NotifyStatusUpdate(context.Background(), row.ID, "status v2")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v; want the forge edit error surfaced", err)
	}
	if got := ops.callsFor("PostIssueCommentWithID"); len(got) != 0 {
		t.Errorf("a non-NotFound edit error must not create; PostIssueCommentWithID calls = %d", len(got))
	}
	if rows := h.au.rowsFor(CategoryStatusCommentPosted); len(rows) != 0 {
		t.Errorf("no audit row on a failed edit; got %+v", rows)
	}
}

// ---------------------------------------------------------------------
// (e) Orphan rediscovery through FetchIssueComments.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitLab_OrphanRediscovery(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeEditor{}
	ops.comments = []forge.IssueComment{
		{ID: 501, Body: "unrelated comment"},
		{ID: 777, Body: stickyMarker(stickyLocusAnchor, row.ID) + "\nstatus v1"},
	}
	h := newForgeHarness(t, row, ops)

	if err := h.n.NotifyStatusUpdate(context.Background(), row.ID, "status v2"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	lists := ops.callsFor("FetchIssueComments")
	if len(lists) != 1 {
		t.Fatalf("FetchIssueComments calls = %d; want 1 (empty audit chain → rediscover)", len(lists))
	}
	assertGitLabTarget(t, lists[0])
	edits := ops.callsFor("EditIssueComment")
	if len(edits) != 1 || edits[0].commentID != 777 {
		t.Fatalf("EditIssueComment calls = %+v; want exactly one on the marker-bearing id 777", edits)
	}
	if got := ops.callsFor("PostIssueCommentWithID"); len(got) != 0 {
		t.Errorf("a rediscovered orphan must be edited, not re-posted; PostIssueCommentWithID calls = %d", len(got))
	}
	rows := h.au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 1 || rows[0]["github_comment_id"] != float64(777) {
		t.Errorf("audit rows = %+v; want the recovered id 777 re-persisted", rows)
	}
	assertNoGitHub(t, h.gh)
}

// A FetchIssueComments error fails OPEN to create (the pre-#3481 posture).
func TestNotifyStatusUpdate_GitLab_RediscoveryListErrorDegradesToCreate(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeEditor{}
	ops.fetchErr = errors.New("gitlab: 500")
	h := newForgeHarness(t, row, ops)

	if err := h.n.NotifyStatusUpdate(context.Background(), row.ID, "status v1"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if got := ops.callsFor("PostIssueCommentWithID"); len(got) != 1 {
		t.Errorf("PostIssueCommentWithID calls = %d; want 1 (list error → create)", len(got))
	}
	if got := ops.callsFor("EditIssueComment"); len(got) != 0 {
		t.Errorf("EditIssueComment calls = %d; want 0", len(got))
	}
}

// ---------------------------------------------------------------------
// (f) Skip rungs: every non-GitHub-family skip is silent — nil error, zero
// forge calls, zero GitHub calls, zero audit rows.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitLab_SkipRungs(t *testing.T) {
	cases := []struct {
		name string
		row  func() *run.Run
		// deps mutates the Deps before New; nil keeps the default (resolver
		// returning the editor for gitlab).
		deps func(d *Deps, ops forge.IssueOperations)
	}{
		{"ForgeIssueOps nil", gitlabRun, func(d *Deps, _ forge.IssueOperations) { d.ForgeIssueOps = nil }},
		{"resolver returns nil", gitlabRun, func(d *Deps, _ forge.IssueOperations) {
			d.ForgeIssueOps = func(string) forge.IssueOperations { return nil }
		}},
		{"gitlab_ci kind with nil ref", func() *run.Run {
			r := gitlabRun()
			r.RunnerKind = run.RunnerKindGitLabCI
			r.InstallationRef = nil
			return r
		}, nil},
		{"empty ref under gitlab_ci kind", func() *run.Run {
			r := gitlabRun()
			r.RunnerKind = run.RunnerKindGitLabCI
			r.InstallationRef = strPtr("")
			return r
		}, nil},
		{"empty scheme ref (:x) resolves to no forge", func() *run.Run {
			r := gitlabRun()
			r.InstallationRef = strPtr(":x")
			return r
		}, nil},
		{"unparseable trigger ref", func() *run.Run {
			r := gitlabRun()
			r.TriggerRef = strPtr("mr:7")
			return r
		}, nil},
		{"nil trigger ref", func() *run.Run {
			r := gitlabRun()
			r.TriggerRef = nil
			return r
		}, nil},
		{"non-issue-anchored source", func() *run.Run {
			r := gitlabRun()
			r.TriggerSource = run.TriggerCLI
			return r
		}, nil},
		{"repo without a slash", func() *run.Run {
			r := gitlabRun()
			r.Repo = "proj"
			return r
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row()
			ops := &fakeForgeEditor{}
			gh := &forgeTestGitHub{}
			au := &forgeTestAudit{}
			d := Deps{
				GitHub: gh,
				ForgeIssueOps: func(forgeID string) forge.IssueOperations {
					if forgeID != commentFamilyGitLab {
						return nil
					}
					return ops
				},
				Runs:  &forgeTestRuns{row: row},
				Audit: au,
			}
			if tc.deps != nil {
				tc.deps(&d, ops)
			}
			n := New(d)
			if n == nil {
				t.Fatal("notifier nil")
			}
			if err := n.NotifyStatusUpdate(context.Background(), row.ID, "status v1"); err != nil {
				t.Fatalf("NotifyStatusUpdate: %v (a skip rung must be a silent nil)", err)
			}
			if len(ops.calls) != 0 {
				t.Errorf("forge calls = %+v; want none", ops.calls)
			}
			assertNoGitHub(t, gh)
			if len(au.appended) != 0 {
				t.Errorf("audit rows appended = %d; want 0 (a misconfiguration is not a fact about the run)", len(au.appended))
			}
		})
	}
}

// ---------------------------------------------------------------------
// (g) The github family NEVER consults the forge resolver.
// ---------------------------------------------------------------------

func TestNotifyStatusUpdate_GitHub_NeverConsultsForgeOps(t *testing.T) {
	row := githubRun()
	gh := &forgeTestGitHub{}
	au := &forgeTestAudit{}
	n := New(Deps{
		GitHub: gh,
		ForgeIssueOps: func(forgeID string) forge.IssueOperations {
			t.Fatalf("ForgeIssueOps consulted for family %q on a github-family run", forgeID)
			return nil
		},
		Runs:  &forgeTestRuns{row: row},
		Audit: au,
	})
	if n == nil {
		t.Fatal("notifier nil")
	}
	if err := n.NotifyStatusUpdate(context.Background(), row.ID, "status v1"); err != nil {
		t.Fatalf("NotifyStatusUpdate: %v", err)
	}
	if len(gh.creates) != 1 {
		t.Fatalf("GitHub CreateIssueComment calls = %d; want 1", len(gh.creates))
	}
	c := gh.creates[0]
	if c.scope != forge.FromGitHubInstallationID(99) || c.repo != (forge.RepoRef{Owner: "x", Name: "y"}) || c.number != 42 {
		t.Errorf("github create = %+v; want scope installation 99, repo x/y, issue 42", c)
	}
	rows := au.rowsFor(CategoryStatusCommentPosted)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d; want 1", len(rows))
	}
	// The github payload is byte-identical to the pre-#3481 shape: NO forge /
	// comment_mode keys.
	for _, k := range []string{"forge", "comment_mode"} {
		if _, present := rows[0][k]; present {
			t.Errorf("github-family status_comment_posted payload must not carry %q; got %+v", k, rows[0])
		}
	}
	if len(rows[0]) != 4 {
		t.Errorf("github payload keys = %d (%+v); want exactly kind/issue_number/repo/github_comment_id", len(rows[0]), rows[0])
	}
}

// ---------------------------------------------------------------------
// (h) New relaxation: ForgeIssueOps alone constructs; a github-family run
// then skips (no GitHub client) without panicking.
// ---------------------------------------------------------------------

func TestNew_ForgeOpsWithoutGitHub(t *testing.T) {
	ghRow := githubRun()
	au := &forgeTestAudit{}
	ops := &fakeForgeEditor{}
	n := New(Deps{
		ForgeIssueOps: func(string) forge.IssueOperations { return ops },
		Runs:          &forgeTestRuns{row: ghRow},
		Audit:         au,
	})
	if n == nil {
		t.Fatal("New with ForgeIssueOps but no GitHub must construct; got nil")
	}
	if err := n.NotifyStatusUpdate(context.Background(), ghRow.ID, "status v1"); err != nil {
		t.Fatalf("github-family run on a GitHub-less notifier must skip silently; got %v", err)
	}
	if len(ops.calls) != 0 || len(au.appended) != 0 {
		t.Errorf("github-family run must not reach the forge or the audit log; forge calls %+v, audit %d", ops.calls, len(au.appended))
	}

	// And a gitlab-family run on the same GitHub-less notifier posts.
	glRow := gitlabRun()
	n2 := New(Deps{
		ForgeIssueOps: func(string) forge.IssueOperations { return ops },
		Runs:          &forgeTestRuns{row: glRow},
		Audit:         au,
	})
	if err := n2.NotifyStatusUpdate(context.Background(), glRow.ID, "status v1"); err != nil {
		t.Fatalf("gitlab run: %v", err)
	}
	if got := ops.callsFor("PostIssueCommentWithID"); len(got) != 1 {
		t.Errorf("gitlab run on a GitHub-less notifier: PostIssueCommentWithID calls = %d; want 1", len(got))
	}

	// Neither posting client → nil (the existing "no github" TestNew case
	// has no ForgeIssueOps and stays nil).
	if New(Deps{Runs: &forgeTestRuns{}, Audit: au}) != nil {
		t.Error("New with neither GitHub nor ForgeIssueOps must return nil")
	}
}

// ---------------------------------------------------------------------
// (i) The other routed issue-locus surfaces.
// ---------------------------------------------------------------------

func TestNotifyPageClassForRun_GitLab(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{} // pings are one-shot: the plain PostIssueComment suffices
	h := newForgeHarness(t, row, ops)
	h.au.seed(row.ID, "scope_amendment_requested", map[string]any{"paths": []string{"a.go"}})

	if err := h.n.NotifyPageClassForRun(context.Background(), row.ID); err != nil {
		t.Fatalf("NotifyPageClassForRun: %v", err)
	}
	posts := ops.callsFor("PostIssueComment")
	if len(posts) != 1 {
		t.Fatalf("PostIssueComment calls = %d; want 1 ping (calls: %+v)", len(posts), ops.calls)
	}
	assertGitLabTarget(t, posts[0])
	if !strings.Contains(posts[0].body, "scope amendment") {
		t.Errorf("ping body = %q; want the scope-amendment page", posts[0].body)
	}
	if rows := h.au.rowsFor(CategoryAnchorPingPosted); len(rows) != 1 {
		t.Errorf("anchor_ping_posted rows = %d; want 1", len(rows))
	}
	assertNoGitHub(t, h.gh)
}

func TestNotifyCIRetry_GitLab(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{}
	h := newForgeHarness(t, row, ops)

	if err := h.n.NotifyCIRetry(context.Background(), row.ID, uuid.New(), "ci/build", 1, 2); err != nil {
		t.Fatalf("NotifyCIRetry: %v", err)
	}
	posts := ops.callsFor("PostIssueComment")
	if len(posts) != 1 {
		t.Fatalf("PostIssueComment calls = %d; want 1 (calls: %+v)", len(posts), ops.calls)
	}
	assertGitLabTarget(t, posts[0])
	if !strings.Contains(posts[0].body, "ci/build") {
		t.Errorf("ci-retry body = %q; want the check name", posts[0].body)
	}
	rows := h.au.rowsFor(CategoryIssueCommented)
	if len(rows) != 1 || rows[0]["kind"] != string(KindCIRetry) || rows[0]["retry_attempt"] != float64(1) {
		t.Errorf("issue_commented rows = %+v; want one ci_retry attempt 1", rows)
	}
	assertNoGitHub(t, h.gh)
}

// Approval condition 2: the budget-alert surface lands via ops.PostIssueComment
// with scope gitlab:5.
func TestNotifyBudgetAlert_GitLab(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{}
	h := newForgeHarness(t, row, ops)
	warn := 0.8
	p := BudgetAlertPayload{
		WorkflowID: "feature_change", Period: "weekly", PeriodStart: "2026-09-14T00:00:00Z",
		Spent: 42, Limit: 50, Fraction: 0.84, WarnAt: &warn, Tier: "warn",
	}

	posted, err := h.n.NotifyBudgetAlert(context.Background(), row.ID, p)
	if err != nil {
		t.Fatalf("NotifyBudgetAlert: %v", err)
	}
	if !posted {
		t.Fatal("posted = false; want true (the comment landed on the forge)")
	}
	posts := ops.callsFor("PostIssueComment")
	if len(posts) != 1 {
		t.Fatalf("PostIssueComment calls = %d; want 1 (calls: %+v)", len(posts), ops.calls)
	}
	assertGitLabTarget(t, posts[0])
	rows := h.au.rowsFor(CategoryIssueCommented)
	if len(rows) != 1 || rows[0]["kind"] != string(KindBudgetAlert) || rows[0]["budget_tier"] != "warn" {
		t.Errorf("issue_commented rows = %+v; want one budget_alert warn", rows)
	}
	assertNoGitHub(t, h.gh)

	// Per-period/per-tier dedup still holds on the forge path.
	posted, err = h.n.NotifyBudgetAlert(context.Background(), row.ID, p)
	if err != nil || posted {
		t.Errorf("second identical alert: posted=%v err=%v; want a silent dedup skip", posted, err)
	}
	if got := ops.callsFor("PostIssueComment"); len(got) != 1 {
		t.Errorf("dedup: PostIssueComment calls = %d; want still 1", len(got))
	}
}

// Approval condition 2: the generic contextFor → post path (no exported
// caller in the tree, so driven directly) lands on the forge fake with the
// gitlab scope and records the per-kind dedup row.
func TestContextFor_GitLab_GenericPost(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{}
	h := newForgeHarness(t, row, ops)
	ctx := context.Background()

	ctxv, ok, err := h.n.contextFor(ctx, row.ID, KindPlan)
	if err != nil || !ok {
		t.Fatalf("contextFor = (ok=%v, err=%v); want eligible", ok, err)
	}
	if ctxv.family != commentFamilyGitLab || ctxv.ops == nil || ctxv.scope != wantGitLabScope || ctxv.repo != wantGitLabRepo || ctxv.issueNumber != 7 {
		t.Fatalf("contextFor context = %+v; want gitlab family, ops set, scope gitlab:5, repo {group/sub proj}, issue 7", ctxv)
	}
	if err := h.n.post(ctx, ctxv, KindPlan, "generic body"); err != nil {
		t.Fatalf("post: %v", err)
	}
	posts := ops.callsFor("PostIssueComment")
	if len(posts) != 1 || posts[0].body != "generic body" {
		t.Fatalf("PostIssueComment calls = %+v; want one with the generic body", posts)
	}
	assertGitLabTarget(t, posts[0])
	rows := h.au.rowsFor(CategoryIssueCommented)
	if len(rows) != 1 || rows[0]["kind"] != string(KindPlan) || rows[0]["repo"] != "group/sub/proj" {
		t.Errorf("issue_commented rows = %+v; want one plan row for group/sub/proj", rows)
	}
	assertNoGitHub(t, h.gh)

	// The per-kind dedup now suppresses a second contextFor for the same kind.
	if _, ok, err := h.n.contextFor(ctx, row.ID, KindPlan); err != nil || ok {
		t.Errorf("second contextFor(KindPlan) = (ok=%v, err=%v); want deduped skip", ok, err)
	}
}

// editComment on an editor-less forge returns the package sentinel rather
// than panicking or silently posting — the guard NotifyStatusUpdate's
// canEdit gate makes unreachable in practice.
func TestEditComment_ForgeWithoutEditor_ReturnsSentinel(t *testing.T) {
	row := gitlabRun()
	ops := &fakeForgeOps{}
	h := newForgeHarness(t, row, ops)
	ctxv, ok := h.n.resolveCommentTarget(context.Background(), row)
	if !ok {
		t.Fatal("resolveCommentTarget: want eligible")
	}
	if ctxv.canEdit() {
		t.Fatal("canEdit = true for a forge without IssueCommentEditor")
	}
	if _, err := h.n.editComment(context.Background(), ctxv, 1, "x"); !errors.Is(err, errEditUnsupported) {
		t.Errorf("editComment err = %v; want errEditUnsupported", err)
	}
	if len(ops.calls) != 0 {
		t.Errorf("forge calls = %+v; want none", ops.calls)
	}
}
