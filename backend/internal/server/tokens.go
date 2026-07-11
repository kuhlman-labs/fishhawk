package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// oauthProvider is the identity provider an OAuth-minted token records.
// v0 has exactly one (GitHub); the value is stamped on the minted row
// (auth_method='oauth', provider) and echoed in the discovery response.
const oauthProvider = "github"

// tokenLoginDiscoveryResponse is the 200 body for GET /v0/tokens/login.
// It advertises the OAuth client_id the CLI drives the device flow with
// and the provider that flow authenticates against.
type tokenLoginDiscoveryResponse struct {
	Provider string `json:"provider"`
	ClientID string `json:"client_id"`
}

// tokenLoginRequest is the POST /v0/tokens/login body: the GitHub user
// access token the CLI obtained by driving the device flow itself, plus
// the scopes to mint. An empty scopes list defaults to the operator
// default set (mirroring `fishhawkd token issue`); any explicitly listed
// scope must fall within that set.
//
// Provider is accepted (the CLI sends it, and the request decoder rejects
// unknown fields) but is not the source of truth: v0 has exactly one
// provider and the backend authoritatively stamps oauthProvider on the
// minted row. When present it must name the supported provider — a
// mismatch is refused rather than silently minting a github token.
type tokenLoginRequest struct {
	AccessToken string   `json:"access_token"`
	Scopes      []string `json:"scopes"`
	Provider    string   `json:"provider"`
}

// handleTokenLoginDiscovery implements GET /v0/tokens/login. It returns
// the configured OAuth client_id so the CLI (`fishhawk token login`) can
// drive the GitHub device flow (E39.3 / #1708). 503 tokens_unconfigured
// when no OAuth client_id is wired — the same config gate that selects
// the deny-by-default NoOp IdentityProvider, so a backend without OAuth
// can neither advertise a client_id nor mint an OAuth token.
func (s *Server) handleTokenLoginDiscovery(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OAuthClientID == "" {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"OAuth token login requires an OAuth client_id to be configured (FISHHAWKD_OAUTH_CLIENT_ID)", nil)
		return
	}
	s.writeJSON(w, r, http.StatusOK, tokenLoginDiscoveryResponse{
		Provider: oauthProvider,
		ClientID: s.cfg.OAuthClientID,
	})
}

