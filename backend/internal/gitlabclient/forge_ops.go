package gitlabclient

// This file extends the minimal GitLab REST v4 client with the operations
// the forge.Forge GitLab adapter (ADR-058 / E45.5, #1859) maps onto the
// forge-neutral vocabulary: branch create/read/delete, the merge-request
// lifecycle (create/read/update/list/merge + the by-commit lookup), commit
// statuses, protected-branch reads, compare/diff, single-commit reads, and
// a by-id project read. It is purely additive: every method reuses the
// existing do/errForStatus plumbing and injectable Doer untouched, and the
// package still imports no forge — the adapter slice owns all sentinel
// mapping and reads *APIError.StatusCode to distinguish (e.g.) a 404 branch
// from a real error.
//
// The types here are GitLab-shaped (a merge request is a MergeRequest, a
// pipeline status is a CommitStatus); the adapter translates them into the
// forge vocabulary. Fields are the subsets the adapter reads back, decoded
// directly onto exported structs with json tags rather than through a
// parallel private response type, since every mapping is an identity.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Commit is the subset of a GitLab commit object the forge adapter reads:
// its SHA (id), the human-facing short id/title/message, its parent SHAs,
// and the browse URL. GitLab exposes no tree SHA on this object (commit
// authoring is POST .../repository/commits with an actions[] array), which
// is why the adapter's git-data Forge methods fail closed rather than
// synthesising one.
type Commit struct {
	ID        string   `json:"id"`
	ShortID   string   `json:"short_id"`
	Title     string   `json:"title"`
	Message   string   `json:"message"`
	ParentIDs []string `json:"parent_ids"`
	WebURL    string   `json:"web_url"`
}

// Branch is the subset of a GitLab repository branch the adapter reads: the
// name, whether it is protected, and the commit it points at (Commit.ID is
// the tip SHA GetBranchSHA maps onto).
type Branch struct {
	Name      string  `json:"name"`
	Merged    bool    `json:"merged"`
	Protected bool    `json:"protected"`
	Commit    *Commit `json:"commit"`
}

// MergeRequest is the subset of a GitLab merge request the adapter maps onto
// forge.PullRequest. State is the GitLab lifecycle word (opened|closed|
// merged|locked); the adapter derives Merged from state=="merged". SHA is
// the MR's diff-head commit; MergeCommitSHA is populated once merged.
type MergeRequest struct {
	ID             int    `json:"id"`
	IID            int    `json:"iid"`
	ProjectID      int    `json:"project_id"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	State          string `json:"state"`
	SourceBranch   string `json:"source_branch"`
	TargetBranch   string `json:"target_branch"`
	SHA            string `json:"sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	WebURL         string `json:"web_url"`
}

// CommitStatus is the subset of a GitLab commit status the adapter reads
// back after publishing one. The response field is `status` (the request
// field is `state`); Name carries the status identity the adapter set (see
// SetCommitStatus).
type CommitStatus struct {
	ID          int    `json:"id"`
	SHA         string `json:"sha"`
	Ref         string `json:"ref"`
	Status      string `json:"status"`
	Name        string `json:"name"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

// ProtectedBranch is the subset of a GitLab protected-branch entry the
// adapter needs. GitLab protection carries push/merge access levels but NO
// required-status-check contexts, so the adapter maps a present entry to an
// empty context list and a 404 to "no classic protection".
type ProtectedBranch struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// CompareDiff is one changed file in a Comparison — GitLab's per-file diff
// entry. Diff is that file's unified-diff body (hunks only, no `diff --git`
// header); the adapter prepends the synthetic header when reconstructing a
// git-style patch.
type CompareDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
}

// Comparison is the GitLab compare result for from..to. Commit is the head
// (target) commit of the comparison; Commits is the commit list; Diffs is
// the per-file changes. CompareTimeout signals GitLab capped the diff — the
// adapter surfaces it as ComparePatchResult.Truncated rather than an error.
type Comparison struct {
	Commit         *Commit       `json:"commit"`
	Commits        []Commit      `json:"commits"`
	Diffs          []CompareDiff `json:"diffs"`
	CompareTimeout bool          `json:"compare_timeout"`
	CompareSameRef bool          `json:"compare_same_ref"`
}

// ProjectInfo is the subset of a GET /projects/:id response the adapter
// reads: the numeric id (echoed for a parse-back sanity check on a
// "gitlab:<id>" scope ref), the web URL, the default branch (forge
// Repository), and the namespaced path. Distinct from the package's
// existing Project (id + web_url only) because the forge adapter also needs
// the default branch and path; kept additive rather than widening Project.
type ProjectInfo struct {
	ID                int    `json:"id"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
	PathWithNamespace string `json:"path_with_namespace"`
}

