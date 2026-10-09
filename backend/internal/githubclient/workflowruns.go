package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// workflowRunsPerPage is GitHub's per_page ceiling for the workflow-runs
// listing. ListWorkflowRunsForHeadSHA reads ONE page, which is assumed to
// cover the CI a single pull-request head triggers (E83.49 / #4082).
const workflowRunsPerPage = 100

// ListWorkflowRunsForHeadSHA lists the Actions workflow runs GitHub recorded
// for one commit, every event (pull_request, push, workflow_dispatch, …), so
// the CI re-trigger verb can pick the completed, non-successful CI runs at a
// pull request's current head (E83.49 / #4082).
//
//	GET /repos/{owner}/{repo}/actions/runs?head_sha={sha}&per_page=100
//
// Only the FIRST page is read. Returns ErrNotFound when the repo is not
// visible, ErrForbidden on auth issues, ErrValidation on a 422; any other
// non-2xx is an untyped status error via classifyStatus.
func (c *Client) ListWorkflowRunsForHeadSHA(ctx context.Context, scope forge.CredentialScope, repo RepoRef, headSHA string) ([]*WorkflowRun, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, err
	}
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if headSHA == "" {
		return nil, errors.New("githubclient: head sha required")
	}

	q := url.Values{}
	q.Set("head_sha", headSHA)
	q.Set("per_page", strconv.Itoa(workflowRunsPerPage))
	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/actions/runs?" + q.Encode())

	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubclient: list workflow runs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("list workflow runs", resp); err != nil {
		return nil, err
	}

	var body struct {
		WorkflowRuns []struct {
			ID         int64   `json:"id"`
			HTMLURL    string  `json:"html_url"`
			Conclusion *string `json:"conclusion"`
			Status     string  `json:"status"`
			Event      string  `json:"event"`
			HeadBranch string  `json:"head_branch"`
			HeadSHA    string  `json:"head_sha"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("githubclient: decode workflow runs: %w", err)
	}
	out := make([]*WorkflowRun, 0, len(body.WorkflowRuns))
	for _, raw := range body.WorkflowRuns {
		wr := &WorkflowRun{
			ID:         raw.ID,
			HTMLURL:    raw.HTMLURL,
			Status:     raw.Status,
			Event:      raw.Event,
			HeadBranch: raw.HeadBranch,
			HeadSHA:    raw.HeadSHA,
		}
		// GitHub reports conclusion as JSON null until the run completes.
		if raw.Conclusion != nil {
			wr.Conclusion = *raw.Conclusion
		}
		out = append(out, wr)
	}
	return out, nil
}

// RerunWorkflowRun re-runs a completed Actions workflow run in place: GitHub
// creates a new attempt of the same run at the same commit, so no ref, commit
// or pull-request state changes (E83.49 / #4082).
//
//	POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun
//
// GitHub answers 201 Created on success. Returns ErrForbidden when the run
// cannot be re-run (including GitHub's 403 for an in-progress run) or on auth
// issues, ErrNotFound when the run id is unknown; any other non-2xx is an
// untyped status error via classifyStatus.
func (c *Client) RerunWorkflowRun(ctx context.Context, scope forge.CredentialScope, repo RepoRef, runID int64) error {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return err
	}
	if c.Tokens == nil {
		return errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return errors.New("githubclient: repo owner and name required")
	}
	if runID <= 0 {
		return errors.New("githubclient: workflow run id must be > 0")
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/actions/runs/" + url.PathEscape(strconv.FormatInt(runID, 10)) + "/rerun")
	req, err := c.buildRequest(ctx, http.MethodPost, endpoint, nil, installationID)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubclient: rerun workflow run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return classifyStatus("rerun workflow run", resp)
}
