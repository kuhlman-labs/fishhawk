package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// `fishhawk token login --oauth` (ADR-076 slice 4, E66.5 / #2393): an OAuth
// 2.1 PUBLIC client against the fishhawkd authorization server. The flow is
// RFC 8414 metadata discovery → PKCE S256 + state → an RFC 8252 §7.3
// ephemeral loopback redirect → browser launch → ONE callback → the
// authorization-code exchange → a refreshable credential in credstore.
// Every control the AS already enforces on this path (PKCE verification,
// redirect matching, RFC 8707 audience binding, client binding on rotation)
// is left untouched; this file is the client half only.
//
// The device-flow fhk_ login in token.go stays the default; this path is
// opt-in, and the two never mix (tokenLogin refuses --oauth with the
// device-flow-only flags rather than silently ignoring one).

// oauthASMetadataDoc is the subset of the RFC 8414 document the CLI reads
// from GET {backend}/.well-known/oauth-authorization-server. Declared
// locally on purpose: cli/go.mod carries no dependency on the backend
// module (ADR-014), so the WIRE document in docs/api/v0.openapi.yaml is
// what binds the two halves.
type oauthASMetadataDoc struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// oauthPRMDoc is the subset of the RFC 9728 protected-resource metadata
// document the CLI reads from GET {backend}/.well-known/oauth-protected-
// resource: the resource identifier the AS binds every token to (RFC
// 8707). The authorize endpoint REQUIRES `resource`, so the CLI learns it
// from the resource itself rather than guessing `<issuer>/mcp`.
type oauthPRMDoc struct {
	Resource string `json:"resource"`
}

// oauthTokenExchangeResponse is the RFC 6749 §5.1 success body plus the
// §5.2 error envelope, decoded from one shape so a non-2xx carrying
// either is read the same way (mirrors credstore's refresh decoder).
type oauthTokenExchangeResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

const (
	// defaultOAuthClientID is the public client_id the CLI presents when
	// --oauth-client-id is not set. It is a plain stable id (no CIMD
	// document), so the operator registers it ONCE with
	// `fishhawkd oauth client register --client-id fishhawk-cli ...`
	// (cli/README.md) and every operator's CLI then logs in with no
	// per-user configuration.
	defaultOAuthClientID = "fishhawk-cli"

	// oauthCallbackPath is the path component of the loopback redirect
	// URI. The operator registers it PORTLESS (http://127.0.0.1/callback);
	// RFC 8252 §7.3 lets the AS ignore the ephemeral port at match time.
	oauthCallbackPath = "/callback"

	// oauthLoopbackHost is the literal the redirect URI is built from AND
	// the address the listener binds. It is 127.0.0.1 explicitly — never
	// "localhost" (which can resolve off-loopback) and never a wildcard —
	// so the callback cannot be reached from another host.
	oauthLoopbackHost = "127.0.0.1"

	// oauthPKCEBytes / oauthStateBytes are the crypto/rand entropy sizes.
	// 32 bytes base64url-encodes to 43 chars, RFC 7636 §4.1's minimum
	// code_verifier length.
	oauthPKCEBytes  = 32
	oauthStateBytes = 32
)

// Test seams. Production binds an ephemeral loopback port, launches the
// platform browser (run.go's openBrowser), and waits up to
// oauthLoginWindow for the human; tests swap the listener/browser and
// shrink the window.
var (
	oauthListen = func() (net.Listener, error) {
		return net.Listen("tcp", net.JoinHostPort(oauthLoopbackHost, "0"))
	}
	oauthOpenBrowser = func(u string) error { return openBrowser(u) }
	oauthLoginWindow = 10 * time.Minute
	oauthHTTPClient  = &http.Client{Timeout: 60 * time.Second}
	oauthNow         = time.Now
)

// oauthLoginParams is everything runOAuthLogin needs from tokenLogin.
type oauthLoginParams struct {
	backend  string
	clientID string
	timeout  time.Duration
}

