package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// projectsFake is a focused fake for the work-management surfaces: REST
// issue create + node-id lookup, and a GraphQL endpoint that dispatches on
// the operation name embedded in the query so one server serves every
// Projects (v2) call.
type projectsFake struct {
	createIssueStatus int
	createIssueBody   string

	getIssueStatus int
	getIssueBody   string

	// graphqlByOp maps a marker substring of the query to its 200 body.
	graphqlByOp map[string]string

	gotCreateBody  []byte
	gotGraphQLVars map[string]map[string]any // op marker -> variables

	// gotGraphQLAuth records the Authorization header of the most recent
	// GraphQL request, so token-selection tests can assert which token
	// (installation vs projects) doGraphQL used.
	gotGraphQLAuth string
}

func newProjectsFake(t *testing.T) (*projectsFake, *Client) {
	t.Helper()
	pf := &projectsFake{graphqlByOp: map[string]string{}, gotGraphQLVars: map[string]map[string]any{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		pf.gotCreateBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(orDefault(pf.createIssueStatus, http.StatusCreated))
		_, _ = io.WriteString(w, pf.createIssueBody)
	})

	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(orDefault(pf.getIssueStatus, http.StatusOK))
		_, _ = io.WriteString(w, pf.getIssueBody)
	})

	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		pf.gotGraphQLAuth = r.Header.Get("Authorization")
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		for marker, resp := range pf.graphqlByOp {
			if strings.Contains(body.Query, marker) {
				pf.gotGraphQLVars[marker] = body.Variables
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, resp)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{}}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return pf, c
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func TestCreateIssue_AppliesLabelsAndReturnsNodeID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.createIssueBody = `{"number":1234,"node_id":"ISSUE_NODE","html_url":"https://github.com/o/r/issues/1234"}`

	got, err := c.CreateIssue(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, CreateIssueParams{
		Title: "boom", Body: "body", Labels: []string{"type:bug", "area:server"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got.Number != 1234 || got.NodeID != "ISSUE_NODE" || got.HTMLURL == "" {
		t.Errorf("created = %+v", got)
	}
	// labels must be on the wire so no separate add-labels round trip is needed.
	var sent struct {
		Title  string   `json:"title"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(pf.gotCreateBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.Title != "boom" || len(sent.Labels) != 2 {
		t.Errorf("sent create body = %+v", sent)
	}
}

func TestCreateIssue_MissingNodeID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.createIssueBody = `{"number":1,"html_url":"u"}`
	_, err := c.CreateIssue(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, CreateIssueParams{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing node_id") {
		t.Fatalf("want missing-node_id error, got %v", err)
	}
}

func TestIssueNodeID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.getIssueBody = `{"node_id":"EPIC_NODE"}`
	got, err := c.IssueNodeID(context.Background(), 7, RepoRef{Owner: "o", Name: "r"}, 1005)
	if err != nil {
		t.Fatalf("IssueNodeID: %v", err)
	}
	if got != "EPIC_NODE" {
		t.Errorf("node id = %q", got)
	}
}

func TestProjectFields_UserOwnerResolvesIDsAndOptions(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ProjectFields"] = `{"data":{"user":{"projectV2":{"id":"PROJ","field":{"id":"FIELD","options":[{"id":"o1","name":"Backlog"},{"id":"o2","name":"Done"}]}}}}}`

	meta, err := c.ProjectFields(context.Background(), 7, ProjectCoord{Owner: "kuhlman-labs", OwnerType: "user", Number: 7}, "Status")
	if err != nil {
		t.Fatalf("ProjectFields: %v", err)
	}
	if meta.ProjectID != "PROJ" || meta.FieldID != "FIELD" {
		t.Errorf("meta = %+v", meta)
	}
	if meta.StatusOptions["Backlog"] != "o1" || meta.StatusOptions["Done"] != "o2" {
		t.Errorf("options = %+v", meta.StatusOptions)
	}
	// The user-owner trap: the query must root at user(login:), not organization.
	if vars := pf.gotGraphQLVars["ProjectFields"]; vars["login"] != "kuhlman-labs" {
		t.Errorf("graphql vars = %+v", vars)
	}
}

func TestProjectFields_OrgOwnerRootsAtOrganization(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ProjectFields"] = `{"data":{"organization":{"projectV2":{"id":"P","field":{"id":"F","options":[{"id":"x","name":"Todo"}]}}}}}`
	meta, err := c.ProjectFields(context.Background(), 7, ProjectCoord{Owner: "acme", OwnerType: "organization", Number: 3}, "Status")
	if err != nil {
		t.Fatalf("ProjectFields: %v", err)
	}
	if meta.ProjectID != "P" || meta.StatusOptions["Todo"] != "x" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestProjectFields_NotFoundWhenProjectNil(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ProjectFields"] = `{"data":{"user":{"projectV2":null}}}`
	_, err := c.ProjectFields(context.Background(), 7, ProjectCoord{Owner: "x", Number: 9}, "Status")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestProjectFields_GraphQLErrorIsValidation(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ProjectFields"] = `{"errors":[{"message":"Could not resolve to a ProjectV2"}]}`
	_, err := c.ProjectFields(context.Background(), 7, ProjectCoord{Owner: "x", Number: 9}, "Status")
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("want graphql validation error, got %v", err)
	}
}

func TestProjectItemStatus_OnBoardMatchesProjectAndReadsStatus(t *testing.T) {
	pf, c := newProjectsFake(t)
	// The issue sits on two projects; only PROJ is ours, and its item is in
	// the In Progress column.
	pf.graphqlByOp["ProjectItemStatus"] = `{"data":{"node":{"projectItems":{"nodes":[
	  {"id":"ITEM_OTHER","project":{"id":"OTHER"},"fieldValueByName":{"name":"Done"}},
	  {"id":"ITEM_OURS","project":{"id":"PROJ"},"fieldValueByName":{"name":"In Progress"}}
	]}}}}`
	got, err := c.ProjectItemStatus(context.Background(), 7, "ISSUE_NODE", "PROJ", "Status")
	if err != nil {
		t.Fatalf("ProjectItemStatus: %v", err)
	}
	if !got.OnBoard || got.ItemID != "ITEM_OURS" || got.Status != "In Progress" {
		t.Errorf("status = %+v", got)
	}
	if vars := pf.gotGraphQLVars["ProjectItemStatus"]; vars["issueId"] != "ISSUE_NODE" || vars["field"] != "Status" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestProjectItemStatus_UnsetStatusReadsEmpty(t *testing.T) {
	pf, c := newProjectsFake(t)
	// On the board but no Status set: fieldValueByName resolves to null.
	pf.graphqlByOp["ProjectItemStatus"] = `{"data":{"node":{"projectItems":{"nodes":[
	  {"id":"ITEM_OURS","project":{"id":"PROJ"},"fieldValueByName":null}
	]}}}}`
	got, err := c.ProjectItemStatus(context.Background(), 7, "ISSUE_NODE", "PROJ", "Status")
	if err != nil {
		t.Fatalf("ProjectItemStatus: %v", err)
	}
	if !got.OnBoard || got.ItemID != "ITEM_OURS" || got.Status != "" {
		t.Errorf("status = %+v, want on-board with empty Status", got)
	}
}

func TestProjectItemStatus_NotOnBoard(t *testing.T) {
	pf, c := newProjectsFake(t)
	// The issue has items, but none on our project → not on board (no error).
	pf.graphqlByOp["ProjectItemStatus"] = `{"data":{"node":{"projectItems":{"nodes":[
	  {"id":"ITEM_OTHER","project":{"id":"OTHER"},"fieldValueByName":{"name":"Done"}}
	]}}}}`
	got, err := c.ProjectItemStatus(context.Background(), 7, "ISSUE_NODE", "PROJ", "Status")
	if err != nil {
		t.Fatalf("ProjectItemStatus: %v", err)
	}
	if got.OnBoard || got.ItemID != "" {
		t.Errorf("status = %+v, want not-on-board", got)
	}
}

func TestProjectItemStatus_ProjectsTokenOptIn(t *testing.T) {
	pf, c := newProjectsFake(t)
	c.ProjectsToken = "pat_projects"
	pf.graphqlByOp["ProjectItemStatus"] = `{"data":{"node":{"projectItems":{"nodes":[
	  {"id":"ITEM_OURS","project":{"id":"PROJ"},"fieldValueByName":{"name":"Backlog"}}
	]}}}}`
	ctx := WithProjectsToken(context.Background())
	if _, err := c.ProjectItemStatus(ctx, 7, "ISSUE_NODE", "PROJ", "Status"); err != nil {
		t.Fatalf("ProjectItemStatus: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer pat_projects" {
		t.Errorf("Authorization = %q, want projects token (user-owned board read)", pf.gotGraphQLAuth)
	}
}

func TestProjectItemStatus_MissingArgs(t *testing.T) {
	_, c := newProjectsFake(t)
	if _, err := c.ProjectItemStatus(context.Background(), 7, "", "PROJ", "Status"); err == nil {
		t.Errorf("want error when issue node id is empty")
	}
	if _, err := c.ProjectItemStatus(context.Background(), 7, "ISSUE_NODE", "PROJ", ""); err == nil {
		t.Errorf("want error when field name is empty")
	}
}

func TestAddProjectItem(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`
	id, err := c.AddProjectItem(context.Background(), 7, "PROJ", "ISSUE_NODE")
	if err != nil {
		t.Fatalf("AddProjectItem: %v", err)
	}
	if id != "ITEM" {
		t.Errorf("item id = %q", id)
	}
	if vars := pf.gotGraphQLVars["AddItem"]; vars["projectId"] != "PROJ" || vars["contentId"] != "ISSUE_NODE" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestSetProjectItemSingleSelect(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["SetField"] = `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"ITEM"}}}}`
	if err := c.SetProjectItemSingleSelect(context.Background(), 7, "PROJ", "ITEM", "FIELD", "OPT"); err != nil {
		t.Fatalf("SetProjectItemSingleSelect: %v", err)
	}
	if vars := pf.gotGraphQLVars["SetField"]; vars["optionId"] != "OPT" || vars["fieldId"] != "FIELD" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestAddSubIssue(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddSubIssue"] = `{"data":{"addSubIssue":{"issue":{"id":"X"}}}}`
	if err := c.AddSubIssue(context.Background(), 7, "PARENT", "CHILD"); err != nil {
		t.Fatalf("AddSubIssue: %v", err)
	}
	if vars := pf.gotGraphQLVars["AddSubIssue"]; vars["issueId"] != "PARENT" || vars["subIssueId"] != "CHILD" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestListSubIssues_PopulatedMapsNodes(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":{"subIssues":{"nodes":[
		{"number":41,"title":"slice A","body":"## Summary","id":"N41","labels":{"nodes":[{"name":"autonomy:low"},{"name":"area:server"}]}},
		{"number":42,"title":"slice B","body":"Depends on: #41","id":"N42","labels":{"nodes":[]}}
	]}}}}`
	subs, err := c.ListSubIssues(context.Background(), 7, "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("subs = %+v, want 2", subs)
	}
	if subs[0].Number != 41 || subs[0].NodeID != "N41" || subs[0].Title != "slice A" {
		t.Errorf("subs[0] = %+v", subs[0])
	}
	if subs[1].Body != "Depends on: #41" {
		t.Errorf("subs[1].Body = %q", subs[1].Body)
	}
	// The labels connection is mapped onto SubIssue.Labels so the campaign
	// source can derive each child's autonomy tier (#1551). An empty labels
	// connection yields a nil/empty slice.
	wantLabels := []string{"autonomy:low", "area:server"}
	if !reflect.DeepEqual(subs[0].Labels, wantLabels) {
		t.Errorf("subs[0].Labels = %v, want %v", subs[0].Labels, wantLabels)
	}
	if len(subs[1].Labels) != 0 {
		t.Errorf("subs[1].Labels = %v, want empty", subs[1].Labels)
	}
	if vars := pf.gotGraphQLVars["ListSubIssues"]; vars["parentId"] != "EPIC_NODE" {
		t.Errorf("vars = %+v, want parentId=EPIC_NODE", vars)
	}
}

func TestListSubIssues_EmptyReturnsNil(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":{"subIssues":{"nodes":[]}}}}`
	subs, err := c.ListSubIssues(context.Background(), 7, "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want empty", subs)
	}
}

