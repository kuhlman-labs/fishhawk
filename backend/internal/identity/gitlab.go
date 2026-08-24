package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitLab identity provider (E66.4 / #2392) — the co-equal sibling of
// GitHubIdentityProvider, implementing the same ADR-055 IdentityProvider
// seam against a configurable GitLab base URL (SaaS or self-managed):
//
//   - VerifyUser drives the RFC 8628 device flow against
//     POST {base}/oauth/authorize_device + POST {base}/oauth/token.
//   - VerifyAccessToken re-verifies a CLI-obtained access token
//     server-side via GET {base}/api/v4/user.
//   - PermissionLevel / ResolveMembership read the project and group
//     members APIs with a BOUNDED PAGINATED EXACT-MATCH walk.
//
// The GitLab specifics (the device grant type, the access_level integer
// ladder, the members pagination contract) are confined to this file.

// DefaultGitLabBaseURL is GitLab SaaS. A self-managed deployment
// overrides it through the constructor (threaded from
// FISHHAWKD_GITLAB_BASE_URL); the device-flow and REST endpoints both
// hang off this ONE host, unlike GitHub where the OAuth host and the
// API host differ.
const DefaultGitLabBaseURL = "https://gitlab.com"

// subjectPrefixGitLab qualifies every subject this provider emits or
// accepts. A subject is "gitlab:<username>".
const subjectPrefixGitLab = "gitlab:"

// gitLabDeviceFlowScope is the OAuth scope the device flow requests.
// read_api — not read_user — for the same reason the browser sign-in leg
// documents at backend/internal/auth/gitlab_oauth.go: read_user does not
// authorize the /api/v4 reads this provider makes, and a token that
// cannot make them is useless for the CLI's subsequent API calls.
const gitLabDeviceFlowScope = "read_api"

// gitLabDeviceGrantType is the RFC 8628 device authorization grant.
const gitLabDeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Members-walk bounds. These mirror backend/internal/auth/gitlab_membership.go
// verbatim in shape and value — that lister is this repo's shipped, tested
// handling of the same GitLab pagination contract, so the two stay in step.
const (
	// gitLabMembersPerPage is the page size (GitLab's documented maximum).
	gitLabMembersPerPage = 100

	// gitLabMaxMemberPages bounds the walk so a pathological project (or a
	// server that never stops advertising a next page) cannot loop
	// unbounded. 50 pages x 100 = 5000 members walked.
	gitLabMaxMemberPages = 50

	// gitLabMaxMemberPageBytes caps how much of ONE page body is read. The
	// page cap bounds the number of requests but not the size of any single
	// response, so an oversized or endlessly-streaming body could otherwise
	// consume memory during an authorization read.
	gitLabMaxMemberPageBytes = 4 << 20
)

// GitLab access_level integers (https://docs.gitlab.com/ee/api/members/#roles).
// Any value NOT listed here — including newly-introduced levels such as
// Planner or Minimal Access — maps to PermissionNone, deny-by-default,
// matching permissionRank's documented posture that an unrecognized tier
// ranks zero.
const (
	gitLabAccessGuest      = 10
	gitLabAccessReporter   = 20
	gitLabAccessDeveloper  = 30
	gitLabAccessMaintainer = 40
	gitLabAccessOwner      = 50
)

// GitLabIdentityProvider is the hand-rolled GitLab REST implementation of
// IdentityProvider.
//
// Concurrent use is safe: the struct holds only immutable config. In
// particular the token accessor supplied in production is a closure over
// an immutable captured string (the FISHHAWKD_GITLAB_TOKEN deployment
// credential), so it performs no round-trip and holds no mutable state —
// deliberately unlike GitHub's operatorRepoToken, which mints a
// short-lived installation token per call.
//
// CREDENTIAL SEPARATION (E66.4 binding condition 1). The deployment
// credential is confined to PermissionLevel and ResolveMembership — the
// reads that ask "what may this OTHER person do". VerifyAccessToken, whose
// only job is to verify the caller's OWN token, sends ONLY that submitted
// token and never the deployment credential: putting two credentials on
// one verification request is an authentication-bypass shape, because if
// the forge honours the deployment credential then an invalid or revoked
// submitted token resolves as the deployment-token user and the mint
// issues a fishhawkd token for an identity the caller never proved.
type GitLabIdentityProvider struct {
	// baseURL is the GitLab instance root (no trailing slash). Both the
	// OAuth device endpoints and the /api/v4 REST endpoints hang off it.
	baseURL string

	// deviceClientID is the client_id of the NON-Confidential GitLab
	// application registered for the device flow. It is used ONLY by the
	// device-flow POSTs. This provider never holds or sends a
	// client_secret: RFC 8628 §3.4 specifies the device access-token
	// request as client_id-only for a public client, and a GitLab
	// application marked Confidential (which the browser sign-in leg
	// requires, because ExchangeCode sends a client_secret) cannot serve
	// it. Hence the separate FISHHAWKD_GITLAB_DEVICE_CLIENT_ID with no
	// fallback to the browser leg's client id.
	deviceClientID string

	// token, when non-nil, returns the DEPLOYMENT credential for the
	// members reads (PermissionLevel / ResolveMembership) — never for
	// VerifyAccessToken. Nil → those reads go out anonymously, a real and
	// stated degrade: GitLab answers an unauthenticated caller 404 on a
	// private project's members endpoint, which maps to PermissionNone,
	// which the token-mint gate denies. Fail-closed, but non-functional
	// for the common private-repo case, so the fishhawkd wiring WARNs at
	// boot when the credential is missing.
	token func(context.Context) (string, error)

	httpClient *http.Client

	// Test seams (in-package, mirroring GitHubIdentityProvider):
	// pollInterval overrides the forge's device-flow interval; sleep is
	// the interval wait (ctx-aware); now is the clock for the expiry
	// deadline. All default to their production behavior in
	// NewGitLabIdentityProvider.
	pollInterval time.Duration
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
}

// Compile-time assertion that GitLabIdentityProvider satisfies the
// interface.
var _ IdentityProvider = (*GitLabIdentityProvider)(nil)

// NewGitLabIdentityProvider constructs a GitLab identity provider from the
// instance base URL, the NON-Confidential device application's client_id,
// and an optional deployment-credential accessor (nil → anonymous members
// reads). It returns the interface, following NewGitHubIdentityProvider's
// idiom; the production defaults (a 30s HTTP client, a ctx-aware sleep,
// time.Now) are filled in here.
//
// An empty baseURL falls back to GitLab SaaS. Trailing slashes are
// trimmed so endpoint concatenation never produces a double slash.
//
// No functional-option variadic is offered: in-package tests construct
// the struct directly (as github_test.go's newTestProvider does), so an
// exported Option type with no implementations would be dead routing.
func NewGitLabIdentityProvider(baseURL, deviceClientID string, token func(context.Context) (string, error)) IdentityProvider {
	return &GitLabIdentityProvider{
		baseURL:        normalizeGitLabBaseURL(baseURL),
		deviceClientID: deviceClientID,
		token:          token,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		sleep:          sleepCtx,
		now:            time.Now,
	}
}

// normalizeGitLabBaseURL trims whitespace and trailing slashes, falling
// back to GitLab SaaS for an empty value.
func normalizeGitLabBaseURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return DefaultGitLabBaseURL
	}
	return base
}

