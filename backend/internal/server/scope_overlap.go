package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// Cross-run scope overlap (E83.60 / #4118): which OTHER non-terminal runs in
// the same repository and tenant share files with a run's effective scope.
// Advisory on these surfaces — nothing here refuses on it, every read is
// fail-open. It is served live by GET /v0/runs/{run_id}/scope-overlap, carried
// on the gate view as scope_overlap, and recorded at plan upload as a
// plan_scope_overlap audit row (written only when an overlap exists, like
// plan_warnings). The one refusing consumer is the merge overlap guard
// (merge_overlap_guard.go, #4119), which reuses overlapCandidates and
// resolveOverlapScope and FAILS CLOSED. Contract:
// backend/internal/server/README.md § "Cross-run scope overlap" and
// § "Merge overlap guard".

// planScopeOverlapCategory is the advisory audit row handleShipPlan records
// when the settling plan's effective scope intersects another live run's.
const planScopeOverlapCategory = "plan_scope_overlap"

// scopeOverlapListLimit caps each per-state ListRuns page. A page that reaches
// it sets Truncated: some live run may have gone unscanned.
const scopeOverlapListLimit = 200

// scopeOverlapLiveStates are the run states scanned for overlap. Terminal runs
// (succeeded, failed, cancelled) are never listed, so they never report.
var scopeOverlapLiveStates = []run.State{run.StatePending, run.StateRunning}

// scopeOverlapRun is one other live run sharing files with the subject run.
// The json tags are the wire contract (mirrored by mcpserver's ScopeOverlapRun).
type scopeOverlapRun struct {
	RunID string `json:"run_id"`
	// IssueRef is the run's TriggerRef. A decomposition child inherits its
	// parent's, so a child is attributed to the parent issue.
	IssueRef       string `json:"issue_ref,omitempty"`
	DecomposedFrom string `json:"decomposed_from,omitempty"`
	// Stage / StageState name the run's current stage: the lowest-sequence
	// non-terminal stage, else its last stage. Empty when the stage read failed.
	Stage      string   `json:"stage,omitempty"`
	StageState string   `json:"stage_state,omitempty"`
	Paths      []string `json:"paths"`
}

// scopeOverlapPayload is the GET /v0/runs/{run_id}/scope-overlap body, the
// gate view's scope_overlap block and the plan_scope_overlap audit payload.
type scopeOverlapPayload struct {
	RunID string `json:"run_id"`
	// SelfScopeResolved is false when the subject run's own effective scope
	// could not be resolved (no plan yet, or a read failed); no other run is
	// listed then and Overlaps is empty.
	SelfScopeResolved bool              `json:"self_scope_resolved"`
	Overlaps          []scopeOverlapRun `json:"overlaps"`
	// ScannedRuns counts the candidate runs examined after the lineage and
	// tenant exclusions; UnscopedRuns counts those whose scope did not resolve.
	ScannedRuns  int  `json:"scanned_runs"`
	UnscopedRuns int  `json:"unscoped_runs"`
	Truncated    bool `json:"truncated"`
}

// planScopeOverlapAuditPayload is the plan_scope_overlap row: the computed
// payload plus the plan stage it was recorded against.
type planScopeOverlapAuditPayload struct {
	scopeOverlapPayload
	StageID string `json:"stage_id"`
}

// resolveOverlapScope returns a run's EFFECTIVE scope as an exact-path set: the
// newest standard_v1 plan (loadApprovedPlanForRun, parent-walk) — a
// decomposition child narrows to its own sub_plan slice, any other run takes
// the top-level scope.files UNION every sub_plan scope — plus the run-level
// approval add_scope_files, plus APPROVED scope amendments, minus the approval
// remove_scope_files. ok is false when no plan resolves or a read fails.
func (s *Server) resolveOverlapScope(ctx context.Context, runRow *run.Run) (map[string]struct{}, bool) {
	p, err := s.loadApprovedPlanForRun(ctx, runRow.ID)
	if err != nil || p == nil {
		return nil, false
	}
	return s.overlayOverlapScope(ctx, runRow, p)
}

