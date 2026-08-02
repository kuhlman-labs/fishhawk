package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
)

// POST /v0/oauth/token (ADR-076 slice 3, #2436): the authorization_code and
// refresh_token grants for a public (`none`-auth) client.
//
// FIX 3 / CONDITION on client auth: the handler refuses ANY Authorization
// header before doing anything else, and reads every parameter from r.PostForm
// ONLY — never r.Form, which merges the URL query. So client_id has EXACTLY ONE
// source (the body's client_id field) and there is no second source it could
// diverge from. The handler also NEVER reads IdentityFrom(ctx): a session cookie
// on this endpoint is inert.

// handleOAuthToken serves POST /v0/oauth/token.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}

	// FIRST, before ParseForm and before any store call: no credential of any
	// kind belongs on a `none`-auth endpoint, so refuse the WHOLE Authorization
	// header (not just Basic) — RFC 6749 §2.3.1 puts client credentials there.
	// §5.2 requires a 401 with a WWW-Authenticate matching the attempted scheme.
	// Inspect EVERY value, not just Header.Get's first: a duplicated header whose
	// first value is empty ("" then "Basic …") would otherwise slip a later
	// credential past Get and defeat this public-client-only refusal.
	if authz := firstNonEmptyOAuth(r.Header.Values("Authorization")); authz != "" {
		scheme := "Bearer"
		if fields := strings.Fields(authz); len(fields) > 0 {
			scheme = fields[0]
		}
		w.Header().Set("WWW-Authenticate", scheme+` realm="fishhawk"`)
		s.writeOAuthError(w, r, http.StatusUnauthorized, oauthas.ErrCodeInvalidClient,
			"this endpoint authenticates public clients only (token_endpoint_auth_method=none); no Authorization header is accepted")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeInvalidRequest,
			"could not parse the token request body")
		return
	}

	// client_secret on a public-client endpoint is refused outright — there is
	// no secret to present. Check EVERY value, not just PostForm.Get's first:
	// client_secret=&client_secret=secret presents an empty first value that Get
	// would return, slipping a real secret past this refusal.
	if anyNonEmptyOAuth(r.PostForm["client_secret"]) {
		s.writeOAuthError(w, r, http.StatusUnauthorized, oauthas.ErrCodeInvalidClient,
			"this endpoint authenticates public clients only; a client_secret is not accepted")
		return
	}

	grantType := r.PostForm.Get("grant_type")
	switch grantType {
	case "authorization_code", "refresh_token":
		// supported
	default:
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeUnsupportedGrantType,
			"grant_type must be authorization_code or refresh_token")
		return
	}

	clientID := r.PostForm.Get("client_id")
	// The shared renderer maps a CIMD limiter refusal to 429 + Retry-After and a
	// store outage to 503; a plain invalid_client stays 401 on this endpoint.
	client, cerr := s.resolveOAuthClient(r, clientID)
	if cerr != nil {
		s.writeOAuthClientError(w, r, http.StatusUnauthorized, cerr)
		return
	}
	// Public client only.
	if client.TokenEndpointAuthMethod != "none" {
		s.writeOAuthError(w, r, http.StatusUnauthorized, oauthas.ErrCodeInvalidClient,
			"the client is not registered as a public (token_endpoint_auth_method=none) client")
		return
	}
	// CONDITION 2: the requested grant must be in the client's registered set
	// (an absent set defaults to ["authorization_code"] per RFC 7591).
	if !containsOAuth(registeredGrantTypes(client), grantType) {
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeUnauthorizedClient,
			"the client is not registered for grant_type="+grantType)
		return
	}

	switch grantType {
	case "authorization_code":
		s.tokenAuthorizationCode(w, r, clientID)
	case "refresh_token":
		s.tokenRefresh(w, r, clientID)
	}
}

