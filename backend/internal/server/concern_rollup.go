package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryConcernRolledUp records one import of a decomposition child's open
// HIGH/MEDIUM review concerns into its parent's concern ledger (E94.1 / #4692,
// ADR-117 D2). One system-actor row per pass that inserted at least one copy,
// on the parent run and the parent implement stage. The payload carries counts
// and ids only — never reviewer note prose.
const CategoryConcernRolledUp = "concern_rolled_up"

// Sites a concern_rolled_up row names: the fan-in (every integrateSlices pass),
// a child review that landed after the child succeeded, and the merge-gate
// reconcile (leg (d) of the merge-queue eligibility predicate re-running the
// idempotent import for the parent's succeeded children).
const (
	concernRollupSiteFanIn          = "fan_in"
	concernRollupSiteChildReview    = "child_review"
	concernRollupSiteMergeReconcile = "merge_reconcile"
)

// Stable log keys (approval condition C5).
const (
	concernRolledUpAuditLostEvent        = "concern_rolled_up_audit_lost"
	concernOpenAtMergeStampFailedEvent   = "concern_open_at_merge_stamp_failed"
	concernRolledUpCopySupersedeFailEvnt = "concern_rolled_up_copy_supersede_failed"
	// concernLateImportFailedEvent keys every failure of a child-slice import
	// outside the fan-in: the late child-review hook (best-effort, so the key
	// is what makes a lost import findable) and the merge-gate reconcile
	// (which also returns its error).
	concernLateImportFailedEvent = "concern_late_import_failed"
)

// ImportSliceConcerns satisfies orchestrator.SliceConcernImporter (E94.1 /
// #4692): the fan-in hook every integrateSlices pass reaches before
// slices_integrated is written. An error fails that fan-in pass closed.
func (s *Server) ImportSliceConcerns(ctx context.Context, parentRunID uuid.UUID, childRunIDs []uuid.UUID) error {
	return s.importSliceConcerns(ctx, parentRunID, childRunIDs, concernRollupSiteFanIn)
}

// sliceConcernRollsUp is the import's severity rule (ADR-117 D2): HIGH and
// MEDIUM roll up, LOW does not. Exact match on the stored severity — the same
// vocabulary the merge-eligibility leg reads.
func sliceConcernRollsUp(c *concern.Concern) bool {
	if c == nil {
		return false
	}
	return c.Severity == string(planreview.SeverityHigh) || c.Severity == string(planreview.SeverityMedium)
}

// rolledUpConcernRecord is one concerns[] entry of the concern_rolled_up
// payload. No note prose: the parent ledger row carries it.
type rolledUpConcernRecord struct {
	ConcernID            string  `json:"concern_id"`
	RolledUpFrom         string  `json:"rolled_up_from"`
	ChildRunID           string  `json:"child_run_id"`
	ChildStageID         string  `json:"child_stage_id"`
	SliceIndex           *int    `json:"slice_index"`
	Severity             string  `json:"severity"`
	Category             string  `json:"category"`
	ReviewerModel        *string `json:"reviewer_model"`
	ReviewerRole         string  `json:"reviewer_role"`
	OriginReviewSequence int64   `json:"origin_review_sequence"`
}

// concernRolledUpPayload is the concern_rolled_up audit payload.
type concernRolledUpPayload struct {
	Site          string                  `json:"site"`
	ParentStageID string                  `json:"parent_stage_id"`
	Count         int                     `json:"count"`
	High          int                     `json:"high"`
	Medium        int                     `json:"medium"`
	Concerns      []rolledUpConcernRecord `json:"concerns"`
}

