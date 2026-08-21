package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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

	// graphqlByOpFn maps a marker substring to a responder that computes the
	// 200 body from the request variables — used by the cursor-aware
	// pagination tests to serve a different page keyed by the `after` cursor.
	// It takes precedence over graphqlByOp for a matching marker.
	graphqlByOpFn map[string]func(vars map[string]any) string

	gotCreateBody   []byte
	gotGraphQLVars  map[string]map[string]any // op marker -> last request's variables
	gotGraphQLQuery map[string]string         // op marker -> full query text
	gotGraphQLReqs  map[string]int            // op marker -> request count

	// gotGraphQLAuth records the Authorization header of the most recent
	// GraphQL request, so token-selection tests can assert which token
	// (installation vs projects) doGraphQL used.
	gotGraphQLAuth string
}

func newProjectsFake(t *testing.T) (*projectsFake, *Client) {
	t.Helper()
	pf := &projectsFake{
		graphqlByOp:     map[string]string{},
		graphqlByOpFn:   map[string]func(vars map[string]any) string{},
		gotGraphQLVars:  map[string]map[string]any{},
		gotGraphQLQuery: map[string]string{},
		gotGraphQLReqs:  map[string]int{},
	}
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
		// A cursor-aware responder (graphqlByOpFn) wins over a fixed body so a
		// pagination test can serve a different page keyed by the `after` cursor.
		for marker, fn := range pf.graphqlByOpFn {
			if strings.Contains(body.Query, marker) {
				pf.gotGraphQLVars[marker] = body.Variables
				pf.gotGraphQLQuery[marker] = body.Query
				pf.gotGraphQLReqs[marker]++
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, fn(body.Variables))
				return
			}
		}
		for marker, resp := range pf.graphqlByOp {
			if strings.Contains(body.Query, marker) {
				pf.gotGraphQLVars[marker] = body.Variables
				pf.gotGraphQLQuery[marker] = body.Query
				pf.gotGraphQLReqs[marker]++
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

	got, err := c.CreateIssue(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "o", Name: "r"}, CreateIssueParams{
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
	_, err := c.CreateIssue(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "o", Name: "r"}, CreateIssueParams{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing node_id") {
		t.Fatalf("want missing-node_id error, got %v", err)
	}
}

func TestIssueNodeID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.getIssueBody = `{"node_id":"EPIC_NODE"}`
	got, err := c.IssueNodeID(context.Background(), forge.FromGitHubInstallationID(7), RepoRef{Owner: "o", Name: "r"}, 1005)
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

	meta, err := c.ProjectFields(context.Background(), forge.FromGitHubInstallationID(7), ProjectCoord{Owner: "kuhlman-labs", OwnerType: "user", Number: 7}, "Status")
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
	meta, err := c.ProjectFields(context.Background(), forge.FromGitHubInstallationID(7), ProjectCoord{Owner: "acme", OwnerType: "organization", Number: 3}, "Status")
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
	_, err := c.ProjectFields(context.Background(), forge.FromGitHubInstallationID(7), ProjectCoord{Owner: "x", Number: 9}, "Status")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestProjectFields_GraphQLErrorIsValidation(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ProjectFields"] = `{"errors":[{"message":"Could not resolve to a ProjectV2"}]}`
	_, err := c.ProjectFields(context.Background(), forge.FromGitHubInstallationID(7), ProjectCoord{Owner: "x", Number: 9}, "Status")
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
	got, err := c.ProjectItemStatus(context.Background(), forge.FromGitHubInstallationID(7), "ISSUE_NODE", "PROJ", "Status")
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
	got, err := c.ProjectItemStatus(context.Background(), forge.FromGitHubInstallationID(7), "ISSUE_NODE", "PROJ", "Status")
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
	got, err := c.ProjectItemStatus(context.Background(), forge.FromGitHubInstallationID(7), "ISSUE_NODE", "PROJ", "Status")
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
	if _, err := c.ProjectItemStatus(ctx, forge.FromGitHubInstallationID(7), "ISSUE_NODE", "PROJ", "Status"); err != nil {
		t.Fatalf("ProjectItemStatus: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer pat_projects" {
		t.Errorf("Authorization = %q, want projects token (user-owned board read)", pf.gotGraphQLAuth)
	}
}

func TestProjectItemStatus_MissingArgs(t *testing.T) {
	_, c := newProjectsFake(t)
	if _, err := c.ProjectItemStatus(context.Background(), forge.FromGitHubInstallationID(7), "", "PROJ", "Status"); err == nil {
		t.Errorf("want error when issue node id is empty")
	}
	if _, err := c.ProjectItemStatus(context.Background(), forge.FromGitHubInstallationID(7), "ISSUE_NODE", "PROJ", ""); err == nil {
		t.Errorf("want error when field name is empty")
	}
}

func TestAddProjectItem(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":"ITEM"}}}}`
	id, err := c.AddProjectItem(context.Background(), forge.FromGitHubInstallationID(7), "PROJ", "ISSUE_NODE")
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
	if err := c.SetProjectItemSingleSelect(context.Background(), forge.FromGitHubInstallationID(7), "PROJ", "ITEM", "FIELD", "OPT"); err != nil {
		t.Fatalf("SetProjectItemSingleSelect: %v", err)
	}
	if vars := pf.gotGraphQLVars["SetField"]; vars["optionId"] != "OPT" || vars["fieldId"] != "FIELD" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestAddSubIssue(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddSubIssue"] = `{"data":{"addSubIssue":{"issue":{"id":"X"}}}}`
	if err := c.AddSubIssue(context.Background(), forge.FromGitHubInstallationID(7), "PARENT", "CHILD"); err != nil {
		t.Fatalf("AddSubIssue: %v", err)
	}
	if vars := pf.gotGraphQLVars["AddSubIssue"]; vars["issueId"] != "PARENT" || vars["subIssueId"] != "CHILD" {
		t.Errorf("vars = %+v", vars)
	}
}

func TestListSubIssues_PopulatedMapsNodes(t *testing.T) {
	pf, c := newProjectsFake(t)
	// #41 is OPEN (stateReason null); #42 is CLOSED as COMPLETED — the
	// closed+completed pair the workmgmt provider maps to EpicChild.Complete.
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":{"subIssues":{"nodes":[
		{"number":41,"title":"slice A","body":"## Summary","id":"N41","state":"OPEN","stateReason":null,"labels":{"nodes":[{"name":"type:feature"},{"name":"autonomy:low"}]}},
		{"number":42,"title":"slice B","body":"Depends on: #41","id":"N42","state":"CLOSED","stateReason":"COMPLETED","labels":{"nodes":[]}}
	]}}}}`
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("subs = %+v, want 2", subs)
	}
	if subs[0].Number != 41 || subs[0].NodeID != "N41" || subs[0].Title != "slice A" {
		t.Errorf("subs[0] = %+v", subs[0])
	}
	// State / stateReason decode onto the SubIssue: an OPEN issue's null
	// stateReason lands as "", a CLOSED+COMPLETED issue carries both enums
	// verbatim (uppercase) so the provider can compute Complete (#2120).
	if subs[0].State != "OPEN" || subs[0].StateReason != "" {
		t.Errorf("subs[0] state/reason = %q/%q, want OPEN/\"\"", subs[0].State, subs[0].StateReason)
	}
	if subs[1].State != "CLOSED" || subs[1].StateReason != "COMPLETED" {
		t.Errorf("subs[1] state/reason = %q/%q, want CLOSED/COMPLETED", subs[1].State, subs[1].StateReason)
	}
	// Labels decode alongside the existing number/title/body/id fields; an
	// empty labels connection yields a nil Labels slice (#1551).
	if got := strings.Join(subs[0].Labels, ","); got != "type:feature,autonomy:low" {
		t.Errorf("subs[0].Labels = %q, want type:feature,autonomy:low", got)
	}
	if len(subs[1].Labels) != 0 {
		t.Errorf("subs[1].Labels = %+v, want empty", subs[1].Labels)
	}
	if subs[1].Body != "Depends on: #41" {
		t.Errorf("subs[1].Body = %q", subs[1].Body)
	}
	if vars := pf.gotGraphQLVars["ListSubIssues"]; vars["parentId"] != "EPIC_NODE" {
		t.Errorf("vars = %+v, want parentId=EPIC_NODE", vars)
	}
	// The query must request the labels connection so the tier is on the wire.
	if q := pf.gotGraphQLQuery["ListSubIssues"]; !strings.Contains(q, "labels(first:") {
		t.Errorf("ListSubIssues query does not request labels: %q", q)
	}
	// The query must select state + stateReason so the completion signal
	// reaches the campaign subset filter (#2120).
	if q := pf.gotGraphQLQuery["ListSubIssues"]; !strings.Contains(q, "state") || !strings.Contains(q, "stateReason") {
		t.Errorf("ListSubIssues query does not request state/stateReason: %q", q)
	}
}

// TestListSubIssues_AutonomyLabelBeyondFirst20 proves the labels connection is
// requested with a page size (first:100) large enough to capture an
// autonomy:low tier that sits past the first 20 labels — GitHub caps an issue
// at 100 labels, so first:100 is complete by construction and a beyond-20 tier
// can never be silently dropped (which would resolve Autonomy="" and
// auto-dispatch a human-led item, the exact risk #1551 closes).
func TestListSubIssues_AutonomyLabelBeyondFirst20(t *testing.T) {
	pf, c := newProjectsFake(t)
	// 24 filler labels, then autonomy:low as the 25th — past a first:20 page.
	nodes := make([]string, 0, 25)
	for i := 0; i < 24; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"name":"filler:%d"}`, i))
	}
	nodes = append(nodes, `{"name":"autonomy:low"}`)
	pf.graphqlByOp["ListSubIssues"] = fmt.Sprintf(
		`{"data":{"node":{"subIssues":{"nodes":[{"number":41,"title":"slice A","body":"b","id":"N41","labels":{"nodes":[%s]}}]}}}}`,
		strings.Join(nodes, ","))
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 1 || len(subs[0].Labels) != 25 {
		t.Fatalf("subs = %+v, want 1 child with 25 labels", subs)
	}
	// The autonomy tier past label 20 must survive the decode so the workmgmt
	// provider resolves Autonomy=="low" rather than "".
	var hasLow bool
	for _, l := range subs[0].Labels {
		if l == "autonomy:low" {
			hasLow = true
		}
	}
	if !hasLow {
		t.Errorf("subs[0].Labels = %v, want it to contain autonomy:low", subs[0].Labels)
	}
	// The wire query must request first:100 labels — GitHub's per-issue max —
	// so no label (and thus no autonomy tier) is ever paged out.
	if vars := pf.gotGraphQLVars["ListSubIssues"]; vars["labelsFirst"] != float64(100) {
		t.Errorf("labelsFirst = %v, want 100 (GitHub's max labels per issue)", vars["labelsFirst"])
	}
}

