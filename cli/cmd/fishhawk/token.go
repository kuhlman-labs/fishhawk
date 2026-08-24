package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// runToken dispatches `fishhawk token <subcommand>` (E39.3 / #1708).
// The command group drives the user-bound OAuth login: `token login`
// runs the selected forge's OAuth device flow — GitHub or, since
// E66.4 / #2392, GitLab — mints a user-bound Fishhawk token via the
// backend, and stores it in the local credential store; `token list`
// shows the stored credentials.
func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk token: subcommand required (login|list)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "login":
		return tokenLogin(rest, stdout, stderr)
	case "list":
		return tokenList(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk token: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// --- device-flow / backend wire shapes ----------------------------

// deviceCodeResponse is the subset of POST {oauth}/login/device/code
// the CLI reads. Mirrors backend/internal/identity/github.go so both
// halves of the flow speak the same GitHub contract.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`

	// Error/ErrorDescription carry GitHub's OAuth error body when the
	// device-code endpoint answers 200 with no device_code (observed
	// live for device_flow_disabled, #1752) — mirrors
	// accessTokenResponse's Error field for the same failure shape.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// accessTokenResponse is the subset of POST
// {oauth}/login/oauth/access_token the CLI reads. Error carries the
// device-flow poll state when AccessToken is empty.
type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Interval    int    `json:"interval"`
}

// tokenLoginDiscovery is the GET /v0/tokens/login response — the
// backend advertises the configured OAuth client_id so the operator
// does not have to know it out of band.
//
// Providers is the forge-agnostic surface added by E66.4 / #2392: one
// entry per configured federated identity provider. The top-level
// Provider/ClientID are RETAINED and mirror the first entry, so this
// CLI still works against a backend predating the array (the legacy
// fallback in resolveProviderEntry).
//
// These structs are declared locally rather than imported from the
// backend on purpose: cli/go.mod carries no dependency on the backend
// module (ADR-014 keeps the two independently taggable), so the WIRE
// contract in docs/api/v0.openapi.yaml — not a shared Go symbol — is
// what binds the two halves.
type tokenLoginDiscovery struct {
	Provider  string                    `json:"provider"`
	ClientID  string                    `json:"client_id"`
	Providers []tokenLoginProviderEntry `json:"providers"`
}

// tokenLoginProviderEntry is one advertised provider. ClientID is the
// NON-Confidential DEVICE application's client_id — the id the device
// flow is actually driven with. BaseURL is the instance root the CLI
// appends /oauth/authorize_device and /oauth/token to; it is omitted
// for github (whose device endpoints are github.com) and required for
// gitlab, which can be SaaS or any self-managed host.
type tokenLoginProviderEntry struct {
	Provider string `json:"provider"`
	ClientID string `json:"client_id"`
	BaseURL  string `json:"base_url,omitempty"`
}

// tokenLoginRequest is the POST /v0/tokens/login body: the CLI hands
// the backend the device-flow access token, which the backend
// re-verifies server-side before minting.
type tokenLoginRequest struct {
	Provider    string   `json:"provider"`
	AccessToken string   `json:"access_token"`
	Scopes      []string `json:"scopes,omitempty"`
}

