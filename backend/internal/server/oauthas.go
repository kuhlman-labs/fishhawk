package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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
}

// resolveOAuthASState computes the AS verdict from cfg. It NEVER panics on a
// bad value — a defective issuer/resource is carried as the misconfigured
// verdict and rendered per request, so New() cannot crash on operator config.
func resolveOAuthASState(cfg Config) oauthASState {
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

// oauthASProviders is the fixed-order supported-provider set the store-first
// client resolver iterates. It is tied to migration 0063's
// oauth_clients_provider_check CHECK constraint (provider IN ('github',
// 'gitlab')); a future provider lands here and in that constraint together.
//
// THE PROVIDER WART: oauth_clients is keyed UNIQUE (provider, client_id), but an
// OAuth client registration describes the MCP client, which is not an identity
// provider and has no forge — so the discriminator is meaningless here. This
// helper iterates the set and fails closed on divergent duplicates until the
// operator follow-up (drop `provider` from the uniqueness key, filed at the
// #2436 plan gate) lands. See backend/internal/server/README.md.
var oauthASProviders = []string{"github", "gitlab"}

// errAmbiguousClientRegistration marks a client_id registered under more than
// one provider with DIVERGENT metadata — an operator misconfiguration that
// fails closed (rendered invalid_client in place) rather than silently
// picking one row or falling through to CIMD.
var errAmbiguousClientRegistration = errors.New("oauth client_id registered under multiple providers with divergent metadata")

// resolvedOAuthClient is the provider-neutral projection of a client
// registration, resolved from the store or a CIMD document. It carries only the
// authority-bearing capability fields; per-row bookkeeping (id, provider,
// account_id, timestamps) is deliberately dropped.
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

// sameProjection reports whether two resolved registrations project identically
// across every authority-bearing field. Two identical rows are one client
// registered twice; any divergence is the ambiguity that fails closed.
func sameProjection(a, b resolvedOAuthClient) bool {
	return oauthStringsEqual(a.RedirectURIs, b.RedirectURIs) &&
		oauthStringsEqual(a.GrantTypes, b.GrantTypes) &&
		oauthStringsEqual(a.ResponseTypes, b.ResponseTypes) &&
		a.TokenEndpointAuthMethod == b.TokenEndpointAuthMethod &&
		a.ClientName == b.ClientName &&
		a.ClientURI == b.ClientURI &&
		a.LogoURI == b.LogoURI &&
		a.Scope == b.Scope
}

func oauthStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveOAuthClient resolves a client_id STORE-FIRST across the fixed provider
// set, then falls through to CIMD only on zero store hits (FIX 1). It returns an
// *oauthas.Error the caller renders directly:
//
//   - a non-ErrNotFound store error from ANY provider aborts with
//     temporarily_unavailable ("we could not look" is never "no such client");
//   - exactly one store hit is used as-is, and the CIMD fetcher is NEVER
//     consulted;
//   - two or more identical hits resolve to the first; any divergence returns
//     invalid_client wrapping errAmbiguousClientRegistration and does NOT fall
//     through to CIMD;
//   - zero store hits validate the client_id as a CIMD URL and fetch it.
//
// Deliberately NO UpsertClient on this hot path: persisting a CIMD-derived row
// would make the store branch shadow every later refresh, converting the
// fetcher's bounded TTL into a permanent pin.
func (s *Server) resolveOAuthClient(ctx context.Context, clientID string) (*resolvedOAuthClient, *oauthas.Error) {
	var hits []resolvedOAuthClient
	for _, p := range oauthASProviders {
		c, err := s.cfg.OAuthStore.GetClient(ctx, p, clientID)
		if err != nil {
			if errors.Is(err, oauthstore.ErrNotFound) {
				continue
			}
			return nil, &oauthas.Error{
				Code:        oauthas.ErrCodeTemporarilyUnavailable,
				Description: "the client registration store is temporarily unavailable",
				Cause:       err,
			}
		}
		hits = append(hits, resolvedFromStore(c))
	}

	switch len(hits) {
	case 1:
		c := hits[0]
		return &c, nil
	case 0:
		// fall through to CIMD.
	default:
		for i := 1; i < len(hits); i++ {
			if !sameProjection(hits[0], hits[i]) {
				return nil, &oauthas.Error{
					Code:        oauthas.ErrCodeInvalidClient,
					Description: "client_id is registered under multiple providers (github, gitlab) with divergent metadata; refusing to guess",
					Cause:       errAmbiguousClientRegistration,
				}
			}
		}
		c := hits[0]
		return &c, nil
	}

	// Zero store hits: validate as a CIMD client_id URL and fetch. Every
	// failure renders in place as invalid_client.
	if _, err := oauthas.ValidateClientIDURL(clientID); err != nil {
		return nil, asInvalidClient(err)
	}
	doc, err := s.cfg.OAuthCIMDFetcher.Fetch(ctx, clientID)
	if err != nil {
		return nil, asInvalidClient(err)
	}
	c := resolvedFromCIMD(doc)
	return &c, nil
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

func containsOAuth(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
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
