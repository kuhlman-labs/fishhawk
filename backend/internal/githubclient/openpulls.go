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

// openPullsPerPage is GitHub's per_page ceiling for the pulls listing.
const openPullsPerPage = 100

// OpenPullRequest is one open pull request as the pulls listing reports it,
// reduced to what the upkeep scan's Dependabot coverage reads (#3750).
type OpenPullRequest struct {
	Number    int
	HTMLURL   string
	Title     string
	Body      string // "" when GitHub reports null
	UserLogin string // "dependabot[bot]" for a Dependabot pull request
	HeadRef   string
	// BaseRef is the branch the pull request targets (`base.ref`) and
	// DefaultBranch the base repository's default branch
	// (`base.repo.default_branch`); "" when GitHub omits either.
	BaseRef       string
	DefaultBranch string
}

// ListOpenPullRequests lists a repository's open pull requests, at most maxPulls
// of them (#3750, the upkeep scan's Dependabot coverage read).
//
//	GET /repos/{owner}/{repo}/pulls?state=open&per_page=100&page=N
//
// Page URLs are built from the client's own base; the Link header is read
// only for whether a rel="next" page exists (the userreports.go posture), so
// a response header never redirects the installation token. truncated is
// true when the walk stops with pull requests remaining — the maxPulls cap was
// reached, or a page came back empty while advertising a next page — and a
// caller must then treat the listing as incomplete. maxPulls < 1 is refused before
// any request. Returns ErrNotFound when the repo isn't visible, ErrForbidden
// on auth issues, ErrValidation on a 422; any other non-2xx is an untyped
// status error via classifyStatus.
func (c *Client) ListOpenPullRequests(ctx context.Context, scope forge.CredentialScope, repo RepoRef, maxPulls int) ([]OpenPullRequest, bool, error) {
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
	if maxPulls < 1 {
		return nil, false, fmt.Errorf("githubclient: list open pulls: maxPulls must be >= 1, got %d", maxPulls)
	}

	base := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) + "/pulls")
	out := []OpenPullRequest{}
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("per_page", strconv.Itoa(openPullsPerPage))
		q.Set("page", strconv.Itoa(page))
		req, err := c.buildRequest(ctx, http.MethodGet, base+"?"+q.Encode(), nil, installationID)
		if err != nil {
			return nil, false, err
		}
		pulls, hasNext, err := c.doOpenPullsPage(req)
		if err != nil {
			return nil, false, err
		}
		for _, p := range pulls {
			if len(out) == maxPulls {
				return out, true, nil
			}
			out = append(out, p)
		}
		if !hasNext {
			return out, false, nil
		}
		if len(out) == maxPulls {
			return out, true, nil
		}
		if len(pulls) == 0 {
			// A next page advertised after an empty one: the walk cannot
			// make progress, so stop rather than loop.
			return out, true, nil
		}
	}
}

// doOpenPullsPage executes one pulls page request and reports whether the
// Link header advertises a next page.
func (c *Client) doOpenPullsPage(req *http.Request) ([]OpenPullRequest, bool, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("githubclient: list open pulls: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classifyStatus("list open pulls", resp); err != nil {
		return nil, false, err
	}
	var body []struct {
		Number  int     `json:"number"`
		HTMLURL string  `json:"html_url"`
		Title   string  `json:"title"`
		Body    *string `json:"body"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo *struct {
				DefaultBranch string `json:"default_branch"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("githubclient: decode open pulls: %w", err)
	}
	pulls := make([]OpenPullRequest, 0, len(body))
	for _, p := range body {
		pr := OpenPullRequest{
			Number:    p.Number,
			HTMLURL:   p.HTMLURL,
			Title:     p.Title,
			UserLogin: p.User.Login,
			HeadRef:   p.Head.Ref,
			BaseRef:   p.Base.Ref,
		}
		if p.Base.Repo != nil {
			pr.DefaultBranch = p.Base.Repo.DefaultBranch
		}
		if p.Body != nil {
			pr.Body = *p.Body
		}
		pulls = append(pulls, pr)
	}
	return pulls, nextPageURL(resp.Header.Get("Link")) != "", nil
}
