package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// reachabilitySweepAuditKind is the audit-log category for the entry
// runPlanReachability writes when the runner ships a symbol-reachability sweep
// result on plan upload (#2056, E50.4). Like plan_warnings the entry is written
// ONLY when a payload is present (the runner ran the sweep because the plan
// carried a split_proposal) — a plan with no split_proposal ships no header and
// gets no entry, so the non-split happy-path audit count is unchanged.
//
// The identifier deliberately does NOT contain "category": the audit-category
// completeness sweep (audit/categories_completeness_test.go) requires every
// "category"-named const literal to be present in audit.KnownCategories, and
// that registry lives in package audit — out of this slice's scope. Until the
// value is registered there (the proper follow-up), it is admitted at every
// registry-validating surface via allow_unknown (see loadReachability), so the
// advisory read path stays functional without a registry entry.
const reachabilitySweepAuditKind = "plan_reachability_sweep"

// reachabilityHeader is the request header the runner carries the advisory
// reachability sweep result in (#2056). It CANNOT ride inside the POST body:
// the body is the standard_v1 plan artifact, signature-verified and stored
// verbatim. The name MUST stay byte-identical to the runner's
// upload.ReachabilityHeader constant (runner/internal/upload/upload.go); a
// drift silently disables the advisory.
const reachabilityHeader = "X-Fishhawk-Plan-Reachability"

// PlanReachabilityPayload is the server's decode struct for the runner's
// reachability.Result wire shape (#2056). The backend cannot import the runner's
// reachability package across the module boundary, so it owns this mirroring
// struct; the json tags MUST stay byte-identical to
// runner/internal/reachability.Result or the advisory silently fails open. It is
// both the header-decode target and the audit-payload shape (the recorder
// re-marshals it verbatim), so fishhawk_get_plan's loadReachability decodes the
// same tags back.
type PlanReachabilityPayload struct {
	Available  bool                        `json:"available"`
	SkipReason string                      `json:"skip_reason,omitempty"`
	Phases     []PlanReachabilityPhase     `json:"phases,omitempty"`
	Violations []PlanReachabilityViolation `json:"violations,omitempty"`
}

// PlanReachabilityPhase mirrors reachability.PhaseResult: one split-proposal
// phase's declared-vs-derived file counts. DerivedCount > DeclaredCount means
// the phase's symbols leak into sibling phases — the partition would produce a
// non-compiling intermediate.
type PlanReachabilityPhase struct {
	Index         int    `json:"index"`
	Title         string `json:"title"`
	DeclaredCount int    `json:"declared_count"`
	DerivedCount  int    `json:"derived_count"`
}

// PlanReachabilityViolation mirrors reachability.Violation: one cross-boundary
// compile-breaking use site pairing a defining symbol (in DefPhase) with a use
// site (in the different UsePhase), classified by Kind.
type PlanReachabilityViolation struct {
	Kind     string `json:"kind"`
	Symbol   string `json:"symbol"`
	DefFile  string `json:"def_file"`
	DefPhase int    `json:"def_phase"`
	UseFile  string `json:"use_file"`
	UsePhase int    `json:"use_phase"`
}

// runPlanReachability decodes the advisory reachability sweep result the runner
// shipped in the reachabilityHeader header and, when present and well-formed,
// records a fail-open plan_reachability_sweep audit entry (#2056). This is the
// server leg of the runner→server→get_plan transport: the runner-side sweep
// (only the runner holds the checked-out Go source) crosses here, the operator
// surface is fishhawk_get_plan's reachability field.
//
// Advisory-only and FAIL-OPEN on every branch, mirroring runPlanWarnings:
//   - nil AuditRepo → skip (nothing to record into).
//   - empty header (the common case: a plan with no split_proposal, or a
//     runner that skipped the sweep) → skip, no entry.
//   - malformed header JSON → WARN-log and skip rather than block the upload.
//   - audit-append failure → WARN-log and continue.
//
// It never transitions or fails the plan stage. Returns the decoded payload so
// a future caller could thread it into the plan-review prompt (not wired in
// this slice); nil on every skip path.
func (s *Server) runPlanReachability(ctx context.Context, runID, stageID uuid.UUID, headerJSON string) *PlanReachabilityPayload {
	if s.cfg.AuditRepo == nil || headerJSON == "" {
		return nil
	}

	var payload PlanReachabilityPayload
	if err := json.Unmarshal([]byte(headerJSON), &payload); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan reachability: decode header failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	marshaled, _ := json.Marshal(payload)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  reachabilitySweepAuditKind,
		ActorKind: &systemKind,
		Payload:   marshaled,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan reachability: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return &payload
}
