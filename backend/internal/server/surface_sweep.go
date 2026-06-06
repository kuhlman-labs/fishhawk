package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// categoryPlanSurfaceSweep is the audit-log category for the entry
// runSurfaceSweep writes when it checks a plan's scope.files against the
// static multi-surface pattern registry (#763). Like plan_scope_precheck
// (#658) the entry is the MCP-readable proxy for "the plan gate ran the
// surface sweep": it is written even on a clean scope (empty findings) so
// a reader can distinguish "checked and clean" from "never checked" (an
// older run predating this feature).
const categoryPlanSurfaceSweep = "plan_surface_sweep"

// SurfaceSweepFinding is one matched multi-surface pattern: the plan's
// scope.files touched TriggerPath (a known surface that moves in lockstep
// with others), but MissingSiblings — sibling surfaces the change should
// touch too — are absent from scope.files. Pattern names the registry
// entry for the operator's benefit.
type SurfaceSweepFinding struct {
	Pattern         string   `json:"pattern"`
	TriggerPath     string   `json:"trigger_path"`
	MissingSiblings []string `json:"missing_siblings"`
}

// SurfaceSweepPayload is the audit-payload shape for a plan_surface_sweep
// entry (#763). Findings is marshaled as [] (never null) on a clean scope
// so the MCP read side can decode "checked and clean" distinctly.
// ScannedFiles is the count of scope.files the sweep evaluated.
type SurfaceSweepPayload struct {
	Findings     []SurfaceSweepFinding `json:"findings"`
	ScannedFiles int                   `json:"scanned_files"`
}

// surfacePattern is one entry in the static registry. When any Triggers
// path appears in a plan's scope.files, every Siblings path that is absent
// from scope.files is flagged. Triggers and Siblings store repo-relative
// paths matched on exact equality against scope.files (no globbing): the
// match stays trivial and deterministic, and the path-existence test
// (surface_sweep_test.go) asserts every path here exists on disk so a
// future rename breaks loudly rather than silently disabling the sweep.
type surfacePattern struct {
	Name     string
	Triggers []string
	Siblings []string
}

// surfacePatterns is the static multi-surface pattern registry (#763). The
// two initial entries key to the production misses cited in the issue:
//
//   - "actor @-mention render surfaces" — the four @-mention render sites
//     where the wrong-user ping kept firing (#751/#755). status_template.go
//     and notifier.go must move in lockstep; touching one without the other
//     is the shape that let the bug recur.
//   - "audit kind requires surfaces doc" — a new audit kind shipped from
//     notifier.go without the mandated docs/issue-comment-surfaces.md entry
//     (#742/#748). Any notifier.go touch advises the doc (intentional
//     over-flagging — advisory only, so a false positive costs one glance).
//
// The issue's "function/const with N>1 call-sites" trigger is deliberately
// NOT here: detecting it reliably needs an AST/call-graph pass (brittle,
// over-budget), and the broadly-implemented-interface case is partly
// covered by the #728 compile gate. See docs/ARCHITECTURE.md.
var surfacePatterns = []surfacePattern{
	{
		Name: "actor @-mention render surfaces",
		Triggers: []string{
			"backend/internal/issuecomment/status_template.go",
			"backend/internal/issuecomment/notifier.go",
		},
		Siblings: []string{
			"backend/internal/issuecomment/status_template.go",
			"backend/internal/issuecomment/notifier.go",
		},
	},
	{
		Name: "audit kind requires surfaces doc",
		Triggers: []string{
			"backend/internal/issuecomment/notifier.go",
		},
		Siblings: []string{
			"docs/issue-comment-surfaces.md",
		},
	},
}

// runSurfaceSweep checks an uploaded plan's scope.files against the static
// multi-surface pattern registry and records the result as an advisory
// plan_surface_sweep audit entry (#763). It shifts left a recurring class
// of misses: a change touches one instance of a surface that must move in
// lockstep with siblings (e.g. one of the @-mention render sites, or a new
// audit kind in notifier.go) but the plan's scope.files omits the siblings.
// The sweep folds the missing siblings into an advisory finding the
// operator sees via fishhawk_get_plan before approving.
//
// Modeled directly on runScopePrecheck (#658): best-effort, fail-open,
// advisory-only, path-driven over scope.files, MCP-readable. The match is
// path-based, not content analysis — at plan time only scope.files paths
// are known (the implement stage has not run), so the sweep keys off the
// path of the surface file likely to define a new kind, intentionally
// over-flagging rather than missing.
//
// Always AppendChained a plan_surface_sweep entry (Findings as [] when
// empty) so "checked and clean" is distinguishable from "never checked".
// Best-effort throughout: every failure path WARN-logs and returns without
// unwinding the upload, matching runScopePrecheck's degradation contract.
func (s *Server) runSurfaceSweep(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) {
	// Guard only AuditRepo: runSurfaceSweep reads nothing from RunRepo (the
	// registry is static and matches purely on scope.files paths), so a
	// server wired with AuditRepo but not RunRepo still runs the sweep.
	if s.cfg.AuditRepo == nil {
		return
	}

	// Validation already passed in handleShipPlan; a parse failure here is
	// an internal inconsistency — log and skip rather than block.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "surface sweep: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	scoped := make(map[string]bool, len(parsedPlan.Scope.Files))
	for _, f := range parsedPlan.Scope.Files {
		scoped[f.Path] = true
	}

	findings := []SurfaceSweepFinding{}
	for _, pat := range surfacePatterns {
		for _, trigger := range pat.Triggers {
			if !scoped[trigger] {
				continue
			}
			var missing []string
			for _, sib := range pat.Siblings {
				if !scoped[sib] {
					missing = append(missing, sib)
				}
			}
			if len(missing) > 0 {
				findings = append(findings, SurfaceSweepFinding{
					Pattern:         pat.Name,
					TriggerPath:     trigger,
					MissingSiblings: missing,
				})
			}
		}
	}

	payload, _ := json.Marshal(SurfaceSweepPayload{
		Findings:     findings,
		ScannedFiles: len(parsedPlan.Scope.Files),
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanSurfaceSweep,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "surface sweep: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
}