// importSliceConcerns copies every OPEN HIGH/MEDIUM concern of the given
// children into the parent's ledger on the parent implement stage (the stage
// the consolidated review's concerns attach to), then appends one
// concern_rolled_up row when at least one copy was inserted. The copies are
// durable before the row is written — a missing row, never a false one — so an
// append failure logs at ERROR and returns nil. A nil ConcernRepo or RunRepo
// imports nothing.
func (s *Server) importSliceConcerns(ctx context.Context, parentRunID uuid.UUID, childRunIDs []uuid.UUID, site string) error {
	if s.cfg.ConcernRepo == nil || s.cfg.RunRepo == nil {
		return nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, parentRunID)
	if err != nil {
		return fmt.Errorf("list parent %s stages: %w", parentRunID, err)
	}
	implStage := consolidatedImplementStage(stages)
	if implStage == nil {
		return fmt.Errorf("parent run %s has no implement stage to attach imported concerns to", parentRunID)
	}
	var sources []*concern.Concern
	byID := map[uuid.UUID]*concern.Concern{}
	for _, childID := range childRunIDs {
		open, err := s.cfg.ConcernRepo.ListOpenByRun(ctx, childID)
		if err != nil {
			return fmt.Errorf("list open concerns of child %s: %w", childID, err)
		}
		for _, c := range open {
			if sliceConcernRollsUp(c) {
				sources = append(sources, c)
				byID[c.ID] = c
			}
		}
	}
	if len(sources) == 0 {
		return nil
	}
	copies, err := s.cfg.ConcernRepo.ImportRolledUp(ctx, concern.ImportRolledUpParams{
		ParentRunID:   parentRunID,
		ParentStageID: implStage.ID,
		Sources:       sources,
	})
	if err != nil {
		return fmt.Errorf("import child concerns into parent %s: %w", parentRunID, err)
	}
	if len(copies) == 0 {
		return nil
	}
	s.appendConcernRolledUp(ctx, parentRunID, implStage.ID, site, copies, byID)
	return nil
}

// appendConcernRolledUp writes the concern_rolled_up row for the copies one
// import pass inserted. Best-effort: the copies are already durable.
func (s *Server) appendConcernRolledUp(ctx context.Context, parentRunID, parentStageID uuid.UUID, site string, copies []*concern.Concern, sources map[uuid.UUID]*concern.Concern) {
	if s.cfg.AuditRepo == nil {
		return
	}
	sliceIdx := map[uuid.UUID]*int{}
	p := concernRolledUpPayload{Site: site, ParentStageID: parentStageID.String(), Concerns: []rolledUpConcernRecord{}}
	for _, cp := range copies {
		if cp == nil || cp.RolledUpFrom == nil {
			continue
		}
		rec := rolledUpConcernRecord{
			ConcernID:            cp.ID.String(),
			RolledUpFrom:         cp.RolledUpFrom.String(),
			Severity:             cp.Severity,
			Category:             cp.Category,
			ReviewerModel:        cp.ReviewerModel,
			ReviewerRole:         cp.ReviewerRole,
			OriginReviewSequence: cp.OriginReviewSequence,
		}
		if src := sources[*cp.RolledUpFrom]; src != nil {
			rec.ChildRunID = src.RunID.String()
			rec.ChildStageID = src.StageID.String()
			idx, seen := sliceIdx[src.RunID]
			if !seen {
				if child, gerr := s.cfg.RunRepo.GetRun(ctx, src.RunID); gerr == nil && child != nil && child.SliceIndex != nil {
					v := *child.SliceIndex
					idx = &v
				}
				sliceIdx[src.RunID] = idx
			}
			rec.SliceIndex = idx
		}
		switch cp.Severity {
		case string(planreview.SeverityHigh):
			p.High++
		case string(planreview.SeverityMedium):
			p.Medium++
		}
		p.Concerns = append(p.Concerns, rec)
	}
	p.Count = len(p.Concerns)
	payload, _ := json.Marshal(p)
	systemKind := audit.ActorSystem
	stageID := parentStageID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernRolledUp,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"concern rollup: concern_rolled_up_audit_lost: the imported copies are durable but their audit row was not written",
			slog.String("event", concernRolledUpAuditLostEvent),
			slog.String("run_id", parentRunID.String()),
			slog.String("site", site),
			slog.Int("count", p.Count),
			slog.String("error", err.Error()))
	}
}

