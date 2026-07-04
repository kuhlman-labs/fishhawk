package githubclient

// This file carries the GitHub surfaces the work-management GitHub
// Projects provider needs (#1005): REST issue creation + node-id lookup,
// and the GraphQL Projects (v2) calls to add an item to a project, set
// its single-select Status field, and link a parent epic as a sub-issue.
//
// The GraphQL calls honor the Project #7 traps in AGENTS.md: the project
// owner may be a USER (not an organization), so ProjectFields builds its
// query against the owner kind the caller declares; and field/option node
// ids must be resolved before a value can be set, so ProjectFields is the
// one-round-trip resolver the mutations depend on.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CreateIssueParams is the typed body for CreateIssue. Labels are applied
// at creation time (GitHub accepts a labels array on the create call), so
// the provider does not need a separate add-labels round trip.
type CreateIssueParams struct {
	Title  string
	Body   string
	Labels []string
}

// CreatedIssue is the slice of a created-issue response the work-item
// provider consumes: the human Number + HTMLURL for the returned item,
// and the NodeID (GraphQL global id) the project/sub-issue mutations key
// on.
type CreatedIssue struct {
	Number  int
	NodeID  string
	HTMLURL string
}

// IssueSearchResult is the slice of a search-result item the feedback
// dedup search consumes: the human Number + HTMLURL to return, and the
// Body the caller re-verifies the fingerprint marker against.
type IssueSearchResult struct {
	Number  int
	HTMLURL string
	Body    string
}

// ProjectCoord identifies a GitHub Projects (v2) board by owner + number.
// OwnerType selects the GraphQL root query: "user" (the Project #7 case)
// or "organization". Empty OwnerType defaults to "user".
type ProjectCoord struct {
	Owner     string
	OwnerType string
	Number    int
}

// ProjectMeta is the resolved node ids a Projects (v2) field mutation
// needs: the project's node id, the single-select field's node id, and
// the field's option-name → option-id map. StatusOptions keys are the
// option labels as configured on the board (e.g. "Backlog").
type ProjectMeta struct {
	ProjectID     string
	FieldID       string
	StatusOptions map[string]string
}

