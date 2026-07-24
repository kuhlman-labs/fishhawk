package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBaseURL is GitHub's REST API root. Overridable (via the
// unexported apiBaseURL field, in-package tests) so tests point it at
// an httptest server — mirroring githubclient.Client.BaseURL.
const DefaultAPIBaseURL = "https://api.github.com"

// DefaultOAuthBaseURL is GitHub's OAuth/device-flow host. The device
// code + access-token endpoints live under here, not under the API
// host.
const DefaultOAuthBaseURL = "https://github.com"

// subjectPrefix qualifies every subject this provider emits or
// accepts. A subject is "github:<login>"; PermissionLevel /
// ResolveMembership strip it back to the bare login for the REST call.
const subjectPrefix = "github:"

// deviceFlowScope is the OAuth scope requested for the device flow —
// enough to read the authenticated user's login. Repository
// permission and org/team reads use the REST token accessor, not this
// device-flow token.
const deviceFlowScope = "read:user"

// slowDownIncrement is the interval bump GitHub's device flow mandates
// on a "slow_down" poll response (add 5s to the polling interval).
const slowDownIncrement = 5 * time.Second

// minPollInterval is the floor applied to the forge-supplied device-flow
// poll interval. GitHub's documented default is 5s; when the device-code
// response omits `interval` or returns a non-positive value, deriving the
// interval directly yields 0, and the ctx-aware sleep returns immediately
// for a non-positive duration — an authorization_pending loop would then
// hammer the OAuth token endpoint until expiry. Clamping the forge value up
// to this floor keeps a missing/zero interval from busy-polling. A positive
// test override (p.pollInterval) still wins.
const minPollInterval = 5 * time.Second

// GitHubIdentityProvider is the hand-rolled GitHub REST implementation
// of IdentityProvider. It confines every GitHub specific (the device
// flow, the collaborators-permission endpoint, the members/teams
// endpoints, and the role_name → Permission mapping) to this package.
//
// Concurrent use is safe: the struct holds only immutable config.
//
// Endpoint binding — Mode 1 (per-DEPLOYMENT) only; per-installation
// (Mode 2) routing is DEFERRED (E44.16 / #2094, binding condition 1).
// apiBaseURL / oauthBaseURL are set once at construction from the
// deployment-default endpoints (WithBaseURLs, threaded from the
// FISHHAWKD_GITHUB_API_URL / FISHHAWKD_OAUTH_* config); they are never
// re-resolved per installation. The reason the per-installation leg is
// deferred rather than wired:
//
//   - The provider is a boot-time SINGLETON and its IdentityProvider
//     interface carries NO installation ref on any method.
//   - oauthBaseURL feeds the device-flow / OAuth LOGIN host, and login is
//     pre-identification — the installation is unknown until AFTER the
//     default-host device flow resolves the subject. So oauth_base_url has
//     no genuine post-identification per-installation consumer here.
//   - PermissionLevel / ResolveMembership run post-identification but take
//     repo / subject / ref (not an installation ref) and read the API host,
//     with no seam to resolve an installation from those inputs.
//
// Shipping a per-installation construction path exercised only by tests
// would be dead routing (binding condition 1 forbids it). A per-installation
// identity leg needs an interface that carries installation context — tracked
// as a follow-up, mirroring the deferred web-OAuth leg
// (backend/internal/auth/github_oauth.go).
type GitHubIdentityProvider struct {
	// clientID is the OAuth App client_id. The device flow needs no
	// client secret.
	clientID string

	// apiBaseURL / oauthBaseURL default to the GitHub hosts. Both are
	// overridable so in-package tests point them at an httptest server.
	apiBaseURL   string
	oauthBaseURL string

	// token, when non-nil, returns a bearer token for authenticated
	// REST reads (PermissionLevel / ResolveMembership). Nil → the
	// reads go out anonymously.
	token func(context.Context) (string, error)

	httpClient *http.Client

	// Test seams (in-package): pollInterval overrides the forge's
	// device-flow interval; sleep is the interval wait (ctx-aware);
	// now is the clock for the expiry deadline. All default to their
	// production behavior in NewGitHubIdentityProvider.
	pollInterval time.Duration
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
}