// runOAuthLogin drives the whole flow and returns the credential it
// stored. Errors are returned, not printed, so tokenLogin owns the
// `fishhawk token login: ...` prefix exactly as it does for the device
// flow.
func runOAuthLogin(ctx context.Context, p oauthLoginParams, stderr io.Writer) (credstore.Credential, error) {
	meta, err := discoverOAuthAS(ctx, p.backend, p.timeout)
	if err != nil {
		return credstore.Credential{}, err
	}
	resource, err := discoverOAuthResource(ctx, p.backend, p.timeout)
	if err != nil {
		return credstore.Credential{}, err
	}

	pkce, err := newPKCE()
	if err != nil {
		return credstore.Credential{}, err
	}
	state, err := randomToken(oauthStateBytes)
	if err != nil {
		return credstore.Credential{}, fmt.Errorf("generate state: %w", err)
	}

	ln, err := oauthListen()
	if err != nil {
		return credstore.Credential{}, fmt.Errorf("open loopback callback listener: %w", err)
	}
	redirectURI := loopbackRedirectURI(ln.Addr())

	authorizeURL, err := buildAuthorizeURL(meta.AuthorizationEndpoint, authorizeParams{
		clientID:      p.clientID,
		redirectURI:   redirectURI,
		codeChallenge: pkce.challenge,
		state:         state,
		resource:      resource,
	})
	if err != nil {
		_ = ln.Close()
		return credstore.Credential{}, err
	}

	// Serve exactly one callback. The handler owns the state check, the
	// error/absent-code refusals and the one-shot guard; the token
	// exchange happens ONLY after it delivers a code.
	cb := newOAuthCallback(state, meta.Issuer)
	srv := &http.Server{Handler: cb, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// The URL is ALWAYS printed: a headless terminal (ssh, a container)
	// has no browser to launch, and the operator copies it instead.
	_, _ = fmt.Fprintln(stderr, "To authorize, open this URL in your browser:")
	_, _ = fmt.Fprintf(stderr, "  %s\n", authorizeURL)
	if err := oauthOpenBrowser(authorizeURL); err != nil {
		_, _ = fmt.Fprintf(stderr, "(could not launch a browser automatically: %v)\n", err)
	}
	_, _ = fmt.Fprintf(stderr, "Waiting for the authorization callback on %s ...\n", redirectURI)

	waitCtx, cancel := context.WithTimeout(ctx, oauthLoginWindow)
	defer cancel()
	var code string
	select {
	case res := <-cb.result:
		if res.err != nil {
			return credstore.Credential{}, res.err
		}
		code = res.code
	case err := <-serveErr:
		return credstore.Credential{}, fmt.Errorf("loopback callback listener stopped: %w", err)
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return credstore.Credential{}, ctx.Err()
		}
		return credstore.Credential{}, fmt.Errorf("no authorization callback arrived within %s", oauthLoginWindow)
	}

	cred, err := exchangeAuthorizationCode(ctx, exchangeParams{
		tokenEndpoint: meta.TokenEndpoint,
		clientID:      p.clientID,
		code:          code,
		redirectURI:   redirectURI,
		codeVerifier:  pkce.verifier,
		resource:      resource,
		timeout:       p.timeout,
	})
	if err != nil {
		return credstore.Credential{}, err
	}
	if err := credstore.Store(p.backend, cred); err != nil {
		return credstore.Credential{}, fmt.Errorf("store credential: %w", err)
	}
	return cred, nil
}

// --- discovery -----------------------------------------------------

// oauthASUnconfiguredHint names the backend flag that enables the AS, so
// the 503 oauth_as_unconfigured envelope is actionable.
const oauthASUnconfiguredHint = "enable the authorization server on the backend with --oauth-issuer / FISHHAWKD_OAUTH_ISSUER"