func TestListSubIssues_EmptyReturnsNil(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":{"subIssues":{"nodes":[]}}}}`
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want empty", subs)
	}
}

// TestListSubIssues_NilNodeFirstPageReturnsNil proves a nil `node` on the
// FIRST request (the issue resolves to no sub-issues connection) is the
// no-children early return — nil result, no error — not a fail-closed error.
func TestListSubIssues_NilNodeFirstPageReturnsNil(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":null}}`
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want empty (nil node, no children)", subs)
	}
	if pf.gotGraphQLReqs["ListSubIssues"] != 1 {
		t.Errorf("requests = %d, want 1 (no second page on a nil first-page node)", pf.gotGraphQLReqs["ListSubIssues"])
	}
}

func TestListSubIssues_MissingParentRejected(t *testing.T) {
	_, c := newProjectsFake(t)
	if _, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), ""); err == nil || !strings.Contains(err.Error(), "parent node id required") {
		t.Fatalf("want parent-required error, got %v", err)
	}
}

// TestListSubIssues_PaginatesAcrossPages proves the accumulation loop threads
// the cursor: a two-page connection returns every node from both pages, and
// page 2's request carries page 1's endCursor as `after` (criterion 1). A
// child overflowing the first :100 page — and its depends_on edges — would
// vanish without this, silently truncating the campaign DAG.
func TestListSubIssues_PaginatesAcrossPages(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOpFn["ListSubIssues"] = func(vars map[string]any) string {
		// Page 1 (after: null/absent) advertises more pages via endCursor "C1";
		// page 2 (after: "C1") is the last page (hasNextPage:false).
		if after, _ := vars["after"].(string); after == "C1" {
			return `{"data":{"node":{"subIssues":{
				"pageInfo":{"hasNextPage":false,"endCursor":"C2"},
				"nodes":[{"number":142,"title":"slice B","body":"Depends on: #41","id":"N142","state":"OPEN","stateReason":null,"labels":{"nodes":[]}}]
			}}}}`
		}
		return `{"data":{"node":{"subIssues":{
			"pageInfo":{"hasNextPage":true,"endCursor":"C1"},
			"nodes":[{"number":41,"title":"slice A","body":"## Summary","id":"N41","state":"OPEN","stateReason":null,"labels":{"nodes":[{"name":"autonomy:low"}]}}]
		}}}}`
	}
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	// Both pages' nodes accumulate, in page order.
	if len(subs) != 2 || subs[0].Number != 41 || subs[1].Number != 142 {
		t.Fatalf("subs = %+v, want [41 142] across two pages", subs)
	}
	if subs[1].Body != "Depends on: #41" {
		t.Errorf("subs[1].Body = %q, want the overflow child's depends_on marker preserved", subs[1].Body)
	}
	// Exactly two GraphQL requests: page 1 then page 2 (no extra round trip).
	if pf.gotGraphQLReqs["ListSubIssues"] != 2 {
		t.Errorf("ListSubIssues requests = %d, want 2 (two-page walk)", pf.gotGraphQLReqs["ListSubIssues"])
	}
	// The LAST request (page 2) must carry page 1's endCursor as `after`.
	if vars := pf.gotGraphQLVars["ListSubIssues"]; vars["after"] != "C1" {
		t.Errorf("page 2 after = %v, want C1 (page 1's endCursor threaded as the next cursor)", vars["after"])
	}
}

