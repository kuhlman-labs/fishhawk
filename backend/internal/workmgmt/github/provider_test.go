package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// stubTokenProvider mints a fixed installation token so the cross-boundary
// test can distinguish it from the static projects PAT by Authorization
// header value.
type stubTokenProvider struct{ token string }

func (s stubTokenProvider) Token(_ context.Context, _ int64) (string, error) {
	return s.token, nil
}

// fakeAPI records calls and returns canned results so the provider's
// orchestration can be asserted without the wire.
type fakeAPI struct {
	createParams githubclient.CreateIssueParams
	createRepo   githubclient.RepoRef
	createErr    error
	created      *githubclient.CreatedIssue

	fieldsCoord githubclient.ProjectCoord
	fieldsName  string
	fieldsErr   error
	meta        *githubclient.ProjectMeta

	itemStatus          *githubclient.ProjectItemStatus
	itemStatusErr       error
	itemStatusIssueNode string
	itemStatusProjectID string

	addItemContent string
	addItemErr     error
	itemID         string

	setProjectID, setItemID, setFieldID, setOptionID string
	setErr                                           error

	nodeIDNumber int
	nodeIDErr    error
	parentNode   string

	subParent, subChild string
	subErr              error
}

func (f *fakeAPI) CreateIssue(_ context.Context, _ int64, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	f.createRepo, f.createParams = repo, p
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.created, nil
}

func (f *fakeAPI) IssueNodeID(_ context.Context, _ int64, _ githubclient.RepoRef, number int) (string, error) {
	f.nodeIDNumber = number
	if f.nodeIDErr != nil {
		return "", f.nodeIDErr
	}
	return f.parentNode, nil
}

func (f *fakeAPI) ProjectFields(_ context.Context, _ int64, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error) {
	f.fieldsCoord, f.fieldsName = coord, fieldName
	if f.fieldsErr != nil {
		return nil, f.fieldsErr
	}
	return f.meta, nil
}

func (f *fakeAPI) ProjectItemStatus(_ context.Context, _ int64, issueNodeID, projectID, _ string) (*githubclient.ProjectItemStatus, error) {
	f.itemStatusIssueNode, f.itemStatusProjectID = issueNodeID, projectID
	if f.itemStatusErr != nil {
		return nil, f.itemStatusErr
	}
	return f.itemStatus, nil
}

func (f *fakeAPI) AddProjectItem(_ context.Context, _ int64, projectID, contentID string) (string, error) {
	f.addItemContent = contentID
	_ = projectID
	if f.addItemErr != nil {
		return "", f.addItemErr
	}
	return f.itemID, nil
}

func (f *fakeAPI) SetProjectItemSingleSelect(_ context.Context, _ int64, projectID, itemID, fieldID, optionID string) error {
	f.setProjectID, f.setItemID, f.setFieldID, f.setOptionID = projectID, itemID, fieldID, optionID
	return f.setErr
}

func (f *fakeAPI) AddSubIssue(_ context.Context, _ int64, parentNodeID, childNodeID string) error {
	f.subParent, f.subChild = parentNodeID, childNodeID
	return f.subErr
}

func baseRequest() workmgmt.ProviderRequest {
	return workmgmt.ProviderRequest{
		Item: workmgmt.WorkItem{
			Type:           "feature",
			Title:          "[E22.7] do the thing",
			Body:           "## Summary\n\ndo the thing\n",
			Classification: workmgmt.Classification{Labels: []string{"type:feature"}, Complexity: "medium"},
			BoardPlacement: workmgmt.BoardPlacement{Status: "Backlog"},
		},
		Target: workmgmt.Target{
			InstallationID: 99,
			Repo:           workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"},
			Project:        &workmgmt.Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7},
		},
	}
}