// tokenAuthorizationCode handles grant_type=authorization_code. The verify
// callback runs BEFORE the code is consumed (RedeemAuthorizationCode's
// contract): a verify error rolls the transaction back and leaves the code
// redeemable, so a wrong parameter never burns the legitimate client's code.
func (s *Server) tokenAuthorizationCode(w http.ResponseWriter, r *http.Request, clientID string) {
	code := r.PostForm.Get("code")
	redirectURI := r.PostForm.Get("redirect_uri")
	verifier := r.PostForm.Get("code_verifier")
	resource := r.PostForm.Get("resource")

	now := s.nowFunc()
	grant, err := s.cfg.OAuthStore.RedeemAuthorizationCode(r.Context(), code, func(c *oauthstore.AuthorizationCode) error {
		if c.ClientID != clientID {
			return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "authorization code was not issued to this client"}
		}
		if c.RedirectURI != redirectURI {
			return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "redirect_uri does not match the authorization request"}
		}
		if err := oauthas.VerifyPKCE(c.CodeChallengeMethod, c.CodeChallenge, verifier); err != nil {
			return err
		}
		if resource != "" && !oauthas.ResourceMatches(c.Resource, resource) {
			return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "requested resource does not match the authorization code's audience"}
		}
		return nil
	}, oauthstore.RedemptionRequest{
		AccessTokenExpiry:  now.Add(s.oauthAS.accessTokenTTL),
		RefreshTokenExpiry: now.Add(s.oauthAS.refreshTokenTTL),
	})
	if err != nil {
		// RFC 6749 §4.1.2 replay response: a SECOND presentation of an
		// already-redeemed code revokes every token descended from it. The
		// store surfaces this as ErrCodeConsumed; the handler drives the
		// lineage sweep (RedeemAuthorizationCode does not, by contract).
		if errors.Is(err, oauthstore.ErrCodeConsumed) {
			// Log rather than discard the lookup/revoke errors: a real-store no-op
			// (the sweep silently failing while the response still says
			// invalid_grant, leaving the replayed code's tokens live) is otherwise
			// invisible — no log, and per-layer unit fakes cannot see the seam.
			if c, lerr := s.cfg.OAuthStore.LookupAuthorizationCode(r.Context(), code); lerr != nil {
				s.cfg.Logger.Error("replay lineage sweep: look up consumed authorization code",
					"error", lerr.Error())
			} else if _, rerr := s.cfg.OAuthStore.RevokeGrantsForCode(r.Context(), c.ID); rerr != nil {
				s.cfg.Logger.Error("replay lineage sweep: revoke grants for replayed code",
					"error", rerr.Error(), "code_id", c.ID.String())
			}
		}
		// Every classification (consumed, expired, not found, revoked,
		// malformed) AND every verify error collapse to ONE opaque
		// invalid_grant so the response does not oracle a credential's state.
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeInvalidGrant,
			"the authorization code is invalid, expired, or already used")
		return
	}
	s.writeTokenResponse(w, r, grant)
}

// tokenRefresh handles grant_type=refresh_token. The verify callback enforces
// the RFC 6749 §6 client binding plus an optional resource match; oauthstore's
// revoked→reused→expired classification and its committed lineage sweep are
// consumed as-is and collapsed to one opaque invalid_grant.
func (s *Server) tokenRefresh(w http.ResponseWriter, r *http.Request, clientID string) {
	refreshToken := r.PostForm.Get("refresh_token")
	resource := r.PostForm.Get("resource")

	now := s.nowFunc()
	grant, err := s.cfg.OAuthStore.RotateRefreshToken(r.Context(), refreshToken, func(t *oauthstore.RefreshToken) error {
		if t.ClientID != clientID {
			return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "refresh token was not issued to this client"}
		}
		if resource != "" && !oauthas.ResourceMatches(t.Audience, resource) {
			return &oauthas.Error{Code: oauthas.ErrCodeInvalidGrant, Description: "requested resource does not match the refresh token's audience"}
		}
		return nil
	}, oauthstore.RotationRequest{
		AccessTokenExpiry:  now.Add(s.oauthAS.accessTokenTTL),
		RefreshTokenExpiry: now.Add(s.oauthAS.refreshTokenTTL),
	})
	if err != nil {
		s.writeOAuthError(w, r, http.StatusBadRequest, oauthas.ErrCodeInvalidGrant,
			"the refresh token is invalid, expired, revoked, or already used")
		return
	}
	s.writeTokenResponse(w, r, grant)
}

// oauthTokenResponse is the RFC 6749 §5.1 successful token response. scope comes
// from the DERIVED token row, never echoed from the request.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

func (s *Server) writeTokenResponse(w http.ResponseWriter, r *http.Request, grant *oauthstore.IssuedGrant) {
	now := s.nowFunc()
	expiresIn := int64(grant.Access.ExpiresAt.Sub(now).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	resp := oauthTokenResponse{
		AccessToken: grant.Access.PlainText,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
		Scope:       oauthas.ScopeString(grant.Access.Scopes),
	}
	if grant.Refresh != nil {
		resp.RefreshToken = grant.Refresh.PlainText
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.writeJSON(w, r, http.StatusOK, resp)
}
