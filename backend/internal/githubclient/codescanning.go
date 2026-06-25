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

// ListCodeScanningAlerts fetches the OPEN code-scanning (CodeQL/SAST) alerts
// for a repository at the given git ref (#1096). It is the typed REST read
// the webhook handler uses to consume the alerts GitHub's default-setup
// CodeQL run produced for an implement PR's head SHA, so high-severity
// findings can hold the implement-review gate instead of first surfacing as a
// blocked required check at merge time.
//
//	GET /repos/{owner}/{repo}/code-scanning/alerts?ref={ref}&state=open&per_page=100
//
// The read is scoped to state=open: only UNRESOLVED alerts can hold the gate,
// and a fixed/dismissed alert dropping out of this list on a clean re-scan is
// exactly how the gate clears after a fixup. ref is normally an implement
// PR's head SHA (the `?ref=` filter accepts a full SHA, a branch, or a tag),
// so the alerts returned are the ones GitHub analyzed for that commit.
//
// Returns securityscan.Finding values (the cross-slice contract type) rather
// than a client-local shape, so the orchestration slice can compose the
// securityscan filters directly on the result. Pages until exhaustion via the
// rel="next" Link header — the same mechanism ListIssueComments relies on.
//
// Returns a typed error on non-2xx (ErrNotFound when code scanning is not
// enabled / no analysis exists for the ref or the repo isn't visible,
// ErrForbidden on auth / Advanced-Security-not-enabled, ErrValidation on a
// bad ref). The merge-gate caller (auditcomplete) fails OPEN on any such
// error rather than treating an unreadable scan as "no findings".
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

// decodeCodeScanningAlertsPage handles one page of code-scanning alerts and
// returns the next-page URL if Link advertises one. Split out so the
// pagination loop stays readable, mirroring decodeIssueCommentsPage.
//
// Severity is sourced from rule.security_severity_level (the
// none/low/medium/high/critical scale used for security gating), NOT
// rule.severity (the note/warning/error tool scale) — the gate keys on
// security severity. Path/line come from the most_recent_instance's location.
func decodeCodeScanningAlertsPage(resp *http.Response) ([]securityscan.Finding, string, error) {
	if err := classifyStatus("list code-scanning alerts", resp); err != nil {
		return nil, "", err
	}
	var body []struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		Rule    struct {
			ID                    string `json:"id"`
			SecuritySeverityLevel string `json:"security_severity_level"`
			Description           string `json:"description"`
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
			Number:      a.Number,
			RuleID:      a.Rule.ID,
			Severity:    a.Rule.SecuritySeverityLevel,
			Description: a.Rule.Description,
			Path:        a.MostRecentInstance.Location.Path,
			Line:        a.MostRecentInstance.Location.StartLine,
			State:       a.State,
			URL:         a.HTMLURL,
		})
	}
	return out, nextPageURL(resp.Header.Get("Link")), nil
}