// gitLabDeviceCodeResponse is the subset of POST /oauth/authorize_device
// we read.
type gitLabDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// gitLabTokenResponse is the subset of POST /oauth/token we read. Error
// carries the RFC 8628 poll state ("authorization_pending", "slow_down",
// "expired_token", "access_denied") when AccessToken is empty.
type gitLabTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Interval    int    `json:"interval"`
}

// VerifyUser drives the RFC 8628 device flow to completion and returns the
// provider-qualified subject ("gitlab:<username>").
func (p *GitLabIdentityProvider) VerifyUser(ctx context.Context, prompt DeviceCodePrompt) (string, error) {
	device, err := p.requestDeviceCode(ctx)
	if err != nil {
		return "", err
	}
	if prompt != nil {
		prompt(device.UserCode, device.VerificationURI)
	}

	// Poll interval: the forge's suggested interval, floored so a
	// missing/zero interval never collapses to a busy-poll on the token
	// endpoint (the #1752-class defect the GitHub provider already
	// guards), unless a test overrides it.
	interval := time.Duration(device.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	if p.pollInterval > 0 {
		interval = p.pollInterval
	}

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

		tok, err := p.pollDeviceToken(ctx, device.DeviceCode)
		if err != nil {
			return "", err
		}
		switch tok.Error {
		case "":
			// Authorized — resolve the subject through the SAME
			// re-verification path VerifyAccessToken uses, so both entry
			// points share one subject derivation.
			return p.VerifyAccessToken(ctx, tok.AccessToken)
		case "authorization_pending":
			continue
		case "slow_down":
			// Honor the mandated back-off: prefer the forge-supplied
			// interval, else add the fixed increment.
			if tok.Interval > 0 {
				interval = time.Duration(tok.Interval) * time.Second
			} else {
				interval += slowDownIncrement
			}
			continue
		case "expired_token":
			return "", ErrVerificationTimeout
		case "access_denied":
			return "", fmt.Errorf("identity: gitlab device authorization denied by user")
		default:
			return "", fmt.Errorf("identity: gitlab device flow error: %s", tok.Error)
		}
	}
}