func TestListSubIssues_MissingParentRejected(t *testing.T) {
	_, c := newProjectsFake(t)
	if _, err := c.ListSubIssues(context.Background(), 7, ""); err == nil || !strings.Contains(err.Error(), "parent node id required") {
		t.Fatalf("want parent-required error, got %v", err)
	}
}

func TestAddProjectItem_MissingItemID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":""}}}}`
	_, err := c.AddProjectItem(context.Background(), 7, "P", "C")
	if err == nil || !strings.Contains(err.Error(), "missing item id") {
		t.Fatalf("want missing-item-id error, got %v", err)
	}
}

// newSearchFake serves GET /search/issues, recording the q parameter and
// returning a canned status + body.
func newSearchFake(t *testing.T, status int, body string) (*string, *Client) {
	t.Helper()
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(orDefault(status, http.StatusOK))
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return &gotQuery, c
}

func TestSearchOpenIssues_HitMapsFields(t *testing.T) {
	gotQuery, c := newSearchFake(t, http.StatusOK,
		`{"total_count":1,"items":[{"number":42,"html_url":"https://github.com/o/r/issues/42","body":"boom <!-- fishhawk-fingerprint:abc -->"}]}`)
	const q = `repo:o/r is:issue is:open in:body "<!-- fishhawk-fingerprint:abc -->"`
	got, err := c.SearchOpenIssues(context.Background(), 7, q)
	if err != nil {
		t.Fatalf("SearchOpenIssues: %v", err)
	}
	if *gotQuery != q {
		t.Errorf("q parameter = %q, want %q", *gotQuery, q)
	}
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Number != 42 || got[0].HTMLURL != "https://github.com/o/r/issues/42" ||
		!strings.Contains(got[0].Body, "fishhawk-fingerprint:abc") {
		t.Errorf("result = %+v", got[0])
	}
}

