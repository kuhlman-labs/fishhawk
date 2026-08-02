package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// OAuth 2.1 authorization-server routes (ADR-076 slice 3, #2436). This file
// carries the resolved AS state machine, the RFC 8414 metadata handler, the
// store-first client resolver, and the two shared error renderers every OAuth
// endpoint funnels through. The authorize and token ladders live in
// oauthauthorize.go and oauthtoken.go; they compose the helpers here.

// Default credential lifetimes, applied when the corresponding Config TTL is
// zero. A short code, a working-session access token, and a two-week refresh
// window.
const (
	defaultOAuthCodeTTL         = 60 * time.Second
	defaultOAuthAccessTokenTTL  = time.Hour
	defaultOAuthRefreshTokenTTL = 336 * time.Hour // 14 days
)

// oauthASMode is the resolved verdict for the AS surface, computed ONCE at
// New() from the immutable Config (mirroring mcpRouteState). handleOAuthAS*
// reads it per request and never recomputes it.
type oauthASMode int

const (
	// oauthASDisabled means no issuer was configured — the operator never
	// asked for the AS. All four routes answer 503 oauth_as_unconfigured.
	oauthASDisabled oauthASMode = iota

	// oauthASEnabled means every prerequisite is present: a valid https
	// issuer, a valid resource identifier, a non-nil store AND a non-nil
	// CIMD fetcher. Only in this verdict does the metadata handler render a
	// document — so a server that cannot fetch CIMD can never advertise that
	// it can (FIX 2).
	oauthASEnabled

	// oauthASMisconfigured means an issuer WAS supplied but some prerequisite
	// is defective (bad issuer, bad resource, nil store, or nil fetcher).
	// All four routes answer 503 with the specific reason; NO partial
	// metadata document is ever rendered.
	oauthASMisconfigured

	// oauthASNotLoopback means every prerequisite is present AND the operator
	// set --oauth-require-loopback, but the listener is reachable off-host. All
	// four routes answer 403 oauth_as_loopback_only naming the listen host and
	// the flag (ADR-076 / #2441). This verdict is UNREACHABLE unless the gate is
	// switched on, which defaults off.
	oauthASNotLoopback
)

// oauthASState is the resolved, immutable AS verdict.
type oauthASState struct {
	mode oauthASMode

	// issuer and resource are populated only in the enabled verdict.
	issuer   oauthas.Issuer
	resource oauthas.ResourceIdentifier

	// authorizationEndpoint and tokenEndpoint are the issuer-rooted URLs the
	// metadata document advertises and the consent form posts back to.
	authorizationEndpoint string
	tokenEndpoint         string

	// TTLs applied to minted credentials.
	codeTTL         time.Duration
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration

	// reason is the human-readable diagnosis carried by the 503 in the
	// misconfigured verdict.
	reason string

	// listenHost is the host portion of Config.Addr, echoed in the 403
	// oauth_as_loopback_only body so an operator can see what was rejected.
	// Populated only in the not-loopback verdict.
	listenHost string
}

