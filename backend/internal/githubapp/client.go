package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultGitHubAPIBase is GitHub's REST API root. Tests override
// this via Client.BaseURL to point at httptest fakes.
const DefaultGitHubAPIBase = "https://api.github.com"

// InstallationToken is the response shape from
// POST /app/installations/{id}/access_tokens. Per GitHub docs,
// `expires_at` is RFC 3339 with timezone Z.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Errors callers may want to switch on.
var (
	ErrInstallationNotFound = errors.New("githubapp: installation not found")
	ErrUnauthorized         = errors.New("githubapp: app JWT rejected")
)

// Client exchanges App JWTs for installation tokens via GitHub's
// REST API. Construct via NewClient. Concurrent use is safe.
type Client struct {
	BaseURL string // empty → DefaultGitHubAPIBase
	Signer  *Signer
	HTTP    *http.Client
}

// NewClient returns a Client with a 30s default timeout. signer
// is required.
func NewClient(signer *Signer) *Client {
	return &Client{
		Signer: signer,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// IssueInstallationToken exchanges the App JWT for a per-
// installation access token. Returns ErrInstallationNotFound on 404
// (installation removed by the customer) and ErrUnauthorized on
// 401 (clock skew, key rotation, App suspended).
func (c *Client) IssueInstallationToken(ctx context.Context, installationID int64) (*InstallationToken, error) {
	if c.Signer == nil {
		return nil, errors.New("githubapp: client missing Signer")
	}
	jwt, err := c.Signer.Sign(0)
	if err != nil {
		return nil, fmt.Errorf("githubapp: sign jwt: %w", err)
	}

	base := c.BaseURL
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", base, formatInt64(installationID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("githubapp: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		// fall through
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		return nil, ErrInstallationNotFound
	default:
		body := readBriefBody(resp.Body)
		return nil, fmt.Errorf("githubapp: %d: %s", resp.StatusCode, body)
	}

	var out InstallationToken
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("githubapp: decode response: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return nil, errors.New("githubapp: response missing required fields")
	}
	return &out, nil
}

// readBriefBody reads up to 256 bytes for use in error messages.
// Caller closes the body.
func readBriefBody(r io.Reader) string {
	limited := io.LimitReader(r, 256)
	b, _ := io.ReadAll(limited)
	return string(b)
}