// CreateBranch creates branch pointing at ref (a branch name, tag, or SHA).
//
//	POST /api/v4/projects/:id/repository/branches?branch=&ref=
//
// GitLab takes both parameters as query fields; the created branch object
// (name + tip commit) is returned.
func (c *Client) CreateBranch(ctx context.Context, projectID int, branch, ref string) (*Branch, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("gitlabclient: branch name required")
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("gitlabclient: ref required")
	}

	q := url.Values{}
	q.Set("branch", branch)
	q.Set("ref", ref)
	path := fmt.Sprintf("/api/v4/projects/%d/repository/branches?%s", projectID, q.Encode())

	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("create branch", resp); err != nil {
		return nil, err
	}

	var out Branch
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode create branch: %w", err)
	}
	return &out, nil
}

// GetBranch reads a single branch.
//
//	GET /api/v4/projects/:id/repository/branches/:branch
//
// A missing branch is a 404 *APIError (StatusCode distinguishable by the
// caller), not a nil result — the adapter maps that to ("", false, nil) in
// GetBranchSHA. The branch name is percent-encoded into one path segment so
// a slash-bearing name (feature/x) survives.
func (c *Client) GetBranch(ctx context.Context, projectID int, branch string) (*Branch, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("gitlabclient: branch name required")
	}

	path := fmt.Sprintf("/api/v4/projects/%d/repository/branches/%s", projectID, url.PathEscape(branch))
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get branch", resp); err != nil {
		return nil, err
	}

	var out Branch
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode get branch: %w", err)
	}
	return &out, nil
}

// DeleteBranch deletes a branch.
//
//	DELETE /api/v4/projects/:id/repository/branches/:branch
//
// GitLab answers 204 on success. The adapter uses delete-then-recreate to
// emulate a force-update (the Branches API has no update/force operation)
// and tolerates a 404 on the delete leg via *APIError.StatusCode.
func (c *Client) DeleteBranch(ctx context.Context, projectID int, branch string) error {
	if projectID <= 0 {
		return fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("gitlabclient: branch name required")
	}

	path := fmt.Sprintf("/api/v4/projects/%d/repository/branches/%s", projectID, url.PathEscape(branch))
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return errForStatus("delete branch", resp)
}

// CreateMergeRequestParams describes a merge request to open. All four
// fields are required by construction at the callsite; Description may be
// empty.
type CreateMergeRequestParams struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
}

// CreateMergeRequest opens a merge request.
//
//	POST /api/v4/projects/:id/merge_requests
//
// GitLab returns 409 when an MR already exists for the source/target pair;
// the adapter maps that to forge.ErrPullRequestExists and recovers via
// ListMergeRequests.
func (c *Client) CreateMergeRequest(ctx context.Context, projectID int, p CreateMergeRequestParams) (*MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(p.SourceBranch) == "" {
		return nil, fmt.Errorf("gitlabclient: source branch required")
	}
	if strings.TrimSpace(p.TargetBranch) == "" {
		return nil, fmt.Errorf("gitlabclient: target branch required")
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("gitlabclient: merge request title required")
	}

	body := map[string]any{
		"source_branch": p.SourceBranch,
		"target_branch": p.TargetBranch,
		"title":         p.Title,
	}
	if p.Description != "" {
		body["description"] = p.Description
	}

	resp, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("create merge request", resp); err != nil {
		return nil, err
	}

	var out MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode create merge request: %w", err)
	}
	return &out, nil
}