// TestListSubIssues_SingleRequestWhenNoNextPage proves the common case issues
// exactly one GraphQL request: a single page reporting hasNextPage:false does
// not make a second, cursorless round trip (criterion 5).
func TestListSubIssues_SingleRequestWhenNoNextPage(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListSubIssues"] = `{"data":{"node":{"subIssues":{
		"pageInfo":{"hasNextPage":false,"endCursor":"C1"},
		"nodes":[{"number":41,"title":"slice A","body":"b","id":"N41","state":"OPEN","stateReason":null,"labels":{"nodes":[]}}]
	}}}}`
	subs, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err != nil {
		t.Fatalf("ListSubIssues: %v", err)
	}
	if len(subs) != 1 || subs[0].Number != 41 {
		t.Fatalf("subs = %+v, want the single child", subs)
	}
	if pf.gotGraphQLReqs["ListSubIssues"] != 1 {
		t.Errorf("ListSubIssues requests = %d, want exactly 1 (hasNextPage:false common case)", pf.gotGraphQLReqs["ListSubIssues"])
	}
	// The first request's cursor is nil (from-the-start), not a stray value.
	if vars := pf.gotGraphQLVars["ListSubIssues"]; vars["after"] != nil {
		t.Errorf("first request after = %v, want nil (start of the connection)", vars["after"])
	}
}

// TestListSubIssues_BoundedByPageCap proves the loop is bounded: a connection
// that ALWAYS reports hasNextPage:true fails closed at the page cap with an
// actionable error (naming the parent node id + accumulated count) rather than
// spinning forever (criterion 2). The request count is bounded by the cap.
func TestListSubIssues_BoundedByPageCap(t *testing.T) {
	pf, c := newProjectsFake(t)
	// Every page advertises another page, so only the hard cap can stop the walk.
	pf.graphqlByOpFn["ListSubIssues"] = func(vars map[string]any) string {
		return `{"data":{"node":{"subIssues":{
			"pageInfo":{"hasNextPage":true,"endCursor":"CURSOR"},
			"nodes":[{"number":1,"title":"t","body":"b","id":"N1","state":"OPEN","stateReason":null,"labels":{"nodes":[]}}]
		}}}}`
	}
	_, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err == nil || !strings.Contains(err.Error(), "exceeded the") || !strings.Contains(err.Error(), "EPIC_NODE") {
		t.Fatalf("want a fail-closed cap error naming the parent node, got %v", err)
	}
	// The error must name the accumulated count so campaign assembly can diagnose.
	if !strings.Contains(err.Error(), "children") {
		t.Errorf("cap error should name the accumulated child count, got %v", err)
	}
	if got := pf.gotGraphQLReqs["ListSubIssues"]; got != listSubIssuesMaxPages || got > listSubIssuesMaxPages+1 {
		t.Errorf("requests = %d, want the %d-page cap (bounded, <= cap+1)", got, listSubIssuesMaxPages)
	}
}

// TestListSubIssues_NilNodeMidPaginationFailsClosed proves a node that decodes
// non-nil on page 1 but nil on a later page is a fail-closed error naming the
// parent node — not a silently-truncated slice of the pages seen so far.
func TestListSubIssues_NilNodeMidPaginationFailsClosed(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOpFn["ListSubIssues"] = func(vars map[string]any) string {
		if after, _ := vars["after"].(string); after == "C1" {
			// Page 2: the node came back null (anomaly) while page 1 promised more.
			return `{"data":{"node":null}}`
		}
		return `{"data":{"node":{"subIssues":{
			"pageInfo":{"hasNextPage":true,"endCursor":"C1"},
			"nodes":[{"number":41,"title":"slice A","body":"b","id":"N41","state":"OPEN","stateReason":null,"labels":{"nodes":[]}}]
		}}}}`
	}
	_, err := c.ListSubIssues(context.Background(), forge.FromGitHubInstallationID(7), "EPIC_NODE")
	if err == nil || !strings.Contains(err.Error(), "nil node mid-pagination") || !strings.Contains(err.Error(), "EPIC_NODE") {
		t.Fatalf("want a fail-closed nil-node-mid-pagination error naming the parent, got %v", err)
	}
}

func TestAddProjectItem_MissingItemID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":""}}}}`
	_, err := c.AddProjectItem(context.Background(), forge.FromGitHubInstallationID(7), "P", "C")
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
	got, err := c.SearchOpenIssues(context.Background(), forge.FromGitHubInstallationID(7), q)
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
	got, err := c.SearchOpenIssues(context.Background(), forge.FromGitHubInstallationID(7), "repo:o/r is:issue is:open")
	if err != nil {
		t.Fatalf("SearchOpenIssues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %d, want 0", len(got))
	}
}

