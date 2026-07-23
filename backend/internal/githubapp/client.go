package githubapp

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

	// ResolveBaseURL, when non-nil, resolves the per-installation API base
	// URL for an installation (Mode 2, data-resident installs on
	// <slug>.ghe.com). IssueInstallationToken consults it per mint, passing
	// the stringified installation id (the forge-neutral installation_ref the
	// account resolver keys on — the int64 stays inside this GitHub-specific
	// package):
	//
	//   - a non-empty return OVERRIDES BaseURL/DefaultGitHubAPIBase — the
	//     request targets the per-installation host. It is validated as a
	//     well-formed absolute https URL before it becomes the target host
	//     (see validateResolvedBaseURL): the mint ships a live App JWT, so an
	//     override that is not https-with-a-host FAILS the mint rather than
	//     transmitting the credential to an unvalidated (or non-TLS) host.
	//   - an empty return falls back to BaseURL then DefaultGitHubAPIBase:
	//     the intentional absence of an override (NULL column / unknown
	//     installation) keeps the deployment default.
	//   - a NON-NIL error FAILS the mint (surfaced to the caller). A real
	//     endpoint-resolution fault must never silently target the default
	//     host for a data-resident install — failing closed is the only safe
	//     posture (E44.2 / #1826).
	//
	// Nil (the default) preserves the pre-#1826 behavior: BaseURL, else the
	// GitHub default.
	ResolveBaseURL func(ctx context.Context, installationRef string) (string, error)

	// AllowedInstallationHosts, when non-empty, restricts the resolved
	// per-installation base URL (see ResolveBaseURL) to an operator-configured
	// allowlist of hosts, enforced at mint time BEFORE the App JWT ships
	// (E44.15 / #2093). Each entry is either an exact host ("acme.ghe.com") or a
	// leading-dot suffix (".ghe.com") that matches any subdomain at a TRUE label
	// boundary (".ghe.com" admits "acme.ghe.com" but NOT the look-alike
	// "notghe.com", and NOT the bare apex "ghe.com" unless it is also listed).
	// Matching is case-insensitive and port-insensitive; see matchesHostAllowlist.
	//
	// Empty/nil (the default) PRESERVES the pre-#2093 posture: the resolved
	// override is validated for scheme/parse/host only (validateResolvedBaseURL),
	// not pinned to an allowlist. That is safe TODAY because the sole writer of
	// installations.forge_base_url is the trusted operator-side UpsertInstallation
	// path (the same trust boundary as any config column), so no live
	// credential-exfiltration path exists (the #2093 operator arbitration).
	// DEFERRAL TRIGGER: a future production / tenant-facing writer of
	// forge_base_url MUST configure this allowlist (fail-closed), closing the
	// residual well-formed-but-attacker-/typo-controlled-HTTPS-host risk before a
	// live App JWT could reach a non-forge endpoint.
	AllowedInstallationHosts []string
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
	// Mode 2 (E44.2 / #1826): a per-installation resolver overrides the
	// deployment default for data-resident installs. A real resolution error
	// FAILS the mint — never a silent fallback to the default host.
	if c.ResolveBaseURL != nil {
		resolved, err := c.ResolveBaseURL(ctx, formatInt64(installationID))
		if err != nil {
			return nil, fmt.Errorf("githubapp: resolve installation base url: %w", err)
		}
		if resolved != "" {
			// Validate BEFORE the JWT ships: an override host that is not a
			// well-formed https URL fails the mint (never a live App JWT to an
			// unvalidated or non-TLS host).
			if err := validateResolvedBaseURL(resolved); err != nil {
				return nil, err
			}
			// Optional host allowlist (E44.15 / #2093): when configured, the
			// resolved per-installation host must be an allowlisted entry BEFORE
			// the App JWT ships. Ordering is load-bearing — scheme/parse
			// validation first, then allowlist, all strictly before the
			// credential is transmitted. Empty allowlist = the default posture
			// (scheme/parse only), safe today because forge_base_url's sole writer
			// is the trusted operator-side UpsertInstallation path (#2093
			// arbitration); a future tenant-facing writer MUST configure it.
			if len(c.AllowedInstallationHosts) > 0 && !hostAllowed(resolved, c.AllowedInstallationHosts) {
				return nil, fmt.Errorf("githubapp: installation base url host not in configured allowlist: %q", resolved)
			}
			base = resolved
		}
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

// validateResolvedBaseURL rejects a per-installation override host that is not
// a well-formed absolute https URL. IssueInstallationToken ships a live App JWT
// in the Authorization header, so an override resolved from the installations
// row must pass a scheme/parse sanity check before it can become the target
// host: an http:// value would transmit the JWT without TLS, and a malformed or
// hostless value could send it somewhere unintended (E44.2 / #1826). This is a
// scheme/parse check, NOT a host allowlist — per-installation host pinning is
// deferred to the follow-up that introduces a production (tenant / forge-sync)
// writer for forge_base_url; today the sole writer is the operator-side
// UpsertInstallation path.
func validateResolvedBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("githubapp: malformed installation base url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("githubapp: installation base url %q must use https, got scheme %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("githubapp: installation base url %q missing host", raw)
	}
	return nil
}

// hostAllowed reports whether resolved's host is permitted by allowlist.
// resolved is expected already validated by validateResolvedBaseURL (an
// absolute https URL with a host), so url.Parse cannot fail on the mint path; a
// parse failure is nonetheless treated as NOT allowed (fail closed) rather than
// trusted. The host is lower-cased and port-stripped before matching.
func hostAllowed(resolved string, allowlist []string) bool {
	u, err := url.Parse(resolved)
	if err != nil {
		return false
	}
	return matchesHostAllowlist(strings.ToLower(u.Hostname()), allowlist)
}

// matchesHostAllowlist reports whether host matches any entry in allowlist.
// host is expected already lower-cased and port-stripped; allowlist entries are
// expected already lower-cased by the configuration parser. An entry is either:
//
//   - an exact host ("acme.ghe.com") — matched by equality; or
//   - a leading-dot suffix (".ghe.com") — matched when host ENDS WITH the
//     dotted suffix, so the dot is a TRUE label boundary. This admits any
//     subdomain ("acme.ghe.com") while rejecting the look-alike "notghe.com"
//     (whose final labels are "…tghe.com", not ".ghe.com") and the bare apex
//     "ghe.com" (unless "ghe.com" is also listed explicitly). Using the
//     dot-prefixed suffix — NOT a raw strings.HasSuffix(host, "ghe.com") — is
//     what pins label-boundary matching over substring matching (#2093,
//     binding condition 2).
func matchesHostAllowlist(host string, allowlist []string) bool {
	for _, entry := range allowlist {
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// readBriefBody reads up to 256 bytes for use in error messages.
// Caller closes the body.
func readBriefBody(r io.Reader) string {
	limited := io.LimitReader(r, 256)
	b, _ := io.ReadAll(limited)
	return string(b)
}