func TestSearchOpenIssues_EmptyMiss(t *testing.T) {
	_, c := newSearchFake(t, http.StatusOK, `{"total_count":0,"items":[]}`)
	got, err := c.SearchOpenIssues(context.Background(), 7, "repo:o/r is:issue is:open")
	if err != nil {
		t.Fatalf("SearchOpenIssues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %d, want 0", len(got))
	}
}

func TestSearchOpenIssues_ErrorStatus(t *testing.T) {
	_, c := newSearchFake(t, http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
	_, err := c.SearchOpenIssues(context.Background(), 7, "repo:o/r bad")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

// titleItemsPage renders a search-results body with n synthetic numbered
// titles, numbering them from startNum so multi-page assertions can verify
// every page's items are collected.
func titleItemsPage(startNum, n int) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		num := startNum + i
		_, _ = io.WriteString(&b, fmt.Sprintf(`{"number":%d,"title":"[ADR-%03d] a decision"}`, 1000+num, num))
	}
	b.WriteString(`]}`)
	return b.String()
}

// newPagedSearchFake serves GET /search/issues, returning pageBodies[page]
// keyed by the requested ?page= and recording how many distinct page requests
// arrived (so the 10-page cap can be asserted). A page absent from the map
// serves an empty items list.
func newPagedSearchFake(t *testing.T, status int, pageBodies map[int]string) (*int, *Client) {
	t.Helper()
	var pages int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			if v, err := strconv.Atoi(p); err == nil {
				page = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(orDefault(status, http.StatusOK))
		body, ok := pageBodies[page]
		if !ok {
			body = `{"items":[]}`
		}
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{
		BaseURL: srv.URL,
		Tokens:  &stubTokens{token: "ghs_canned"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
	return &pages, c
}

func TestSearchIssuesByTitle_SinglePageMapsNumberAndTitle(t *testing.T) {
	gotQuery, c := newSearchFake(t, http.StatusOK,
		`{"items":[{"number":865,"title":"[ADR-035] run branch ownership"},{"number":900,"title":"[ADR-040] operator role"}]}`)
	const q = `repo:o/r in:title "[ADR-"`
	got, err := c.SearchIssuesByTitle(context.Background(), 7, q)
	if err != nil {
		t.Fatalf("SearchIssuesByTitle: %v", err)
	}
	if *gotQuery != q {
		t.Errorf("q parameter = %q, want %q", *gotQuery, q)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2", len(got))
	}
	if got[0].Number != 865 || got[0].Title != "[ADR-035] run branch ownership" {
		t.Errorf("result[0] = %+v", got[0])
	}
	if got[1].Title != "[ADR-040] operator role" {
		t.Errorf("result[1] = %+v", got[1])
	}
}

func TestSearchIssuesByTitle_PaginatesAcrossPages(t *testing.T) {
	// A full 100-item first page forces a second fetch; the short second page
	// stops the walk. Every page's items must be collected.
	pages, c := newPagedSearchFake(t, http.StatusOK, map[int]string{
		1: titleItemsPage(1, 100),
		2: titleItemsPage(101, 5),
	})
	got, err := c.SearchIssuesByTitle(context.Background(), 7, `repo:o/r in:title "[ADR-"`)
	if err != nil {
		t.Fatalf("SearchIssuesByTitle: %v", err)
	}
	if len(got) != 105 {
		t.Errorf("results = %d, want 105 (100 + 5 across two pages)", len(got))
	}
	if *pages != 2 {
		t.Errorf("requested %d pages, want 2 (stop on the short page)", *pages)
	}
}

func TestSearchIssuesByTitle_StopsAtPageCap(t *testing.T) {
	// Every page is full (100 items), so only the hard 10-page cap can stop the
	// walk — the GitHub search 1000-result ceiling. Assert it fetches exactly 10
	// pages and no more.
	bodies := map[int]string{}
	for p := 1; p <= 12; p++ {
		bodies[p] = titleItemsPage((p-1)*100+1, 100)
	}
	pages, c := newPagedSearchFake(t, http.StatusOK, bodies)
	got, err := c.SearchIssuesByTitle(context.Background(), 7, `repo:o/r in:title "[ADR-"`)
	if err != nil {
		t.Fatalf("SearchIssuesByTitle: %v", err)
	}
	if *pages != searchByTitleMaxPages {
		t.Errorf("requested %d pages, want the %d-page cap", *pages, searchByTitleMaxPages)
	}
	if len(got) != searchByTitleMaxPages*searchByTitlePerPage {
		t.Errorf("results = %d, want %d (capped)", len(got), searchByTitleMaxPages*searchByTitlePerPage)
	}
}

func TestSearchIssuesByTitle_EmptyMiss(t *testing.T) {
	_, c := newSearchFake(t, http.StatusOK, `{"items":[]}`)
	got, err := c.SearchIssuesByTitle(context.Background(), 7, `repo:o/r in:title "[ADR-"`)
	if err != nil {
		t.Fatalf("SearchIssuesByTitle: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %d, want 0", len(got))
	}
}

func TestSearchIssuesByTitle_ErrorStatus(t *testing.T) {
	_, c := newSearchFake(t, http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
	_, err := c.SearchIssuesByTitle(context.Background(), 7, "repo:o/r bad")
	if err == nil || !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

func TestDoGraphQL_ProjectsTokenSelected(t *testing.T) {
	pf, c := newProjectsFake(t)
	// Tokens stub mints "ghs_canned"; configure a distinct projects PAT so
	// the Authorization header unambiguously identifies the token used.
	c.ProjectsToken = "pat_projects"
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`

	// With the opt-in flag AND a non-empty ProjectsToken, the request must
	// carry the projects PAT, not the installation token.
	ctx := WithProjectsToken(context.Background())
	if _, err := c.AddProjectItem(ctx, 7, "PROJ", "ISSUE_NODE"); err != nil {
		t.Fatalf("AddProjectItem: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer pat_projects" {
		t.Errorf("Authorization = %q, want projects token", pf.gotGraphQLAuth)
	}
}

func TestDoGraphQL_FallsBackToInstallationToken(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`

	// Flag set but ProjectsToken empty → installation-token fallback,
	// preserving the #1107 best-effort path when unconfigured.
	ctx := WithProjectsToken(context.Background())
	if _, err := c.AddProjectItem(ctx, 7, "PROJ", "ISSUE_NODE"); err != nil {
		t.Fatalf("AddProjectItem: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer ghs_canned" {
		t.Errorf("Authorization = %q, want installation token", pf.gotGraphQLAuth)
	}

	// No flag, even with a projects token set → installation token (the
	// flag is the explicit opt-in seam).
	c.ProjectsToken = "pat_projects"
	if _, err := c.AddProjectItem(context.Background(), 7, "PROJ", "ISSUE_NODE"); err != nil {
		t.Fatalf("AddProjectItem (no flag): %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer ghs_canned" {
		t.Errorf("Authorization without flag = %q, want installation token", pf.gotGraphQLAuth)
	}
}