func TestSearchOpenIssues_ErrorStatus(t *testing.T) {
	_, c := newSearchFake(t, http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
	_, err := c.SearchOpenIssues(context.Background(), forge.FromGitHubInstallationID(7), "repo:o/r bad")
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
	got, err := c.SearchIssuesByTitle(context.Background(), forge.FromGitHubInstallationID(7), q)
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
	got, err := c.SearchIssuesByTitle(context.Background(), forge.FromGitHubInstallationID(7), `repo:o/r in:title "[ADR-"`)
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
	got, err := c.SearchIssuesByTitle(context.Background(), forge.FromGitHubInstallationID(7), `repo:o/r in:title "[ADR-"`)
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
	got, err := c.SearchIssuesByTitle(context.Background(), forge.FromGitHubInstallationID(7), `repo:o/r in:title "[ADR-"`)
	if err != nil {
		t.Fatalf("SearchIssuesByTitle: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("results = %d, want 0", len(got))
	}
}

func TestSearchIssuesByTitle_ErrorStatus(t *testing.T) {
	_, c := newSearchFake(t, http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`)
	_, err := c.SearchIssuesByTitle(context.Background(), forge.FromGitHubInstallationID(7), "repo:o/r bad")
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
	if _, err := c.AddProjectItem(ctx, forge.FromGitHubInstallationID(7), "PROJ", "ISSUE_NODE"); err != nil {
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
	if _, err := c.AddProjectItem(ctx, forge.FromGitHubInstallationID(7), "PROJ", "ISSUE_NODE"); err != nil {
		t.Fatalf("AddProjectItem: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer ghs_canned" {
		t.Errorf("Authorization = %q, want installation token", pf.gotGraphQLAuth)
	}

	// No flag, even with a projects token set → installation token (the
	// flag is the explicit opt-in seam).
	c.ProjectsToken = "pat_projects"
	if _, err := c.AddProjectItem(context.Background(), forge.FromGitHubInstallationID(7), "PROJ", "ISSUE_NODE"); err != nil {
		t.Fatalf("AddProjectItem (no flag): %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer ghs_canned" {
		t.Errorf("Authorization without flag = %q, want installation token", pf.gotGraphQLAuth)
	}
}

// repoIssueNode renders one repository.issues node with a labels connection
// and, when projectItems is non-empty, the nested project-items selection.
func repoIssueNode(number int, projectItems string) string {
	items := ""
	if projectItems != "" {
		items = `,"projectItems":{"nodes":[` + projectItems + `]}`
	}
	return fmt.Sprintf(`{"number":%d,"title":"issue %d","url":"https://github.com/o/r/issues/%d","body":"body %d","state":"OPEN","stateReason":null,"labels":{"nodes":[{"name":"type:feature"}]}%s}`,
		number, number, number, number, items)
}

// TestListRepoIssues_PaginatesBeyondOneHundred is the acceptance-criterion-2
// beyond-the-cap fixture at the TRANSPORT layer: 150 issues served as two
// cursor pages (100 + 50) must all come back, ascending by number, with the
// per-issue fields decoded. A single-page read (the truncation this issue
// exists to eliminate) would return 100.
func TestListRepoIssues_PaginatesBeyondOneHundred(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOpFn["ListRepoIssues"] = func(vars map[string]any) string {
		if after, _ := vars["after"].(string); after == "C1" {
			var nodes []string
			for n := 101; n <= 150; n++ {
				nodes = append(nodes, repoIssueNode(n, ""))
			}
			return `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":"C2"},"nodes":[` +
				strings.Join(nodes, ",") + `]}}}}`
		}
		var nodes []string
		for n := 1; n <= 100; n++ {
			nodes = append(nodes, repoIssueNode(n, ""))
		}
		return `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[` +
			strings.Join(nodes, ",") + `]}}}}`
	}
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{})
	if err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	if len(issues) != 150 {
		t.Fatalf("issues = %d, want 150 across two cursor pages (a one-page read returns 100)", len(issues))
	}
	for i, iss := range issues {
		if iss.Number != i+1 {
			t.Fatalf("issues[%d].Number = %d, want %d (ascending by number)", i, iss.Number, i+1)
		}
	}
	last := issues[149]
	if last.Title != "issue 150" || last.URL != "https://github.com/o/r/issues/150" || last.Body != "body 150" {
		t.Errorf("last issue = %+v, want title/url/body decoded", last)
	}
	if last.State != "OPEN" || last.StateReason != "" {
		t.Errorf("last issue state = %q/%q, want OPEN with an empty stateReason", last.State, last.StateReason)
	}
	if len(last.Labels) != 1 || last.Labels[0] != "type:feature" {
		t.Errorf("last issue labels = %v, want [type:feature]", last.Labels)
	}
	if pf.gotGraphQLReqs["ListRepoIssues"] != 2 {
		t.Errorf("requests = %d, want 2 (two-page walk)", pf.gotGraphQLReqs["ListRepoIssues"])
	}
	if vars := pf.gotGraphQLVars["ListRepoIssues"]; vars["after"] != "C1" {
		t.Errorf("page 2 after = %v, want C1 (page 1's endCursor threaded)", vars["after"])
	}
}