// GetMergeRequest reads a merge request by its project-scoped iid.
//
//	GET /api/v4/projects/:id/merge_requests/:iid
func (c *Client) GetMergeRequest(ctx context.Context, projectID, iid int) (*MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: merge request iid required")
	}

	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", projectID, iid), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get merge request", resp); err != nil {
		return nil, err
	}

	var out MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode get merge request: %w", err)
	}
	return &out, nil
}

// UpdateMergeRequestParams describes a merge-request edit. A nil field is
// left unchanged; a non-nil Description sends the field (empty string
// clears it), which is how the adapter's EditPullRequest replaces the body.
// StateEvent="close" closes the MR (ClosePullRequest).
type UpdateMergeRequestParams struct {
	Title       *string
	Description *string
	StateEvent  string
}

// UpdateMergeRequest edits a merge request.
//
//	PUT /api/v4/projects/:id/merge_requests/:iid
func (c *Client) UpdateMergeRequest(ctx context.Context, projectID, iid int, p UpdateMergeRequestParams) (*MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: merge request iid required")
	}

	body := map[string]any{}
	if p.Title != nil {
		body["title"] = *p.Title
	}
	if p.Description != nil {
		body["description"] = *p.Description
	}
	if p.StateEvent != "" {
		body["state_event"] = p.StateEvent
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("gitlabclient: update merge request: no fields to update")
	}

	resp, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d", projectID, iid), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("update merge request", resp); err != nil {
		return nil, err
	}

	var out MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode update merge request: %w", err)
	}
	return &out, nil
}

// ListMergeRequestsParams filters a merge-request list. Empty fields are
// omitted from the query. State is a GitLab lifecycle word (opened, closed,
// merged, locked, all).
type ListMergeRequestsParams struct {
	State        string
	SourceBranch string
	TargetBranch string
}

// ListMergeRequests lists a project's merge requests filtered by state and
// source/target branch.
//
//	GET /api/v4/projects/:id/merge_requests?state=&source_branch=&target_branch=
//
// This is the adapter's recovery read for a create-time 409
// (ListOpenPullRequestsByHead).
func (c *Client) ListMergeRequests(ctx context.Context, projectID int, p ListMergeRequestsParams) ([]MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}

	q := url.Values{}
	if p.State != "" {
		q.Set("state", p.State)
	}
	if p.SourceBranch != "" {
		q.Set("source_branch", p.SourceBranch)
	}
	if p.TargetBranch != "" {
		q.Set("target_branch", p.TargetBranch)
	}
	path := fmt.Sprintf("/api/v4/projects/%d/merge_requests", projectID)
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("list merge requests", resp); err != nil {
		return nil, err
	}

	var out []MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode list merge requests: %w", err)
	}
	return out, nil
}

// ListMergeRequestsForCommit lists the merge requests associated with a
// commit SHA.
//
//	GET /api/v4/projects/:id/repository/commits/:sha/merge_requests
//
// The adapter's ListPullRequestsForCommit maps this onto the release-
// evidence PR walk.
func (c *Client) ListMergeRequestsForCommit(ctx context.Context, projectID int, sha string) ([]MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("gitlabclient: commit sha required")
	}

	path := fmt.Sprintf("/api/v4/projects/%d/repository/commits/%s/merge_requests", projectID, url.PathEscape(sha))
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("list merge requests for commit", resp); err != nil {
		return nil, err
	}

	var out []MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode list merge requests for commit: %w", err)
	}
	return out, nil
}

// MergeMergeRequestParams controls a merge. Squash squashes the MR's
// commits; MergeWhenPipelineSucceeds queues the merge to run once the
// pipeline passes (the adapter's EnableAutoMerge). SquashCommitMessage is
// optional.
type MergeMergeRequestParams struct {
	Squash                    bool
	MergeWhenPipelineSucceeds bool
	SquashCommitMessage       string
}

