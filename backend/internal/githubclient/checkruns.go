package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// checkRunsPerPage is GitHub's per_page ceiling for the check-runs listing.
const checkRunsPerPage = 100

// checkRunsPageCap bounds one listing at 5 pages (500 check runs); a ref with
// more is reported truncated rather than read to the end.
const checkRunsPageCap = 5

// CheckRunSummary is one check run as the check-runs listing reports it —
// names, states and forge timestamps only; no output text is read.
type CheckRunSummary struct {
	ID         int64
	Name       string
	Status     string // queued | in_progress | completed | waiting | requested | pending
	Conclusion string // empty until Status is completed
	// StartedAt / CompletedAt are nil when GitHub reports null.
	StartedAt   *time.Time
	CompletedAt *time.Time
	// CheckSuiteID groups re-run attempts: a re-run lands in the same suite,
	// while the same name in a different suite (another workflow, another
	// app) is a distinct check that happens to share the name.
	CheckSuiteID int64
	AppSlug      string
}

// ListCheckRunsForRef lists every check run on a commit — EVERY attempt, not
// only the latest per name — so a caller can fix the conclusion as of a past
// instant (E82.2 / #3779, the merge-commit CI observation).
//
//	GET /repos/{owner}/{repo}/commits/{ref}/check-runs?filter=all&per_page=100&page=N
//
// Pages until total_count is reached. truncated is true when the 5-page cap
// stops the read early or a page comes back short of total_count; a caller
// must then treat the listing as incomplete. Returns ErrNotFound when the
// repo/ref isn't visible, ErrForbidden on auth issues, ErrValidation on a
// 422; any other non-2xx is an untyped status error via classifyStatus.
func (c *Client) ListCheckRunsForRef(ctx context.Context, scope forge.CredentialScope, repo RepoRef, ref string) ([]CheckRunSummary, bool, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return nil, false, err
	}
	if c.Tokens == nil {
		return nil, false, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, false, errors.New("githubclient: repo owner and name required")
	}
	if ref == "" {
		return nil, false, errors.New("githubclient: commit ref required")
	}

	base := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/commits/" + escapePath(ref) + "/check-runs")
	var out []CheckRunSummary
	for page := 1; page <= checkRunsPageCap; page++ {
		q := url.Values{}
		q.Set("filter", "all")
		q.Set("per_page", strconv.Itoa(checkRunsPerPage))
		q.Set("page", strconv.Itoa(page))
		req, err := c.buildRequest(ctx, http.MethodGet, base+"?"+q.Encode(), nil, installationID)
		if err != nil {
			return nil, false, err
		}
		total, runs, err := c.doCheckRunsPage(req)
		if err != nil {
			return nil, false, err
		}
		out = append(out, runs...)
		if len(out) >= total {
			return out, false, nil
		}
		if len(runs) < checkRunsPerPage {
			// A short page before total_count: the listing shifted under
			// us or GitHub capped it. Incomplete either way.
			return out, true, nil
		}
	}
	return out, true, nil
}

// doCheckRunsPage executes one check-runs page request.
func (c *Client) doCheckRunsPage(req *http.Request) (int, []CheckRunSummary, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("githubclient: list check runs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("list check runs", resp); err != nil {
		return 0, nil, err
	}
	var body struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			ID          int64      `json:"id"`
			Name        string     `json:"name"`
			Status      string     `json:"status"`
			Conclusion  *string    `json:"conclusion"`
			StartedAt   *time.Time `json:"started_at"`
			CompletedAt *time.Time `json:"completed_at"`
			CheckSuite  struct {
				ID int64 `json:"id"`
			} `json:"check_suite"`
			App struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, nil, fmt.Errorf("githubclient: decode check runs: %w", err)
	}
	runs := make([]CheckRunSummary, 0, len(body.CheckRuns))
	for _, r := range body.CheckRuns {
		s := CheckRunSummary{
			ID:           r.ID,
			Name:         r.Name,
			Status:       r.Status,
			StartedAt:    r.StartedAt,
			CompletedAt:  r.CompletedAt,
			CheckSuiteID: r.CheckSuite.ID,
			AppSlug:      r.App.Slug,
		}
		if r.Conclusion != nil {
			s.Conclusion = *r.Conclusion
		}
		runs = append(runs, s)
	}
	return body.TotalCount, runs, nil
}