// overlayOverlapScope builds the effective scope from an already-loaded plan:
// the slice or full-union base, then the approval and amendment overlays.
func (s *Server) overlayOverlapScope(ctx context.Context, runRow *run.Run, p *plan.Plan) (map[string]struct{}, bool) {
	set := map[string]struct{}{}
	if sub, _ := matchDecomposedSubPlan(runRow, p); sub != nil && sub.Scope != nil && len(sub.Scope.Files) > 0 {
		for _, f := range sub.Scope.Files {
			set[f.Path] = struct{}{}
		}
	} else {
		for _, f := range scopeFilesFromApprovedPlanFull(p) {
			set[f.Path] = struct{}{}
		}
	}
	for _, path := range s.loadApprovalAddScopeFiles(ctx, runRow.ID) {
		set[path] = struct{}{}
	}
	if s.cfg.ScopeAmendmentRepo != nil {
		items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runRow.ID)
		if err != nil {
			return nil, false
		}
		for _, path := range approvedAmendmentPaths(items) {
			set[path] = struct{}{}
		}
	}
	for _, path := range s.loadApprovalRemoveScopeFiles(ctx, runRow.ID) {
		delete(set, path)
	}
	return set, true
}

// approvedAmendmentPaths returns the paths of the APPROVED amendments only;
// pending, denied and lapsed requests never widen the effective scope.
func approvedAmendmentPaths(items []*scopeamendment.Amendment) []string {
	var paths []string
	for _, a := range items {
		if a.Status != scopeamendment.StatusApproved {
			continue
		}
		for _, entry := range a.Paths {
			paths = append(paths, entry.Path)
		}
	}
	return paths
}

// isOverlapLineage reports whether other is self or in self's own
// decomposition/recovery lineage: parent and child, sibling slices of one
// decomposition (governed by the within-plan checkCrossSliceSharedFiles rule
// and integrated into one consolidated PR), and a ParentRunID recovery/retry
// pair re-executing the same plan. Overlap within a lineage is not reported.
func isOverlapLineage(self, other *run.Run) bool {
	if self == nil || other == nil {
		return false
	}
	if other.ID == self.ID {
		return true
	}
	if other.DecomposedFrom != nil && *other.DecomposedFrom == self.ID {
		return true
	}
	if self.DecomposedFrom != nil && *self.DecomposedFrom == other.ID {
		return true
	}
	if self.DecomposedFrom != nil && other.DecomposedFrom != nil && *self.DecomposedFrom == *other.DecomposedFrom {
		return true
	}
	if other.ParentRunID != nil && *other.ParentRunID == self.ID {
		return true
	}
	return self.ParentRunID != nil && *self.ParentRunID == other.ID
}

// overlapRecoveredSlice resolves ONE ParentRunID hop for a recovery run of a
// decomposition slice: it returns the parent row when r carries no
// DecomposedFrom of its own and its parent is a decomposition slice, else nil.
// The caller then judges lineage against that slice too, so a slice's recovery
// child is excluded relative to the slice's decomposed parent and siblings
// (approval condition 4). A failed parent read returns nil (no hop). cache
// memoizes parent rows across one computation.
func (s *Server) overlapRecoveredSlice(ctx context.Context, r *run.Run, cache map[uuid.UUID]*run.Run) *run.Run {
	if r == nil || r.DecomposedFrom != nil || r.ParentRunID == nil {
		return nil
	}
	parent, ok := cache[*r.ParentRunID]
	if !ok {
		got, err := s.cfg.RunRepo.GetRun(ctx, *r.ParentRunID)
		if err != nil {
			got = nil
		}
		cache[*r.ParentRunID] = got
		parent = got
	}
	if parent == nil || parent.DecomposedFrom == nil {
		return nil
	}
	return parent
}

// overlapSameTenant reports whether a candidate run is visible to the subject
// run's tenant. Untenanted rows stay visible (the ListRunsFilter.AccountID
// contract); a tenanted run never sees another tenant's run, and an untenanted
// subject sees only untenanted runs.
func overlapSameTenant(selfAccount, otherAccount string) bool {
	if otherAccount == "" {
		return true
	}
	return otherAccount == selfAccount
}

