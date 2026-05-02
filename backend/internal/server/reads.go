package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// stageResponse mirrors docs/api/v0.openapi.yaml's `Stage` schema.
// Field types align with the json:"" tags in run.Stage but the
// envelope is built explicitly so we never accidentally leak an
// internal representation change through to the wire format.
type stageResponse struct {
	ID              uuid.UUID     `json:"id"`
	RunID           uuid.UUID     `json:"run_id"`
	Sequence        int           `json:"sequence"`
	Type            string        `json:"type"`
	Executor        stageExecutor `json:"executor"`
	State           string        `json:"state"`
	StartedAt       *time.Time    `json:"started_at"`
	EndedAt         *time.Time    `json:"ended_at"`
	FailureCategory *string       `json:"failure_category"`
	FailureReason   *string       `json:"failure_reason"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type stageExecutor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func toStageResponse(s *run.Stage) stageResponse {
	var failureCategory *string
	if s.FailureCategory != nil {
		v := string(*s.FailureCategory)
		failureCategory = &v
	}
	return stageResponse{
		ID:              s.ID,
		RunID:           s.RunID,
		Sequence:        s.Sequence,
		Type:            string(s.Type),
		Executor:        stageExecutor{Kind: string(s.ExecutorKind), Ref: s.ExecutorRef},
		State:           string(s.State),
		StartedAt:       s.StartedAt,
		EndedAt:         s.EndedAt,
		FailureCategory: failureCategory,
		FailureReason:   s.FailureReason,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
	}
}

// handleListRunStages implements GET /v0/runs/{run_id}/stages.
// Returns stages ordered by sequence ascending; no pagination.
// MVP_SPEC §4.2 caps stages-per-workflow at a small N, so flat
// listing is fine for v0.
func (s *Server) handleListRunStages(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"runs endpoint requires a configured run repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}

	items := make([]stageResponse, 0, len(stages))
	for _, st := range stages {
		items = append(items, toStageResponse(st))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"items": items})
}

// auditEntryResponse mirrors docs/api/v0.openapi.yaml's
// `AuditEntry` schema.
type auditEntryResponse struct {
	ID           uuid.UUID       `json:"id"`
	Sequence     int64           `json:"sequence"`
	RunID        uuid.UUID       `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *string         `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
}

func toAuditEntryResponse(e *audit.Entry) auditEntryResponse {
	var actorKind *string
	if e.ActorKind != nil {
		v := string(*e.ActorKind)
		actorKind = &v
	}
	return auditEntryResponse{
		ID:           e.ID,
		Sequence:     e.Sequence,
		RunID:        e.RunID,
		StageID:      e.StageID,
		Timestamp:    e.Timestamp,
		Category:     e.Category,
		ActorKind:    actorKind,
		ActorSubject: e.ActorSubject,
		Payload:      e.Payload,
		PrevHash:     e.PrevHash,
		EntryHash:    e.EntryHash,
	}
}

// auditPagination matches the OpenAPI Pagination envelope: cursor
// is opaque to the client (we encode an offset under the hood; v0+
// migrates to keyset pagination once sequence becomes a primary
// sort key in the query).
const (
	auditDefaultLimit = 100
	auditMaxLimit     = 500
)

// handleListRunAudit implements GET /v0/runs/{run_id}/audit.
// Cursor-paginated, sequence ascending, optional category filter.
// Used by the run-detail UI and the eventual compliance export.
//
// Cursor encoding: base64("offset:<n>"). Opaque to clients per the
// OpenAPI doc; we'll change it to a keyset cursor when audit logs
// per run grow large enough that offset scans become expensive.
func (s *Server) handleListRunAudit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"audit endpoint requires a configured audit repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	q := r.URL.Query()
	category := q.Get("category")
	limit, err := parseLimit(q.Get("limit"), auditDefaultLimit, auditMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	offset, err := decodeOffsetCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid",
			err.Error(), nil)
		return
	}

	var entries []*audit.Entry
	if category != "" {
		entries, err = s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, category)
	} else {
		entries, err = s.cfg.AuditRepo.ListForRun(r.Context(), runID)
	}
	if err != nil {
		if errors.Is(err, audit.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list audit failed", map[string]any{"error": err.Error()})
		return
	}

	page, nextCursor := pageOffset(entries, offset, limit)
	items := make([]auditEntryResponse, 0, len(page))
	for _, e := range page {
		items = append(items, toAuditEntryResponse(e))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// parseLimit reads a query value with min=1, max=hardMax, returning
// def when the value is absent. Clamping with an explicit error
// keeps clients honest about the contract rather than silently
// truncating absurd asks.
func parseLimit(raw string, def, hardMax int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer; got %q", raw)
	}
	if n < 1 || n > hardMax {
		return 0, fmt.Errorf("limit must be between 1 and %d; got %d", hardMax, n)
	}
	return n, nil
}

// pageOffset slices entries[offset:offset+limit] and returns the
// page along with the next cursor (empty string when at the end).
// Pure function — separate from the handler so tests can hit it
// directly with a synthetic slice.
func pageOffset[T any](items []T, offset, limit int) ([]T, string) {
	if offset >= len(items) {
		return nil, ""
	}
	end := offset + limit
	if end >= len(items) {
		return items[offset:], ""
	}
	return items[offset:end], encodeOffsetCursor(end)
}

func encodeOffsetCursor(offset int) string {
	return base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf("offset:%d", offset)))
}

func decodeOffsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("cursor is not valid base64")
	}
	var offset int
	if _, err := fmt.Sscanf(string(raw), "offset:%d", &offset); err != nil {
		return 0, fmt.Errorf("cursor is not in expected shape")
	}
	if offset < 0 {
		return 0, fmt.Errorf("cursor offset must be non-negative")
	}
	return offset, nil
}