// MergeMergeRequest merges a merge request.
//
//	PUT /api/v4/projects/:id/merge_requests/:iid/merge
//
// GitLab returns 405/406 when the MR is not in a mergeable state; the
// adapter maps that to forge.ErrPullRequestNotMergeable.
func (c *Client) MergeMergeRequest(ctx context.Context, projectID, iid int, p MergeMergeRequestParams) (*MergeRequest, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if iid <= 0 {
		return nil, fmt.Errorf("gitlabclient: merge request iid required")
	}

	body := map[string]any{}
	if p.Squash {
		body["squash"] = true
	}
	if p.MergeWhenPipelineSucceeds {
		body["merge_when_pipeline_succeeds"] = true
	}
	if p.SquashCommitMessage != "" {
		body["squash_commit_message"] = p.SquashCommitMessage
	}

	var reqBody any
	if len(body) > 0 {
		reqBody = body
	}
	resp, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/merge", projectID, iid), reqBody)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("merge merge request", resp); err != nil {
		return nil, err
	}

	var out MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode merge merge request: %w", err)
	}
	return &out, nil
}

// SetCommitStatusParams describes a commit status to publish. State is the
// GitLab pipeline-status word (pending, running, success, failed, canceled).
// Name is the status IDENTITY: it MUST be sent so the status does not
// collapse to GitLab's "default" label — see SetCommitStatus.
type SetCommitStatusParams struct {
	State       string
	Name        string
	Ref         string
	TargetURL   string
	Description string
}

// SetCommitStatus publishes a pipeline/commit status on a commit.
//
//	POST /api/v4/projects/:id/statuses/:sha
//
// The status identity is sent as the `name` parameter, which GitLab's
// commit-status create API documents as the field distinguishing this
// status from other systems'
// (https://docs.gitlab.com/ee/api/commits.html#set-the-pipeline-status-of-a-commit).
// The API also accepts the legacy `context` alias, but `name` is the
// canonical field and is what we send; without it GitLab records the status
// under its "default" label and the check identity is lost.
func (c *Client) SetCommitStatus(ctx context.Context, projectID int, sha string, p SetCommitStatusParams) (*CommitStatus, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("gitlabclient: commit sha required")
	}
	if strings.TrimSpace(p.State) == "" {
		return nil, fmt.Errorf("gitlabclient: commit status state required")
	}

	body := map[string]any{"state": p.State}
	if p.Name != "" {
		body["name"] = p.Name
	}
	if p.Ref != "" {
		body["ref"] = p.Ref
	}
	if p.TargetURL != "" {
		body["target_url"] = p.TargetURL
	}
	if p.Description != "" {
		body["description"] = p.Description
	}

	path := fmt.Sprintf("/api/v4/projects/%d/statuses/%s", projectID, url.PathEscape(sha))
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("set commit status", resp); err != nil {
		return nil, err
	}

	var out CommitStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode set commit status: %w", err)
	}
	return &out, nil
}

// GetProtectedBranch reads a branch's protection entry.
//
//	GET /api/v4/projects/:id/protected_branches/:branch
//
// A 404 *APIError means the branch has no protection configured — the
// adapter treats that as "no classic protection" per ADR-017. GitLab
// protection carries no required-status-check contexts, so the adapter maps
// a present entry to an empty context list.
func (c *Client) GetProtectedBranch(ctx context.Context, projectID int, branch string) (*ProtectedBranch, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("gitlabclient: branch name required")
	}

	path := fmt.Sprintf("/api/v4/projects/%d/protected_branches/%s", projectID, url.PathEscape(branch))
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get protected branch", resp); err != nil {
		return nil, err
	}

	var out ProtectedBranch
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode get protected branch: %w", err)
	}
	return &out, nil
}

// Compare compares two refs (from..to).
//
//	GET /api/v4/projects/:id/repository/compare?from=&to=&straight=
//
// The adapter passes straight=false for merge-base (three-dot) semantics
// matching the GitHub base...head contract
// (https://docs.gitlab.com/ee/api/repositories.html#compare-branches-tags-or-commits).
func (c *Client) Compare(ctx context.Context, projectID int, from, to string, straight bool) (*Comparison, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(from) == "" {
		return nil, fmt.Errorf("gitlabclient: compare from ref required")
	}
	if strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("gitlabclient: compare to ref required")
	}

	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	q.Set("straight", strconv.FormatBool(straight))
	path := fmt.Sprintf("/api/v4/projects/%d/repository/compare?%s", projectID, q.Encode())

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("compare", resp); err != nil {
		return nil, err
	}

	var out Comparison
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode compare: %w", err)
	}
	return &out, nil
}