// resolveOAuthASState computes the AS verdict from cfg. It NEVER panics on a
// bad value — a defective issuer/resource is carried as the misconfigured
// verdict and rendered per request, so New() cannot crash on operator config.
//
// The ladder is ORDERED disabled -> misconfigured -> not-loopback -> enabled,
// and the loopback check runs LAST on purpose: a deployment with BOTH a
// defective issuer and a public bind reports the CONFIG defect, which is the
// actionable diagnosis (the same "cheapest, most specific diagnosis wins"
// ordering resolveMCPRouteState documents). lookupIP is the DNS seam shared with
// the /mcp route (cfg.mcpLookupIP; nil means net.LookupIP), reusing the ONE
// mcpLoopbackHost classifier rather than a second copy.
func resolveOAuthASState(cfg Config, lookupIP func(string) ([]net.IP, error)) oauthASState {
	codeTTL := cfg.OAuthCodeTTL
	if codeTTL <= 0 {
		codeTTL = defaultOAuthCodeTTL
	}
	accessTTL := cfg.OAuthAccessTokenTTL
	if accessTTL <= 0 {
		accessTTL = defaultOAuthAccessTokenTTL
	}
	refreshTTL := cfg.OAuthRefreshTokenTTL
	if refreshTTL <= 0 {
		refreshTTL = defaultOAuthRefreshTokenTTL
	}

	// DISABLED: no issuer means the operator never asked for the AS.
	if cfg.OAuthASIssuer == "" {
		return oauthASState{mode: oauthASDisabled}
	}

	misconfigured := func(reason string) oauthASState {
		return oauthASState{mode: oauthASMisconfigured, reason: reason}
	}

	issuer, err := oauthas.ParseIssuer(cfg.OAuthASIssuer)
	if err != nil {
		return misconfigured("the OAuth issuer is not a valid RFC 8414 https issuer: " + err.Error())
	}

	// The resource defaults to <issuer>/mcp when unset — the audience this AS
	// mints tokens for.
	rawResource := cfg.OAuthASResource
	if rawResource == "" {
		rawResource = issuer.String() + "/mcp"
	}
	resource, err := oauthas.ParseResourceIdentifier(rawResource)
	if err != nil {
		return misconfigured("the OAuth resource is not a valid RFC 8707 resource identifier: " + err.Error())
	}

	if cfg.OAuthStore == nil {
		return misconfigured("the OAuth AS has an issuer configured but no OAuthStore wired")
	}
	// FIX 2: the CIMD fetcher is folded into the enabled predicate. A nil
	// fetcher is MISCONFIGURED, never a conditional downgrade — the metadata
	// handler runs only in the enabled verdict, so an AS that cannot fetch
	// CIMD can never advertise client_id_metadata_document_supported.
	if cfg.OAuthCIMDFetcher == nil {
		return misconfigured("the OAuth AS has an issuer configured but no OAuthCIMDFetcher wired")
	}

	// LOOPBACK GATE (last, default-off, #2441). It fires only when the operator
	// set --oauth-require-loopback; an unresolvable listen host UNDER the gate is
	// misconfigured (fail closed), never silently enabled. Reuses the SAME
	// mcpLoopbackHost predicate and DNS seam as the /mcp route.
	if cfg.OAuthASRequireLoopback {
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			return misconfigured("the OAuth AS loopback gate cannot parse the listen address " + strconv.Quote(cfg.Addr) + ": " + err.Error())
		}
		if _, ok, err := mcpLoopbackHost(host, lookupIP); err != nil {
			return misconfigured("the OAuth AS loopback gate cannot classify the listen address: " + err.Error())
		} else if !ok {
			return oauthASState{mode: oauthASNotLoopback, listenHost: host}
		}
	}

	return oauthASState{
		mode:                  oauthASEnabled,
		issuer:                issuer,
		resource:              resource,
		authorizationEndpoint: issuer.String() + "/v0/oauth/authorize",
		tokenEndpoint:         issuer.String() + "/v0/oauth/token",
		codeTTL:               codeTTL,
		accessTokenTTL:        accessTTL,
		refreshTokenTTL:       refreshTTL,
	}
}

// oauthASMetadata is the RFC 8414 authorization-server metadata document. Every
// field is an unconditional struct literal in the enabled verdict — neither
// CIMD gate field is computed or conditional (see handleOAuthASMetadata).
type oauthASMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	ScopesSupported                            []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
}