// currentOverlapStage returns the lowest-Sequence stage that is not terminal,
// else the highest-Sequence stage; nil on an empty list.
func currentOverlapStage(stages []*run.Stage) *run.Stage {
	var firstLive, last *run.Stage
	for _, st := range stages {
		if st == nil {
			continue
		}
		if !st.State.IsTerminal() && (firstLive == nil || st.Sequence < firstLive.Sequence) {
			firstLive = st
		}
		if last == nil || st.Sequence > last.Sequence {
			last = st
		}
	}
	if firstLive != nil {
		return firstLive
	}
	return last
}

// computeScopeOverlap intersects self's effective scope with every other
// non-terminal run in the same repo and tenant, outside self's lineage. The
// only returned error is a ListRuns failure; every per-candidate read failure
// degrades (the candidate counts as unscoped, or its stage stays empty).
func (s *Server) computeScopeOverlap(ctx context.Context, self *run.Run) (*scopeOverlapPayload, error) {
	selfSet, ok := s.resolveOverlapScope(ctx, self)
	return s.computeScopeOverlapFrom(ctx, self, selfSet, ok)
}

// computeScopeOverlapFrom is computeScopeOverlap with self's effective scope
// already resolved (ok false = unresolved: no listing).
func (s *Server) computeScopeOverlapFrom(ctx context.Context, self *run.Run, selfSet map[string]struct{}, ok bool) (*scopeOverlapPayload, error) {
	out := &scopeOverlapPayload{RunID: self.ID.String(), Overlaps: []scopeOverlapRun{}}
	if !ok {
		return out, nil
	}
	out.SelfScopeResolved = true

	cands, truncated, err := s.overlapCandidates(ctx, self)
	if err != nil {
		return nil, err
	}
	out.Truncated = truncated
	for _, other := range cands {
		out.ScannedRuns++
		otherSet, ok := s.resolveOverlapScope(ctx, other)
		if !ok {
			out.UnscopedRuns++
			continue
		}
		paths := []string{}
		for path := range otherSet {
			if _, shared := selfSet[path]; shared {
				paths = append(paths, path)
			}
		}
		if len(paths) == 0 {
			continue
		}
		sort.Strings(paths)
		entry := scopeOverlapRun{RunID: other.ID.String(), Paths: paths}
		if other.TriggerRef != nil {
			entry.IssueRef = *other.TriggerRef
		}
		if other.DecomposedFrom != nil {
			entry.DecomposedFrom = other.DecomposedFrom.String()
		}
		if stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, other.ID); err == nil {
			if st := currentOverlapStage(stages); st != nil {
				entry.Stage = string(st.Type)
				entry.StageState = string(st.State)
			}
		}
		out.Overlaps = append(out.Overlaps, entry)
	}
	sort.Slice(out.Overlaps, func(i, j int) bool {
		if out.Overlaps[i].IssueRef != out.Overlaps[j].IssueRef {
			return out.Overlaps[i].IssueRef < out.Overlaps[j].IssueRef
		}
		return out.Overlaps[i].RunID < out.Overlaps[j].RunID
	})
	return out, nil
}

// overlapCandidates enumerates the OTHER live runs self's overlap is judged
// against: every run in scopeOverlapLiveStates in self's repo and tenant
// (ListRuns per state, Limit scopeOverlapListLimit), minus nil rows, self's
// own lineage (isOverlapLineage), the one-hop recovery-slice exclusions
// (overlapRecoveredSlice, for self and for each candidate) and other tenants
// (overlapSameTenant). truncated is true when any per-state page reached
// scopeOverlapListLimit, so some live run may have gone unlisted. The only
// error is a ListRuns failure. Shared by computeScopeOverlapFrom (advisory)
// and mergeOverlapGuard (fail-closed, #4119), so both judge the same set.
func (s *Server) overlapCandidates(ctx context.Context, self *run.Run) (cands []*run.Run, truncated bool, err error) {
	parents := map[uuid.UUID]*run.Run{}
	selfSlice := s.overlapRecoveredSlice(ctx, self, parents)
	for _, state := range scopeOverlapLiveStates {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			Repo:      self.Repo,
			State:     string(state),
			AccountID: self.AccountID,
			Limit:     scopeOverlapListLimit,
		})
		if err != nil {
			return nil, false, fmt.Errorf("list %s runs in %s: %w", state, self.Repo, err)
		}
		if len(page) >= scopeOverlapListLimit {
			truncated = true
		}
		for _, other := range page {
			if other == nil {
				continue
			}
			if isOverlapLineage(self, other) {
				continue
			}
			if selfSlice != nil && isOverlapLineage(selfSlice, other) {
				continue
			}
			if otherSlice := s.overlapRecoveredSlice(ctx, other, parents); otherSlice != nil && isOverlapLineage(self, otherSlice) {
				continue
			}
			if !overlapSameTenant(self.AccountID, other.AccountID) {
				continue
			}
			cands = append(cands, other)
		}
	}
	return cands, truncated, nil
}

