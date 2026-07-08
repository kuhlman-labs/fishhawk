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
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/cli/internal/credstore"
)

// runToken dispatches `fishhawk token <subcommand>` (E39.3 / #1708).
// The command group drives the user-bound OAuth login: `token login`
// runs the GitHub device flow, mints a user-bound Fishhawk token via
// the backend, and stores it in the local credential store; `token
// list` shows the stored credentials.
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
type tokenLoginDiscovery struct {
	Provider string `json:"provider"`
	ClientID string `json:"client_id"`
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
)

// tokenLogin implements `fishhawk token login`. It resolves the OAuth
// client_id (flag/env override, else backend discovery), drives the
// GitHub device flow to an access token, POSTs that token to the
// backend mint endpoint, stores the minted token, and prints the
// resulting subject / scope / expiry.
func tokenLogin(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk token login"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	provider := fs.String("provider", "github", "identity provider (only github is supported today)")
	clientID := fs.String("client-id",
		envOr("FISHHAWK_OAUTH_CLIENT_ID", ""),
		"OAuth App client_id; overrides backend discovery")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk token login [--provider github] [--client-id ID]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Log in via the GitHub OAuth device flow and mint a user-bound Fishhawk token.")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "The command prints a short user code and a github.com verification URL, waits")
		_, _ = fmt.Fprintln(stderr, "for you to authorize in the browser, then hands the resulting access token to")
		_, _ = fmt.Fprintln(stderr, "the backend, which re-verifies it server-side and mints a token scoped to the")
		_, _ = fmt.Fprintln(stderr, "operator default set. The minted token is saved in the local credential store")
		_, _ = fmt.Fprintln(stderr, "(see `fishhawk token list`) and used automatically when --token / FISHHAWK_TOKEN")
		_, _ = fmt.Fprintln(stderr, "is empty.")
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
	if *provider != "github" {
		_, _ = fmt.Fprintf(stderr, "%s: unsupported --provider %q (only \"github\" is supported)\n", name, *provider)
		return exitUsage
	}
	backend := strings.TrimRight(*cf.backendURL, "/")

	// Resolve the client_id: an explicit flag/env value wins;
	// otherwise ask the backend's discovery endpoint.
	cid := *clientID
	if cid == "" {
		disc, err := discoverClientID(context.Background(), backend, *cf.timeout)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return exitFailure
		}
		cid = disc
	}
	if cid == "" {
		_, _ = fmt.Fprintf(stderr,
			"%s: no OAuth client_id available (backend did not advertise one; pass --client-id or set FISHHAWK_OAUTH_CLIENT_ID)\n", name)
		return exitFailure
	}

	// The device flow blocks on the human authorizing in a browser,
	// so it must not be bounded by the per-request --timeout. It self-
	// bounds on the device code's expiry.
	accessToken, err := runDeviceFlow(context.Background(), cid, stderr)
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

// runDeviceFlow requests a device code, prints the user prompt to
// stderr, and polls for the access token until GitHub authorizes, the
// code expires, or ctx is cancelled.
func runDeviceFlow(ctx context.Context, clientID string, stderr io.Writer) (string, error) {
	device, err := requestDeviceCode(ctx, clientID)
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
		tok, err := pollAccessToken(ctx, clientID, device.DeviceCode)
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
		return nil, fmt.Errorf("request device code: %w", err)
	}
	return &out, nil
}

func pollAccessToken(ctx context.Context, clientID, deviceCode string) (*accessTokenResponse, error) {
	var out accessTokenResponse
	err := postForJSON(ctx, strings.TrimRight(githubDeviceBaseURL, "/")+"/login/oauth/access_token",
		map[string]string{
			"client_id":   clientID,
			"device_code": deviceCode,
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
		}, &out)
	if err != nil {
		return nil, fmt.Errorf("poll access token: %w", err)
	}
	return &out, nil
}

// --- backend calls ------------------------------------------------

// discoverClientID GETs /v0/tokens/login and returns the advertised
// client_id. A 503 (tokens_unconfigured) or any API error is surfaced
// so the operator learns the backend has no OAuth configured.
func discoverClientID(ctx context.Context, backend string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var disc tokenLoginDiscovery
	if err := getForJSON(ctx, backend+"/v0/tokens/login", &disc); err != nil {
		return "", fmt.Errorf("discover OAuth client_id: %w", err)
	}
	return disc.ClientID, nil
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
