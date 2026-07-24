package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitLabOAuthURLs lets tests substitute httptest.Server URLs for a
// GitLab instance's OAuth + API endpoints. Empty fields are filled from
// the base URL passed to NewGitLabOAuth.
type GitLabOAuthURLs struct {
	AuthorizeURL string
	TokenURL     string
	UserURL      string
}

// gitLabReadAPIScope is the OAuth scope the sign-in flow requests.
//
// read_api is REQUIRED because the flow makes TWO authenticated reads with
// the resulting user token: GET /api/v4/user (the profile) AND GET
// /api/v4/groups (the group-membership auto-join list, gitlab_membership.go).
// read_user grants ONLY the former — it does not authorize /api/v4/groups —
// so it would silently fail the group listing and deny every group-granularity
// auto-join. read_api authorizes both, so it is the single scope the whole
// flow needs.
const gitLabReadAPIScope = "read_api"

// GitLabOAuth wraps the OAuth web-app flow against a GitLab instance
// (SaaS gitlab.com or self-managed), mirroring GitHubOAuth.
//
// AuthorizeURL renders the browser redirect on /v0/auth/gitlab/login.
// ExchangeCode swaps the authorization code for an access token.
// FetchProfile uses that token to pull the user's GitLab profile via
// GET /api/v4/user.
//
// Endpoint binding — DEPLOYMENT DEFAULT ONLY, mirroring GitHubOAuth
// (github_oauth.go). Every operation runs at or BEFORE the user is
// identified, so none can know which installation (hence which data-
// resident host) applies. The whole flow targets the deployment-default
// GitLab host configured on this client (FISHHAWKD_GITLAB_BASE_URL); there
// is no per-installation ResolveBaseURL hook to consult, because no post-
// identification web-OAuth consumer of installations.oauth_base_url exists
// (E44.16 / #2094).
//
// Production wiring: NewGitLabOAuth(baseURL, clientID, clientSecret,
// callbackURL, GitLabOAuthURLs{}).
type GitLabOAuth struct {
	clientID     string
	clientSecret string
	callbackURL  string
	urls         GitLabOAuthURLs
	http         *http.Client
}

// NewGitLabOAuth returns a configured client. baseURL is the GitLab
// instance root (e.g. https://gitlab.com or a self-managed host); the
// OAuth authorize/token endpoints live at {base}/oauth/authorize +
// {base}/oauth/token and the profile at {base}/api/v4/user. clientID +
// clientSecret are the GitLab (group-scoped) OAuth application credentials;
// callbackURL is the publicly-reachable URL of /v0/auth/gitlab/callback.
// A non-empty urls field overrides the derived endpoint (tests point it at
// an httptest.Server).
func NewGitLabOAuth(baseURL, clientID, clientSecret, callbackURL string, urls GitLabOAuthURLs) *GitLabOAuth {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if urls.AuthorizeURL == "" {
		urls.AuthorizeURL = base + "/oauth/authorize"
	}
	if urls.TokenURL == "" {
		urls.TokenURL = base + "/oauth/token"
	}
	if urls.UserURL == "" {
		urls.UserURL = base + "/api/v4/user"
	}
	return &GitLabOAuth{
		clientID:     clientID,
		clientSecret: clientSecret,
		callbackURL:  callbackURL,
		urls:         urls,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthorizeURL builds the URL the login handler redirects the browser
// to. It requests the read_api scope (see gitLabReadAPIScope) because
// the callback makes two authenticated reads — GET /api/v4/user for the
// profile and GET /api/v4/groups for the group-membership auto-join
// list — and only read_api authorizes both.
func (g *GitLabOAuth) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", g.callbackURL)
	q.Set("response_type", "code")
	// read_api authorizes BOTH GET /api/v4/user and GET /api/v4/groups;
	// read_user would not grant the groups read the auto-join gate needs.
	q.Set("scope", gitLabReadAPIScope)
	q.Set("state", state)
	return g.urls.AuthorizeURL + "?" + q.Encode()
}

// ExchangeCode swaps an authorization code for an access token via
// POST {base}/oauth/token (grant_type=authorization_code). Returns the
// access token string on success.
func (g *GitLabOAuth) ExchangeCode(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("auth: empty OAuth code")
	}
	body := url.Values{}
	body.Set("client_id", g.clientID)
	body.Set("client_secret", g.clientSecret)
	body.Set("code", code)
	body.Set("grant_type", "authorization_code")
	body.Set("redirect_uri", g.callbackURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.urls.TokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("auth: build gitlab token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: exchange gitlab code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		brief := readBriefBody(resp.Body)
		return "", fmt.Errorf("auth: gitlab token exchange returned %d: %s", resp.StatusCode, brief)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("auth: decode gitlab token response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("auth: gitlab oauth error: %s: %s", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", errors.New("auth: gitlab token response missing access_token")
	}
	return out.AccessToken, nil
}

// FetchProfile reads the authenticated user's GitLab profile via GET
// {base}/api/v4/user. Returns the bits we persist on the users row, in
// the shared GitHubProfile shape (ID/Login/Name/Email) the callback and
// SignIn path consume for either forge. Login is the GitLab username;
// Name falls back to the username so the User row's NOT NULL stays
// satisfied when a GitLab account has an empty name.
func (g *GitLabOAuth) FetchProfile(ctx context.Context, accessToken string) (*GitHubProfile, error) {
	if accessToken == "" {
		return nil, errors.New("auth: empty access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.urls.UserURL, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build gitlab user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch gitlab user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: gitlab user endpoint returned %d", resp.StatusCode)
	}

	var body struct {
		ID       int64   `json:"id"`
		Username string  `json:"username"`
		Name     string  `json:"name"`
		Email    *string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth: decode gitlab user: %w", err)
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.ID == 0 || body.Username == "" {
		return nil, errors.New("auth: gitlab user response missing id or username")
	}
	if body.Name == "" {
		body.Name = body.Username
	}
	if body.Email != nil && *body.Email == "" {
		// Normalize an empty-string email to NULL so it round-trips like
		// GitHub's *string (which is nil when the user hides their email).
		body.Email = nil
	}
	return &GitHubProfile{
		ID:    body.ID,
		Login: body.Username,
		Name:  body.Name,
		Email: body.Email,
	}, nil
}
