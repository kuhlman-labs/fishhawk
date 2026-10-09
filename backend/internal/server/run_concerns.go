package server

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Concern-listing state filter values for GET /v0/runs/{run_id}/concerns
// (#4101). open is the default: the rows an operator collects ids from for
// waive / defer.
const (
	runConcernsStateOpen = "open"
	runConcernsStateAll  = "all"
)

// runConcernListItem is one concern on the run-concerns listing (#4101). It
// carries the identifying and triage fields plus the bounded short_summary
// label — never the full reviewer note, which stays on the gate view. RunID is
// the OWNING run; ChildRunID and SliceIndex are set only on a decomposition
// child's row so a caller can tell a child's concern from the parent's without
// comparing ids.
type runConcernListItem struct {
	ID                   string `json:"id"`
	RunID                string `json:"run_id"`
	ChildRunID           string `json:"child_run_id,omitempty"`
	SliceIndex           *int   `json:"slice_index,omitempty"`
	StageID              string `json:"stage_id"`
	StageKind            string `json:"stage_kind"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	State                string `json:"state"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	ReviewerRole         string `json:"reviewer_role,omitempty"`
	ShortSummary         string `json:"short_summary,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
	HasSuggestedPatch    bool   `json:"has_suggested_patch"`
	Provenance           string `json:"provenance,omitempty"`
}

// runConcernListChild summarizes one decomposition child on an
// include_children=true listing: its id, slice index, run state and how many
// items it contributed under the requested state filter.
type runConcernListChild struct {
	RunID      string `json:"run_id"`
	SliceIndex *int   `json:"slice_index,omitempty"`
	State      string `json:"state"`
	ItemCount  int    `json:"item_count"`
}

// runConcernListResponse is the GET /v0/runs/{run_id}/concerns body (#4101).
// Items is never nil, so an empty listing marshals as []. Children is a
// pointer so it is ABSENT when include_children=false and a non-nil [] when
// include_children=true on a run with no decomposition children — presence
// tells the caller the children were actually read.
type runConcernListResponse struct {
	RunID           string                 `json:"run_id"`
	State           string                 `json:"state"`
	IncludeChildren bool                   `json:"include_children"`
	Count           int                    `json:"count"`
	Items           []runConcernListItem   `json:"items"`
	Children        *[]runConcernListChild `json:"children,omitempty"`
}

// sortDecomposedChildren orders decomposition children deterministically:
// SliceIndex ascending with a nil index last, then CreatedAt ascending, then
// ID string. It is the ONE ordering shared by every surface that walks a
// parent's children (the child-scope-amendment provenance and the
// run-concerns listing), so both render children in the same order.
func sortDecomposedChildren(children []*run.Run) {
	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]
		ai, bi := a.SliceIndex, b.SliceIndex
		switch {
		case ai != nil && bi != nil:
			if *ai != *bi {
				return *ai < *bi
			}
		case ai != nil && bi == nil:
			return true // known slice index sorts before an unknown one
		case ai == nil && bi != nil:
			return false
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID.String() < b.ID.String()
	})
}

// handleListRunConcerns implements GET /v0/runs/{run_id}/concerns (#4101): a
// run's review concerns and, with include_children=true, those of its
// decomposition children, so an operator collecting concern ids across a
// fan-out needs one call instead of a gate-view read per child.
//
// Children are discriminated by DecomposedFrom (listAllDecomposedChildren),
// NEVER ParentRunID, which also links recovery children.
//
// Authorization mirrors handleGetRunGateView (the reviewer-prose read
// posture): an mcp:run:<uuid> token may read only its own run and may not
// use include_children (children are other runs); every other caller needs
// scopeGateViewRead. Every store read fails CLOSED with 500 — including one
// child's — because a silently partial list would read as "no open concerns
// on that child", the wrong signal for an operator collecting ids.
func (s *Server) handleListRunConcerns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConcernRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"concern listing requires configured concern and run repositories", nil)
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
	stateFilter := q.Get("state")
	switch stateFilter {
	case "":
		stateFilter = runConcernsStateOpen
	case runConcernsStateOpen, runConcernsStateAll:
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"state must be 'open' or 'all' when set",
			map[string]any{"field": "state", "got": stateFilter})
		return
	}
	includeChildren := false
	if raw := q.Get("include_children"); raw != "" {
		v, perr := strconv.ParseBool(raw)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"include_children must be a boolean when set",
				map[string]any{"field": "include_children", "got": raw})
			return
		}
		includeChildren = v
	}

	// Read authorization, mirroring handleGetRunGateView: a run-bound token is
	// authorized by the cross-run subject guard alone, and it stays bounded to
	// its OWN run — include_children would read other runs, so it is refused.
	id := IdentityFrom(r.Context())
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_concerns",
				"mcp token may only list the concerns of its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"path_run_id":  runID.String(),
				})
			return
		}
		if includeChildren {
			s.writeError(w, r, http.StatusForbidden, "cross_run_concerns",
				"mcp token may not list decomposition children's concerns: children are other runs",
				map[string]any{
					"token_run_id":     subjectRunID.String(),
					"include_children": true,
				})
			return
		}
	} else if !s.requireWriteScope(w, r, scopeGateViewRead) {
		return
	}

	parent, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	rows, err := s.cfg.ConcernRepo.ListByRun(r.Context(), parent.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list concerns failed", map[string]any{"error": err.Error()})
		return
	}
	resp := runConcernListResponse{
		RunID:           parent.ID.String(),
		State:           stateFilter,
		IncludeChildren: includeChildren,
		Items:           []runConcernListItem{},
	}
	resp.Items = appendRunConcernItems(resp.Items, rows, stateFilter, nil)

	if includeChildren {
		children, cerr := s.listAllDecomposedChildren(r.Context(), parent.ID)
		if cerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list decomposition children failed", map[string]any{"error": cerr.Error()})
			return
		}
		sortDecomposedChildren(children)
		summaries := make([]runConcernListChild, 0, len(children))
		for _, child := range children {
			childRows, lerr := s.cfg.ConcernRepo.ListByRun(r.Context(), child.ID)
			if lerr != nil {
				// Fail closed: a partial list would read as "no concerns on
				// this child" to an operator collecting ids. The child id is
				// in the MESSAGE because the 5xx detail redactor keeps only
				// allow-listed keys (errors.go); details.child_run_id still
				// reaches the server log under the same error_ref.
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"list concerns failed for decomposition child run "+child.ID.String(),
					map[string]any{"child_run_id": child.ID.String(), "error": lerr.Error()})
				return
			}
			before := len(resp.Items)
			resp.Items = appendRunConcernItems(resp.Items, childRows, stateFilter, child)
			summaries = append(summaries, runConcernListChild{
				RunID:      child.ID.String(),
				SliceIndex: copyIntPtr(child.SliceIndex),
				State:      string(child.State),
				ItemCount:  len(resp.Items) - before,
			})
		}
		resp.Children = &summaries
	}
	resp.Count = len(resp.Items)
	s.writeJSON(w, r, http.StatusOK, resp)
}

// appendRunConcernItems appends rows matching stateFilter to items, in the
// store's order. child is nil for the parent's own rows; for a decomposition
// child's rows it stamps child_run_id and slice_index.
func appendRunConcernItems(items []runConcernListItem, rows []*concern.Concern, stateFilter string, child *run.Run) []runConcernListItem {
	for _, c := range rows {
		if stateFilter == runConcernsStateOpen && !c.State.IsOpen() {
			continue
		}
		item := runConcernListItem{
			ID:                   c.ID.String(),
			RunID:                c.RunID.String(),
			StageID:              c.StageID.String(),
			StageKind:            c.StageKind,
			Severity:             c.Severity,
			Category:             c.Category,
			State:                string(c.State),
			ReviewerModel:        derefStr(c.ReviewerModel),
			ReviewerRole:         c.ReviewerRole,
			ShortSummary:         concernShortSummary(c.DisplayNote()),
			OriginReviewSequence: c.OriginReviewSequence,
			HasSuggestedPatch:    c.SuggestedPatch != "",
			Provenance:           c.Provenance,
		}
		if child != nil {
			item.ChildRunID = child.ID.String()
			item.SliceIndex = copyIntPtr(child.SliceIndex)
		}
		items = append(items, item)
	}
	return items
}

// copyIntPtr returns a fresh copy of p so a response never aliases a run row.
func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