// requestDeviceCode performs POST {base}/oauth/authorize_device. The body
// is form-encoded (GitLab's OAuth endpoints, unlike GitHub's, take
// application/x-www-form-urlencoded) and carries client_id + scope and NO
// client_secret.
func (p *GitLabIdentityProvider) requestDeviceCode(ctx context.Context) (*gitLabDeviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", p.deviceClientID)
	form.Set("scope", gitLabDeviceFlowScope)

	resp, err := p.postForm(ctx, p.baseURL+"/oauth/authorize_device", form)
	if err != nil {
		return nil, fmt.Errorf("identity: request gitlab device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := classify("gitlab device code", resp); err != nil {
		return nil, err
	}
	var out gitLabDeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("identity: decode gitlab device code: %w", err)
	}
	if out.DeviceCode == "" {
		return nil, fmt.Errorf("identity: gitlab device code response carried no device_code")
	}
	return &out, nil
}

// pollDeviceToken performs one POST {base}/oauth/token with the device
// grant.
//
// Status handling differs from the GitHub provider on purpose: RFC 8628
// §3.5 specifies the pending/slow_down states as OAuth 2.0 error
// responses, and GitLab (Doorkeeper) serves them with HTTP 400. So a
// non-2xx response is decoded and dispatched on its `error` field rather
// than being treated as terminal; only a non-2xx carrying no recognizable
// error body is a hard failure.
func (p *GitLabIdentityProvider) pollDeviceToken(ctx context.Context, deviceCode string) (*gitLabTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", p.deviceClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", gitLabDeviceGrantType)

	resp, err := p.postForm(ctx, p.baseURL+"/oauth/token", form)
	if err != nil {
		return nil, fmt.Errorf("identity: poll gitlab device token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if rlErr := gitLabRateLimitError(resp); rlErr != nil {
		return nil, rlErr
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, gitLabMaxMemberPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("identity: read gitlab device token: %w", err)
	}
	if len(raw) > gitLabMaxMemberPageBytes {
		return nil, fmt.Errorf("identity: gitlab device token response exceeded %d bytes", gitLabMaxMemberPageBytes)
	}

	var out gitLabTokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		// An undecodable body is only reportable as an error; on a non-2xx
		// it is the more useful of the two facts, so report the status.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("identity: gitlab device token: %d: %s",
				resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 256)])))
		}
		return nil, fmt.Errorf("identity: decode gitlab device token: %w", err)
	}
	if out.Error == "" && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return nil, fmt.Errorf("identity: gitlab device token: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 256)])))
	}
	if out.Error == "" && out.AccessToken == "" {
		return nil, fmt.Errorf("identity: gitlab device token response carried neither access_token nor error")
	}
	return &out, nil
}