// recordPlanScopeOverlap is the plan-gate leg (handleShipPlan): it computes the
// settling plan's overlap and appends ONE plan_scope_overlap row when an
// overlap exists. Fail-open on every leg — nil AuditRepo/RunRepo, GetRun error
// or a compute error WARN-logs (where there is something to say) and returns
// nil; the plan settle is never blocked. The row is the snapshot the plan saw
// when it settled; the live surfaces recompute on every read.
func (s *Server) recordPlanScopeOverlap(ctx context.Context, runID, stageID uuid.UUID) *scopeOverlapPayload {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan scope overlap: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	// The settling plan is read from the shipped stage itself rather than via
	// loadApprovedPlanForRun, so the upload path adds no ListStagesForRun.
	var selfSet map[string]struct{}
	resolved := false
	if p := s.stagePlanForOverlap(ctx, stageID); p != nil {
		selfSet, resolved = s.overlayOverlapScope(ctx, runRow, p)
	}
	result, err := s.computeScopeOverlapFrom(ctx, runRow, selfSet, resolved)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan scope overlap: compute failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if !result.SelfScopeResolved || len(result.Overlaps) == 0 {
		return result
	}
	payload, _ := json.Marshal(planScopeOverlapAuditPayload{scopeOverlapPayload: *result, StageID: stageID.String()})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  planScopeOverlapCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan scope overlap: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}

// stagePlanForOverlap returns the newest standard_v1 plan artifact on the
// stage, or nil when none is readable (the plan gate then records nothing).
func (s *Server) stagePlanForOverlap(ctx context.Context, stageID uuid.UUID) *plan.Plan {
	if s.cfg.ArtifactRepo == nil {
		return nil
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, stageID)
	if err != nil {
		return nil
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan || a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		return nil
	}
	return &p
}

// handleGetRunScopeOverlap implements GET /v0/runs/{run_id}/scope-overlap.
//
// Read authorization: the payload names OTHER runs' ids, issue refs and paths,
// so a run-bound mcp:run:<uuid> token is refused 403 run_token_forbidden even
// for its own run (approval condition 1). Every other caller must clear
// scopeGateViewRead (read:audit) via requireWriteScope: 401 anonymous, 403 a
// token missing the scope; a cookie-session operator passes.
func (s *Server) handleGetRunScopeOverlap(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"scope-overlap endpoint requires a configured run repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	if isRunBoundSubject(IdentityFrom(r.Context()).Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not read cross-run scope overlap; it names other runs' issues and paths", nil)
		return
	}
	if !s.requireWriteScope(w, r, scopeGateViewRead) {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
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
	result, err := s.computeScopeOverlap(r.Context(), runRow)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list live runs for scope overlap failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, result)
}

// gateViewScopeOverlapFor is the gate view's scope_overlap block: nil for a
// run-bound caller (it would name other runs, approval condition 1) and on a
// compute error (WARN-logged; the gate view still answers 200).
func (s *Server) gateViewScopeOverlapFor(ctx context.Context, runRow *run.Run) *scopeOverlapPayload {
	if runRow == nil || isRunBoundSubject(IdentityFrom(ctx).Subject) {
		return nil
	}
	result, err := s.computeScopeOverlap(ctx, runRow)
	if err != nil {
		s.cfg.Logger.Warn("gate-view: scope overlap compute failed; omitting scope_overlap",
			"run_id", runRow.ID.String(), "error", err.Error())
		return nil
	}
	return result
}