// TestListRepoIssues_QueriesIssuesNotBoardItems is the DONE-MEANS behavioral
// test for the enumeration rule (#1169): the rule is a convention compilation
// cannot enforce, so assert on the GraphQL DOCUMENT the server received. A
// rewrite to a ProjectV2 board item list would introduce a `projectV2(`
// selection and fail here, where a comment-only touch would pass.
func TestListRepoIssues_QueriesIssuesNotBoardItems(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`
	if _, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{ProjectID: "PROJ", WantBoardStatus: true}); err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	q := pf.gotGraphQLQuery["ListRepoIssues"]
	if !strings.Contains(q, "repository(") || !strings.Contains(q, "issues(") {
		t.Fatalf("query must enumerate repository issues, got:\n%s", q)
	}
	// A board ITEM-LIST enumeration roots at a projectV2 node
	// (user(login:){projectV2(number:){items(first:…)}}). Its absence is the
	// discriminator; the `... on ProjectV2ItemFieldSingleSelectValue` fragment
	// used to READ a status is not a `projectV2(` selection.
	if strings.Contains(strings.ToLower(q), "projectv2(") {
		t.Errorf("query enumerates a ProjectV2 board item list, which caps and truncates silently; enumerate repository.issues instead:\n%s", q)
	}
	if !strings.Contains(q, "orderBy: {field: NUMBER, direction: ASC}") {
		t.Errorf("query must order ascending by number for a deterministic walk, got:\n%s", q)
	}
}

// TestListRepoIssues_PageCapFailsClosed proves the walk is bounded and fails
// CLOSED: a connection that always reports hasNextPage:true errors at the cap
// naming the repo and the accumulated count, and returns NO partial slice.
func TestListRepoIssues_PageCapFailsClosed(t *testing.T) {
	pf, c := newProjectsFake(t)
	// TWO nodes per page, so the accumulated issue count (200) differs from the
	// page cap (100) and from the per-page node count: an assertion on the
	// count cannot be satisfied by either of the other numbers in the message.
	pf.graphqlByOpFn["ListRepoIssues"] = func(map[string]any) string {
		return `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":true,"endCursor":"CURSOR"},"nodes":[` +
			repoIssueNode(1, "") + `,` + repoIssueNode(2, "") + `]}}}}`
	}
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, ListRepoIssuesOptions{})
	if err == nil {
		t.Fatalf("want a fail-closed cap error, got %d issues and nil error", len(issues))
	}
	if issues != nil {
		t.Errorf("issues = %d entries alongside the error, want NO partial slice", len(issues))
	}
	if !strings.Contains(err.Error(), "kuhlman-labs/fishhawk") || !strings.Contains(err.Error(), "exceeded the") {
		t.Errorf("cap error must name the repo, got %v", err)
	}
	// Discriminating: the exact accumulated count, not the bare word "issues"
	// (which the fixed "list issues" prefix already supplies).
	if wantCount := fmt.Sprintf("accumulating %d issues", 2*listRepoIssuesMaxPages); !strings.Contains(err.Error(), wantCount) {
		t.Errorf("cap error must name the accumulated issue count %q, got %v", wantCount, err)
	}
	if got := pf.gotGraphQLReqs["ListRepoIssues"]; got != listRepoIssuesMaxPages {
		t.Errorf("requests = %d, want the %d-page cap (bounded)", got, listRepoIssuesMaxPages)
	}
}