// VerifyAccessToken verifies a GitLab access token server-side and returns
// the provider-qualified subject ("gitlab:<username>").
//
// BINDING CONDITION 1 (E66.4). This request carries EXACTLY ONE
// credential: the submitted token, as Authorization: Bearer. The
// deployment credential is deliberately NOT attached — not as a second
// header, not as a fallback on any status. The whole job of this call is
// to prove the caller holds a valid GitLab token; a request that would
// also succeed on the deployment's own credential proves nothing, and a
// forge that honoured the deployment credential would resolve an invalid
// or revoked submitted token to the deployment-token user, minting a
// fishhawkd token for an identity the caller never proved. Pinned by
// TestGitLabVerifyAccessToken_DeploymentCredentialNotSent, whose
// discriminating case answers 401 to the submitted token and 200 to the
// deployment credential and requires the call to FAIL.
func (p *GitLabIdentityProvider) VerifyAccessToken(ctx context.Context, accessToken string) (string, error) {
	if accessToken == "" {
		return "", fmt.Errorf("identity: access token is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/v4/user", nil)
	if err != nil {
		return "", fmt.Errorf("identity: build gitlab user request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("identity: get gitlab user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if rlErr := gitLabRateLimitError(resp); rlErr != nil {
		return "", rlErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("identity: gitlab get user: %d: %s", resp.StatusCode, readBrief(resp.Body))
	}

	var out struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("identity: decode gitlab user: %w", err)
	}
	username := strings.TrimSpace(out.Username)
	if out.ID == 0 || username == "" {
		return "", fmt.Errorf("identity: gitlab user response carried no id/username")
	}
	return subjectPrefixGitLab + username, nil
}

// PermissionLevel maps GitLab's project member access_level onto the
// forge-neutral vocabulary.
//
//	GET {base}/api/v4/projects/{id}/members/all?query=…&per_page=100&page=N
//
// /members/all — not /members — is deliberate: it includes members
// INHERITED from an ancestor group, and a user granted Developer on the
// parent group but never added directly to the project is a genuine
// member who must not be denied.
//
// The walk is complete, not first-page: see findMemberExact.
func (p *GitLabIdentityProvider) PermissionLevel(ctx context.Context, repo, subject string) (Permission, error) {
	login, ok := gitLabLoginFromSubject(subject)
	if !ok {
		// A subject qualified for a DIFFERENT provider (or unqualified)
		// is not resolvable against GitLab. Deny with ZERO network calls:
		// a github: login and a gitlab: login of the same spelling are
		// different humans, so asking GitLab about one would answer the
		// wrong authorization question.
		return PermissionNone, nil
	}
	endpoint := p.baseURL + "/api/v4/projects/" + url.PathEscape(repo) + "/members/all"

	level, found, err := p.findMemberExact(ctx, endpoint, login)
	if err != nil {
		return PermissionNone, err
	}
	if !found {
		return PermissionNone, nil
	}
	return mapGitLabAccessLevel(level), nil
}

// ResolveMembership reports whether subject belongs to the GitLab group
// named by ref. ref is a group full_path ("group" or "group/subgroup") —
// the same forge-neutral "org" / "org/team" ref the GitHub provider takes,
// read as GitLab's group hierarchy.
//
//	GET {base}/api/v4/groups/{id}/members/all?query=…&per_page=100&page=N
func (p *GitLabIdentityProvider) ResolveMembership(ctx context.Context, ref, subject string) (bool, error) {
	login, ok := gitLabLoginFromSubject(subject)
	if !ok {
		return false, nil
	}
	endpoint := p.baseURL + "/api/v4/groups/" + url.PathEscape(ref) + "/members/all"

	level, found, err := p.findMemberExact(ctx, endpoint, login)
	if err != nil {
		return false, err
	}
	// A member row with a non-positive access level is not a membership.
	return found && level > 0, nil
}

// gitLabLoginFromSubject strips the "gitlab:" qualification, reporting
// ok=false for a subject this provider must not resolve: one qualified for
// another provider, an unqualified bare login (not provably a GitLab
// account), or an empty login.
func gitLabLoginFromSubject(subject string) (string, bool) {
	if !strings.HasPrefix(subject, subjectPrefixGitLab) {
		return "", false
	}
	login := strings.TrimSpace(strings.TrimPrefix(subject, subjectPrefixGitLab))
	if login == "" {
		return "", false
	}
	return login, true
}

// mapGitLabAccessLevel maps a GitLab access_level integer to the
// forge-neutral Permission. An unrecognized level is deny-by-default
// (PermissionNone) — never a guessed tier.
func mapGitLabAccessLevel(level int) Permission {
	switch level {
	case gitLabAccessGuest:
		return PermissionRead
	case gitLabAccessReporter:
		return PermissionTriage
	case gitLabAccessDeveloper:
		return PermissionWrite
	case gitLabAccessMaintainer:
		return PermissionMaintain
	case gitLabAccessOwner:
		return PermissionAdmin
	default:
		return PermissionNone
	}
}

// gitLabMember is the subset of a members-endpoint row we read.
type gitLabMember struct {
	Username    string `json:"username"`
	AccessLevel int    `json:"access_level"`
}

// findMemberExact walks a GitLab members endpoint page by page until an
// EXACT username match is found or the pages are exhausted. It is the
// single implementation both PermissionLevel and ResolveMembership use, so
// the pagination rule, the byte cap, the page bound, the Link handling and
// the exact-match comparison exist in exactly ONE place and any fix reaches
// both (E66.4 binding constraint 9).
//
// Why a walk and not a filtered single page: GitLab's `query=` parameter is
// a PARTIAL (substring/prefix) filter, not an exact one, and the members
// endpoints are paginated. So a first-page-only read can silently deny a
// real member whose exact row sits behind a hundred near-miss matches —
// an invisible wrong-authorization answer. `query=` is still sent, but
// purely as a server-side NARROWING optimization: the exact comparison
// happens client-side on every page, so a GitLab that widens or ignores
// `query` changes only how many pages are walked, never the verdict.
//
// Outcomes:
//
//	(level, true, nil)  exact match found
//	(0, false, nil)     walked to exhaustion with no match, OR 404 (the
//	                    resource is not visible to this caller — including
//	                    the anonymous-read degrade on a private project)
//	(0, false, err)     transport/decode failure, an oversized page, a
//	                    rate-limit signal (ErrRateLimited), or the page
//	                    bound exceeded
//
// The page bound returns an ERROR rather than a silent not-found on
// purpose: a truncated walk that denies is exactly the invisible
// wrong-authorization answer this walk exists to prevent, so it must be
// loud (the mint handler surfaces it as a 500) rather than look like a
// clean "not a member".
func (p *GitLabIdentityProvider) findMemberExact(ctx context.Context, membersEndpoint, login string) (int, bool, error) {
	for page := 1; ; page++ {
		if page > gitLabMaxMemberPages {
			return 0, false, fmt.Errorf("identity: gitlab members walk exceeded %d pages", gitLabMaxMemberPages)
		}
		members, more, visible, err := p.memberPage(ctx, membersEndpoint, login, page)
		if err != nil {
			return 0, false, err
		}
		if !visible {
			return 0, false, nil
		}
		for _, m := range members {
			if m.Username == login {
				return m.AccessLevel, true, nil
			}
		}
		if !more {
			return 0, false, nil
		}
	}
}

// memberPage fetches one members page and reports whether another follows.
// visible is false when the endpoint answered 404 — GitLab returns 404
// rather than 403 for a resource the caller cannot see, so this is both
// "no such project/group" and the anonymous-read degrade.
//
// "Another follows" is decided by the RFC 5988 Link rel="next" header when
// the server sends one, falling back to a full page of results (GitLab
// omits Link headers on some deployments and behind some proxies) — the
// same rule backend/internal/auth/gitlab_membership.go already ships.
func (p *GitLabIdentityProvider) memberPage(ctx context.Context, membersEndpoint, login string, page int) ([]gitLabMember, bool, bool, error) {
	q := url.Values{}
	q.Set("query", login)
	q.Set("per_page", strconv.Itoa(gitLabMembersPerPage))
	q.Set("page", strconv.Itoa(page))

	resp, err := p.get(ctx, membersEndpoint+"?"+q.Encode())
	if err != nil {
		return nil, false, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if rlErr := gitLabRateLimitError(resp); rlErr != nil {
		return nil, false, false, rlErr
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, false, fmt.Errorf("identity: gitlab members: %d: %s", resp.StatusCode, readBrief(resp.Body))
	}

	// Read at most gitLabMaxMemberPageBytes (+1 byte, purely to distinguish
	// "exactly at the cap" from "over it"). The forge response is
	// semi-trusted input on an authorization path, so an oversized body is
	// REJECTED rather than truncated-and-parsed: a truncated page is a
	// partial member set, and answering an authorization question from one
	// is the same silent wrong answer the page walk exists to prevent.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, gitLabMaxMemberPageBytes+1))
	if err != nil {
		return nil, false, false, fmt.Errorf("identity: read gitlab members: %w", err)
	}
	if len(raw) > gitLabMaxMemberPageBytes {
		return nil, false, false, fmt.Errorf("identity: gitlab members response exceeded %d bytes", gitLabMaxMemberPageBytes)
	}

	var members []gitLabMember
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, false, false, fmt.Errorf("identity: decode gitlab members: %w", err)
	}
	if link := resp.Header.Get("Link"); link != "" {
		return members, gitLabHasNextLink(link), true, nil
	}
	return members, len(members) == gitLabMembersPerPage, true, nil
}

// gitLabHasNextLink reports whether an RFC 5988 Link header advertises a
// rel="next" relation. Mirrors auth.hasNextLink; duplicated rather than
// shared because that helper is unexported in a package this one must not
// depend on (identity carries no forge-client dependency by design).
func gitLabHasNextLink(header string) bool {
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

// get issues a members read against the GitLab REST API, attaching the
// DEPLOYMENT credential as PRIVATE-TOKEN when an accessor is configured.
// A nil accessor sends no auth header (the stated anonymous-read degrade).
//
// An accessor that ERRORS propagates rather than falling back to
// anonymous: a broken deployment credential must be loud, not a silent
// permission downgrade to whatever the anonymous view happens to show.
//
// Only PermissionLevel and ResolveMembership reach this method.
// VerifyAccessToken deliberately does not (binding condition 1).
func (p *GitLabIdentityProvider) get(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("identity: build gitlab request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if p.token != nil {
		tok, err := p.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("identity: resolve gitlab token: %w", err)
		}
		if tok != "" {
			req.Header.Set("PRIVATE-TOKEN", tok)
		}
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity: do gitlab request: %w", err)
	}
	return resp, nil
}

// postForm issues a form-encoded POST (the GitLab OAuth endpoints take
// application/x-www-form-urlencoded, matching auth.GitLabOAuth).
func (p *GitLabIdentityProvider) postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return p.client().Do(req)
}

// gitLabRateLimitError detects GitLab's rate-limit signal and returns
// ErrRateLimited, or nil when the response is not rate-limited.
//
// GitLab's headers are the un-prefixed RateLimit-* family (not GitHub's
// X-RateLimit-*), and a 429 from GitLab is unambiguously the rate limiter,
// so it is honoured with or without headers. A 403 is ambiguous —
// it is also plain "forbidden" — so it counts only when it carries
// Retry-After or RateLimit-Remaining: 0.
func gitLabRateLimitError(resp *http.Response) error {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return nil
	}
	retryAfter := resp.Header.Get("Retry-After")
	remaining := resp.Header.Get("RateLimit-Remaining")
	if resp.StatusCode == http.StatusForbidden && retryAfter == "" && remaining != "0" {
		return nil
	}
	reset := resp.Header.Get("RateLimit-Reset")
	return fmt.Errorf("%w: retry-after=%q remaining=%q reset=%q", ErrRateLimited, retryAfter, remaining, reset)
}

func (p *GitLabIdentityProvider) client() *http.Client {
	if p.httpClient == nil {
		return http.DefaultClient
	}
	return p.httpClient
}
