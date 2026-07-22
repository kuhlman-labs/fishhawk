package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitLab group membership for the login gate (E44.8 / #1832).
//
// SEAM-FIRST: this lister ships AHEAD of a GitLab browser sign-in flow.
// No GitLab OAuth callback exists yet, so in production the resolver
// never sees provider="gitlab" and this client is unreachable; what
// lands here is the membership seam behind the same ForgeMembershipLister
// interface the GitHub path uses, so the sign-in flow (a separate,
// operator-filed follow-up) plugs in without touching admission logic.

// gitLabGroupsPerPage is the page size for the group listing. Each page
// is followed until exhausted (see gitLabMaxGroupPages) — a matching
// group on a later page must not silently deny a member.
const gitLabGroupsPerPage = 100

// gitLabMaxGroupPages bounds the pagination walk so a pathological
// account (or a server that never stops advertising a next page) cannot
// loop unbounded. 50 pages x 100 = 5000 groups.
const gitLabMaxGroupPages = 50

// GitLabMembershipLister reads the authenticated user's GitLab group
// memberships (GET /api/v4/groups) with the USER's OAuth access token —
// never the deployment's FISHHAWKD_GITLAB_TOKEN. It satisfies
// ForgeMembershipLister, feeding group-granularity auto-join.
type GitLabMembershipLister struct {
	baseURL string
	http    *http.Client
}

// NewGitLabMembershipLister builds the lister for a GitLab instance
// base URL (SaaS or self-managed). An empty base URL returns nil so an
// unconfigured deployment registers NO GitLab lister and the resolver
// keeps denying provider="gitlab".
func NewGitLabMembershipLister(baseURL string) *GitLabMembershipLister {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &GitLabMembershipLister{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// ListUserOrgKeys returns the full_path of every group the token's user
// belongs to. full_path — not name — is the stable addressable key an
// accounts.account_key records for a group-granularity account.
//
// Every failure (empty token, transport error, non-200, undecodable
// body, pagination cap exceeded) returns an error so the caller fails
// CLOSED rather than admitting on a partial listing.
func (l *GitLabMembershipLister) ListUserOrgKeys(ctx context.Context, accessToken string) ([]string, error) {
	if accessToken == "" {
		return nil, errors.New("auth: empty access token")
	}
	keys := make([]string, 0, gitLabGroupsPerPage)
	for page := 1; ; page++ {
		if page > gitLabMaxGroupPages {
			return nil, fmt.Errorf("auth: gitlab group listing exceeded %d pages", gitLabMaxGroupPages)
		}
		pageKeys, more, err := l.listGroupPage(ctx, accessToken, page)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pageKeys...)
		if !more {
			return keys, nil
		}
	}
}

// listGroupPage fetches one page and reports whether another follows.
// "Another follows" is decided by the Link rel="next" header when the
// server sends one, falling back to a full page of results (GitLab
// omits Link headers on some deployments/proxies).
func (l *GitLabMembershipLister) listGroupPage(ctx context.Context, accessToken string, page int) ([]string, bool, error) {
	endpoint := l.baseURL + "/api/v4/groups?min_access_level=10&per_page=" +
		strconv.Itoa(gitLabGroupsPerPage) + "&page=" + strconv.Itoa(page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("auth: build gitlab groups request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := l.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("auth: list gitlab groups: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("auth: gitlab groups endpoint returned %d", resp.StatusCode)
	}

	var body []struct {
		FullPath string `json:"full_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("auth: decode gitlab groups: %w", err)
	}
	keys := make([]string, 0, len(body))
	for _, g := range body {
		if g.FullPath != "" {
			keys = append(keys, g.FullPath)
		}
	}
	if link := resp.Header.Get("Link"); link != "" {
		return keys, hasNextLink(link), nil
	}
	return keys, len(body) == gitLabGroupsPerPage, nil
}

// hasNextLink reports whether an RFC 5988 Link header advertises a
// rel="next" relation.
func hasNextLink(header string) bool {
	for _, part := range strings.Split(header, ",") {
		for _, param := range strings.Split(part, ";")[1:] {
			v := strings.TrimSpace(param)
			if v == `rel="next"` || v == "rel=next" {
				return true
			}
		}
	}
	return false
}

var _ ForgeMembershipLister = (*GitLabMembershipLister)(nil)