// GetCommit reads a single commit by SHA.
//
//	GET /api/v4/projects/:id/repository/commits/:sha
//
// GitLab does not expose the commit's tree SHA on this object, so the
// adapter's git-data GetCommit Forge method returns ErrUnsupported rather
// than backing it with this read.
func (c *Client) GetCommit(ctx context.Context, projectID int, sha string) (*Commit, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("gitlabclient: commit sha required")
	}

	path := fmt.Sprintf("/api/v4/projects/%d/repository/commits/%s", projectID, url.PathEscape(sha))
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get commit", resp); err != nil {
		return nil, err
	}

	var out Commit
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode get commit: %w", err)
	}
	return &out, nil
}

// CreatePipeline creates a new pipeline for a project at ref, passing the
// given CI/CD variables.
//
//	POST /api/v4/projects/:id/pipeline
//
// ref selects BOTH which .gitlab-ci.yml is evaluated AND which commit the
// pipeline (and its statuses) run against — the run branch under the ADR-035
// sole-writer lineage (https://docs.gitlab.com/ee/api/pipelines.html#create-a-new-pipeline).
// Variables ride as the API's `variables` array of {key,value} objects. The
// dispatch is fire-and-forget from the backend's view: the created pipeline id
// is not read back (the runner reports its own progress), so this returns only
// an error.
func (c *Client) CreatePipeline(ctx context.Context, projectID int, ref string, variables map[string]string) error {
	if projectID <= 0 {
		return fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("gitlabclient: pipeline ref required")
	}

	body := map[string]any{"ref": ref}
	if len(variables) > 0 {
		// Sort keys so the request body is deterministic (stable tests, stable
		// logs). GitLab ignores variable order.
		keys := make([]string, 0, len(variables))
		for k := range variables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vars := make([]map[string]string, 0, len(variables))
		for _, k := range keys {
			vars = append(vars, map[string]string{"key": k, "value": variables[k]})
		}
		body["variables"] = vars
	}

	resp, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%d/pipeline", projectID), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return errForStatus("create pipeline", resp)
}

// GetRawFile reads the raw bytes of a repository file at ref.
//
//	GET /api/v4/projects/:id/repository/files/:path/raw?ref=
//
// The file path is URL-encoded into one path segment (so the slashes in
// ".fishhawk/workflows.yaml" become %2F as GitLab requires,
// https://docs.gitlab.com/ee/api/repository_files.html#get-raw-file-from-repository).
// This is the GitLab-side workflow-spec read for run creation from a GitLab
// trigger (#1861): forge.Forge has no spec-read method, so the dispatcher
// reads .fishhawk/workflows.yaml through this getter keyed on the
// gitlab:<project_id> scope. A missing file is a 404 *APIError the caller can
// distinguish via StatusCode.
func (c *Client) GetRawFile(ctx context.Context, projectID int, filePath, ref string) ([]byte, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}
	if strings.TrimSpace(filePath) == "" {
		return nil, fmt.Errorf("gitlabclient: file path required")
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("gitlabclient: ref required")
	}

	q := url.Values{}
	q.Set("ref", ref)
	path := fmt.Sprintf("/api/v4/projects/%d/repository/files/%s/raw?%s",
		projectID, url.PathEscape(filePath), q.Encode())

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get raw file", resp); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gitlabclient: read raw file: %w", err)
	}
	return raw, nil
}

// GetProjectByID reads a project by its numeric id (as opposed to GetProject,
// which resolves a namespaced path).
//
//	GET /api/v4/projects/:id
//
// The adapter uses this to parse-back a "gitlab:<id>" credential scope ref
// and to resolve the default branch for the forge Repository read.
func (c *Client) GetProjectByID(ctx context.Context, projectID int) (*ProjectInfo, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("gitlabclient: project id required")
	}

	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%d", projectID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := errForStatus("get project by id", resp); err != nil {
		return nil, err
	}

	var out ProjectInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitlabclient: decode get project by id: %w", err)
	}
	return &out, nil
}