// tokenLoginResponse is the POST /v0/tokens/login response — the
// minted Fishhawk bearer token plus its recorded identity. ExpiresAt
// is nil in v0 (no token TTL yet).
type tokenLoginResponse struct {
	Token      string     `json:"token"`
	Subject    string     `json:"subject"`
	Scopes     []string   `json:"scopes"`
	AuthMethod string     `json:"auth_method"`
	Provider   string     `json:"provider"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// Test seams. Production points the device flow at github.com and
// waits the forge-supplied interval floored at 5s; tests override the
// base URL to an httptest server and shrink the interval so the poll
// loop runs in milliseconds.
var (
	githubDeviceBaseURL = "https://github.com"
	deviceFlowInterval  time.Duration // >0 overrides the forge interval (tests)
	deviceFlowSleep     = sleepCtx
)

const (
	deviceFlowScope   = "read:user"
	deviceFlowMinWait = 5 * time.Second
	slowDownIncrement = 5 * time.Second

	// deviceGrantType is RFC 8628 §3.4's device-code grant. GitHub and
	// GitLab both take it verbatim.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// gitLabDeviceFlowScope mirrors backend/internal/identity/gitlab.go:
	// read_api, not read_user, because the API reads the mint performs
	// server-side are not authorized by read_user.
	gitLabDeviceFlowScope = "read_api"

	providerGitHub = "github"
	providerGitLab = "gitlab"
)

// supportedProviders is the set `--provider` accepts, in the order the
// rejection message names them.
var supportedProviders = []string{providerGitHub, providerGitLab}

// tokenLogin implements `fishhawk token login`. It validates
// --provider against the supported set, resolves that provider's device
// client_id and instance base URL (flag/env override, else backend
// discovery), drives the matching device flow to an access token, POSTs
// that token — with the provider — to the backend mint endpoint, stores
// the minted token, and prints the resulting subject / scope / expiry.
func tokenLogin(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk token login"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	provider := fs.String("provider", providerGitHub,
		"identity provider to log in with ("+strings.Join(supportedProviders, "|")+")")
	clientID := fs.String("client-id",
		envOr("FISHHAWK_OAUTH_CLIENT_ID", ""),
		"OAuth App client_id; overrides backend discovery")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk token login [--provider github|gitlab] [--client-id ID]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Log in via the forge's OAuth device flow and mint a user-bound Fishhawk token.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "The command prints a short user code and a verification URL, waits")
		_, _ = fmt.Fprintln(stderr, "for you to authorize in the browser, then hands the resulting access token to")
		_, _ = fmt.Fprintln(stderr, "the backend, which re-verifies it server-side and mints a token scoped to the")
		_, _ = fmt.Fprintln(stderr, "operator default set. The minted token is saved in the local credential store")
		_, _ = fmt.Fprintln(stderr, "(see `fishhawk token list`) and used automatically when --token / FISHHAWK_TOKEN")
		_, _ = fmt.Fprintln(stderr, "is empty.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "--provider selects which forge verifies you; the backend advertises the")
		_, _ = fmt.Fprintln(stderr, "configured set (and each one's device client_id + instance base URL) from")
		_, _ = fmt.Fprintln(stderr, "GET /v0/tokens/login.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if !isSupportedProvider(*provider) {
		_, _ = fmt.Fprintf(stderr, "%s: unsupported --provider %q (supported: %s)\n",
			name, *provider, strings.Join(supportedProviders, ", "))
		return exitUsage
	}
	backend := strings.TrimRight(*cf.backendURL, "/")

	// Resolve the provider's device client_id and (for gitlab) its
	// instance base URL. An explicit --client-id / FISHHAWK_OAUTH_CLIENT_ID
	// overrides the advertised id, but it cannot supply a base URL — so a
	// gitlab login still consults discovery to learn which instance to
	// drive. A github login with an explicit id skips discovery entirely,
	// exactly as it did before this change.
	entry := tokenLoginProviderEntry{Provider: *provider, ClientID: *clientID}
	if *clientID == "" || *provider != providerGitHub {
		disc, err := discoverProvider(context.Background(), backend, *cf.timeout, *provider)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return exitFailure
		}
		entry.BaseURL = disc.BaseURL
		if *clientID == "" {
			entry.ClientID = disc.ClientID
		}
	}
	if entry.ClientID == "" {
		_, _ = fmt.Fprintf(stderr,
			"%s: no OAuth client_id available for provider %q (backend did not advertise one; pass --client-id or set FISHHAWK_OAUTH_CLIENT_ID)\n",
			name, *provider)
		return exitFailure
	}

	// The device flow blocks on the human authorizing in a browser,
	// so it must not be bounded by the per-request --timeout. It self-
	// bounds on the device code's expiry.
	driver, err := deviceFlowDriverFor(entry)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitFailure
	}
	accessToken, err := runDeviceFlow(context.Background(), driver, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitFailure
	}

	// Mint the user-bound Fishhawk token: the backend re-verifies the
	// access token server-side and applies the operator-permission
	// gate before issuing.
	minted, err := mintToken(context.Background(), backend, *cf.timeout, tokenLoginRequest{
		Provider:    *provider,
		AccessToken: accessToken,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitFailure
	}

	if err := credstore.Store(backend, credstore.Credential{
		Token:     minted.Token,
		Subject:   minted.Subject,
		Scopes:    minted.Scopes,
		Provider:  minted.Provider,
		ExpiresAt: minted.ExpiresAt,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: store credential: %v\n", name, err)
		return exitFailure
	}

	path, _ := credstore.Path()
	_, _ = fmt.Fprintf(stdout, "Logged in to %s\n", backend)
	_, _ = fmt.Fprintf(stdout, "subject: %s\n", minted.Subject)
	_, _ = fmt.Fprintf(stdout, "scope:   %s\n", scopeDisplay(minted.Scopes))
	_, _ = fmt.Fprintf(stdout, "expiry:  %s\n", expiryDisplay(minted.ExpiresAt))
	if path != "" {
		_, _ = fmt.Fprintf(stdout, "stored:  %s\n", path)
	}
	return exitOK
}

// tokenList implements `fishhawk token list`. It renders the stored
// credentials (one line per backend URL) without contacting any
// backend — the store is the source of truth for what the operator
// is logged in to.
func tokenList(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk token list"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	all, err := credstore.List()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitFailure
	}
	if len(all) == 0 {
		_, _ = fmt.Fprintln(stdout, "(no stored credentials; run `fishhawk token login`)")
		return exitOK
	}
	// Stable output: sort keys.
	urls := make([]string, 0, len(all))
	for u := range all {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		c := all[u]
		_, _ = fmt.Fprintf(stdout, "%s\n", u)
		_, _ = fmt.Fprintf(stdout, "  subject: %s\n", orDash(c.Subject))
		_, _ = fmt.Fprintf(stdout, "  scope:   %s\n", scopeDisplay(c.Scopes))
		if c.Provider != "" {
			_, _ = fmt.Fprintf(stdout, "  provider: %s\n", c.Provider)
		}
		_, _ = fmt.Fprintf(stdout, "  expiry:  %s\n", expiryDisplay(c.ExpiresAt))
	}
	return exitOK
}

// --- device flow --------------------------------------------------

// deviceFlowDriver is the per-forge half of the device flow: the two
// HTTP exchanges. The poll SEMANTICS (interval floor, slow_down
// back-off, expiry, terminal errors) are forge-independent and live in
// runDeviceFlow, so GitLab inherits every guard GitHub already had.
type deviceFlowDriver struct {
	requestCode func(ctx context.Context) (*deviceCodeResponse, error)
	poll        func(ctx context.Context, deviceCode string) (*accessTokenResponse, error)
}

// deviceFlowDriverFor builds the driver for a resolved discovery entry.
// A gitlab entry with no base_url is refused here rather than producing
// requests against a bare "/oauth/authorize_device" path: the backend
// advertising gitlab without FISHHAWKD_GITLAB_BASE_URL is a
// misconfiguration the operator has to fix, not something the CLI can
// guess an instance for.
func deviceFlowDriverFor(entry tokenLoginProviderEntry) (deviceFlowDriver, error) {
	switch entry.Provider {
	case providerGitHub:
		return deviceFlowDriver{
			requestCode: func(ctx context.Context) (*deviceCodeResponse, error) {
				return requestDeviceCode(ctx, entry.ClientID)
			},
			poll: func(ctx context.Context, deviceCode string) (*accessTokenResponse, error) {
				return pollAccessToken(ctx, entry.ClientID, deviceCode)
			},
		}, nil
	case providerGitLab:
		base := strings.TrimRight(entry.BaseURL, "/")
		if base == "" {
			return deviceFlowDriver{}, fmt.Errorf(
				"backend advertised the gitlab provider without a base_url (set FISHHAWKD_GITLAB_BASE_URL on the backend)")
		}
		return deviceFlowDriver{
			requestCode: func(ctx context.Context) (*deviceCodeResponse, error) {
				return requestGitLabDeviceCode(ctx, base, entry.ClientID)
			},
			poll: func(ctx context.Context, deviceCode string) (*accessTokenResponse, error) {
				return pollGitLabAccessToken(ctx, base, entry.ClientID, deviceCode)
			},
		}, nil
	default:
		return deviceFlowDriver{}, fmt.Errorf("unsupported provider %q", entry.Provider)
	}
}

// runDeviceFlow requests a device code, prints the user prompt to
// stderr, and polls for the access token until the forge authorizes,
// the code expires, or ctx is cancelled.
func runDeviceFlow(ctx context.Context, driver deviceFlowDriver, stderr io.Writer) (string, error) {
	device, err := driver.requestCode(ctx)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(stderr,
		"To authorize, open %s and enter code: %s\n",
		device.VerificationURI, device.UserCode)
	_, _ = fmt.Fprintln(stderr, "Waiting for authorization...")

	interval := time.Duration(device.Interval) * time.Second
	if interval < deviceFlowMinWait {
		interval = deviceFlowMinWait
	}
	if deviceFlowInterval > 0 {
		interval = deviceFlowInterval
	}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("device authorization timed out before you approved it")
		}
		if err := deviceFlowSleep(ctx, interval); err != nil {
			return "", err
		}
		tok, err := driver.poll(ctx, device.DeviceCode)
		if err != nil {
			return "", err
		}
		switch tok.Error {
		case "":
			return tok.AccessToken, nil
		case "authorization_pending":
			continue
		case "slow_down":
			if tok.Interval > 0 {
				interval = time.Duration(tok.Interval) * time.Second
			} else {
				interval += slowDownIncrement
			}
			continue
		case "expired_token":
			return "", fmt.Errorf("device authorization timed out before you approved it")
		case "access_denied":
			return "", fmt.Errorf("device authorization was denied")
		default:
			return "", fmt.Errorf("device flow error: %s", tok.Error)
		}
	}
}

func requestDeviceCode(ctx context.Context, clientID string) (*deviceCodeResponse, error) {
	var out deviceCodeResponse
	err := postForJSON(ctx, strings.TrimRight(githubDeviceBaseURL, "/")+"/login/device/code",
		map[string]string{"client_id": clientID, "scope": deviceFlowScope}, &out)
	if err != nil {
		wrapped := fmt.Errorf("request device code: %w", err)
		return nil, annotateDeviceFlowError(wrapped, err.Error())
	}
	// GitHub can answer 200 with no device_code and the OAuth error in
	// the body instead of a non-2xx status (observed live for
	// device_flow_disabled, #1752). Treat that the same as a transport
	// error rather than proceeding with an empty verification URI.
	if out.DeviceCode == "" && out.Error != "" {
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		return nil, annotateDeviceFlowError(fmt.Errorf("request device code: %s", msg), out.Error)
	}
	return &out, nil
}

// deviceFlowDisabledHint names the exact GitHub setting that must be
// enabled before the device flow will work, so the operator doesn't
// have to go spelunking through GitHub's docs on a first-install
// failure (#1752).
const deviceFlowDisabledHint = "enable the app's Device Flow: GitHub → Settings → Developer settings → GitHub Apps → <your app> → General → check 'Enable Device Flow' → Update application"

// annotateDeviceFlowError appends deviceFlowDisabledHint to err when
// probeText carries GitHub's device_flow_disabled token — whether that
// token arrived in a non-2xx raw response body or a 200-body error
// field, both of which requestDeviceCode's two failure branches surface
// via postForJSON/readBrief. Any other error passes through unchanged.
func annotateDeviceFlowError(err error, probeText string) error {
	if err == nil || !strings.Contains(probeText, "device_flow_disabled") {
		return err
	}
	return fmt.Errorf("%w (%s)", err, deviceFlowDisabledHint)
}

func pollAccessToken(ctx context.Context, clientID, deviceCode string) (*accessTokenResponse, error) {
	var out accessTokenResponse
	err := postForJSON(ctx, strings.TrimRight(githubDeviceBaseURL, "/")+"/login/oauth/access_token",
		map[string]string{
			"client_id":   clientID,
			"device_code": deviceCode,
			"grant_type":  deviceGrantType,
		}, &out)
	if err != nil {
		return nil, fmt.Errorf("poll access token: %w", err)
	}
	return &out, nil
}

// --- GitLab device flow (RFC 8628) --------------------------------
//
// GitLab's OAuth endpoints differ from GitHub's on two axes that the
// shared poll loop must not have to know about, which is why they get
// their own exchanges: the bodies are application/x-www-form-urlencoded
// (not JSON), and RFC 8628 §3.5 poll states arrive as OAuth error
// responses carried on HTTP 400 (Doorkeeper), not on a 200. Both
// mirror backend/internal/identity/gitlab.go so the CLI half and the
// server half speak one contract.

// requestGitLabDeviceCode performs POST {base}/oauth/authorize_device.
// The body carries client_id + scope and NO client_secret — the device
// application is the NON-Confidential one (E66.4 constraint 7).
func requestGitLabDeviceCode(ctx context.Context, base, clientID string) (*deviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", gitLabDeviceFlowScope)

	var out deviceCodeResponse
	if err := postFormForJSON(ctx, base+"/oauth/authorize_device", form, &out); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if out.DeviceCode == "" {
		msg := out.ErrorDescription
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = "response carried no device_code"
		}
		return nil, fmt.Errorf("request device code: %s", msg)
	}
	return &out, nil
}

// pollGitLabAccessToken performs one POST {base}/oauth/token with the
// device grant. A non-2xx carrying a decodable OAuth `error` is a POLL
// STATE, not a transport failure, so it is returned for the shared loop
// to dispatch on; only a non-2xx with no recognizable error body is a
// hard failure.
func pollGitLabAccessToken(ctx context.Context, base, clientID, deviceCode string) (*accessTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", deviceGrantType)

	var out accessTokenResponse
	if err := postFormForJSON(ctx, base+"/oauth/token", form, &out); err != nil {
		if out.Error != "" {
			return &out, nil
		}
		return nil, fmt.Errorf("poll access token: %w", err)
	}
	return &out, nil
}

// --- backend calls ------------------------------------------------

// discoverProvider GETs /v0/tokens/login and returns the entry for the
// requested provider. A 503 (tokens_unconfigured) or any API error is
// surfaced so the operator learns the backend has no OAuth configured.
func discoverProvider(ctx context.Context, backend string, timeout time.Duration, provider string) (tokenLoginProviderEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var disc tokenLoginDiscovery
	if err := getForJSON(ctx, backend+"/v0/tokens/login", &disc); err != nil {
		return tokenLoginProviderEntry{}, fmt.Errorf("discover OAuth client_id: %w", err)
	}
	return resolveProviderEntry(disc, provider)
}

// resolveProviderEntry selects the requested provider's entry from a
// discovery response.
//
// Two branches, and the difference matters. When `providers` is present
// it is AUTHORITATIVE: a requested provider absent from it is refused,
// naming what the backend does advertise, because silently falling back
// to another forge's client_id would drive the device flow against the
// wrong instance and mint under an identity the operator did not ask
// for. When `providers` is ABSENT the backend predates E66.4 / #2392
// and only ever served github, so the legacy top-level provider/client_id
// are used — and only for the provider they actually name.
func resolveProviderEntry(disc tokenLoginDiscovery, provider string) (tokenLoginProviderEntry, error) {
	if len(disc.Providers) > 0 {
		advertised := make([]string, 0, len(disc.Providers))
		for _, e := range disc.Providers {
			if e.Provider == provider {
				return e, nil
			}
			advertised = append(advertised, e.Provider)
		}
		return tokenLoginProviderEntry{}, fmt.Errorf(
			"backend does not offer provider %q (configured: %s)", provider, strings.Join(advertised, ", "))
	}

	// Legacy backend: the top-level fields are the whole surface.
	legacy := disc.Provider
	if legacy == "" {
		legacy = providerGitHub
	}
	if legacy != provider {
		return tokenLoginProviderEntry{}, fmt.Errorf(
			"backend does not offer provider %q (configured: %s)", provider, legacy)
	}
	return tokenLoginProviderEntry{Provider: legacy, ClientID: disc.ClientID}, nil
}

func isSupportedProvider(p string) bool {
	for _, s := range supportedProviders {
		if s == p {
			return true
		}
	}
	return false
}

// mintToken POSTs the device-flow access token to /v0/tokens/login
// and returns the minted Fishhawk token.
func mintToken(ctx context.Context, backend string, timeout time.Duration, req tokenLoginRequest) (*tokenLoginResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out tokenLoginResponse
	if err := postJSONForJSON(ctx, backend+"/v0/tokens/login", req, &out); err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("mint token: backend returned an empty token")
	}
	return &out, nil
}

// --- small HTTP + formatting helpers ------------------------------
//
// token.go does its own HTTP (rather than the typed httpclient)
// because the device flow talks to GitHub, the mint/discovery
// endpoints are unauthenticated bootstrap calls, and this slice must
// not touch the httpclient package.

// tokenHTTPClient is the shared client for token.go's calls. A modest
// per-call timeout is applied by the callers via context; the client
// timeout is a backstop.
var tokenHTTPClient = &http.Client{Timeout: 60 * time.Second}

// apiErrorEnvelope mirrors the backend's error wire shape so a failed
// mint/discovery surfaces the server's code + message.
type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	// Details mirrors the backend's map[string]any{"error": ...} detail
	// payload. On a failed mint the backend puts the underlying cause (e.g.
	// the wrapped permission-check error) here; surfacing it gives the
	// operator the real reason a `token login` 500'd (E39.10 / #1753).
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

// postForJSON POSTs a JSON body and decodes a JSON response. Used for
// the GitHub device-flow exchanges, which always answer 200 with a
// JSON body (device-flow poll states ride in the body, not the
// status).
func postForJSON(ctx context.Context, url string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readBrief(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// postFormForJSON POSTs an application/x-www-form-urlencoded body and
// decodes a JSON response. Used for GitLab's OAuth endpoints, which
// take form bodies and — per RFC 8628 §3.5 — serve the device-flow poll
// states as OAuth error responses on HTTP 400. So the body is decoded
// into out on EVERY status before the status is judged, letting the
// caller distinguish a poll state from a transport failure.
func postFormForJSON(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read response: %w", readErr)
	}
	decodeErr := json.Unmarshal(raw, out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 256)])))
	}
	if decodeErr != nil {
		return fmt.Errorf("decode response: %w", decodeErr)
	}
	return nil
}

// getForJSON GETs a backend endpoint and decodes JSON, translating a
// non-2xx into the backend's error envelope.
func getForJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkAPIStatus(resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// postJSONForJSON POSTs a JSON body to a backend endpoint and decodes
// JSON, translating a non-2xx into the backend's error envelope.
func postJSONForJSON(ctx context.Context, url string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkAPIStatus(resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// checkAPIStatus returns an error carrying the backend's error code +
// message for any non-2xx response.
func checkAPIStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var env apiErrorEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
		if env.Details.Error != "" {
			return fmt.Errorf("HTTP %d (%s): %s: %s",
				resp.StatusCode, env.Error.Code, env.Error.Message, env.Details.Error)
		}
		return fmt.Errorf("HTTP %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

// sleepCtx waits d honoring ctx cancellation.
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

func readBrief(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(b))
}

func scopeDisplay(scopes []string) string {
	if len(scopes) == 0 {
		return "(none)"
	}
	return strings.Join(scopes, " ")
}

func expiryDisplay(t *time.Time) string {
	if t == nil {
		return "none (token does not expire)"
	}
	return t.Format(time.RFC3339)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
