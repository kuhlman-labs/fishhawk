package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// signingKeyResponse mirrors the 201 body in
// docs/api/v0.openapi.yaml for `POST /v0/runs/{id}/signing-key`.
// The private key is returned exactly once and is never persisted
// server-side; clients must capture it on this response.
type signingKeyResponse struct {
	RunID      uuid.UUID `json:"run_id"`
	PublicKey  string    `json:"public_key"`  // base64-encoded 32-byte ed25519 public
	PrivateKey string    `json:"private_key"` // base64-encoded 64-byte ed25519 private
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// signingKeyRequest mirrors the OpenAPI request body. Both the body
// and ttl_seconds are optional; when omitted the handler falls back
// to signing.DefaultTTL.
type signingKeyRequest struct {
	TTLSeconds *int `json:"ttl_seconds,omitempty"`
}

// minTTLSeconds and maxTTLSeconds bracket the request-side cap per
// the OpenAPI schema. The signing layer accepts any positive ttl;
// the handler is the gate against silly values from clients.
const (
	minTTLSeconds = 60
	maxTTLSeconds = 3600
)

// signingKeyTTLBuffer is added to the resolved stage budget when
// computing the server-side signing key TTL, so a stage that runs
// its full budget still has a valid key when it uploads its trace.
const signingKeyTTLBuffer = 5 * time.Minute

// resolveSigningKeyTTL returns the TTL to use when issuing a signing
// key for runID. It resolves the stage via the active-or-next rule
// (first dispatched/running stage, else first non-terminal — the
// #1030 local-runner first-stage fallback, so a run whose first
// stage is a still-pending implement stage resolves that stage's
// budget instead of the default), then resolves that stage's budget
// via resolveAgentTimeout and returns max(DefaultTTL, budget +
// buffer). Falls back to DefaultTTL when RunRepo is unconfigured,
// the run cannot be fetched, the spec is absent, or every stage is
// terminal.
func (s *Server) resolveSigningKeyTTL(ctx context.Context, runID uuid.UUID) time.Duration {
	if s.cfg.RunRepo == nil {
		return signing.DefaultTTL
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || runRow.WorkflowSpec == nil {
		return signing.DefaultTTL
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return signing.DefaultTTL
	}
	activeStage := activeOrNextStage(stages)
	if activeStage == nil {
		return signing.DefaultTTL
	}
	budgetSeconds := s.resolveAgentTimeout(ctx, runRow, activeStage.Type)
	if budgetSeconds == 0 {
		return signing.DefaultTTL
	}
	candidate := time.Duration(budgetSeconds)*time.Second + signingKeyTTLBuffer
	if candidate > signing.DefaultTTL {
		return candidate
	}
	return signing.DefaultTTL
}

// handleIssueSigningKey implements POST /v0/runs/{run_id}/signing-key.
//
// AUTH: per the OpenAPI contract this endpoint requires a GitHub
// Actions OIDC token (E3.10). The configured Verifier validates
// the JWT signature against GitHub's JWKS and binds the token's
// `repository` + `workflow` claims to the path's run_id. When no
// Verifier is wired (cfg.OIDCVerifier == nil) the endpoint falls
// back to "the run_id is a UUIDv7 the caller had to learn from
// the dispatch path" — the v0 self-execution posture. Operators
// flip OIDC on for any non-toy deploy.
func (s *Server) handleIssueSigningKey(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "signing_repo_unconfigured",
			"signing-key endpoint requires a configured repository", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	if !s.verifyOIDC(w, r, runID) {
		return
	}

	// Body is optional. An empty / missing body uses the spec-resolved
	// TTL (max of DefaultTTL and the active stage budget + buffer);
	// a present body must parse cleanly.
	ttl := s.resolveSigningKeyTTL(r.Context(), runID)
	if r.ContentLength != 0 {
		var req signingKeyRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body is not valid JSON or contains unknown fields",
				map[string]any{"error": err.Error()})
			return
		}
		if req.TTLSeconds != nil {
			if *req.TTLSeconds < minTTLSeconds || *req.TTLSeconds > maxTTLSeconds {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"ttl_seconds out of range",
					map[string]any{"field": "ttl_seconds", "min": minTTLSeconds, "max": maxTTLSeconds, "got": *req.TTLSeconds})
				return
			}
			ttl = time.Duration(*req.TTLSeconds) * time.Second
		}
	}

	issued, err := s.cfg.SigningRepo.Issue(r.Context(), runID, ttl)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"issue signing key failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, signingKeyResponse{
		RunID:      issued.RunID,
		PublicKey:  base64.StdEncoding.EncodeToString(issued.PublicKey),
		PrivateKey: base64.StdEncoding.EncodeToString(issued.PrivateKey),
		IssuedAt:   issued.IssuedAt,
		ExpiresAt:  issued.ExpiresAt,
	})
}

// verifyOIDC validates the Authorization: Bearer <jwt> header
// against the configured Verifier. Binds the token's repository +
// workflow claims to the path's run_id by looking up the run.
//
// Returns true on success. On failure writes the appropriate 401
// or 503 response and returns false so the caller short-circuits.
//
// When cfg.OIDCVerifier is nil the check is skipped — that's the
// v0 self-execution posture where the only auth is "the run_id is
// hard to guess." Operators wiring OIDC also wire RunRepo so the
// claim-binding lookup is available.
func (s *Server) verifyOIDC(w http.ResponseWriter, r *http.Request, runID uuid.UUID) bool {
	if s.cfg.OIDCVerifier == nil {
		return true
	}
	if s.cfg.OIDCAudience == "" {
		s.writeError(w, r, http.StatusServiceUnavailable, "oidc_misconfigured",
			"OIDCVerifier set without OIDCAudience", nil)
		return false
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "oidc_misconfigured",
			"OIDC verification requires RunRepo to be configured for claim binding", nil)
		return false
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "oidc_missing",
			"Authorization: Bearer <github-oidc-token> required", nil)
		return false
	}
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		s.writeError(w, r, http.StatusUnauthorized, "oidc_invalid",
			"Authorization header must use Bearer scheme", nil)
		return false
	}
	rawToken := strings.TrimPrefix(authHeader, bearerPrefix)

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return false
	}

	exp := githuboidc.Expectations{
		Audience:   s.cfg.OIDCAudience,
		Repository: runRow.Repo,
		Workflow:   runRow.WorkflowID,
		AllowedEvents: []string{
			"issues",
			"issue_comment",
			"workflow_dispatch",
			"pull_request",
		},
	}
	if _, err := s.cfg.OIDCVerifier.Verify(r.Context(), rawToken, exp); err != nil {
		switch {
		case errors.Is(err, githuboidc.ErrTokenExpired):
			s.writeError(w, r, http.StatusUnauthorized, "oidc_invalid",
				"OIDC token expired or not yet valid",
				map[string]any{"error": err.Error()})
		case errors.Is(err, githuboidc.ErrUnknownKID):
			s.writeError(w, r, http.StatusUnauthorized, "oidc_invalid",
				"OIDC token signed by unknown key",
				map[string]any{"error": err.Error()})
		case errors.Is(err, githuboidc.ErrClaimMismatch):
			s.writeError(w, r, http.StatusUnauthorized, "oidc_invalid",
				"OIDC token claims don't bind to this run",
				map[string]any{"error": err.Error()})
		default:
			s.writeError(w, r, http.StatusUnauthorized, "oidc_invalid",
				"OIDC token verification failed",
				map[string]any{"error": err.Error()})
		}
		return false
	}
	return true
}
