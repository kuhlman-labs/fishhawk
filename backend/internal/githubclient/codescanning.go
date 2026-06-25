package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// ListCodeScanningAlerts fetches the open code-scanning (CodeQL/SAST)
// alerts for a repo at the given ref (#1096). It is the GitHub read
// surface the implement-review security-findings gate (slices 2-5) is
// built on; this slice adds only the typed client method and decodes
// the live REST shape directly into []securityscan.Finding, so the
// Finding seam lives entirely in the securityscan package.
//
//	GET /repos/{owner}/{repo}/code-scanning/alerts?ref={ref}&state=open&per_page=100
//
// Each alert carries `rule.id`, `rule.security_severity_level`
// (low|medium|high|critical, null for non-security rules → ""), and
// `most_recent_instance.location.path`/`.start_line`. Pages until
// exhaustion via the shared rel="next" Link header, accumulating in
// order — the same mechanism ListIssueComments/ListTeamMembers rely on.
//
// Returns ErrNotFound when the repo/ref isn't visible to the
// installation or code scanning is not enabled (404), ErrForbidden on
// auth issues (401/403), ErrValidation on a malformed request (422).
func (c *Client) ListCodeScanningAlerts(ctx context.Context, installationID int64, repo RepoRef, ref string) ([]securityscan.Finding, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}
	if ref == "" {
		return nil, errors.New("githubclient: ref is required")
	}

	pagePath := "/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/code-scanning/alerts?ref=" + url.QueryEscape(ref) +
		"&state=open&per_page=100"
	endpoint := c.endpoint(pagePath)

	var out []securityscan.Finding
	for endpoint != "" {
		req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
		if err != nil {
			return nil, err
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("githubclient: list code-scanning alerts: %w", err)
		}
		findings, next, err := decodeCodeScanningAlertsPage(resp)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
		endpoint = next
	}
	return out, nil
}

// decodeCodeScanningAlertsPage handles one page of code-scanning alerts
// and returns the next-page URL if Link advertises one. Split out so
// the pagination loop above stays readable, mirroring
// decodeIssueCommentsPage / decodeTeamMembersPage.
func decodeCodeScanningAlertsPage(resp *http.Response) ([]securityscan.Finding, string, error) {
	if err := classifyStatus("list code-scanning alerts", resp); err != nil {
		return nil, "", err
	}
	var body []struct {
		Rule struct {
			ID                    string `json:"id"`
			SecuritySeverityLevel string `json:"security_severity_level"`
		} `json:"rule"`
		MostRecentInstance struct {
			Location struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
			} `json:"location"`
		} `json:"most_recent_instance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("githubclient: decode code-scanning alerts: %w", err)
	}
	out := make([]securityscan.Finding, 0, len(body))
	for _, a := range body {
		out = append(out, securityscan.Finding{
			RuleID:    a.Rule.ID,
			Severity:  a.Rule.SecuritySeverityLevel,
			Path:      a.MostRecentInstance.Location.Path,
			StartLine: a.MostRecentInstance.Location.StartLine,
		})
	}
	return out, nextPageURL(resp.Header.Get("Link")), nil
}