// TestListRepoIssues_ForbiddenClassified proves a 403 surfaces as the
// ErrForbidden sentinel, which is what the provider wraps into a typed
// ReasonForbidden while keeping errors.Is matching.
func TestListRepoIssues_ForbiddenClassified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Tokens: &stubTokens{token: "ghs"}, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// TestListRepoIssues_ForwardsFiltersAndReadsBoardStatus proves the label/state
// filters reach the GraphQL variables verbatim and that the per-issue board
// status is read off the item whose project node id matches the target.
func TestListRepoIssues_ForwardsFiltersAndReadsBoardStatus(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
		repoIssueNode(7, `{"project":{"id":"OTHER"},"fieldValueByName":{"name":"Done"}},{"project":{"id":"PROJ"},"fieldValueByName":{"name":"In Progress"}}`) + `,` +
		repoIssueNode(8, `{"project":{"id":"OTHER"},"fieldValueByName":{"name":"Done"}}`) + `]}}}}`
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{
			Labels: []string{"type:feature", "area:backend"}, States: []string{"OPEN"},
			ProjectID: "PROJ", WantBoardStatus: true,
		})
	if err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2", len(issues))
	}
	// The matching item's status wins; the other board's item is ignored.
	if !issues[0].OnBoard || issues[0].BoardStatus != "In Progress" {
		t.Errorf("issue 7 board = %v/%q, want on-board In Progress from the TARGET project's item", issues[0].OnBoard, issues[0].BoardStatus)
	}
	// An issue on a DIFFERENT board only, with the page NOT full, is off-board.
	if issues[1].OnBoard || issues[1].BoardStatus != "" {
		t.Errorf("issue 8 board = %v/%q, want off-board (its only item is another project)", issues[1].OnBoard, issues[1].BoardStatus)
	}
	vars := pf.gotGraphQLVars["ListRepoIssues"]
	gotLabels, _ := vars["labels"].([]any)
	if len(gotLabels) != 2 || gotLabels[0] != "type:feature" || gotLabels[1] != "area:backend" {
		t.Errorf("labels variable = %v, want the caller's labels forwarded verbatim", vars["labels"])
	}
	gotStates, _ := vars["states"].([]any)
	if len(gotStates) != 1 || gotStates[0] != "OPEN" {
		t.Errorf("states variable = %v, want [OPEN]", vars["states"])
	}
}

