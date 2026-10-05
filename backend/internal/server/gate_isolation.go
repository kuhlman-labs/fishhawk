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

// CategoryGateIsolationRecorded is the stage-scoped audit row recording which
// ADR-063 gate isolation path a stage's gates ran under (E51.2 / #2135). It is
// appended at RAW trace upload from the bundle's gate_evidence.gate_isolation
// member — the runner's own recorded selection, flattened and pre-redacted —
// and distilled by the gate view into its gate_isolation block. INTERNAL: no
// issue-comment surface reads it.
const CategoryGateIsolationRecorded = "gate_isolation_recorded"

// gateViewGapGateIsolation is the history_gaps entry the gate-isolation block
// records when its read or decode fails.
const gateViewGapGateIsolation = CategoryGateIsolationRecorded

// gateIsolationRecordedPayload is the gate_isolation_recorded audit payload:
// the stage + bundle coordinates plus the flat evidence member, embedded so
// its fields sit at the payload's top level.
type gateIsolationRecordedPayload struct {
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	bundle.GateIsolationEvidence
}

// recordGateIsolation appends one gate_isolation_recorded row for a raw trace
// bundle that carries a gate_evidence.gate_isolation member. The trace handler
// calls it inside its raw-variant guard BEFORE the budget short-circuits and
// the agent-failed branch, so a refused or red gate is on the chain before the
// stage is failed.
//
// Best-effort throughout — the bundle is already stored and audited:
//   - no AuditRepo, no gate_evidence (an older runner, or a stage where no gate
//     reached the runner's exec seam), an unparsable gate_evidence, or a
//     gate_evidence without the member → no row (an unparsable one WARN-logs);
//   - a row already recorded for the same (stage_id, content_hash) → no row, so
//     a re-POST of the same raw bundle does not duplicate the record;
//   - a failed dedup read WARN-logs and still appends (a duplicate is
//     harmless to the gate view; a lost record is not);
//   - a failed append WARN-logs.
//
// The dedup is read-then-append, so two CONCURRENT re-POSTs of one bundle can
// still both append; the gate view is newest-wins and its worst-class scan is
// idempotent over duplicates, so that residual changes nothing it renders.
func (s *Server) recordGateIsolation(ctx context.Context, runID, stageID uuid.UUID, contentHash string, bundleBytes []byte) {
	if s.cfg.AuditRepo == nil {
		return
	}
	ev, err := bundle.ExtractGateEvidence(bundleBytes)
	if err != nil {
		if !errors.Is(err, bundle.ErrNoGateEvidence) {
			s.cfg.Logger.Warn("gate isolation: extract gate evidence failed; not recording gate_isolation_recorded",
				"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		}
		return
	}
	if ev.GateIsolation == nil {
		return
	}
	if s.gateIsolationAlreadyRecorded(ctx, runID, stageID, contentHash) {
		return
	}
	payload, err := json.Marshal(gateIsolationRecordedPayload{
		StageID:               stageID.String(),
		ContentHash:           contentHash,
		GateIsolationEvidence: *ev.GateIsolation,
	})
	if err != nil {
		s.cfg.Logger.Warn("gate isolation: marshal payload failed",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
		return
	}
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryGateIsolationRecorded,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.Warn("gate isolation: append gate_isolation_recorded failed",
			"run_id", runID.String(), "stage_id", stageID.String(), "error", err.Error())
	}
}

// gateIsolationAlreadyRecorded reports whether a gate_isolation_recorded row
// for the same (stage_id, content_hash) is already on the run's chain. A read
// failure reports false (append anyway — see recordGateIsolation).
func (s *Server) gateIsolationAlreadyRecorded(ctx context.Context, runID, stageID uuid.UUID, contentHash string) bool {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryGateIsolationRecorded)
	if err != nil {
		s.cfg.Logger.Warn("gate isolation: dedup read failed; appending without dedup",
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

// gateViewGateIsolation is the gate-view distillation of the run's
// gate_isolation_recorded rows (#2135). The top-level fields describe the
// NEWEST row. WorstClass / WorstStageID / WorstSequence name the most severe
// class recorded on ANY stage of the run (refused > fallback > container), the
// newest row at that class winning a tie, so an earlier fallback or refusal is
// never hidden behind a later container stage; on a run whose every row is
// container they equal the newest row's.
type gateViewGateIsolation struct {
	StageID              string    `json:"stage_id"`
	Sequence             int64     `json:"sequence"`
	RecordedAt           time.Time `json:"recorded_at"`
	Path                 string    `json:"path"`
	Class                string    `json:"class"`
	Mode                 string    `json:"mode"`
	Profile              string    `json:"profile"`
	Image                string    `json:"image,omitempty"`
	RuntimeKind          string    `json:"runtime_kind"`
	RuntimeSafe          bool      `json:"runtime_safe"`
	RuntimeReason        string    `json:"runtime_reason,omitempty"`
	RuntimeVersion       string    `json:"runtime_version,omitempty"`
	SandboxAvailable     bool      `json:"sandbox_available"`
	SandboxReason        string    `json:"sandbox_reason,omitempty"`
	Reason               string    `json:"reason"`
	ContainerUnavailable string    `json:"container_unavailable,omitempty"`
	WorstClass           string    `json:"worst_class"`
	WorstStageID         string    `json:"worst_stage_id"`
	WorstSequence        int64     `json:"worst_sequence"`
}

// gateIsolationClassRank orders the recorded classes by severity for the
// worst-class scan. An unrecognized class ranks 0, below container, so it can
// only be the worst when no row carries a recognized class.
func gateIsolationClassRank(class string) int {
	switch class {
	case "refused":
		return 3
	case "fallback":
		return 2
	case "container":
		return 1
	default:
		return 0
	}
}

// gateIsolationForRun builds the gate view's gate_isolation block. It is
// run-level and ignores the stage_kind filter: isolation is a property of the
// runner process that ran a stage's gates, not of a concern.
//
// Degradation is visible and wholesale: a list error, or ANY row whose payload
// does not decode, returns nil with a gate_isolation_recorded history_gaps
// entry and history_incomplete — never a block built from a partial read,
// whose worst_class could silently miss the row that failed to decode. No
// AuditRepo, or no rows, returns nil with no gap (the key is absent).
func (s *Server) gateIsolationForRun(ctx context.Context, runID uuid.UUID, resp *gateViewResponse) *gateViewGateIsolation {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	gap := func(what string, err error) *gateViewGateIsolation {
		s.cfg.Logger.Warn("gate-view: "+what+"; omitting gate_isolation block",
			"run_id", runID.String(), "error", err.Error())
		resp.HistoryIncomplete = true
		resp.HistoryGaps = append(resp.HistoryGaps, gateViewGapGateIsolation)
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryGateIsolationRecorded)
	if err != nil {
		return gap("list gate_isolation_recorded failed", err)
	}
	if len(entries) == 0 {
		return nil
	}
	var (
		block                    *gateViewGateIsolation
		worstRank                = -1
		worstClass, worstStageID string
		worstSequence            int64
	)
	// ListForRunByCategory is sequence-ascending, so the last row is the
	// newest and a later row at an equal rank wins the worst-class tie.
	for _, e := range entries {
		var p gateIsolationRecordedPayload
		if uerr := json.Unmarshal(e.Payload, &p); uerr != nil {
			return gap("decode gate_isolation_recorded payload failed", uerr)
		}
		ev := p.GateIsolationEvidence
		if rank := gateIsolationClassRank(ev.Class); rank >= worstRank {
			worstRank = rank
			worstClass, worstStageID, worstSequence = ev.Class, p.StageID, e.Sequence
		}
		block = &gateViewGateIsolation{
			StageID:              p.StageID,
			Sequence:             e.Sequence,
			RecordedAt:           e.Timestamp,
			Path:                 ev.Path,
			Class:                ev.Class,
			Mode:                 ev.Mode,
			Profile:              ev.Profile,
			Image:                ev.Image,
			RuntimeKind:          ev.RuntimeKind,
			RuntimeSafe:          ev.RuntimeSafe,
			RuntimeReason:        ev.RuntimeReason,
			RuntimeVersion:       ev.RuntimeVersion,
			SandboxAvailable:     ev.SandboxAvailable,
			SandboxReason:        ev.SandboxReason,
			Reason:               ev.Reason,
			ContainerUnavailable: ev.ContainerUnavailable,
		}
	}
	block.WorstClass, block.WorstStageID, block.WorstSequence = worstClass, worstStageID, worstSequence
	return block
}