// discoverOAuthAS GETs the RFC 8414 document from the backend. A 503
// oauth_as_unconfigured surfaces verbatim plus the --oauth-issuer hint;
// a document missing either endpoint, or not advertising S256, is
// refused before any secret is generated.
func discoverOAuthAS(ctx context.Context, backend string, timeout time.Duration) (oauthASMetadataDoc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var doc oauthASMetadataDoc
	if err := getForJSON(ctx, backend+"/.well-known/oauth-authorization-server", &doc); err != nil {
		if strings.Contains(err.Error(), "oauth_as_unconfigured") {
			return oauthASMetadataDoc{}, fmt.Errorf("discover authorization server: %w (%s)", err, oauthASUnconfiguredHint)
		}
		return oauthASMetadataDoc{}, fmt.Errorf("discover authorization server: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return oauthASMetadataDoc{}, fmt.Errorf("discover authorization server: metadata from %s names no authorization_endpoint/token_endpoint", backend)
	}
	if !containsString(doc.CodeChallengeMethodsSupported, "S256") {
		return oauthASMetadataDoc{}, fmt.Errorf("discover authorization server: %s does not advertise code_challenge_methods_supported S256; this client only speaks PKCE S256", backend)
	}
	return doc, nil
}

// discoverOAuthResource GETs the RFC 9728 document and returns the
// resource identifier the authorize request must carry (RFC 8707).
func discoverOAuthResource(ctx context.Context, backend string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var doc oauthPRMDoc
	if err := getForJSON(ctx, backend+"/.well-known/oauth-protected-resource", &doc); err != nil {
		return "", fmt.Errorf("discover protected resource: %w", err)
	}
	if doc.Resource == "" {
		return "", fmt.Errorf("discover protected resource: metadata from %s names no resource identifier", backend)
	}
	return doc.Resource, nil
}

func containsString(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// --- PKCE + state --------------------------------------------------

// pkcePair is an RFC 7636 code_verifier and its S256 code_challenge.
type pkcePair struct {
	verifier  string
	challenge string
}

// newPKCE draws a 32-byte verifier from crypto/rand (base64url, no
// padding → 43 chars) and derives the S256 challenge:
// BASE64URL(SHA256(ASCII(verifier))).
func newPKCE() (pkcePair, error) {
	verifier, err := randomToken(oauthPKCEBytes)
	if err != nil {
		return pkcePair{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	return pkcePair{verifier: verifier, challenge: pkceChallengeS256(verifier)}, nil
}

func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomToken returns n crypto/rand bytes as unpadded base64url.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- loopback redirect + authorize URL ------------------------------

// loopbackRedirectURI builds http://127.0.0.1:<port>/callback from the
// bound listener address. The host is the oauthLoopbackHost literal, not
// addr's textual IP, so the URI shape is fixed by construction — it is
// the shape testdata/wire/oauth_cli_redirect_uri.json describes and the
// backend's redirect matcher is tested against.
func loopbackRedirectURI(addr net.Addr) string {
	port := "0"
	if tcp, ok := addr.(*net.TCPAddr); ok {
		port = fmt.Sprint(tcp.Port)
	}
	return "http://" + net.JoinHostPort(oauthLoopbackHost, port) + oauthCallbackPath
}

type authorizeParams struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	state         string
	resource      string
}

// buildAuthorizeURL appends the RFC 6749 §4.1.1 + RFC 7636 + RFC 8707
// parameters to the advertised authorization endpoint. No scope is
// requested: the AS defaults an absent scope to the client's registered
// scope (or its whole vocabulary), so the operator's registration — not
// the CLI — decides the authority.
func buildAuthorizeURL(endpoint string, p authorizeParams) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("authorization_endpoint %q is not an absolute URL", endpoint)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.clientID)
	q.Set("redirect_uri", p.redirectURI)
	q.Set("code_challenge", p.codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.state)
	q.Set("resource", p.resource)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// --- the one-shot callback ------------------------------------------

// oauthCallbackResult is what the handler delivers to the flow: a code,
// or the reason the callback was refused.
type oauthCallbackResult struct {
	code string
	err  error
}

// oauthCallback serves exactly ONE authorization response on
// oauthCallbackPath. Its refusal ladder, in order: (1) a state that does
// not match the generated one (constant-time compare) — refused BEFORE
// any token exchange can happen, since the exchange only runs on a
// delivered code; (2) an `error=` parameter, surfaced with the AS's
// error/error_description; (3) an RFC 9207 iss that names a different
// issuer than the one discovered; (4) an absent code; (5) any request
// after the first has been consumed. Every request on another path is
// a 404 that consumes nothing (a browser's favicon probe must not end
// the login).
type oauthCallback struct {
	state  string
	issuer string
	result chan oauthCallbackResult

	mu       sync.Mutex
	consumed bool
}

func newOAuthCallback(state, issuer string) *oauthCallback {
	return &oauthCallback{state: state, issuer: issuer, result: make(chan oauthCallbackResult, 1)}
}

func (c *oauthCallback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != oauthCallbackPath {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	if c.consumed {
		c.mu.Unlock()
		http.Error(w, "authorization callback already received; this login accepts exactly one", http.StatusConflict)
		return
	}
	c.consumed = true
	c.mu.Unlock()

	q := r.URL.Query()
	res := c.evaluate(q)
	if res.err != nil {
		http.Error(w, "fishhawk token login: "+res.err.Error(), http.StatusBadRequest)
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>fishhawk</title><p>Fishhawk login complete. You can close this window.</p>")
	}
	c.result <- res
}

// evaluate applies the refusal ladder to the callback query.
func (c *oauthCallback) evaluate(q url.Values) oauthCallbackResult {
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1 {
		return oauthCallbackResult{err: errors.New("authorization callback carried a state that does not match this login; refusing to exchange the code")}
	}
	if e := q.Get("error"); e != "" {
		if d := q.Get("error_description"); d != "" {
			return oauthCallbackResult{err: fmt.Errorf("authorization refused by the server: %s: %s", e, d)}
		}
		return oauthCallbackResult{err: fmt.Errorf("authorization refused by the server: %s", e)}
	}
	if iss := q.Get("iss"); iss != "" && c.issuer != "" && iss != c.issuer {
		return oauthCallbackResult{err: fmt.Errorf("authorization callback names issuer %q but the discovered issuer is %q; refusing to exchange the code", iss, c.issuer)}
	}
	code := q.Get("code")
	if code == "" {
		return oauthCallbackResult{err: errors.New("authorization callback carried no code")}
	}
	return oauthCallbackResult{code: code}
}

// --- the code exchange ----------------------------------------------

type exchangeParams struct {
	tokenEndpoint string
	clientID      string
	code          string
	redirectURI   string
	codeVerifier  string
	resource      string
	timeout       time.Duration
}

// exchangeAuthorizationCode POSTs the RFC 6749 §4.1.3 grant as a PUBLIC
// client: code + redirect_uri + client_id + code_verifier + resource, with
// NO client_secret and NO Authorization header (the fishhawkd token
// endpoint refuses both outright). A non-2xx decodes the §5.2 envelope
// and names the error code; a 2xx with no access_token is an error,
// never a silently empty bearer. The returned credential carries
// everything credstore's refresh needs: the ROTATING refresh token, the
// client_id, the token endpoint, IssuedAt and ExpiresAt.
func exchangeAuthorizationCode(ctx context.Context, p exchangeParams) (credstore.Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", p.code)
	form.Set("redirect_uri", p.redirectURI)
	form.Set("client_id", p.clientID)
	form.Set("code_verifier", p.codeVerifier)
	if p.resource != "" {
		form.Set("resource", p.resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return credstore.Credential{}, fmt.Errorf("exchange authorization code: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return credstore.Credential{}, fmt.Errorf("exchange authorization code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return credstore.Credential{}, fmt.Errorf("exchange authorization code: read response: %w", err)
	}
	var tr oauthTokenExchangeResponse
	decodeErr := json.Unmarshal(raw, &tr)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if decodeErr == nil && tr.Error != "" {
			if tr.ErrorDescription != "" {
				return credstore.Credential{}, fmt.Errorf("exchange authorization code: refused (HTTP %d %s): %s", resp.StatusCode, tr.Error, tr.ErrorDescription)
			}
			return credstore.Credential{}, fmt.Errorf("exchange authorization code: refused (HTTP %d %s)", resp.StatusCode, tr.Error)
		}
		return credstore.Credential{}, fmt.Errorf("exchange authorization code: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 256)])))
	}
	if decodeErr != nil {
		return credstore.Credential{}, fmt.Errorf("exchange authorization code: response is not JSON: %w", decodeErr)
	}
	if tr.AccessToken == "" {
		return credstore.Credential{}, errors.New("exchange authorization code: response carries no access_token")
	}

	now := oauthNow()
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		// expires_in is only RECOMMENDED by §5.1; an absent value is read
		// as the server's shipped default rather than as non-expiring, so
		// the credential is never one that is never refreshed.
		lifetime = credstore.DefaultLifetime
	}
	expires := now.Add(lifetime)
	return credstore.Credential{
		Token:         tr.AccessToken,
		Scopes:        strings.Fields(tr.Scope),
		RefreshToken:  tr.RefreshToken,
		ClientID:      p.clientID,
		TokenEndpoint: p.tokenEndpoint,
		IssuedAt:      &now,
		ExpiresAt:     &expires,
	}, nil
}
