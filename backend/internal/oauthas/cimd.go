package oauthas

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClientMetadata is the subset of an OAuth Client ID Metadata Document (CIMD) this
// server models. It is decoded LENIENTLY (unknown JSON fields ignored): a
// conformant document carries fields we do not model and rejecting them would
// break a valid client.
type ClientMetadata struct {
	ClientID                string   `json:"client_id"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// clone returns a deep copy so a caller mutating a returned document's slices
// cannot poison the validated cache entry (#2427 condition 2).
func (m *ClientMetadata) clone() *ClientMetadata {
	if m == nil {
		return nil
	}
	cp := *m
	cp.RedirectURIs = append([]string(nil), m.RedirectURIs...)
	cp.GrantTypes = append([]string(nil), m.GrantTypes...)
	cp.ResponseTypes = append([]string(nil), m.ResponseTypes...)
	return &cp
}

// ValidateClientIDURL validates a CIMD client_id URL: https only, non-empty host,
// no userinfo, no fragment. A query is permitted (the self-referential equality
// check binds it). The fragment check is lexical for the same reason as in
// validateRedirectURIComponents.
func ValidateClientIDURL(raw string) (*url.URL, error) {
	if strings.Contains(raw, "#") {
		return nil, newError(ErrCodeInvalidClient, "client_id URL must not contain a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, newError(ErrCodeInvalidClient, "client_id is not a valid URL").withCause(err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, newError(ErrCodeInvalidClient, "client_id URL scheme must be https")
	}
	if u.Host == "" {
		return nil, newError(ErrCodeInvalidClient, "client_id URL host is empty")
	}
	if u.User != nil {
		return nil, newError(ErrCodeInvalidClient, "client_id URL must not contain userinfo")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, newError(ErrCodeInvalidClient, "client_id URL must not contain a fragment")
	}
	return u, nil
}

const (
	defaultFetchTimeout = 5 * time.Second
	defaultMaxBytes     = 64 * 1024
	defaultMaxRedirects = 3
	defaultCacheTTL     = 15 * time.Minute
)

type cacheEntry struct {
	doc       *ClientMetadata
	expiresAt time.Time
}

// Fetcher fetches and validates a CIMD document behind injected HTTP-client,
// address-guard and DNS-lookup seams, with a connect-time SSRF guard. The zero
// value is usable: an unset HTTPClient (or one with a nil Transport) is given a
// Transport backed by the PublicOnlyAddrGuard guarded dialer, and every bound has
// a default.
type Fetcher struct {
	// HTTPClient, when nil, is built with a guarded dialer using Guard and Lookup.
	// A non-nil client with a nil Transport ALSO gets the guarded dialer, so a
	// caller passing &http.Client{Timeout: …} is not silently unguarded; a non-nil
	// Transport is used as-is and the caller owns its egress policy.
	HTTPClient *http.Client
	// Guard defaults to PublicOnlyAddrGuard.
	Guard addrGuard
	// Lookup defaults to net.DefaultResolver.LookupNetIP.
	Lookup lookupFunc
	// Timeout bounds a single Fetch (default 5s).
	Timeout time.Duration
	// MaxBytes caps the document body (default 64 KiB).
	MaxBytes int64
	// MaxRedirects caps redirect hops (default 3).
	MaxRedirects int
	// CacheTTL bounds a validated cache entry's lifetime (default 15m).
	CacheTTL time.Duration
	// Now is the clock used for cache expiry (default time.Now).
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

func (f *Fetcher) guard() addrGuard {
	if f.Guard != nil {
		return f.Guard
	}
	return PublicOnlyAddrGuard
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaultFetchTimeout
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return defaultMaxBytes
}

func (f *Fetcher) maxRedirects() int {
	if f.MaxRedirects > 0 {
		return f.MaxRedirects
	}
	return defaultMaxRedirects
}

func (f *Fetcher) cacheTTL() time.Duration {
	if f.CacheTTL > 0 {
		return f.CacheTTL
	}
	return defaultCacheTTL
}

// effectiveClient returns a client that reuses any injected Transport but always
// applies this Fetcher's redirect-revalidating CheckRedirect. When neither an
// injected client nor its Transport is set, it installs the guarded transport, so
// a caller passing &http.Client{Timeout: …} (a nil Transport) still gets the
// connect-time SSRF guard rather than silently inheriting http.DefaultTransport. A
// non-nil injected Transport is used as-is (the caller owns its egress policy). It
// never mutates an injected *http.Client.
func (f *Fetcher) effectiveClient() *http.Client {
	base := f.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	transport := base.Transport
	if transport == nil {
		gd := newGuardedDialer(f.guard(), f.Lookup)
		transport = &http.Transport{DialContext: gd.DialContext}
	}
	return &http.Client{
		Transport:     transport,
		Jar:           base.Jar,
		CheckRedirect: f.checkRedirect,
	}
}

// checkRedirect caps hops at MaxRedirects and revalidates each hop against the
// PARSED client_id predicate, so an https client_id cannot downgrade the fetch to
// http and a hop cannot introduce userinfo or a non-empty fragment. The
// connect-address guard applies to every hop automatically via the shared dialer.
//
// One lexical corner is structurally unavailable at the hop boundary: the hop URL
// is revalidated via req.URL.String(), and url.URL.String() cannot re-serialize a
// bare trailing '#' (net/url records no empty-fragment flag — the same limitation
// documented for redirect-URI matching), so a Location header ending in a bare '#'
// is NOT caught by the lexical empty-fragment rule; only non-empty hop fragments
// are. This is negligible in practice — fragments are never transmitted in
// requests — but it means the hop check enforces the PARSED predicate, not the
// lexical raw-string one applied to the initial client_id.
func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.maxRedirects() {
		return newError(ErrCodeInvalidClient, "client_id document exceeded the redirect limit").withCause(ErrNotSelfReferential)
	}
	if _, err := ValidateClientIDURL(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// Fetch retrieves and validates the CIMD document for clientID. A validated
// document is cached (deep-copied); failures are never cached. Every returned
// document is a deep copy, so a caller mutating it cannot poison the cache.
func (f *Fetcher) Fetch(ctx context.Context, clientID string) (*ClientMetadata, error) {
	if doc, ok := f.cacheGet(clientID); ok {
		return doc, nil
	}

	u, err := ValidateClientIDURL(clientID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, newError(ErrCodeServerError, "could not build client_id request").withCause(err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.effectiveClient().Do(req)
	if err != nil {
		return nil, wrapFetchErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, newError(ErrCodeInvalidClient, "client_id document returned HTTP %d", resp.StatusCode).withCause(ErrNotSelfReferential)
	}
	if !isJSONContentType(resp.Header.Get("Content-Type")) {
		return nil, newError(ErrCodeInvalidClient, "client_id document has a non-JSON content type").withCause(ErrNotSelfReferential)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes()+1))
	if err != nil {
		return nil, newError(ErrCodeServerError, "could not read client_id document").withCause(err)
	}
	if int64(len(body)) > f.maxBytes() {
		return nil, newError(ErrCodeInvalidClient, "client_id document exceeds the maximum size").withCause(ErrDocumentTooLarge)
	}

	var doc ClientMetadata
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, newError(ErrCodeInvalidClient, "client_id document is not valid JSON").withCause(err)
	}
	if err := validateClientMetadata(clientID, &doc); err != nil {
		return nil, err
	}

	f.cacheStore(clientID, &doc)
	return doc.clone(), nil
}

// validateClientMetadata enforces the CIMD contract: client_id byte-exactly equal
// to the ORIGINALLY REQUESTED id (so a redirect cannot re-point identity),
// non-empty redirect_uris each passing the component check,
// token_endpoint_auth_method present and equal to "none" (public clients only;
// absent defaults to client_secret_basic which is unsupported, so absent is
// refused), and grant_types (when present) containing authorization_code.
func validateClientMetadata(requestedClientID string, doc *ClientMetadata) error {
	if doc.ClientID != requestedClientID {
		return newError(ErrCodeInvalidClient, "client_id document is not self-referential").withCause(ErrNotSelfReferential)
	}
	if len(doc.RedirectURIs) == 0 {
		return newError(ErrCodeInvalidClient, "client_id document declares no redirect_uris").withCause(ErrNotSelfReferential)
	}
	for _, ru := range doc.RedirectURIs {
		if _, err := validateRedirectURIComponents(ru); err != nil {
			return newError(ErrCodeInvalidClient, "client_id document declares an invalid redirect_uri").withCause(err)
		}
	}
	if doc.TokenEndpointAuthMethod != "none" {
		return newError(ErrCodeInvalidClient, "client_id document must declare token_endpoint_auth_method \"none\" (public client)").withCause(ErrNotSelfReferential)
	}
	if len(doc.GrantTypes) > 0 && !containsString(doc.GrantTypes, "authorization_code") {
		return newError(ErrCodeInvalidClient, "client_id document grant_types must include authorization_code").withCause(ErrNotSelfReferential)
	}
	return nil
}

func (f *Fetcher) cacheGet(clientID string) (*ClientMetadata, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[clientID]
	if !ok {
		return nil, false
	}
	if !f.now().Before(e.expiresAt) {
		delete(f.cache, clientID)
		return nil, false
	}
	return e.doc.clone(), true
}

func (f *Fetcher) cacheStore(clientID string, doc *ClientMetadata) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = make(map[string]cacheEntry)
	}
	f.cache[clientID] = cacheEntry{doc: doc.clone(), expiresAt: f.now().Add(f.cacheTTL())}
}

// wrapFetchErr surfaces a typed *Error carried in the transport error chain (e.g.
// a guard refusal buried inside a *url.Error) unchanged, so its Code and sentinel
// survive; otherwise it wraps the raw error as an invalid_client failure.
func wrapFetchErr(err error) error {
	var oe *Error
	if errors.As(err, &oe) {
		return oe
	}
	return newError(ErrCodeInvalidClient, "client_id document could not be fetched").withCause(err)
}

// isJSONContentType reports whether ct is application/json, with or without
// parameters.
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json"
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
