package githubclient

// This file carries the typed read of GitHub's code-scanning alerts REST
// API (#1096): the implement-review gate consumes existing CodeQL/SAST
// alerts so a new high-severity finding on the implement diff is caught
// in-loop instead of first appearing as a blocked required check at merge.
// We only READ alerts here — CodeQL itself runs via GitHub default-setup
// (.github/workflows/** is human-led), this client never enables it.
//
// The decode targets securityscan.Finding (the cross-slice contract type)
// directly rather than a private alert struct, so the webhook ingest can
// compose the securityscan.FilterHighSeverity / FilterToDiffFiles pure
// filters on the result without a second mapping. securityscan is a leaf
// contract package with no dependencies, so this import adds no cycle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// ListCodeScanningAlerts reads the OPEN code-scanning alerts for a repo,
// optionally scoped to a git ref, and returns them as the normalized
// securityscan.Finding contract shape.
//
//	GET /repos/{owner}/{repo}/code-scanning/alerts?state=open&ref={ref}
//
// ref="" reads the repository's default-branch alerts; a non-empty ref
// (a branch, tag, or "refs/pull/{n}/merge") scopes to that ref. Only
// state=open alerts are returned: a fixed or dismissed alert must not
// gate, and a clean re-scan after a fixup naturally drops out of the
// result. Severity is NOT filtered here — that is securityscan's pure
// FilterHighSeverity, the single severity authority — so callers get
// every open alert and threshold it themselves.
//
// Pages until exhaustion via the rel="next" Link header (the same
// mechanism ListTeamMembers relies on). Returns ErrNotFound when the
// repo isn't visible OR code scanning is not enabled (GitHub returns 404
// for a repo without code scanning), and ErrForbidden on auth/permission
// issues — callers tolerate both as "no findings recorded".
func (c *Client) ListCodeScanningAlerts(ctx context.Context, installationID int64, repo RepoRef, ref string) ([]securityscan.Finding, error) {
	if c.Tokens == nil {
		return nil, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return nil, errors.New("githubclient: repo owner and name required")
	}

	q := url.Values{}
	q.Set("state", "open")
	q.Set("per_page", "100")
	if ref != "" {
		q.Set("ref", ref)
	}
	endpoint := c.endpoint("/repos/"+url.PathEscape(repo.Owner)+
		"/"+url.PathEscape(repo.Name)+"/code-scanning/alerts") + "?" + q.Encode()

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
		findings, next, err := decodeCodeScanningPage(resp)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
		endpoint = next
	}
	return out, nil
}

// codeScanningAlert is the subset of the code-scanning alert JSON shape we
// decode. Kept private to this file: callers see securityscan.Finding.
type codeScanningAlert struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Rule    struct {
		ID                    string `json:"id"`
		Name                  string `json:"name"`
		Description           string `json:"description"`
		SecuritySeverityLevel string `json:"security_severity_level"`
	} `json:"rule"`
	Tool struct {
		Name string `json:"name"`
	} `json:"tool"`
	MostRecentInstance struct {
		Ref       string `json:"ref"`
		CommitSHA string `json:"commit_sha"`
		Location  struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
		} `json:"location"`
	} `json:"most_recent_instance"`
}

// decodeCodeScanningPage handles one page of alerts, mapping each to a
// securityscan.Finding, and returns the rel="next" page URL if Link
// advertises one. Split out so the pagination loop stays readable.
func decodeCodeScanningPage(resp *http.Response) ([]securityscan.Finding, string, error) {
	if err := classifyStatus("list code-scanning alerts", resp); err != nil {
		return nil, "", err
	}
	var body []codeScanningAlert
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("githubclient: decode code-scanning alerts: %w", err)
	}
	out := make([]securityscan.Finding, 0, len(body))
	for _, a := range body {
		description := a.Rule.Description
		if description == "" {
			description = a.Rule.Name
		}
		out = append(out, securityscan.Finding{
			Number:      a.Number,
			RuleID:      a.Rule.ID,
			Description: description,
			Severity:    a.Rule.SecuritySeverityLevel,
			State:       a.State,
			Path:        a.MostRecentInstance.Location.Path,
			StartLine:   a.MostRecentInstance.Location.StartLine,
			CommitSHA:   a.MostRecentInstance.CommitSHA,
			Ref:         a.MostRecentInstance.Ref,
			Tool:        a.Tool.Name,
			HTMLURL:     a.HTMLURL,
		})
	}
	return out, nextPageURL(resp.Header.Get("Link")), nil
}