// TestListRepoIssues_UnfilteredSendsNullVariables proves an unfiltered call
// encodes `labels`/`states` as GraphQL null ("no filter") rather than an empty
// list, which a connection argument can read as "match nothing".
//
// The options carry EXPLICITLY EMPTY, non-nil slices — the shape a caller that
// built a filter and then found it empty produces. A nil slice would marshal
// to null on its own, so an all-nil fixture could not tell the nilIfEmpty
// control apart from its absence.
func TestListRepoIssues_UnfilteredSendsNullVariables(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`
	if _, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{Labels: []string{}, States: []string{}}); err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	vars := pf.gotGraphQLVars["ListRepoIssues"]
	if vars["labels"] != nil || vars["states"] != nil {
		t.Errorf("labels/states = %v/%v, want null (no filter)", vars["labels"], vars["states"])
	}
	// Board status was not requested, so the projectItems selection is absent.
	if strings.Contains(pf.gotGraphQLQuery["ListRepoIssues"], "projectItems(") {
		t.Errorf("query selects projectItems without a board-status request:\n%s", pf.gotGraphQLQuery["ListRepoIssues"])
	}
}

// TestListRepoIssues_TruncatedProjectItemsFailsClosed is the condition-C3
// fixture: an issue whose projectItems page comes back FULL and does NOT carry
// the target project has UNDECIDABLE board membership — the item may sit
// beyond the page. Reporting OnBoard=false there would be a silent wrong
// answer for an issue that IS on the board, so the read must fail closed.
func TestListRepoIssues_TruncatedProjectItemsFailsClosed(t *testing.T) {
	pf, c := newProjectsFake(t)
	var items []string
	for i := 0; i < listRepoIssuesProjectItemsFirst; i++ {
		items = append(items, fmt.Sprintf(`{"project":{"id":"OTHER_%d"},"fieldValueByName":null}`, i))
	}
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
		repoIssueNode(42, strings.Join(items, ",")) + `]}}}}`
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{ProjectID: "PROJ", WantBoardStatus: true})
	if err == nil {
		t.Fatalf("want a fail-closed truncation error; got issues = %+v", issues)
	}
	if issues != nil {
		t.Errorf("issues = %+v alongside the error, want NO result", issues)
	}
	if !strings.Contains(err.Error(), "#42") || !strings.Contains(err.Error(), "undecidable") {
		t.Errorf("error must name the issue and the undecidable membership, got %v", err)
	}
}

// TestListRepoIssues_FullProjectItemsPageContainingTargetIsFine proves the
// truncation guard is not over-broad: a FULL page that DOES carry the target
// project resolves normally.
func TestListRepoIssues_FullProjectItemsPageContainingTargetIsFine(t *testing.T) {
	pf, c := newProjectsFake(t)
	items := []string{`{"project":{"id":"PROJ"},"fieldValueByName":{"name":"Backlog"}}`}
	for i := 1; i < listRepoIssuesProjectItemsFirst; i++ {
		items = append(items, fmt.Sprintf(`{"project":{"id":"OTHER_%d"},"fieldValueByName":null}`, i))
	}
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
		repoIssueNode(42, strings.Join(items, ",")) + `]}}}}`
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{ProjectID: "PROJ", WantBoardStatus: true})
	if err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	if len(issues) != 1 || !issues[0].OnBoard || issues[0].BoardStatus != "Backlog" {
		t.Fatalf("issues = %+v, want one on-board Backlog item", issues)
	}
}

// TestListRepoIssues_NilRepositoryFailsClosed proves an invisible repo is a
// hard error, not an empty (and misreadable) issue set.
func TestListRepoIssues_NilRepositoryFailsClosed(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":null}}`
	issues, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{})
	if err == nil || issues != nil {
		t.Fatalf("want a fail-closed nil-repository error, got %+v / %v", issues, err)
	}
	if !strings.Contains(err.Error(), "nil node") {
		t.Errorf("error should name the nil repository node, got %v", err)
	}
}

// TestListRepoIssues_MissingRepoRejected proves the argument guard.
func TestListRepoIssues_MissingRepoRejected(t *testing.T) {
	_, c := newProjectsFake(t)
	if _, err := c.ListRepoIssues(context.Background(), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o"}, ListRepoIssuesOptions{}); err == nil {
		t.Fatal("want an error for a missing repo name")
	}
}

// TestListRepoIssues_ProjectsTokenOptIn proves the enumeration honours the
// user-owned-board token opt-in, since it routes through doGraphQL.
func TestListRepoIssues_ProjectsTokenOptIn(t *testing.T) {
	pf, c := newProjectsFake(t)
	c.ProjectsToken = "pat_projects"
	pf.graphqlByOp["ListRepoIssues"] = `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`
	if _, err := c.ListRepoIssues(WithProjectsToken(context.Background()), forge.FromGitHubInstallationID(7),
		RepoRef{Owner: "o", Name: "r"}, ListRepoIssuesOptions{}); err != nil {
		t.Fatalf("ListRepoIssues: %v", err)
	}
	if pf.gotGraphQLAuth != "Bearer pat_projects" {
		t.Errorf("Authorization = %q, want the static projects token", pf.gotGraphQLAuth)
	}
}
