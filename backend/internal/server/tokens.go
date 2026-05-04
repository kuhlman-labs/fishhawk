package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
)

// apiTokenResponse mirrors the OpenAPI `ApiToken` schema. The
// plaintext is sent ONCE on POST (via apiTokenCreatedResponse) and
// never appears in any List/Get response.
type apiTokenResponse struct {
	ID         string     `json:"id"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
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

// logTokenEvent emits a structured slog line for token issuance /
// revocation. The plaintext is NEVER included — only the token id,
// subject, and scopes.
//
// We don't write to the audit log because audit_entries enforces
// run_id NOT NULL (chained per-run integrity); token events aren't
// tied to a run. A future "global audit chain" feature can land
// these as auditable rows; until then, structured logs are the
// compliance trail. Tracked separately.
func (s *Server) logTokenEvent(r *http.Request, event string, tok *apitoken.Token, actor Identity) {
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, event,
		slog.String("token_id", tok.ID.String()),
		slog.String("subject", tok.Subject),
		slog.Any("scopes", tok.Scopes),
		slog.String("actor", actor.Subject),
		slog.String("actor_token_id", actor.TokenID),
		slog.String("request_id", RequestIDFrom(r.Context())),
	)
}