func TestProvider_File_FullPath(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 1234, NodeID: "ISSUE_NODE", HTMLURL: "https://github.com/kuhlman-labs/fishhawk/issues/1234"},
		meta:    &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{"Backlog": "OPT_BACKLOG"}},
		itemID:  "ITEM",
	}
	req := baseRequest()
	req.Item.Relations.ParentEpic = "#1005"
	api.parentNode = "EPIC_NODE"

	p := New(api)
	if p.Name() != ProviderName {
		t.Fatalf("Name = %q", p.Name())
	}
	created, err := p.File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	if created.Number != 1234 || created.URL == "" {
		t.Errorf("created = %+v", created)
	}
	// Happy path: both best-effort enrichment steps landed.
	if !created.Boarded || !created.EpicLinked {
		t.Errorf("boarded=%v epic_linked=%v, want both true", created.Boarded, created.EpicLinked)
	}
	if created.BoardingError != "" || created.EpicLinkError != "" {
		t.Errorf("unexpected enrichment errors: boarding=%q epic=%q", created.BoardingError, created.EpicLinkError)
	}
	if api.createParams.Title != "[E22.7] do the thing" || len(api.createParams.Labels) != 1 {
		t.Errorf("create params = %+v", api.createParams)
	}
	if api.fieldsCoord.Number != 7 || api.fieldsName != "Status" {
		t.Errorf("project fields lookup = %+v name=%q", api.fieldsCoord, api.fieldsName)
	}
	if api.addItemContent != "ISSUE_NODE" {
		t.Errorf("add project item content = %q, want ISSUE_NODE", api.addItemContent)
	}
	if api.setOptionID != "OPT_BACKLOG" || api.setFieldID != "FIELD" || api.setItemID != "ITEM" {
		t.Errorf("set field call = proj=%q item=%q field=%q opt=%q", api.setProjectID, api.setItemID, api.setFieldID, api.setOptionID)
	}
	if api.nodeIDNumber != 1005 {
		t.Errorf("parent epic resolved number = %d, want 1005", api.nodeIDNumber)
	}
	if api.subParent != "EPIC_NODE" || api.subChild != "ISSUE_NODE" {
		t.Errorf("sub-issue link = parent=%q child=%q", api.subParent, api.subChild)
	}
}

func TestProvider_File_NoProjectSkipsBoard(t *testing.T) {
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 5, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.Project = nil
	req.Item.Relations = workmgmt.Relations{}

	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Number != 5 {
		t.Errorf("created = %+v", created)
	}
	if api.fieldsName != "" {
		t.Errorf("project fields should not be queried when no project configured")
	}
	// No project configured: nothing to board, and no boarding error.
	if created.Boarded || created.BoardingError != "" {
		t.Errorf("boarded=%v boarding_error=%q, want false with no error", created.Boarded, created.BoardingError)
	}
}

func TestProvider_File_UnknownStatusBestEffort(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 5, NodeID: "N", HTMLURL: "https://x/5"},
		meta:    &githubclient.ProjectMeta{ProjectID: "P", FieldID: "F", StatusOptions: map[string]string{"Done": "OPT"}},
		itemID:  "ITEM",
	}
	req := baseRequest()
	req.Item.Relations = workmgmt.Relations{}
	// Status "Backlog" is not an option on the board. Board placement is
	// best-effort (#1107): the issue is the durable result, so File returns
	// it with a nil error and Boarded=false + a BoardingError naming the
	// cause rather than discarding the created issue.
	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File should not error on a board-placement failure: %v", err)
	}
	if created == nil || created.Number != 5 || created.URL != "https://x/5" {
		t.Fatalf("created item not returned: %+v", created)
	}
	if created.Boarded {
		t.Errorf("boarded = true, want false when status is not a board option")
	}
	if !strings.Contains(created.BoardingError, "is not a Status option") {
		t.Errorf("boarding_error should name the cause, got %q", created.BoardingError)
	}
}

func TestProvider_File_EpicLinkBestEffort(t *testing.T) {
	api := &fakeAPI{
		created: &githubclient.CreatedIssue{Number: 6, NodeID: "N6", HTMLURL: "https://x/6"},
		meta:    &githubclient.ProjectMeta{ProjectID: "P", FieldID: "F", StatusOptions: map[string]string{"Backlog": "OPT"}},
		itemID:  "ITEM",
		subErr:  errors.New("sub-issue API rejected the link"),
	}
	req := baseRequest()
	req.Item.Relations.ParentEpic = "#1005"
	// Epic linking is best-effort: a link failure files the issue (and
	// boards it) with EpicLinked=false and an EpicLinkError naming the cause.
	created, err := New(api).File(context.Background(), req)
	if err != nil {
		t.Fatalf("File should not error on an epic-link failure: %v", err)
	}
	if !created.Boarded {
		t.Errorf("boarded = false, want true (board placement succeeded)")
	}
	if created.EpicLinked {
		t.Errorf("epic_linked = true, want false when the link failed")
	}
	if !strings.Contains(created.EpicLinkError, "sub-issue API rejected the link") {
		t.Errorf("epic_link_error should name the cause, got %q", created.EpicLinkError)
	}
}