// warnConcernLateImportFailed is the ONE WARN every failure of a child-slice
// import outside the fan-in goes through, under the stable key
// concern_late_import_failed. extra carries branch-specific attributes.
func (s *Server) warnConcernLateImportFailed(ctx context.Context, msg, site string, runID uuid.UUID, err error, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("event", concernLateImportFailedEvent),
		slog.String("site", site),
		slog.String("run_id", runID.String()),
		slog.String("error", err.Error()),
	}
	attrs = append(attrs, extra...)
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
}

// importLateSliceConcerns imports the open HIGH/MEDIUM concerns of a
// decomposition child whose review verdict landed AFTER the child succeeded
// (advisory implement reviews run detached, so the last slice's verdict
// routinely lands after the fan-in). Its caller runs it only for a verdict NOT
// superseded by a stage retry at either read (approval condition C1(a)). It
// imports only when the run is a decomposition child, the child run is
// succeeded and the parent is not terminal. Best-effort: every failure
// WARN-logs under the stable key concern_late_import_failed (site child_review)
// and nothing is returned. A lost import is not final: the merge-queue
// eligibility predicate's concern leg re-runs the idempotent import
// (reconcileSliceConcerns), so the copy reaches the parent ledger at the next
// merge-gate evaluation.
func (s *Server) importLateSliceConcerns(ctx context.Context, childRunID uuid.UUID) {
	if s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil {
		return
	}
	child, err := s.cfg.RunRepo.GetRun(ctx, childRunID)
	if err != nil {
		s.warnConcernLateImportFailed(ctx, "concern rollup: concern_late_import_failed: late child review: get run failed",
			concernRollupSiteChildReview, childRunID, err)
		return
	}
	if child == nil || child.DecomposedFrom == nil || child.State != run.StateSucceeded {
		return
	}
	parent, err := s.cfg.RunRepo.GetRun(ctx, *child.DecomposedFrom)
	if err != nil {
		s.warnConcernLateImportFailed(ctx, "concern rollup: concern_late_import_failed: late child review: get parent run failed",
			concernRollupSiteChildReview, childRunID, err)
		return
	}
	if parent == nil || parent.State.IsTerminal() {
		return
	}
	if err := s.importSliceConcerns(ctx, parent.ID, []uuid.UUID{child.ID}, concernRollupSiteChildReview); err != nil {
		s.warnConcernLateImportFailed(ctx, "concern rollup: concern_late_import_failed: late child review import failed",
			concernRollupSiteChildReview, childRunID, err, slog.String("parent_run_id", parent.ID.String()))
	}
}

// reconcileSliceConcerns re-runs the idempotent child-slice import for the
// parent's SUCCEEDED decomposition children: the merge-gate reconcile that
// recovers a late import the best-effort hook lost (importLateSliceConcerns
// only WARN-logs). It is the merge-queue eligibility predicate's one write
// (mergeQueueConcernLeg runs it before reading the parent ledger), so the
// copies it inserts are what leg (d) then blocks on.
//
// The child set is the fan-in's (succeeded children only), and a terminal
// parent is skipped (the late hook's rule: a copy on a merged parent would be
// a false open row). A run with no succeeded child returns before any parent
// stage read. A nil RunRepo imports nothing. A failure WARN-logs under
// concern_late_import_failed (site merge_reconcile) and is RETURNED: the leg
// maps it to undeterminable, which every caller holds on (fail closed). An
// already-closed child source is not imported and the unique index never
// re-mints a closed copy, so a repeat pass inserts nothing.
func (s *Server) reconcileSliceConcerns(ctx context.Context, parent *run.Run) error {
	if s.cfg.RunRepo == nil {
		return nil
	}
	if parent.State.IsTerminal() {
		return nil
	}
	children, err := s.listAllDecomposedChildren(ctx, parent.ID)
	if err != nil {
		s.warnConcernLateImportFailed(ctx, "concern rollup: concern_late_import_failed: merge reconcile: list decomposed children failed",
			concernRollupSiteMergeReconcile, parent.ID, err)
		return fmt.Errorf("list decomposed children of %s: %w", parent.ID, err)
	}
	var succeeded []uuid.UUID
	for _, c := range children {
		if c != nil && c.State == run.StateSucceeded {
			succeeded = append(succeeded, c.ID)
		}
	}
	if len(succeeded) == 0 {
		return nil
	}
	if err := s.importSliceConcerns(ctx, parent.ID, succeeded, concernRollupSiteMergeReconcile); err != nil {
		s.warnConcernLateImportFailed(ctx, "concern rollup: concern_late_import_failed: merge reconcile import failed",
			concernRollupSiteMergeReconcile, parent.ID, err)
		return err
	}
	return nil
}