// handleOAuthASMetadata answers GET /.well-known/oauth-authorization-server. It
// is unauthenticated and Cache-Control: no-store. In the enabled verdict it
// renders the COMPLETE RFC 8414 field set; a disabled or misconfigured AS
// returns 503 with NO partial document — fail closed, and in particular never a
// document from a fetcher-less server.
func (s *Server) handleOAuthASMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}
	st := s.oauthAS
	doc := oauthASMetadata{
		Issuer:                        st.issuer.String(),
		AuthorizationEndpoint:         st.authorizationEndpoint,
		TokenEndpoint:                 st.tokenEndpoint,
		ResponseTypesSupported:        []string{"code"},
		GrantTypesSupported:           []string{"authorization_code", "refresh_token"},
		ScopesSupported:               append([]string(nil), oauthas.SupportedScopes...),
		CodeChallengeMethodsSupported: []string{"S256"},
		AuthorizationResponseISSParameterSupported: true,
		ClientIDMetadataDocumentSupported:          true,
		TokenEndpointAuthMethodsSupported:          []string{"none"},
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusOK, doc)
}

// oauthASEnabled writes the 503 oauth_as_unconfigured envelope and returns
// false when the AS is not enabled. Every OAuth route calls it first, so a
// disabled or misconfigured AS answers all four patterns identically and never
// leaks a partial document.
func (s *Server) oauthASEnabled(w http.ResponseWriter, r *http.Request) bool {
	switch s.oauthAS.mode {
	case oauthASEnabled:
		return true
	case oauthASNotLoopback:
		s.writeError(w, r, http.StatusForbidden, "oauth_as_loopback_only",
			"the OAuth 2.1 authorization server is loopback-only on this deployment (--oauth-require-loopback) and this daemon listens on "+
				strconv.Quote(s.oauthAS.listenHost)+", which is reachable off-host; set FISHHAWKD_ADDR=127.0.0.1:8080, or clear --oauth-require-loopback to serve it on a public bind",
			map[string]any{"listen_host": s.oauthAS.listenHost})
		return false
	case oauthASMisconfigured:
		s.writeError(w, r, http.StatusServiceUnavailable, "oauth_as_unconfigured",
			"the OAuth 2.1 authorization server is misconfigured on this deployment: "+s.oauthAS.reason, nil)
		return false
	default:
		s.writeError(w, r, http.StatusServiceUnavailable, "oauth_as_unconfigured",
			"the OAuth 2.1 authorization server is not enabled on this deployment (no --oauth-issuer configured)", nil)
		return false
	}
}

// resolvedOAuthClient is the projection of a client registration, resolved from
// the store or a CIMD document. It carries only the authority-bearing
// capability fields; per-row bookkeeping (id, account_id, timestamps) is
// deliberately dropped.
type resolvedOAuthClient struct {
	RedirectURIs            []string
	GrantTypes              []string
	ResponseTypes           []string
	TokenEndpointAuthMethod string
	ClientName              string
	ClientURI               string
	LogoURI                 string
	Scope                   string
}

func resolvedFromStore(c *oauthstore.Client) resolvedOAuthClient {
	return resolvedOAuthClient{
		RedirectURIs:            c.RedirectURIs,
		GrantTypes:              c.GrantTypes,
		ResponseTypes:           c.ResponseTypes,
		TokenEndpointAuthMethod: c.TokenEndpointAuthMethod,
		ClientName:              c.ClientName,
		ClientURI:               c.ClientURI,
		LogoURI:                 c.LogoURI,
		Scope:                   c.Scope,
	}
}

func resolvedFromCIMD(m *oauthas.ClientMetadata) resolvedOAuthClient {
	return resolvedOAuthClient{
		RedirectURIs:            m.RedirectURIs,
		GrantTypes:              m.GrantTypes,
		ResponseTypes:           m.ResponseTypes,
		TokenEndpointAuthMethod: m.TokenEndpointAuthMethod,
		ClientName:              m.ClientName,
		ClientURI:               m.ClientURI,
		LogoURI:                 m.LogoURI,
		Scope:                   m.Scope,
	}
}