// Option customizes a GitHubIdentityProvider at construction. Options are
// applied after the production defaults, so a test can point the provider at
// an httptest mock server. Additive and backward-compatible: existing
// two-arg callers pass no options and are unchanged.
type Option func(*GitHubIdentityProvider)

// WithBaseURLs overrides the REST API and OAuth base URLs. Intended for
// tests that exercise the real provider end-to-end against an httptest mock
// server (E39.5 / #1710) — production callers omit it and keep the GitHub
// hosts.
func WithBaseURLs(apiBase, oauthBase string) Option {
	return func(p *GitHubIdentityProvider) {
		p.apiBaseURL = apiBase
		p.oauthBaseURL = oauthBase
	}
}

// NewGitHubIdentityProvider constructs a GitHub identity provider from
// the OAuth App client_id and an optional REST token accessor (nil →
// anonymous REST reads). It returns the interface, following the
// githuboidc.New idiom; the production defaults (GitHub hosts, a 30s
// HTTP client, a ctx-aware sleep, time.Now) are filled in here. Optional
// Options (e.g. WithBaseURLs) are applied last, overriding the defaults.
func NewGitHubIdentityProvider(clientID string, token func(context.Context) (string, error), opts ...Option) IdentityProvider {
	p := &GitHubIdentityProvider{
		clientID:     clientID,
		apiBaseURL:   DefaultAPIBaseURL,
		oauthBaseURL: DefaultOAuthBaseURL,
		token:        token,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		sleep:        sleepCtx,
		now:          time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Compile-time assertion that GitHubIdentityProvider satisfies the
// interface.
var _ IdentityProvider = (*GitHubIdentityProvider)(nil)

// sleepCtx waits d honoring ctx cancellation; the default poll wait.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// deviceCodeResponse is the subset of POST /login/device/code we read.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// accessTokenResponse is the subset of POST /login/oauth/access_token
// we read. Error carries the device-flow poll state
// ("authorization_pending", "slow_down", "expired_token",
// "access_denied") when AccessToken is empty.
type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Interval    int    `json:"interval"`
}

// VerifyUser drives the GitHub OAuth device flow to completion and
// returns the provider-qualified subject ("github:<login>").
func (p *GitHubIdentityProvider) VerifyUser(ctx context.Context, prompt DeviceCodePrompt) (string, error) {
	device, err := p.requestDeviceCode(ctx)
	if err != nil {
		return "", err
	}
	if prompt != nil {
		prompt(device.UserCode, device.VerificationURI)
	}

	// Poll interval: the forge's suggested interval, floored so a
	// missing/zero interval never collapses to a busy-poll, unless a test
	// overrides it.
	interval := time.Duration(device.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	if p.pollInterval > 0 {
		interval = p.pollInterval
	}

	// The device code expires after ExpiresIn seconds; poll until then,
	// the forge authorizes, or ctx is cancelled.
	deadline := p.now().Add(time.Duration(device.ExpiresIn) * time.Second)

	for {
		if ctx.Err() != nil {
			return "", ErrVerificationTimeout
		}
		if !p.now().Before(deadline) {
			return "", ErrVerificationTimeout
		}
		if err := p.sleep(ctx, interval); err != nil {
			return "", ErrVerificationTimeout
		}

		tok, err := p.pollAccessToken(ctx, device.DeviceCode)
		if err != nil {
			return "", err
		}
		switch tok.Error {
		case "":
			// Authorized — resolve the login with the fresh token.
			return p.resolveLogin(ctx, tok.AccessToken)
		case "authorization_pending":
			continue
		case "slow_down":
			// Honor the mandated back-off: prefer the forge-supplied
			// interval, else add the fixed 5s increment.
			if tok.Interval > 0 {
				interval = time.Duration(tok.Interval) * time.Second
			} else {
				interval += slowDownIncrement
			}
			continue
		case "expired_token":
			return "", ErrVerificationTimeout
		case "access_denied":
			return "", fmt.Errorf("identity: device authorization denied by user")
		default:
			return "", fmt.Errorf("identity: device flow error: %s", tok.Error)
		}
	}
}

// VerifyAccessToken re-verifies a CLI-obtained GitHub user access token
// server-side and returns the provider-qualified subject
// ("github:<login>"). It reuses resolveLogin — the same GET {api}/user
// exchange VerifyUser performs after the device flow authorizes — so a
// token minted through the CLI's own device flow (E39.3 / #1708)
// resolves to the identical subject the interactive path would. An
// empty token is rejected before any HTTP call.
func (p *GitHubIdentityProvider) VerifyAccessToken(ctx context.Context, accessToken string) (string, error) {
	if accessToken == "" {
		return "", fmt.Errorf("identity: access token is empty")
	}
	return p.resolveLogin(ctx, accessToken)
}

// requestDeviceCode performs POST {oauth}/login/device/code.
func (p *GitHubIdentityProvider) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	body, err := json.Marshal(map[string]string{
		"client_id": p.clientID,
		"scope":     deviceFlowScope,
	})
	if err != nil {
		return nil, fmt.Errorf("identity: marshal device code request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.oauthBase()+"/login/device/code", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("identity: build device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: request device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classify("device code", resp); err != nil {
		return nil, err
	}
	var out deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("identity: decode device code: %w", err)
	}
	return &out, nil
}

// pollAccessToken performs one POST {oauth}/login/oauth/access_token.
func (p *GitHubIdentityProvider) pollAccessToken(ctx context.Context, deviceCode string) (*accessTokenResponse, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":   p.clientID,
		"device_code": deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	})
	if err != nil {
		return nil, fmt.Errorf("identity: marshal access token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.oauthBase()+"/login/oauth/access_token", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("identity: build access token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: poll access token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classify("access token", resp); err != nil {
		return nil, err
	}
	var out accessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("identity: decode access token: %w", err)
	}
	return &out, nil
}

// resolveLogin performs GET {api}/user with the device-flow token and
// returns the provider-qualified subject.
func (p *GitHubIdentityProvider) resolveLogin(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase()+"/user", nil)
	if err != nil {
		return "", fmt.Errorf("identity: build user request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("identity: get user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classify("get user", resp); err != nil {
		return "", err
	}
	var out struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("identity: decode user: %w", err)
	}
	login := canonicalGitHubLogin(out.Login)
	if login == "" {
		return "", fmt.Errorf("identity: user response carried no login")
	}
	return subjectPrefix + login, nil
}

// canonicalGitHubLogin returns the canonical Fishhawk login for a raw GitHub
// login value. Enterprise Managed User (EMU) logins carry a
// "<username>_<shortcode>" enterprise short-code suffix (e.g. "alice_acme");
// Fishhawk keys identity on the FULL login (short code included), so the
// suffix is preserved verbatim — never stripped or split — and only
// surrounding whitespace is trimmed. A plain github.com login (no underscore)
// passes through unchanged. "<username>_<shortcode>" and a bare "<username>"
// are distinct accounts (different enterprises), so collapsing them would let
// one EMU user impersonate another's subject.
func canonicalGitHubLogin(login string) string {
	return strings.TrimSpace(login)
}

// PermissionLevel maps GitHub's collaborator role_name onto the
// forge-neutral vocabulary.
//
//	GET /repos/{owner}/{repo}/collaborators/{login}/permission
//
// 404 (no access) → PermissionNone. A rate-limit signal → ErrRateLimited.
func (p *GitHubIdentityProvider) PermissionLevel(ctx context.Context, repo, subject string) (Permission, error) {
	login := strings.TrimPrefix(subject, subjectPrefix)
	endpoint := p.apiBase() + "/repos/" + repo + "/collaborators/" + url.PathEscape(login) + "/permission"

	resp, err := p.get(ctx, endpoint)
	if err != nil {
		return PermissionNone, err
	}
	defer func() { _ = resp.Body.Close() }()

	if rlErr := rateLimitError(resp); rlErr != nil {
		return PermissionNone, rlErr
	}
	if resp.StatusCode == http.StatusNotFound {
		return PermissionNone, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PermissionNone, fmt.Errorf("identity: permission: %d: %s", resp.StatusCode, readBrief(resp.Body))
	}

	var out struct {
		RoleName string `json:"role_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PermissionNone, fmt.Errorf("identity: decode permission: %w", err)
	}
	return mapRoleName(out.RoleName), nil
}

// mapRoleName maps GitHub's role_name to the forge-neutral Permission.
// An unrecognized role is deny-by-default (PermissionNone).
func mapRoleName(role string) Permission {
	switch role {
	case "read":
		return PermissionRead
	case "triage":
		return PermissionTriage
	case "write":
		return PermissionWrite
	case "maintain":
		return PermissionMaintain
	case "admin":
		return PermissionAdmin
	default:
		return PermissionNone
	}
}

// ResolveMembership reports org or team membership. ref is "org" or
// "org/team".
//
//	org:  GET /orgs/{org}/members/{login}                     204 → true, 404 → false
//	team: GET /orgs/{org}/teams/{team}/memberships/{login}    200 active → true, 404 → false
func (p *GitHubIdentityProvider) ResolveMembership(ctx context.Context, ref, subject string) (bool, error) {
	login := strings.TrimPrefix(subject, subjectPrefix)
	parts := strings.Split(ref, "/")
	switch len(parts) {
	case 1:
		return p.orgMembership(ctx, parts[0], login)
	case 2:
		return p.teamMembership(ctx, parts[0], parts[1], login)
	default:
		return false, fmt.Errorf("identity: membership ref %q is not \"org\" or \"org/team\"", ref)
	}
}

func (p *GitHubIdentityProvider) orgMembership(ctx context.Context, org, login string) (bool, error) {
	endpoint := p.apiBase() + "/orgs/" + url.PathEscape(org) + "/members/" + url.PathEscape(login)
	resp, err := p.get(ctx, endpoint)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if rlErr := rateLimitError(resp); rlErr != nil {
		return false, rlErr
	}
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("identity: org membership: %d: %s", resp.StatusCode, readBrief(resp.Body))
	}
}

func (p *GitHubIdentityProvider) teamMembership(ctx context.Context, org, team, login string) (bool, error) {
	endpoint := p.apiBase() + "/orgs/" + url.PathEscape(org) +
		"/teams/" + url.PathEscape(team) + "/memberships/" + url.PathEscape(login)
	resp, err := p.get(ctx, endpoint)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if rlErr := rateLimitError(resp); rlErr != nil {
		return false, rlErr
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("identity: team membership: %d: %s", resp.StatusCode, readBrief(resp.Body))
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("identity: decode team membership: %w", err)
	}
	return out.State == "active", nil
}

// get issues an authenticated (or anonymous) GET against the REST API.
func (p *GitHubIdentityProvider) get(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if p.token != nil {
		tok, err := p.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("identity: resolve token: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: do request: %w", err)
	}
	return resp, nil
}

// rateLimitError detects GitHub's rate-limit signal and returns
// ErrRateLimited wrapping the reset hint. It returns nil when the
// response is not rate-limited.
//
// githubclient itself performs NO rate-limit handling — see
// backend/internal/githubclient/client.go (CreateIssueComment's
// "Caller is responsible for any rate-limit / dedup logic" contract),
// so this provider owns the detection net-new. A 403/429 carrying
// X-RateLimit-Remaining: 0 or a Retry-After header is the primary /
// secondary rate-limit signature.
func rateLimitError(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	retryAfter := resp.Header.Get("Retry-After")
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if retryAfter == "" && remaining != "0" {
		return nil
	}
	reset := resp.Header.Get("X-RateLimit-Reset")
	return fmt.Errorf("%w: retry-after=%q reset=%q", ErrRateLimited, retryAfter, reset)
}

// classify turns a non-2xx device/token/user response into an error,
// promoting the rate-limit signal to ErrRateLimited.
func classify(op string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if rlErr := rateLimitError(resp); rlErr != nil {
		return rlErr
	}
	return fmt.Errorf("identity: %s: %d: %s", op, resp.StatusCode, readBrief(resp.Body))
}

func (p *GitHubIdentityProvider) apiBase() string {
	if p.apiBaseURL == "" {
		return DefaultAPIBaseURL
	}
	return p.apiBaseURL
}

func (p *GitHubIdentityProvider) oauthBase() string {
	if p.oauthBaseURL == "" {
		return DefaultOAuthBaseURL
	}
	return p.oauthBaseURL
}

func (p *GitHubIdentityProvider) client() *http.Client {
	if p.httpClient == nil {
		return http.DefaultClient
	}
	return p.httpClient
}

// readBrief returns up to 256 bytes of a response body for error
// context (mirrors githubclient.readBriefBody).
func readBrief(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(b))
}