// CreateIssue opens an issue with labels applied at creation time.
//
//	POST /repos/{owner}/{repo}/issues
//
// Returns the created issue's number, GraphQL node id, and html_url.
// Requires the App to hold `issues:write`. Returns ErrNotFound when the
// repo isn't visible to the installation, ErrForbidden on auth issues,
// ErrValidation when GitHub rejects the body.
func (c *Client) CreateIssue(ctx context.Context, installationID int64, repo RepoRef, p CreateIssueParams) (*CreatedIssue, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if p.Title == "" {
		return nil, errors.New("githubclient: issue title required")
	}

	payload := map[string]any{"title": p.Title, "body": p.Body}
	if len(p.Labels) > 0 {
		payload["labels"] = p.Labels
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("githubclient: marshal create issue: %w", err)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/issues")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(raw), installationID)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: create issue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("create issue", resp); err != nil {
		return nil, err
	}
	var out struct {
		Number  int    `json:"number"`
		NodeID  string `json:"node_id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode create issue: %w", err)
	}
	if out.NodeID == "" {
		return nil, fmt.Errorf("githubclient: create issue response missing node_id")
	}
	return &CreatedIssue{Number: out.Number, NodeID: out.NodeID, HTMLURL: out.HTMLURL}, nil
}

// SearchOpenIssues runs an issue search and returns the matched items.
//
//	GET /search/issues?q={query}
//
// The caller composes the full query string (including any repo:owner/name
// and is:open qualifiers); this method just forwards it as the q parameter
// and decodes the {items:[{number,html_url,body}]} envelope. The feedback
// dedup search uses it to find an open report already carrying a
// fingerprint marker. Requires the App to hold `issues:read`. Returns
// ErrForbidden on auth issues, ErrValidation when GitHub rejects the query.
func (c *Client) SearchOpenIssues(ctx context.Context, installationID int64, query string) ([]IssueSearchResult, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("githubclient: search query required")
	}

	endpoint := c.endpoint("/search/issues") + "?q=" + url.QueryEscape(query)
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: search issues: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("search issues", resp); err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubclient: decode search issues: %w", err)
	}
	results := make([]IssueSearchResult, 0, len(out.Items))
	for _, it := range out.Items {
		results = append(results, IssueSearchResult{Number: it.Number, HTMLURL: it.HTMLURL, Body: it.Body})
	}
	return results, nil
}

// IssueTitleResult is the slice of a search-result item the work-management
// number-discovery consumes: the issue Number (unused by discovery) and the
// Title the provider re-parses for the numbered-type [PREFIX-N] token.
type IssueTitleResult struct {
	Number int
	Title  string
}

// searchByTitlePerPage is the issue Search API page size (its 100-item max).
// searchByTitleMaxPages caps the paginated walk at the GitHub search 1000-
// result ceiling (10 * 100), bounding the network surface of one discovery.
const (
	searchByTitlePerPage  = 100
	searchByTitleMaxPages = 10
)

// SearchIssuesByTitle runs a paginated issue search and returns the matched
// items' number + title.
//
//	GET /search/issues?q={query}&per_page=100&page=N
//
// The caller composes the full query string (including any repo:owner/name
// and in:title qualifiers). Unlike SearchOpenIssues this method imposes NO
// state qualifier — the caller's query omits is:open so CLOSED items count
// (decided ADRs are closed, and undercounting them would re-allocate an
// existing number). It paginates from page 1, stopping when a page returns
// fewer than per_page items (or zero), with a hard cap of
// searchByTitleMaxPages pages (the GitHub search 1000-result ceiling).
// Requires the App to hold `issues:read`. Returns ErrForbidden on auth
// issues, ErrValidation when GitHub rejects the query.
func (c *Client) SearchIssuesByTitle(ctx context.Context, installationID int64, query string) ([]IssueTitleResult, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("githubclient: search query required")
	}

	var results []IssueTitleResult
	for page := 1; page <= searchByTitleMaxPages; page++ {
		endpoint := fmt.Sprintf("%s?q=%s&per_page=%d&page=%d",
			c.endpoint("/search/issues"), url.QueryEscape(query), searchByTitlePerPage, page)
		req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubclient: search issues by title: %w", err)
		}
		var out struct {
			Items []struct {
				Number int    `json:"number"`
				Title  string `json:"title"`
			} `json:"items"`
		}
		if err := classifyStatus("search issues by title", resp); err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
		decErr := json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if decErr != nil {
			return nil, fmt.Errorf("githubclient: decode search issues by title: %w", decErr)
		}
		for _, it := range out.Items {
			results = append(results, IssueTitleResult{Number: it.Number, Title: it.Title})
		}
		// A short (or empty) page is the last page — stop before the cap.
		if len(out.Items) < searchByTitlePerPage {
			break
		}
	}
	return results, nil
}

// IssueNodeID resolves an existing issue's GraphQL node id by number.
//
//	GET /repos/{owner}/{repo}/issues/{number}
//
// The work-item provider uses it to turn a parent-epic reference (#N)
// into the node id AddSubIssue links against. Returns ErrNotFound when
// the issue isn't visible to the installation.
func (c *Client) IssueNodeID(ctx context.Context, installationID int64, repo RepoRef, number int) (string, error) {
	if c.Tokens == nil {
		return "", errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return "", errors.New("githubclient: repo owner and name required")
	}
	if number <= 0 {
		return "", errors.New("githubclient: issue number must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/issues/" + url.PathEscape(fmt.Sprintf("%d", number)))
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("githubclient: get issue node id: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("get issue node id", resp); err != nil {
		return "", err
	}
	var out struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("githubclient: decode issue node id: %w", err)
	}
	if out.NodeID == "" {
		return "", fmt.Errorf("githubclient: issue %d response missing node_id", number)
	}
	return out.NodeID, nil
}

// ProjectFields resolves the project node id and the named single-select
// field's id + options in one GraphQL round trip. fieldName is the board
// field to resolve (e.g. "Status"). It honors the user-vs-organization
// owner trap by rooting the query at the owner kind ProjectCoord
// declares.
//
// Returns ErrNotFound-shaped errors via classifyStatus on transport
// failures and ErrValidation when GraphQL reports an application error
// (e.g. the project or field doesn't exist).
func (c *Client) ProjectFields(ctx context.Context, installationID int64, coord ProjectCoord, fieldName string) (*ProjectMeta, error) {
	if coord.Owner == "" || coord.Number <= 0 {
		return nil, errors.New("githubclient: project owner and number required")
	}
	if fieldName == "" {
		return nil, errors.New("githubclient: project field name required")
	}
	ownerRoot := "user"
	if coord.OwnerType == "organization" {
		ownerRoot = "organization"
	}

	query := fmt.Sprintf(`query ProjectFields($login: String!, $number: Int!, $field: String!) {
  %s(login: $login) {
    projectV2(number: $number) {
      id
      field(name: $field) {
        ... on ProjectV2SingleSelectField {
          id
          options { id name }
        }
      }
    }
  }
}`, ownerRoot)

	type ownerHolder struct {
		ProjectV2 *struct {
			ID    string `json:"id"`
			Field *struct {
				ID      string `json:"id"`
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
			} `json:"field"`
		} `json:"projectV2"`
	}
	var data struct {
		User         *ownerHolder `json:"user"`
		Organization *ownerHolder `json:"organization"`
	}
	if err := c.doGraphQL(ctx, installationID, query, map[string]any{
		"login":  coord.Owner,
		"number": coord.Number,
		"field":  fieldName,
	}, &data); err != nil {
		return nil, err
	}

	holder := data.User
	if ownerRoot == "organization" {
		holder = data.Organization
	}
	if holder == nil || holder.ProjectV2 == nil {
		return nil, fmt.Errorf("%w: project %s/%d not found", ErrNotFound, coord.Owner, coord.Number)
	}
	if holder.ProjectV2.Field == nil || holder.ProjectV2.Field.ID == "" {
		return nil, fmt.Errorf("%w: single-select field %q not found on project %s/%d", ErrNotFound, fieldName, coord.Owner, coord.Number)
	}
	opts := make(map[string]string, len(holder.ProjectV2.Field.Options))
	for _, o := range holder.ProjectV2.Field.Options {
		opts[o.Name] = o.ID
	}
	return &ProjectMeta{
		ProjectID:     holder.ProjectV2.ID,
		FieldID:       holder.ProjectV2.Field.ID,
		StatusOptions: opts,
	}, nil
}

// ProjectItemStatus is the current board placement of an already-filed
// issue on one project: whether the issue has an item on that project
// (OnBoard), the item id the Status mutation keys on (ItemID), and the
// item's current single-select Status option name (Status, empty when the
// item is on the board but its Status is unset). The board-state-sync
// Transition reads it to decide whether to move the card and from what.
type ProjectItemStatus struct {
	OnBoard bool
	ItemID  string
	Status  string
}

// ProjectItemStatus resolves an issue's existing project-item id and its
// current single-select Status on the project identified by projectID (the
// project node id, e.g. from ProjectFields). It looks up the issue's
// project items by node id and matches the one whose project node id equals
// projectID — unambiguous across boards the issue also sits on.
//
//	query node(id: $issueId) { ... on Issue { projectItems { ... } } }
//
// Returns OnBoard=false (no error) when the issue has no item on that
// project — the not-on-board path the Transition skips on. Honors the
// user-owned-projects token opt-in (WithProjectsToken) like the other
// Projects (v2) calls, since it routes through doGraphQL.
func (c *Client) ProjectItemStatus(ctx context.Context, installationID int64, issueNodeID, projectID, fieldName string) (*ProjectItemStatus, error) {
	if issueNodeID == "" || projectID == "" {
		return nil, errors.New("githubclient: issue node id and project id required")
	}
	if fieldName == "" {
		return nil, errors.New("githubclient: project field name required")
	}
	const query = `query ProjectItemStatus($issueId: ID!, $field: String!) {
  node(id: $issueId) {
    ... on Issue {
      projectItems(first: 50) {
        nodes {
          id
          project { id }
          fieldValueByName(name: $field) {
            ... on ProjectV2ItemFieldSingleSelectValue { name }
          }
        }
      }
    }
  }
}`
	var data struct {
		Node *struct {
			ProjectItems struct {
				Nodes []struct {
					ID      string `json:"id"`
					Project struct {
						ID string `json:"id"`
					} `json:"project"`
					FieldValueByName *struct {
						Name string `json:"name"`
					} `json:"fieldValueByName"`
				} `json:"nodes"`
			} `json:"projectItems"`
		} `json:"node"`
	}
	if err := c.doGraphQL(ctx, installationID, query, map[string]any{
		"issueId": issueNodeID,
		"field":   fieldName,
	}, &data); err != nil {
		return nil, err
	}
	if data.Node == nil {
		return &ProjectItemStatus{OnBoard: false}, nil
	}
	for _, n := range data.Node.ProjectItems.Nodes {
		if n.Project.ID != projectID {
			continue
		}
		status := ""
		if n.FieldValueByName != nil {
			status = n.FieldValueByName.Name
		}
		return &ProjectItemStatus{OnBoard: true, ItemID: n.ID, Status: status}, nil
	}
	return &ProjectItemStatus{OnBoard: false}, nil
}

// AddProjectItem adds an issue (by content node id) to a project and
// returns the created project-item id, the handle the field mutation
// keys on.
//
//	mutation addProjectV2ItemById
func (c *Client) AddProjectItem(ctx context.Context, installationID int64, projectID, contentID string) (string, error) {
	if projectID == "" || contentID == "" {
		return "", errors.New("githubclient: project id and content id required")
	}
	const mutation = `mutation AddItem($projectId: ID!, $contentId: ID!) {
  addProjectV2ItemById(input: { projectId: $projectId, contentId: $contentId }) {
    item { id }
  }
}`
	var data struct {
		AddProjectV2ItemByID struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := c.doGraphQL(ctx, installationID, mutation, map[string]any{
		"projectId": projectID,
		"contentId": contentID,
	}, &data); err != nil {
		return "", err
	}
	if data.AddProjectV2ItemByID.Item.ID == "" {
		return "", fmt.Errorf("githubclient: add project item response missing item id")
	}
	return data.AddProjectV2ItemByID.Item.ID, nil
}

// SetProjectItemSingleSelect sets a project item's single-select field
// (e.g. Status) to the given option id.
//
//	mutation updateProjectV2ItemFieldValue
func (c *Client) SetProjectItemSingleSelect(ctx context.Context, installationID int64, projectID, itemID, fieldID, optionID string) error {
	if projectID == "" || itemID == "" || fieldID == "" || optionID == "" {
		return errors.New("githubclient: project id, item id, field id, and option id required")
	}
	const mutation = `mutation SetField($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
  updateProjectV2ItemFieldValue(input: {
    projectId: $projectId, itemId: $itemId, fieldId: $fieldId,
    value: { singleSelectOptionId: $optionId }
  }) {
    projectV2Item { id }
  }
}`
	return c.doGraphQL(ctx, installationID, mutation, map[string]any{
		"projectId": projectID,
		"itemId":    itemID,
		"fieldId":   fieldID,
		"optionId":  optionID,
	}, nil)
}

// AddSubIssue links childNodeID as a sub-issue of parentNodeID — the
// work-item provider's parent-epic link.
//
//	mutation addSubIssue
func (c *Client) AddSubIssue(ctx context.Context, installationID int64, parentNodeID, childNodeID string) error {
	if parentNodeID == "" || childNodeID == "" {
		return errors.New("githubclient: parent and child node ids required")
	}
	const mutation = `mutation AddSubIssue($issueId: ID!, $subIssueId: ID!) {
  addSubIssue(input: { issueId: $issueId, subIssueId: $subIssueId }) {
    issue { id }
  }
}`
	return c.doGraphQL(ctx, installationID, mutation, map[string]any{
		"issueId":    parentNodeID,
		"subIssueId": childNodeID,
	}, nil)
}

// SubIssue is the slice of a sub-issue connection node the work-management
// epic-children query consumes: the child issue's human Number, GraphQL
// NodeID, Title, Body (the depends_on body marker is parsed from Body), and
// Labels (the child's issue-label names, from which the campaign source
// derives the autonomy tier — #1551).
type SubIssue struct {
	Number int
	NodeID string
	Title  string
	Body   string
	Labels []string
}

// listSubIssuesFirst is the sub-issues connection page size. v0 reads a
// single first:100 page — an epic with more than 100 children is out of
// scope for the campaign DAG source (ADR-047 / #1437) and the cap is noted
// here so a >100-child epic silently reads only the first 100.
const listSubIssuesFirst = 100

// ListSubIssues returns the sub-issues (children) of the issue identified by
// parentNodeID — its number, node id, title, and body.
//
//	query node(id: $parentId) { ... on Issue { subIssues(first: 100) { nodes { number title body id } } } }
//
// It reads a SINGLE first:100 page (listSubIssuesFirst); a parent with more
// than 100 sub-issues is truncated to the first 100 — out of scope for the
// v0 campaign DAG source. The `subIssues` connection is the same GraphQL
// surface the AddSubIssue mutation operates on, so the children read and the
// parent-epic link agree on one relation. Honors the user-owned-projects
// token opt-in (WithProjectsToken) like the other GraphQL calls, since it
// routes through doGraphQL; sub-issues are repo-scoped, so the installation
// token reaches them. Returns ErrForbidden on auth issues, ErrValidation
// when GraphQL reports an application error.
func (c *Client) ListSubIssues(ctx context.Context, installationID int64, parentNodeID string) ([]SubIssue, error) {
	if parentNodeID == "" {
		return nil, errors.New("githubclient: parent node id required")
	}
	const query = `query ListSubIssues($parentId: ID!, $first: Int!) {
  node(id: $parentId) {
    ... on Issue {
      subIssues(first: $first) {
        nodes {
          number
          title
          body
          id
          labels(first: 20) { nodes { name } }
        }
      }
    }
  }
}`
	var data struct {
		Node *struct {
			SubIssues struct {
				Nodes []struct {
					Number int    `json:"number"`
					Title  string `json:"title"`
					Body   string `json:"body"`
					ID     string `json:"id"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
				} `json:"nodes"`
			} `json:"subIssues"`
		} `json:"node"`
	}
	if err := c.doGraphQL(ctx, installationID, query, map[string]any{
		"parentId": parentNodeID,
		"first":    listSubIssuesFirst,
	}, &data); err != nil {
		return nil, err
	}
	if data.Node == nil {
		return nil, nil
	}
	results := make([]SubIssue, 0, len(data.Node.SubIssues.Nodes))
	for _, n := range data.Node.SubIssues.Nodes {
		var labels []string
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}
		results = append(results, SubIssue{Number: n.Number, NodeID: n.ID, Title: n.Title, Body: n.Body, Labels: labels})
	}
	return results, nil
}

