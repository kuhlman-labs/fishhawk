package githubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestAddProjectItem_MissingItemID(t *testing.T) {
	pf, c := newProjectsFake(t)
	pf.graphqlByOp["AddItem"] = `{"data":{"addProjectV2ItemById":{"item":{"id":""}}}}`
	_, err := c.AddProjectItem(context.Background(), 7, "P", "C")
	if err == nil || !strings.Contains(err.Error(), "missing item id") {
		t.Fatalf("want missing-item-id error, got %v", err)
	}
}
