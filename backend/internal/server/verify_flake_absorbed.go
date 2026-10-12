package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// CategoryVerifyFlakeAbsorbed is the stage-scoped audit row recording that the
// runner absorbed an outside-only verify flake at the verify-fix loop's
// terminal failure (#4229): the stage's verified tree passed ONE scoped re-run
// of the untouched failing packages, not the full test loop. It is appended at
// RAW trace upload from the bundle's gate_evidence.flake_absorbed member — the
// runner's own record, flattened and pre-redacted. INTERNAL: no issue-comment
// surface reads it.
const CategoryVerifyFlakeAbsorbed = "verify_flake_absorbed"

// verifyFlakeAbsorbedPayload is the verify_flake_absorbed audit payload: the
// stage + bundle coordinates plus the flat evidence member, embedded so its
// fields sit at the payload's top level.
type verifyFlakeAbsorbedPayload struct {
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	bundle.FlakeAbsorbedEvidence
}

// recordVerifyFlakeAbsorbed appends one verify_flake_absorbed row for a raw
// trace bundle whose gate_evidence carries a flake_absorbed member. The trace
// handler calls it inside its raw-variant guard, beside recordGateIsolation.
//
// Best-effort throughout, on recordGateIsolation's contract — the bundle is
// already stored and audited:
//   - no AuditRepo, no gate_evidence, or a gate_evidence without the member →
//     no row; an unparsable gate_evidence → no row and a WARN log;
//   - a row already recorded for the same (stage_id, content_hash) → no row, so
//     a re-POST of the same raw bundle does not duplicate the record;
//   - a failed dedup read WARN-logs and still appends (a duplicate is harmless,
//     a lost record is not);
//   - a failed append WARN-logs.
func (s *Server) recordVerifyFlakeAbsorbed(ctx context.Context, runID, stageID uuid.UUID, contentHash string, bundleBytes []byte) {
	if s.cfg.AuditRepo == nil {
		return
	}
	ev, err := bundle.ExtractGateEvidence(bundleBytes)
	if err != nil {
		if !errors.Is(err, bundle.ErrNoGateEvidence) {
			s.cfg.Logger.Warn("verify flake absorbed: extract gate evidence failed; not recording verify_flake_absorbed",
				"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		}
		return
	}
	if ev.FlakeAbsorbed == nil {
		return
	}
	if s.verifyFlakeAbsorbedAlreadyRecorded(ctx, runID, stageID, contentHash) {
		return
	}
	payload, err := json.Marshal(verifyFlakeAbsorbedPayload{
		StageID:               stageID.String(),
		ContentHash:           contentHash,
		FlakeAbsorbedEvidence: *ev.FlakeAbsorbed,
	})
	if err != nil {
		s.cfg.Logger.Warn("verify flake absorbed: marshal payload failed",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		return
	}
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryVerifyFlakeAbsorbed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.Warn("verify flake absorbed: append verify_flake_absorbed failed",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
	}
}

// verifyFlakeAbsorbedAlreadyRecorded reports whether a verify_flake_absorbed
// row for the same (stage_id, content_hash) is already on the run's chain. A
// read failure reports false (append anyway — see recordVerifyFlakeAbsorbed).
func (s *Server) verifyFlakeAbsorbedAlreadyRecorded(ctx context.Context, runID, stageID uuid.UUID, contentHash string) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryVerifyFlakeAbsorbed)
	if err != nil {
		s.cfg.Logger.Warn("verify flake absorbed: dedup read failed; appending without dedup",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		return false
	}
	for _, e := range entries {
		var p struct {
			StageID     string `json:"stage_id"`
			ContentHash string `json:"content_hash"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.StageID == stageID.String() && p.ContentHash == contentHash {
			return true
		}
	}
	return false
}
