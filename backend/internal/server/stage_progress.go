package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// progressLastEventMaxLen bounds the persisted last_event so an unbounded or
// nonsensical runner payload can never bloat the stage row. Measured in runes,
// clamped at ingest.
const progressLastEventMaxLen = 128

// stageProgressStore is the CONSUMER-SIDE narrow capability the progress ingest
// and stage-read handlers assert for on s.cfg.RunRepo (the runCostRecorder /
// runnerKindResolver precedent). It is deliberately NOT part of run.Repository
// — that would fan the three progress-only methods out across the 23
// Repository implementations. A RunRepo that does not satisfy it answers 503
// progress_unsupported rather than panicking. It mirrors run.StageProgressStore
// method-for-method.
type stageProgressStore interface {
	RecordStageProgress(ctx context.Context, stageID uuid.UUID, p run.StageProgress) (bool, error)
	StageProgressByID(ctx context.Context, stageID uuid.UUID) (*run.StageProgress, error)
	StageProgressForRun(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]run.StageProgress, error)
}

// stageProgressReport is the request body of the ingest endpoint. reported_at
// is NOT accepted from the client — it is stamped server-side from the request
// clock, so the runner cannot backdate or forge a heartbeat's time.
type stageProgressReport struct {
	LastEvent         string `json:"last_event"`
	TurnsThisAttempt  int    `json:"turns_this_attempt"`
	TokensThisAttempt int    `json:"tokens_this_attempt"`
}

// handleReportStageProgress implements
// POST /v0/runs/{run_id}/stages/{stage_id}/progress (#2541).
//
// It projects the runner's mid-execution stage_progress heartbeat onto the
// stage row, last-writer-wins, so an operator poll returns last_event / turns /
// tokens instead of a single 'running' bit. Registered at the SAME memberWrite
// tier and identical path shape as the sibling host-dispatch route, so the
// run-bound fhm_ bearer the runner already holds admits it and the auth-change
// impact inventory is empty.
//
// The terminal refusal is the UPDATE's own WHERE-state predicate: a heartbeat
// that arrives after the stage settled matches zero rows (applied=false) and is
// answered 409 stage_terminal, with no prior read and so no check-then-write
// window (#2536). Fail-open by design at the far end — the reporter must never
// be able to fail a stage — but the ingest endpoint itself validates its
// inputs and returns precise 4xx codes.
func (s *Server) handleReportStageProgress(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "progress_unsupported",
			"progress endpoint requires a configured run repository", nil)
		return
	}
	store, ok := s.cfg.RunRepo.(stageProgressStore)
	if !ok {
		s.writeError(w, r, http.StatusServiceUnavailable, "progress_unsupported",
			"configured run repository does not support stage progress", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body stageProgressReport
	if err := dec.Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {last_event, turns_this_attempt, tokens_this_attempt}",
			map[string]any{"error": err.Error()})
		return
	}
	if body.TurnsThisAttempt < 0 || body.TokensThisAttempt < 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"turns_this_attempt and tokens_this_attempt must be non-negative",
			map[string]any{"turns_this_attempt": body.TurnsThisAttempt, "tokens_this_attempt": body.TokensThisAttempt})
		return
	}

	// Verify the (run_id, stage_id) handle: a stage whose run_id differs from
	// the path does not exist AT THIS PATH → 404.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist", map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	p := run.StageProgress{
		LastEvent:         clampRunes(body.LastEvent, progressLastEventMaxLen),
		TurnsThisAttempt:  body.TurnsThisAttempt,
		TokensThisAttempt: body.TokensThisAttempt,
		ReportedAt:        time.Now().UTC(),
	}
	applied, err := store.RecordStageProgress(r.Context(), stageID, p)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"record stage progress failed", map[string]any{"error": err.Error()})
		return
	}
	if !applied {
		s.writeError(w, r, http.StatusConflict, "stage_terminal",
			"stage is terminal; progress is no longer accepted",
			map[string]any{"stage_id": stageID.String(), "state": string(stage.State)})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clampRunes truncates s to at most max runes, preserving UTF-8 boundaries.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max])
}