func TestProvider_File_CreateIssueErrorPropagates(t *testing.T) {
	api := &fakeAPI{createErr: errors.New("boom")}
	_, err := New(api).File(context.Background(), baseRequest())
	if err == nil || !strings.Contains(err.Error(), "create issue") {
		t.Fatalf("want create-issue error, got %v", err)
	}
}

func TestProvider_File_MissingRepoRejected(t *testing.T) {
	req := baseRequest()
	req.Target.Repo = workmgmt.Repo{}
	_, err := New(&fakeAPI{}).File(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "repo owner and name required") {
		t.Fatalf("want repo-required error, got %v", err)
	}
}

func TestProvider_File_MissingInstallationRejected(t *testing.T) {
	// #1005 concern-2: the run-absent path leaves InstallationID 0; the
	// provider must fail closed with an actionable error naming the v0
	// run-scoped constraint rather than dispatching an untokened REST call.
	api := &fakeAPI{created: &githubclient.CreatedIssue{Number: 1, NodeID: "N", HTMLURL: "u"}}
	req := baseRequest()
	req.Target.InstallationID = 0
	_, err := New(api).File(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "no installation id available") {
		t.Fatalf("want missing-installation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "run-scoped in v0") {
		t.Errorf("error should name the v0 run-scoped constraint: %v", err)
	}
	// Must fail closed before any issue is created.
	if api.createParams.Title != "" {
		t.Errorf("issue should not be created when installation id is absent")
	}
}

func TestParseIssueRef(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"#1005", 1005, false},
		{"1005", 1005, false},
		{" #42 ", 42, false},
		{"abc", 0, true},
		{"#0", 0, true},
	} {
		got, err := parseIssueRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseIssueRef(%q) want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseIssueRef(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

// realClientFixture builds a real *githubclient.Client pointed at an
// httptest mux, recording the Authorization header the REST issue-create
// call and the GraphQL board-placement calls each carried. projectsToken
// empty exercises the #1107 unconfigured path.
type realClientFixture struct {
	restAuth    string
	graphqlAuth string
}

func newRealClient(t *testing.T, projectsToken string) (*githubclient.Client, *realClientFixture) {
	t.Helper()
	fx := &realClientFixture{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		fx.restAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":1234,"node_id":"ISSUE_NODE","html_url":"https://github.com/kuhlman-labs/fishhawk/issues/1234"}`)
	})

	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		fx.graphqlAuth = r.Header.Get("Authorization")
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch {
		case strings.Contains(body.Query, "ProjectFields"):
			_, _ = io.WriteString(w, `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[{"id":"OPT_BACKLOG","name":"Backlog"}]}}}}}`)
		case strings.Contains(body.Query, "AddItem"):
			_, _ = io.WriteString(w, `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`)
		case strings.Contains(body.Query, "SetField"):
			_, _ = io.WriteString(w, `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"ITEM"}}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	c.ProjectsToken = projectsToken
	return c, fx
}

func TestProvider_File_CrossBoundary_ProjectsTokenBoardsUserProject(t *testing.T) {
	// End-to-end seam (config -> client token selection -> provider): a
	// real *githubclient.Client with a projects PAT boards a USER-owned
	// project. The board-placement GraphQL must carry the PAT while the
	// issue-create REST call stays on the installation token (#1114).
	c, fx := newRealClient(t, "pat_projects")
	created, err := New(c).File(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !created.Boarded {
		t.Errorf("boarded = false (%q), want true", created.BoardingError)
	}
	if fx.restAuth != "Bearer ghs_install" {
		t.Errorf("issue-create REST Authorization = %q, want installation token", fx.restAuth)
	}
	if fx.graphqlAuth != "Bearer pat_projects" {
		t.Errorf("board GraphQL Authorization = %q, want projects token", fx.graphqlAuth)
	}
}

func TestProvider_File_CrossBoundary_NoProjectsTokenDegradesBoarded(t *testing.T) {
	// #1107 preserved: with no projects token, a user-owned board placement
	// falls back to the installation token. GitHub answers an installation
	// token's user-Projects GraphQL with "Could not resolve to a ProjectV2",
	// so board placement degrades to boarded:false with a BoardingError — the
	// change is inert until the operator sets the token.
	mux := http.NewServeMux()
	var graphqlAuth string
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":1234,"node_id":"ISSUE_NODE","html_url":"https://x/1234"}`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Could not resolve to a ProjectV2 with the number 7"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := githubclient.New(stubTokenProvider{token: "ghs_install"})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	created, err := New(c).File(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("File should not error on a board-placement failure: %v", err)
	}
	if created.Boarded {
		t.Errorf("boarded = true, want false (#1107 degradation)")
	}
	if created.BoardingError == "" {
		t.Errorf("want a BoardingError naming the cause")
	}
	if graphqlAuth != "Bearer ghs_install" {
		t.Errorf("board GraphQL Authorization = %q, want installation-token fallback", graphqlAuth)
	}
}

// canonicalStates is the conventions states map the transition tests
// resolve canonical states to board options through.
var canonicalStates = map[string]string{
	workmgmt.CanonicalStateBacklog:    "Backlog",
	workmgmt.CanonicalStateInProgress: "In Progress",
	workmgmt.CanonicalStateInReview:   "In Review",
	workmgmt.CanonicalStateBlocked:    "Blocked",
	workmgmt.CanonicalStateDone:       "Done",
}

// transitionAPI returns a fakeAPI primed for a run_started move: the issue
// resolves to a node id, the board exposes every Status option, and the
// issue's current item carries currentStatus.
func transitionAPI(currentStatus string, onBoard bool) *fakeAPI {
	return &fakeAPI{
		parentNode: "ISSUE_NODE",
		meta: &githubclient.ProjectMeta{ProjectID: "PROJ", FieldID: "FIELD", StatusOptions: map[string]string{
			"Backlog": "OPT_BACKLOG", "In Progress": "OPT_IP", "In Review": "OPT_IR", "Blocked": "OPT_BLOCKED", "Done": "OPT_DONE",
		}},
		itemStatus: &githubclient.ProjectItemStatus{OnBoard: onBoard, ItemID: "ITEM", Status: currentStatus},
	}
}

// runStartedRequest is the canonical run_started move: advance to In
// Progress from an expected source of Backlog (unset counts as Backlog).
func runStartedRequest() workmgmt.TransitionRequest {
	return workmgmt.TransitionRequest{
		IssueNumber:          1012,
		Trigger:              "run_started",
		Target:               baseRequest().Target,
		CanonicalState:       workmgmt.CanonicalStateInProgress,
		ExpectedSourceStates: []string{workmgmt.CanonicalStateBacklog},
		States:               canonicalStates,
	}
}

func TestProvider_Transition_MovesFromExpectedSource(t *testing.T) {
	api := transitionAPI("Backlog", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.From != "Backlog" || res.To != "In Progress" {
		t.Fatalf("result = %+v, want moved Backlog->In Progress", res)
	}
	// The Status single-select must be set to the In Progress option on the
	// resolved item — and nothing else (Status-only scope, #1005 split).
	if api.setOptionID != "OPT_IP" || api.setItemID != "ITEM" || api.setFieldID != "FIELD" {
		t.Errorf("set field call = item=%q field=%q opt=%q", api.setItemID, api.setFieldID, api.setOptionID)
	}
	if api.itemStatusIssueNode != "ISSUE_NODE" || api.itemStatusProjectID != "PROJ" {
		t.Errorf("status read = node=%q project=%q", api.itemStatusIssueNode, api.itemStatusProjectID)
	}
	// Transition must never create an issue or touch epic links.
	if api.createParams.Title != "" || api.subParent != "" {
		t.Errorf("transition must not file or link: create=%+v sub=%q", api.createParams, api.subParent)
	}
}

func TestProvider_Transition_UnsetStatusCountsAsBacklog(t *testing.T) {
	// A fresh card with no Status set still advances on run_started, because
	// unset is treated as Backlog (an expected source).
	api := transitionAPI("", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.To != "In Progress" {
		t.Errorf("result = %+v, want moved to In Progress from unset", res)
	}
	if api.setOptionID != "OPT_IP" {
		t.Errorf("set option = %q, want OPT_IP", api.setOptionID)
	}
}

func TestProvider_Transition_NeverFightsHumanParkedCard(t *testing.T) {
	// A human parked the card in Blocked. run_started expects a Backlog
	// source, so the move is SKIPPED with no mutation — never-fight-the-human.
	api := transitionAPI("Blocked", true)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition should not error on a skip: %v", err)
	}
	if res.Moved || !res.Skipped {
		t.Fatalf("result = %+v, want skipped (not moved)", res)
	}
	if res.From != "Blocked" || !strings.Contains(res.SkipReason, "expected source") {
		t.Errorf("skip = from=%q reason=%q", res.From, res.SkipReason)
	}
	if api.setOptionID != "" {
		t.Errorf("Status must not be mutated on a skip, got set opt %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenOffBoard(t *testing.T) {
	api := transitionAPI("", false)
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if res.Moved || !res.Skipped || !strings.Contains(res.SkipReason, "not on the project board") {
		t.Errorf("result = %+v, want off-board skip", res)
	}
	if api.setOptionID != "" {
		t.Errorf("off-board skip must not set Status, got %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenAlreadyAtTarget(t *testing.T) {
	// Idempotency: a card already In Progress is a no-op skip, so a
	// reconciler re-assertion never thrashes the board.
	api := transitionAPI("In Progress", true)
	req := runStartedRequest()
	// In Progress is an acceptable source for the re-assertion too.
	req.ExpectedSourceStates = []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateInProgress}
	res, err := New(api).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if res.Moved || !res.Skipped || !strings.Contains(res.SkipReason, "already at target") {
		t.Errorf("result = %+v, want already-at-target skip", res)
	}
	if api.setOptionID != "" {
		t.Errorf("no mutation expected, got set opt %q", api.setOptionID)
	}
}

func TestProvider_Transition_SkipsWhenNoProject(t *testing.T) {
	req := runStartedRequest()
	req.Target.Project = nil
	res, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "no project configured") {
		t.Errorf("result = %+v, want no-project skip", res)
	}
}

func TestProvider_Transition_SkipsWhenTargetNotABoardOption(t *testing.T) {
	api := transitionAPI("Backlog", true)
	// Drop In Progress from the board's options: the target can't be set.
	delete(api.meta.StatusOptions, "In Progress")
	res, err := New(api).Transition(context.Background(), runStartedRequest())
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "not a Status option") {
		t.Errorf("result = %+v, want target-not-an-option skip", res)
	}
}

func TestProvider_Transition_SkipsWhenCanonicalStateUnmapped(t *testing.T) {
	req := runStartedRequest()
	req.CanonicalState = workmgmt.CanonicalStateDone
	delete(req.States, workmgmt.CanonicalStateDone)
	res, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Skipped || !strings.Contains(res.SkipReason, "no configured provider option") {
		t.Errorf("result = %+v, want unmapped-canonical-state skip", res)
	}
}

func TestProvider_Transition_ResolveErrorsPropagate(t *testing.T) {
	api := transitionAPI("Backlog", true)
	api.nodeIDErr = errors.New("issue gone")
	_, err := New(api).Transition(context.Background(), runStartedRequest())
	if err == nil || !strings.Contains(err.Error(), "resolve issue") {
		t.Fatalf("want resolve-issue error, got %v", err)
	}
}

func TestProvider_Transition_MissingInstallationRejected(t *testing.T) {
	req := runStartedRequest()
	req.Target.InstallationID = 0
	_, err := New(transitionAPI("Backlog", true)).Transition(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "no installation id available") {
		t.Fatalf("want missing-installation error, got %v", err)
	}
}

// Provider must satisfy workmgmt.Provider and the optional board-sync
// capability workmgmt.Transitioner.
var (
	_ workmgmt.Provider     = (*Provider)(nil)
	_ workmgmt.Transitioner = (*Provider)(nil)
)