// supersedeRolledUpCopies supersedes the parent copies of child concerns a
// retry sweep just superseded (approval condition C1(b)), so no parent copy
// outlives its source. A copy that is no longer open is left alone. Best-effort:
// a non-child run no-ops; every failure WARN-logs.
func (s *Server) supersedeRolledUpCopies(ctx context.Context, childRunID uuid.UUID, superseded []*concern.Concern, reason string) {
	if len(superseded) == 0 || s.cfg.RunRepo == nil || s.cfg.ConcernRepo == nil {
		return
	}
	child, err := s.cfg.RunRepo.GetRun(ctx, childRunID)
	if err != nil || child == nil || child.DecomposedFrom == nil {
		return
	}
	rows, err := s.cfg.ConcernRepo.ListByRun(ctx, *child.DecomposedFrom)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "concern rollup: list parent concerns for copy supersession failed",
			slog.String("event", concernRolledUpCopySupersedeFailEvnt),
			slog.String("run_id", childRunID.String()),
			slog.String("error", err.Error()))
		return
	}
	bySource := map[uuid.UUID]*concern.Concern{}
	for _, src := range superseded {
		bySource[src.ID] = src
	}
	for _, cp := range rows {
		if cp == nil || cp.RolledUpFrom == nil || !cp.State.IsOpen() {
			continue
		}
		src, ok := bySource[*cp.RolledUpFrom]
		if !ok {
			continue
		}
		if _, err := s.cfg.ConcernRepo.ApplyResolution(ctx, cp.ID, concern.StateSuperseded, concern.RolledUpSupersededReason(src, reason)); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "concern rollup: supersede parent copy failed",
				slog.String("event", concernRolledUpCopySupersedeFailEvnt),
				slog.String("run_id", childRunID.String()),
				slog.String("concern_id", cp.ID.String()),
				slog.String("error", err.Error()))
		}
	}
}

// stampConcernsOpenAtMerge records the open_at_merge outcome (ADR-117 D1) on
// every still-open or superseded concern, with no outcome yet, of the merged
// run and of every decomposition child. It never unwinds a merge: a children
// read failure WARN-logs and still stamps the run's own rows, and a store
// failure WARN-logs under the stable key concern_open_at_merge_stamp_failed.
// No reconcile pass re-stamps a failed write (the trade-off the concern
// README records).
func (s *Server) stampConcernsOpenAtMerge(ctx context.Context, runID uuid.UUID) {
	if s.cfg.ConcernRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	ids := []uuid.UUID{runID}
	children, err := s.listAllDecomposedChildren(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "concern open_at_merge stamp: list decomposed children failed; stamping the run's own concerns",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
	for _, c := range children {
		if c != nil {
			ids = append(ids, c.ID)
		}
	}
	if _, err := s.cfg.ConcernRepo.StampOpenAtMerge(ctx, ids); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"concern open_at_merge stamp: concern_open_at_merge_stamp_failed: outcome not recorded",
			slog.String("event", concernOpenAtMergeStampFailedEvent),
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}