// handleTokenLoginMint implements POST /v0/tokens/login. It verifies the
// posted GitHub user access token to a provider-qualified subject
// server-side, enforces mint authz — the subject must hold at least the
// configured minimum permission on the operator repo AND request only
// scopes within the operator default set — then mints an OAuth token
// (auth_method='oauth', provider='github') via the APITokenRepo's
// OAuthIssuer capability. This is the "server-side re-verify" mint half
// of the CLI-driven device flow (E39.3 / #1708).
//
// Fail-closed config gates (each 503 tokens_unconfigured): a nil
// APITokenRepo, a repo that does not implement OAuthIssuer, an
// unconfigured OperatorRepo, or an unconfigured identity provider
// (VerifyAccessToken returns ErrNotConfigured on the NoOp). The mint is
// deliberately UNAUTHENTICATED at the bearer layer — the access token in
// the body IS the credential the backend re-verifies.
func (s *Server) handleTokenLoginMint(w http.ResponseWriter, r *http.Request) {
	if s.cfg.APITokenRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"token endpoint requires APITokenRepo to be configured", nil)
		return
	}
	issuer, ok := s.cfg.APITokenRepo.(apitoken.OAuthIssuer)
	if !ok {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"configured APITokenRepo does not support OAuth token minting", nil)
		return
	}
	if s.cfg.OperatorRepo == "" {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"OAuth token login requires an operator repo to be configured (FISHHAWKD_OPERATOR_REPO)", nil)
		return
	}

	var req tokenLoginRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if req.AccessToken == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"access_token is required", map[string]any{"field": "access_token"})
		return
	}
	if req.Provider != "" && req.Provider != oauthProvider {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"unsupported provider", map[string]any{"field": "provider", "got": req.Provider, "want": oauthProvider})
		return
	}

	// Server-side re-verify: turn the CLI-obtained access token into a
	// verified provider-qualified subject. Never trust a client-supplied
	// login. ErrNotConfigured (the NoOp provider) degrades to 503; any
	// other failure means the presented token is bad → 401, not a mint.
	subject, err := s.cfg.IdentityProvider.VerifyAccessToken(r.Context(), req.AccessToken)
	if err != nil {
		if errors.Is(err, identity.ErrNotConfigured) {
			s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
				"OAuth token login requires an identity provider to be configured", nil)
			return
		}
		s.writeError(w, r, http.StatusUnauthorized, "token_verification_failed",
			"the presented access token could not be verified", nil)
		return
	}

	// Mint authz: the verified subject must hold at least the configured
	// minimum permission on the operator repo.
	perm, err := s.cfg.IdentityProvider.PermissionLevel(r.Context(), s.cfg.OperatorRepo, subject)
	if err != nil {
		// Log the wrapped cause server-side so the underlying failure mode
		// (401 anonymous read, rate-limit, network) is visible in fishhawkd
		// logs, not only in the response details (E39.10 / #1753).
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "token-login permission check failed",
			slog.String("error", err.Error()),
			slog.String("repo", s.cfg.OperatorRepo))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"permission check failed", map[string]any{"error": err.Error()})
		return
	}
	if !perm.AtLeast(s.cfg.OperatorMinPermission) {
		s.writeError(w, r, http.StatusForbidden, "insufficient_permission",
			"subject does not hold the minimum repository permission required to mint a token",
			map[string]any{
				"subject":             subject,
				"repo":                s.cfg.OperatorRepo,
				"permission":          string(perm),
				"required_permission": string(s.cfg.OperatorMinPermission),
			})
		return
	}

	// Scope rejection: an explicit scopes list may not exceed the operator
	// default set; an empty list defaults to that set (mirroring
	// `fishhawkd token issue`).
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = append([]string(nil), s.cfg.OperatorDefaultScopes...)
	} else if bad := scopesOutside(scopes, s.cfg.OperatorDefaultScopes); bad != "" {
		s.writeError(w, r, http.StatusForbidden, "scope_not_allowed",
			"requested scope is outside the operator default set",
			map[string]any{"scope": bad})
		return
	}

	tok, err := issuer.IssueOAuth(r.Context(), subject, scopes, oauthProvider)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"issue token failed", map[string]any{"error": err.Error()})
		return
	}

	// The actor is the verified subject itself — an OAuth login mints a
	// token for the human who just authenticated.
	s.logTokenEvent(r, "api_token_issued", tok, Identity{Subject: subject})

	s.writeJSON(w, r, http.StatusCreated, apiTokenCreatedResponse{
		apiTokenResponse: toTokenResponse(tok),
		Token:            tok.PlainText,
	})
}

// scopesOutside returns the first requested scope not present in the
// allow set, or "" when every requested scope is allowed. It is
// deny-by-default: an empty allow set admits no explicit scope, so the
// first requested scope is reported as outside. This matches the rest of
// the mint handler's fail-closed gates — a deployment that wires OAuth but
// leaves OperatorDefaultScopes empty cannot mint an arbitrary explicit
// scope. serve.go always wires a non-empty operator default set, so the
// production path never relies on this posture.
func scopesOutside(requested, allow []string) string {
	allowed := make(map[string]bool, len(allow))
	for _, s := range allow {
		allowed[s] = true
	}
	for _, s := range requested {
		if !allowed[s] {
			return s
		}
	}
	return ""
}

// apiTokenResponse mirrors the OpenAPI `ApiToken` schema. The
// plaintext is sent ONCE on POST (via apiTokenCreatedResponse) and
// never appears in any List/Get response.
type apiTokenResponse struct {
	ID         string     `json:"id"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	// AuthMethod / Provider surface how the token was authenticated at
	// issue time (E39.3 / #1708): "static" for operator-minted tokens,
	// "oauth" + a provider (e.g. "github") for device-flow tokens. Both
	// omitempty so a legacy row predating the columns (empty) projects
	// byte-identically to before, and a static token omits Provider.
	AuthMethod string `json:"auth_method,omitempty"`
	Provider   string `json:"provider,omitempty"`
	// Subject is the provider-qualified identity the token is bound to
	// (e.g. "github:octocat"), the verified subject stamped at issue time.
	// Surfacing it on the mint 201 lets `fishhawk token login` / `token
	// list` show the operator which identity they authenticated as (#1755).
	// omitempty preserves byte-identical projection for a legacy row whose
	// subject is empty.
	Subject string `json:"subject,omitempty"`
}

// apiTokenCreatedResponse is the 201 body for POST /v0/tokens. The
// `token` field is the plaintext bearer string the caller stores;
// it's not retrievable later.
type apiTokenCreatedResponse struct {
	apiTokenResponse
	Token string `json:"token"`
}

