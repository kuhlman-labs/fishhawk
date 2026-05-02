package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// maxTraceBundleBytes mirrors the runner's bundle.MaxBundleBytes so
// the runner and backend agree on the gzipped payload ceiling. v0
// trace volumes are bounded by token budgets; this is the
// belt-and-suspenders cap that protects backend storage from a
// runaway agent.
const maxTraceBundleBytes = 64 * 1024 * 1024

// traceUploadResponse is the 202 body in docs/api/v0.openapi.yaml
// for `POST /v0/runs/{run_id}/trace`. Returns the (run, stage,
// variant, content_hash) tuple so the runner can confirm the
// backend stored the same bytes it sent and stash the hash for
// later cross-reference.
type traceUploadResponse struct {
	RunID       uuid.UUID `json:"run_id"`
	StageID     uuid.UUID `json:"stage_id"`
	Variant     string    `json:"variant"`
	ContentHash string    `json:"content_hash"`
}

// handleShipTrace implements POST /v0/runs/{run_id}/trace.
//
// Auth is the Ed25519 signature itself: the runner produced the
// signature with the per-run private key (issued by the
// signing-key endpoint); the backend looks up the stored public
// half and verifies. A forged or expired signature → 401, no
// audit log entry, no bundle stored.
//
// On success the handler writes a kind=trace_uploaded audit
// entry via AppendChained — the chain hash links the upload event
// to the run's prior audit history, so a tampered or replayed
// trace can't slip into an unrelated run's chain.
func (s *Server) handleShipTrace(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.TraceStore == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "trace_upload_unconfigured",
			"trace upload requires signing, tracestore, and audit to be configured", nil)
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

	variant := tracestore.Variant(r.URL.Query().Get("variant"))
	if !variant.Valid() {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"variant must be raw or redacted",
			map[string]any{"field": "variant", "got": string(variant)})
		return
	}

	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxTraceBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxTraceBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"trace bundle exceeds size cap",
			map[string]any{"limit_bytes": maxTraceBundleBytes})
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

	contentHash := sha256Hex(body)
	ref := tracestore.BundleRef{
		RunID:       runID,
		Variant:     variant,
		ContentHash: contentHash,
	}
	if err := s.cfg.TraceStore.Put(r.Context(), ref, bytes.NewReader(body)); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"store trace bundle failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit: append a chained entry tying the upload to this run's
	// prior history. AppendChained holds a row-lock on runs so two
	// concurrent uploads can't fork the chain.
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":       runID.String(),
		"stage_id":     stageID.String(),
		"variant":      string(variant),
		"content_hash": contentHash,
		"size_bytes":   len(body),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		// Bundle is already stored; failing here would leave us
		// with a stored bundle and no audit record. We surface
		// 500 so the runner retries (idempotent at the storage
		// layer) and the audit row eventually lands.
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
		RunID:       runID,
		StageID:     stageID,
		Variant:     string(variant),
		ContentHash: contentHash,
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