// resolveOAuthClient resolves a client_id STORE-FIRST, then falls through to
// CIMD only on a store miss (FIX 1). It returns an *oauthas.Error the caller
// renders directly:
//
//   - a store hit is used as-is, and the CIMD fetcher is NEVER consulted;
//   - a non-ErrNotFound store error aborts with temporarily_unavailable ("we
//     could not look" is never "no such client");
//   - ErrNotFound validates the client_id as a CIMD URL and fetches it.
//
// ONE store read, because oauth_clients is keyed on client_id ALONE (migration
// 0064 / #2437). The #2436 interim workaround this replaces looped over a fixed
// provider set and failed closed on divergent duplicates; that guard is not
// relaxed, it is UNREACHABLE — the database no longer permits two rows to share
// a client_id, and TestSchema_OAuthClientsKeyedOnClientIDAlone is what holds
// that claim up.
//
// Deliberately NO UpsertClient on this hot path: persisting a CIMD-derived row
// would make the store branch shadow every later refresh, converting the
// fetcher's bounded TTL into a permanent pin.
//
// It takes the whole *http.Request (not just its context) so the source key for
// the CIMD limiter is derived by the ONE oauthCIMDSourceKey helper inside the
// resolver, rather than by each route — the seam that lets both routes share the
// guard through a single call site.
func (s *Server) resolveOAuthClient(r *http.Request, clientID string) (*resolvedOAuthClient, *oauthas.Error) {
	ctx := r.Context()
	c, err := s.cfg.OAuthStore.GetClientByID(ctx, clientID)
	switch {
	case err == nil:
		resolved := resolvedFromStore(c)
		return &resolved, nil
	case errors.Is(err, oauthstore.ErrNotFound):
		// Fall through to CIMD.
	default:
		return nil, &oauthas.Error{
			Code:        oauthas.ErrCodeTemporarilyUnavailable,
			Description: "the client registration store is temporarily unavailable",
			Cause:       err,
		}
	}

	// Store miss: this is the ONLY branch that can induce an outbound CIMD
	// fetch, so the rate limiter is consulted HERE and only here (#2441). A
	// store-resolved client_id short-circuited above and was never throttled —
	// throttling the pre-registered flow would degrade it for no benefit, since
	// it dials nothing. HONEST NARROWING: the guard sits on the CIMD RESOLUTION
	// branch, a superset of real outbound fetches — a client_id already warm in
	// the fetcher's LRU costs a token though it dials nothing. Peeking the cache
	// first would need a new exported oauthas seam and open a check-then-fetch
	// window; erring toward throttling is the safe direction for a control whose
	// purpose is bounding outbound VOLUME, and the burst default is sized so a
	// legitimate re-authorising client never notices. The wait is carried as a
	// TYPED cimdRateLimitedError in Cause (binding CONDITION 2), so the renderer
	// reads Retry-After structurally instead of re-consulting the limiter.
	if ok, wait := s.oauthCIMDLimiter.Allow(oauthCIMDSourceKey(r)); !ok {
		return nil, &oauthas.Error{
			Code:        oauthas.ErrCodeTemporarilyUnavailable,
			Description: "client resolution is temporarily rate limited; retry after the interval in Retry-After",
			Cause:       &cimdRateLimitedError{retryAfter: wait},
		}
	}

	// Validate as a CIMD client_id URL and fetch. Every failure renders in place
	// as invalid_client.
	if _, err := oauthas.ValidateClientIDURL(clientID); err != nil {
		return nil, asInvalidClient(err)
	}
	doc, fetchErr := s.cfg.OAuthCIMDFetcher.Fetch(ctx, clientID)
	if fetchErr != nil {
		return nil, asInvalidClient(fetchErr)
	}
	resolved := resolvedFromCIMD(doc)
	return &resolved, nil
}