// projectsTokenKey is the unexported context-key type for the
// request-scoped flag that asks doGraphQL to authenticate with the
// static projects token (Client.ProjectsToken) instead of the
// installation token. A dedicated unexported type avoids collisions
// with any other package's context keys.
type projectsTokenKey struct{}

// WithProjectsToken returns a child context that opts the GraphQL call
// it threads through into the static projects token (Client.ProjectsToken).
// It is the explicit seam the work-management provider uses to route the
// user-owned board-placement GraphQL through the projects token WITHOUT
// changing any method signature: doGraphQL honors the flag only when
// Client.ProjectsToken is non-empty, so setting it is inert (installation-
// token fallback) when no projects token is configured (#1114).
func WithProjectsToken(ctx context.Context) context.Context {
	return context.WithValue(ctx, projectsTokenKey{}, true)
}

// ProjectsTokenRequested reports whether ctx carries the WithProjectsToken
// opt-in flag.
func ProjectsTokenRequested(ctx context.Context) bool {
	v, _ := ctx.Value(projectsTokenKey{}).(bool)
	return v
}

// ProjectsTokenConfigured reports whether a static projects token is set on
// the client. The work-management board-sync Transition consults it to fail
// fast for user-owned Projects v2 boards the App installation token cannot
// reach (#1114): with no projects token configured the board is unreachable,
// so the move degrades to a best-effort skip (the #1107/#1114 posture) rather
// than dispatching a GraphQL call the installation-token fallback would error
// on — an error that would drop the mandated work_item_transitioned audit.
func (c *Client) ProjectsTokenConfigured() bool {
	return c.ProjectsToken != ""
}

