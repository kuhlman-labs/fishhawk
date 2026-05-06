package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxInstallationTokenRequestBytes caps the body of the
// installation-token request. Body is empty or `{}` in v0; this is
// just a safety net for malformed clients.
const maxInstallationTokenRequestBytes = 1024

// handleIssueInstallationToken implements
// POST /v0/runs/{run_id}/installation-token?stage_id=…
//
// Returns the App's installation token for the run's repo so the
// runner can `git push` and open a PR via the App's identity rather
// than the workflow's GITHUB_TOKEN. Lets customers install the App
// once and skip the "Allow Actions to create PRs" repo-level toggle
// (E5.X / #197).
//
// Auth: per-run Ed25519 signature in `X-Fishhawk-Signature`, same
// shape as /trace, /plan, /pull-request. The signed body is the raw
// request bytes; v0 sends `{}` (or empty), keeping the URL path's
// run_id as the scoping mechanism.
func (s *Server) handleIssueInstallationToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.RunRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.GitHubTokens == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "installation_token_unconfigured",
			"installation-token endpoint requires signing, run, audit, and github-app token provider", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}

	// Stage must exist and belong to the run; the run must have an
	// installation_id (otherwise there's no App on the repo to mint
	// a token for). We need the run row anyway to read its
	// installation_id, so we pull both up front.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist",
			map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "run_not_found",
			"run does not exist", map[string]any{"run_id": runID.String()})
		return
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.writeError(w, r, http.StatusBadRequest, "no_installation_for_run",
			"run has no GitHub App installation; this run was created without a webhook-attributed installation",
			map[string]any{"run_id": runID.String()})
		return
	}

	// Dual auth (#201): the runner's runtime call signs with the
	// per-run Ed25519 key (X-Fishhawk-Signature). The pre-checkout
	// auth action presents a GitHub Actions OIDC token via
	// `Authorization: Bearer <jwt>` — the same scheme the
	// signing-key endpoint uses. We pick the path based on which
	// header is present; OIDC wins when both are supplied.
	authMethod := "ed25519"
	bearerHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(bearerHeader, "Bearer ") {
		// OIDC path. verifyOIDC handles 401/404/503 responses
		// internally and returns false on any failure.
		if !s.verifyOIDC(w, r, runID) {
			return
		}
		authMethod = "oidc"
	} else {
		sigHeader := r.Header.Get("X-Fishhawk-Signature")
		if sigHeader == "" {
			s.writeError(w, r, http.StatusUnauthorized, "auth_required",
				"either X-Fishhawk-Signature (Ed25519) or Authorization: Bearer <oidc-jwt> required", nil)
			return
		}
		signature, err := hex.DecodeString(sigHeader)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"X-Fishhawk-Signature is not valid hex",
				map[string]any{"error": err.Error()})
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxInstallationTokenRequestBytes+1))
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"could not read request body", map[string]any{"error": err.Error()})
			return
		}
		if len(body) > maxInstallationTokenRequestBytes {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
				"request body exceeds size cap",
				map[string]any{"limit_bytes": maxInstallationTokenRequestBytes})
			return
		}

		message := signing.ComputeMessage(body)
		if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
			switch {
			case errors.Is(err, signing.ErrNotFound):
				s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
					"no signing key issued for this run", map[string]any{"run_id": runID.String()})
			case errors.Is(err, signing.ErrExpired):
				s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
					"signing key TTL has passed", map[string]any{"run_id": runID.String()})
			case errors.Is(err, signing.ErrSignatureInvalid):
				s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
					"signature does not match the run's stored public key", nil)
			default:
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"signature verification failed", map[string]any{"error": err.Error()})
			}
			return
		}
	}

	token, err := s.cfg.GitHubTokens.Token(r.Context(), *runRow.InstallationID)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "installation_token_issuance_failed",
			"GitHub rejected the App JWT or the installation no longer exists",
			map[string]any{"error": err.Error(), "installation_id": *runRow.InstallationID})
		return
	}

	// Audit. We log a SHA-256 of the token rather than the token
	// itself so the audit chain doesn't carry a secret. Auditors
	// can confirm "this token's hash was issued at this time" by
	// hashing whatever the runner shipped to GitHub.
	tokenHash := sha256.Sum256([]byte(token))
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":          runID.String(),
		"stage_id":        stageID.String(),
		"installation_id": *runRow.InstallationID,
		"token_sha256":    hex.EncodeToString(tokenHash[:]),
		"auth_method":     authMethod,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "installation_token_issued",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, installationTokenResponse{
		Token: token,
	})
}

// installationTokenResponse is intentionally minimal: just the
// token. v0 doesn't surface expires_at (would require a wider
// TokenProvider interface; runners use the token immediately and
// don't cache, so wall-clock guidance isn't actionable). v1+ can
// extend if a use case appears.
type installationTokenResponse struct {
	Token string `json:"token"`
}