func toTokenResponse(t *apitoken.Token) apiTokenResponse {
	return apiTokenResponse{
		ID:         t.ID.String(),
		Scopes:     t.Scopes,
		LastUsedAt: t.LastUsedAt,
		CreatedAt:  t.CreatedAt,
		RevokedAt:  t.RevokedAt,
		AuthMethod: t.AuthMethod,
		Provider:   t.Provider,
		Subject:    t.Subject,
	}
}

// handleCreateToken implements POST /v0/tokens. Mints a new token
// for the authenticated user and returns the plaintext exactly once.
//
// AUTH: requires non-anonymous Identity. Bootstrap (the very first
// token for an installation) is issued via the `fishhawkd token
// issue` CLI command, which calls APITokenRepo directly without
// going through this handler.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.APITokenRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"token endpoint requires APITokenRepo to be configured", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"creating a token requires an authenticated session or an existing bearer token", nil)
		return
	}

	var req struct {
		Scopes []string `json:"scopes"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	tok, err := s.cfg.APITokenRepo.Issue(r.Context(), id.Subject, req.Scopes)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"issue token failed", map[string]any{"error": err.Error()})
		return
	}

	s.logTokenEvent(r, "api_token_issued", tok, id)

	s.writeJSON(w, r, http.StatusCreated, apiTokenCreatedResponse{
		apiTokenResponse: toTokenResponse(tok),
		Token:            tok.PlainText,
	})
}

// handleListTokens implements GET /v0/tokens. Returns the active
// tokens belonging to the authenticated user. Plaintext is never
// included.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if s.cfg.APITokenRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"token endpoint requires APITokenRepo to be configured", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"listing tokens requires an authenticated session or bearer token", nil)
		return
	}

	tokens, err := s.cfg.APITokenRepo.ListForSubject(r.Context(), id.Subject)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list tokens failed", map[string]any{"error": err.Error()})
		return
	}
	items := make([]apiTokenResponse, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, toTokenResponse(t))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"items": items})
}

// handleRevokeToken implements DELETE /v0/tokens/{token_id}.
// Idempotent: a second revoke on the same token returns 204 with no
// state change. The repository's ownership check produces 403 if
// the caller doesn't own the token.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.APITokenRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "tokens_unconfigured",
			"token endpoint requires APITokenRepo to be configured", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "auth_required",
			"revoking a token requires an authenticated session or bearer token", nil)
		return
	}

	tokenID, err := uuid.Parse(r.PathValue("token_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"token_id must be a valid UUID",
			map[string]any{"field": "token_id", "got": r.PathValue("token_id")})
		return
	}

	tok, err := s.cfg.APITokenRepo.Revoke(r.Context(), tokenID, id.Subject)
	if err != nil {
		switch {
		case errors.Is(err, apitoken.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "token_not_found",
				"no token with that id", map[string]any{"token_id": tokenID.String()})
		case errors.Is(err, apitoken.ErrForbidden):
			s.writeError(w, r, http.StatusForbidden, "token_forbidden",
				"caller does not own this token", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"revoke token failed", map[string]any{"error": err.Error()})
		}
		return
	}

	s.logTokenEvent(r, "api_token_revoked", tok, id)
	w.WriteHeader(http.StatusNoContent)
}

// logTokenEvent emits both a structured slog line and a chained
// audit entry on the global chain (E2.7) for token issuance /
// revocation. The plaintext is NEVER included — only the token id,
// subject, and scopes.
//
// Audit append failures log a warning but don't unwind the token
// state change: by this point the row is already created or
// revoked. A missing audit row is a regression signal, not a
// reason to keep the caller from completing the request.
func (s *Server) logTokenEvent(r *http.Request, event string, tok *apitoken.Token, actor Identity) {
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, event,
		slog.String("token_id", tok.ID.String()),
		slog.String("subject", tok.Subject),
		slog.Any("scopes", tok.Scopes),
		slog.String("actor", actor.Subject),
		slog.String("actor_token_id", actor.TokenID),
		slog.String("request_id", RequestIDFrom(r.Context())),
	)
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"token_id":       tok.ID.String(),
		"subject":        tok.Subject,
		"scopes":         tok.Scopes,
		"actor":          actor.Subject,
		"actor_token_id": actor.TokenID,
		"request_id":     RequestIDFrom(r.Context()),
	})
	actorKind := audit.ActorUser
	actorSubject := actor.Subject
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     event,
		ActorKind:    &actorKind,
		ActorSubject: &actorSubject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"audit append failed for token event",
			slog.String("event", event),
			slog.String("token_id", tok.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}