// asInvalidClient coerces a CIMD fetch/validation failure into an
// invalid_client *oauthas.Error, preserving the safe description and chaining
// the original cause for logging.
func asInvalidClient(err error) *oauthas.Error {
	var oe *oauthas.Error
	if errors.As(err, &oe) {
		return &oauthas.Error{Code: oauthas.ErrCodeInvalidClient, Description: oe.Description, Cause: err}
	}
	return &oauthas.Error{Code: oauthas.ErrCodeInvalidClient, Description: "client_id could not be resolved", Cause: err}
}

// registeredResponseTypes returns the client's registered response_types,
// defaulting an ABSENT set to the RFC 7591 default ["code"] rather than an
// empty set (which would lock out a client that omits the field — the
// portless-redirect landmine, CONDITION 2).
func registeredResponseTypes(c *resolvedOAuthClient) []string {
	if len(c.ResponseTypes) == 0 {
		return []string{"code"}
	}
	return c.ResponseTypes
}

// registeredGrantTypes returns the client's registered grant_types, defaulting
// an ABSENT set to the RFC 7591 default ["authorization_code"].
func registeredGrantTypes(c *resolvedOAuthClient) []string {
	if len(c.GrantTypes) == 0 {
		return []string{"authorization_code"}
	}
	return c.GrantTypes
}

// registeredScopeSet splits the client's registered scope string on ASCII
// spaces (RFC 6749 §3.3 / RFC 7591). An ABSENT (empty) registered scope returns
// nil — meaning NO restriction, distinct from an empty set — so the caller
// enforces a restriction only when len(set) > 0. The docs promise this exact
// distinction (backend/internal/server/README.md); it is what keeps a
// scope-omitting client (e.g. the Claude Code CIMD document) unrestricted while
// a client that DID pin a narrow scope is bounded to it.
func registeredScopeSet(c *resolvedOAuthClient) []string {
	return strings.Fields(c.Scope)
}

func containsOAuth(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// firstNonEmptyOAuth returns the first non-empty entry in ss, or "". Used to
// inspect ALL values of a possibly-duplicated request header/field rather than
// only the first (which http.Header.Get / url.Values.Get would return) — a
// duplicate whose first value is empty must not mask a later credential.
func firstNonEmptyOAuth(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// anyNonEmptyOAuth reports whether ss holds any non-empty entry. Same
// duplicate-value defense as firstNonEmptyOAuth, for a presence-only check.
func anyNonEmptyOAuth(ss []string) bool {
	return firstNonEmptyOAuth(ss) != ""
}

// oauthErrorBody is the RFC 6749 §5.2 error envelope — a FLAT object, distinct
// from the server's fishhawk errorEnvelope, because OAuth clients parse this
// exact shape.
type oauthErrorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// writeOAuthError emits the RFC 6749 §5.2 JSON envelope with the no-store cache
// headers. Used by the token endpoint and by the two in-place authorize
// branches (unresolvable client, redirect-URI mismatch).
func (s *Server) writeOAuthError(w http.ResponseWriter, r *http.Request, status int, code oauthas.ErrorCode, description string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.writeJSON(w, r, status, oauthErrorBody{Error: string(code), ErrorDescription: description})
}

// redirectOAuthError builds the RFC 6749 §4.1.2.1 error redirect and ALWAYS
// attaches error, error_description, the byte-exact echoed state (when one was
// supplied), and iss (the RFC 9207 §2 requirement once the AS advertises
// authorization_response_iss_parameter_supported). EVERY redirected authorize
// error funnels through this ONE function, so no branch can forget state or iss.
func (s *Server) redirectOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state string, oerr *oauthas.Error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// A redirect URI that already matched the registered set should parse;
		// if it somehow does not, fail closed in place rather than 302 to a
		// broken target.
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeInvalidRequest,
			"internal error building the error redirect")
		return
	}
	q := u.Query()
	q.Set("error", string(oerr.Code))
	if oerr.Description != "" {
		q.Set("error_description", oerr.Description)
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.oauthAS.issuer.String())
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}
