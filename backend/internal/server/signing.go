package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

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

// handleIssueSigningKey implements POST /v0/runs/{run_id}/signing-key.
//
// AUTH NOTE: per the OpenAPI contract this endpoint requires a
// GitHub Actions OIDC token. v0 self-execution ships without OIDC
// verification — the security boundary is currently "the run_id is
// a UUIDv7 the caller had to learn from the dispatch path." Proper
// OIDC verification (JWKS fetch, JWT verify, claim binding to
// repository + workflow) is tracked separately; until it lands,
// callers can sign for any run_id they know.
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

	// Body is optional. An empty / missing body uses the default
	// TTL; a present body must parse cleanly.
	ttl := signing.DefaultTTL
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
		switch {
		case errors.Is(err, signing.ErrAlreadyIssued):
			s.writeError(w, r, http.StatusConflict, "signing_key_already_issued",
				"a signing key has already been issued for this run",
				map[string]any{"run_id": runID.String()})
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"issue signing key failed", map[string]any{"error": err.Error()})
		}
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
