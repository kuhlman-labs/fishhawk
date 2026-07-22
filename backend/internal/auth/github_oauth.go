package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub's published OAuth endpoints. Override via NewGitHubOAuth's
// urls argument in tests.
const (
	defaultAuthorizeURL = "https://github.com/login/oauth/authorize"
	defaultTokenURL     = "https://github.com/login/oauth/access_token"
	defaultUserURL      = "https://api.github.com/user"
	defaultOrgsURL      = "https://api.github.com/user/orgs"
)

// OAuthURLs lets tests substitute httptest.Server URLs for the
// real GitHub endpoints.
type OAuthURLs struct {
	AuthorizeURL string
	TokenURL     string
	UserURL      string
	OrgsURL      string
}

// GitHubOAuth wraps the OAuth web-app flow against GitHub.
//
// AuthorizeURL renders the URL the browser should redirect to on
// /v0/auth/github/login. ExchangeCode swaps the authorization
// code for an access token. FetchProfile uses that token to
// pull the user's GitHub profile.
//
// Production wiring: NewGitHubOAuth(clientID, clientSecret,
// callbackURL, OAuthURLs{}) (zero URLs → defaults).
type GitHubOAuth struct {
	clientID     string
	clientSecret string
	callbackURL  string
	urls         OAuthURLs
	http         *http.Client
}

// NewGitHubOAuth returns a configured client. clientID +
// clientSecret are the GitHub OAuth App credentials; callbackURL
// is the publicly-reachable URL of /v0/auth/github/callback.
func NewGitHubOAuth(clientID, clientSecret, callbackURL string, urls OAuthURLs) *GitHubOAuth {
	if urls.AuthorizeURL == "" {
		urls.AuthorizeURL = defaultAuthorizeURL
	}
	if urls.TokenURL == "" {
		urls.TokenURL = defaultTokenURL
	}
	if urls.UserURL == "" {
		urls.UserURL = defaultUserURL
	}
	if urls.OrgsURL == "" {
		urls.OrgsURL = defaultOrgsURL
	}
	return &GitHubOAuth{
		clientID:     clientID,
		clientSecret: clientSecret,
		callbackURL:  callbackURL,
		urls:         urls,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthorizeURL builds the URL the login handler redirects the
// browser to. Includes state + redirect_uri + the requested scopes:
// read:user + user:email for the profile, and read:org so the
// auto-join bootstrap's /user/orgs read (E44.3) sees private org
// memberships, not just public ones.
func (g *GitHubOAuth) AuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", g.callbackURL)
	q.Set("scope", "read:user user:email read:org")
	q.Set("state", state)
	q.Set("allow_signup", "false")
	return g.urls.AuthorizeURL + "?" + q.Encode()
}

// ExchangeCode swaps an authorization code for an access token.
// Returns the access token string on success.
func (g *GitHubOAuth) ExchangeCode(ctx context.Context, code string) (string, error) {
	if code == "" {
		return "", errors.New("auth: empty OAuth code")
	}
	body := url.Values{}
	body.Set("client_id", g.clientID)
	body.Set("client_secret", g.clientSecret)
	body.Set("code", code)
	body.Set("redirect_uri", g.callbackURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		g.urls.TokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("auth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: exchange code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		brief := readBriefBody(resp.Body)
		return "", fmt.Errorf("auth: token exchange returned %d: %s", resp.StatusCode, brief)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("auth: decode token response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("auth: github oauth error: %s: %s", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", errors.New("auth: token response missing access_token")
	}
	return out.AccessToken, nil
}

// FetchProfile reads the authenticated user's GitHub profile via
// /user. Returns the bits we persist on the users row.
func (g *GitHubOAuth) FetchProfile(ctx context.Context, accessToken string) (*GitHubProfile, error) {
	if accessToken == "" {
		return nil, errors.New("auth: empty access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.urls.UserURL, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: user endpoint returned %d", resp.StatusCode)
	}

	var body struct {
		ID    int64   `json:"id"`
		Login string  `json:"login"`
		Name  string  `json:"name"`
		Email *string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth: decode user: %w", err)
	}
	body.Login = canonicalGitHubLogin(body.Login)
	if body.ID == 0 || body.Login == "" {
		return nil, errors.New("auth: user response missing id or login")
	}
	if body.Name == "" {
		// GitHub allows users to keep `name` empty; fall back to
		// the login so the User row's NOT NULL stays satisfied.
		body.Name = body.Login
	}
	return &GitHubProfile{
		ID:    body.ID,
		Login: body.Login,
		Name:  body.Name,
		Email: body.Email,
	}, nil
}

// ListUserOrgKeys lists the org logins of the authenticated user via
// GET /user/orgs, using the USER's OAuth access token (never an App
// token) — the sole live-forge read of the E44.3 membership gate,
// consumed only by the auto-join bootstrap. Single page, per_page=100:
// a membership beyond the first page fails auto-join CLOSED and the
// remedy is an invited account_members row, which admits DB-only.
// Note the same caveat applies to GitHub's third-party-application
// restrictions, which can silently omit an org from this listing.
func (g *GitHubOAuth) ListUserOrgKeys(ctx context.Context, accessToken string) ([]string, error) {
	if accessToken == "" {
		return nil, errors.New("auth: empty access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.urls.OrgsURL+"?per_page=100", nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build orgs request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: list user orgs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: orgs endpoint returned %d", resp.StatusCode)
	}

	var body []struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth: decode orgs: %w", err)
	}
	keys := make([]string, 0, len(body))
	for _, org := range body {
		if org.Login != "" {
			keys = append(keys, org.Login)
		}
	}
	return keys, nil
}

func readBriefBody(r io.Reader) string {
	limited := io.LimitReader(r, 256)
	b, _ := io.ReadAll(limited)
	return strings.TrimSpace(string(b))
}

// canonicalGitHubLogin returns the canonical Fishhawk login for a raw GitHub
// login value. Enterprise Managed User (EMU) logins carry a
// "<username>_<shortcode>" enterprise short-code suffix (e.g. "alice_acme");
// Fishhawk keys identity on the FULL login (short code included), so the
// suffix is preserved verbatim — never stripped or split — and only
// surrounding whitespace is trimmed. A plain github.com login (no underscore)
// passes through unchanged. Mirrors identity.canonicalGitHubLogin so the
// OAuth web flow and the device flow agree on the subject/profile login.
func canonicalGitHubLogin(login string) string {
	return strings.TrimSpace(login)
}