// doGraphQL POSTs a GraphQL query/mutation to /graphql and decodes the
// `data` field into out (out may be nil to ignore the payload). GraphQL
// returns HTTP 200 even for application-level errors, so the `errors`
// array is surfaced as ErrValidation — matching EnableAutoMerge's
// handling so callers can switch on the error kind without re-parsing.
//
// Token selection: when the request opted in via WithProjectsToken AND
// Client.ProjectsToken is non-empty, the request authenticates with that
// static user token (user-owned Projects v2 boards, which installation
// tokens cannot reach — #1114). Otherwise the installation-token path is
// used unchanged, which also preserves the #1107 best-effort boarded:false
// degradation when the flag is set but no projects token is configured.
func (c *Client) doGraphQL(ctx context.Context, installationID int64, query string, variables map[string]any, out any) error {
	useProjectsToken := ProjectsTokenRequested(ctx) && c.ProjectsToken != ""
	if c.Tokens == nil && !useProjectsToken {
		return errors.New("githubclient: client missing TokenProvider")
	}
	body := map[string]any{"query": query}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("githubclient: marshal graphql request: %w", err)
	}
	var req *http.Request
	if useProjectsToken {
		req, err = c.buildStaticTokenRequest(ctx, http.MethodPost, c.endpoint("/graphql"), bytes.NewReader(raw), c.ProjectsToken)
	} else {
		req, err = c.buildRequest(ctx, http.MethodPost, c.endpoint("/graphql"), bytes.NewReader(raw), installationID)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: graphql request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("graphql", resp); err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("githubclient: decode graphql response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("%w: graphql: %s", ErrValidation, envelope.Errors[0].Message)
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("githubclient: decode graphql data: %w", err)
		}
	}
	return nil
}
