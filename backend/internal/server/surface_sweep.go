package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// categoryPlanSurfaceSweep is the audit-log category for the entry
// runSurfaceSweep writes when it evaluates a plan's scope.files against the
// static surface-pattern registry (#763). Like plan_scope_precheck it is
// written even on a clean sweep (empty Findings) so a reader can
// distinguish "checked and clean" from "never checked" (an older run
// predating this feature).
const categoryPlanSurfaceSweep = "plan_surface_sweep"

// SurfaceSweepFinding is one missing-sibling result: the plan touched a
// trigger path belonging to a known multi-surface pattern but omitted one
// or more sibling surfaces that pattern requires move in lockstep.
//
// SubPlanTitle attributes the finding to a decomposition sub-plan when the
// trigger came from that sub-plan's own scope.files rather than the flat
// parent scope (#1077). Empty for parent-scope findings.
type SurfaceSweepFinding struct {
	Pattern         string   `json:"pattern"`
	TriggerPath     string   `json:"trigger_path"`
	MissingSiblings []string `json:"missing_siblings"`
	SubPlanTitle    string   `json:"sub_plan_title,omitempty"`
}

// SurfaceSweepPayload is the audit-payload shape for a plan_surface_sweep
// entry (#763). Findings is marshalled as an empty array (not null) on a
// clean sweep, mirroring scope_precheck's "checked and clean vs never
// checked" rationale. ScannedFiles is the count of scope.files evaluated.
type SurfaceSweepPayload struct {
	Findings     []SurfaceSweepFinding `json:"findings"`
	ScannedFiles int                   `json:"scanned_files"`
}

// surfacePattern is one entry in the static surface registry: when any
// Triggers path appears in a plan's scope.files, every Siblings path must
// also appear or the missing ones are flagged. A self-referential pattern
// (Triggers ∩ Siblings non-empty) is intentional — touching one render
// surface flags the missing peer.
type surfacePattern struct {
	Name     string
	Triggers []string
	Siblings []string
}

// surfacePatterns is the static registry keyed to the two production misses
// cited in #763. It is NOT call-graph analysis — broadening to that is
// explicitly out of scope. A rename of any path below would silently
// disable the sweep, so surface_sweep_test.go asserts every Trigger and
// Sibling path exists on disk to make a rename break loudly.
var surfacePatterns = []surfacePattern{
	{
		// #751/#755: the actor @-mention render surfaces. status_template.go
		// owns actorMention/renderActor; notifier.go owns renderApproverHandle.
		// Touching one without the other risks the wrong-user-ping class of
		// bug that recurred until both were changed together.
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
		// #742/#748: a new audit kind shipped without the mandated
		// docs/issue-comment-surfaces.md entry. The audit-kind emitters are
		// notifier.go and pullrequest.go; touching either without the
		// surfaces doc flags the doc per the CLAUDE.md mandate.
		Name: "audit kind requires surfaces doc",
		Triggers: []string{
			"backend/internal/issuecomment/notifier.go",
			"backend/internal/server/pullrequest.go",
		},
		Siblings: []string{
			"docs/issue-comment-surfaces.md",
		},
	},
	{
		// #873/#867: registering or removing a fishhawk_* MCP tool in
		// tools.go must move in lockstep with tools_test.go — whose
		// TestToolDescriptions_ConformToHouseStyle hardcodes
		// wantToolCount = 16 (plus the when/eligibility-leading house-style
		// assertion) and red-lines verify if the count drifts — and with
		// README.md, the human-facing MCP tool listing. tools.go is the
		// trigger only (not a sibling): the pattern fires when the
		// registration file is in scope but a coupled file is missing.
		Name: "mcp tool registration requires count test + readme",
		Triggers: []string{
			"backend/cmd/fishhawk-mcp/tools.go",
		},
		Siblings: []string{
			"backend/cmd/fishhawk-mcp/tools_test.go",
			"backend/cmd/fishhawk-mcp/README.md",
		},
	},
	// #1077: a canonical docs/spec/*.schema.json edited without its
	// embedded mirror copies fails CI's schema-sync gate. scripts/sync-schemas
	// is the authoritative routing; each canonical schema fans out to a
	// DISTINCT mirror set, so each is its own pattern rather than one glob.
	// Each pattern is self-referential (Triggers == Siblings): touching any
	// member — including a mirror without the canonical — flags the missing
	// peers. The cli/internal/spec/schemas copy is the one routinely omitted.
	{
		Name: "plan-standard schema requires every mirror",
		Triggers: []string{
			"docs/spec/plan-standard-v1.schema.json",
			"backend/internal/plan/schemas/plan-standard-v1.schema.json",
			"runner/internal/plan/schemas/plan-standard-v1.schema.json",
		},
		Siblings: []string{
			"docs/spec/plan-standard-v1.schema.json",
			"backend/internal/plan/schemas/plan-standard-v1.schema.json",
			"runner/internal/plan/schemas/plan-standard-v1.schema.json",
		},
	},
	{
		Name: "workflow schema requires every mirror",
		Triggers: []string{
			"docs/spec/workflow-v0.schema.json",
			"backend/internal/spec/schemas/workflow-v0.schema.json",
			"cli/internal/spec/schemas/workflow-v0.schema.json",
		},
		Siblings: []string{
			"docs/spec/workflow-v0.schema.json",
			"backend/internal/spec/schemas/workflow-v0.schema.json",
			"cli/internal/spec/schemas/workflow-v0.schema.json",
		},
	},
	{
		Name: "operator-role schema requires every mirror",
		Triggers: []string{
			"docs/spec/operator-role.schema.json",
			"backend/internal/operatorrole/schemas/operator-role.schema.json",
		},
		Siblings: []string{
			"docs/spec/operator-role.schema.json",
			"backend/internal/operatorrole/schemas/operator-role.schema.json",
		},
	},
	{
		Name: "operator-role-overlay schema requires every mirror",
		Triggers: []string{
			"docs/spec/operator-role-overlay.schema.json",
			"backend/internal/operatorrole/schemas/operator-role-overlay.schema.json",
		},
		Siblings: []string{
			"docs/spec/operator-role-overlay.schema.json",
			"backend/internal/operatorrole/schemas/operator-role-overlay.schema.json",
		},
	},
}

// evaluateSurfaceSweep is the pure matcher: for each pattern, if any
// Trigger path is in scope, it reports the Siblings absent from scope as a
// finding. Reporting only absent siblings means a self-referential pattern
// never flags a sibling already present. MissingSiblings is sorted for
// deterministic output. Paths are slash-normalized so a plan listing
// backslash-separated paths still matches.
func evaluateSurfaceSweep(scopeFiles []string, patterns []surfacePattern) []SurfaceSweepFinding {
	scope := make(map[string]bool, len(scopeFiles))
	for _, f := range scopeFiles {
		scope[filepath.ToSlash(f)] = true
	}

	var findings []SurfaceSweepFinding
	for _, p := range patterns {
		trigger, matched := "", false
		for _, t := range p.Triggers {
			if pathMatches(scope, t) {
				trigger, matched = filepath.ToSlash(t), true
				break
			}
		}
		if !matched {
			continue
		}
		var missing []string
		for _, sib := range p.Siblings {
			if !pathMatches(scope, sib) {
				missing = append(missing, filepath.ToSlash(sib))
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		findings = append(findings, SurfaceSweepFinding{
			Pattern:         p.Name,
			TriggerPath:     trigger,
			MissingSiblings: missing,
		})
	}
	return findings
}

// pathMatches reports whether the registry path is present in the scope
// set. It is a thin indirection over exact slash-normalized equality so the
// matcher stays glob-ready: a future registry entry needing prefix/glob
// triggers extends this helper without touching evaluateSurfaceSweep.
func pathMatches(scope map[string]bool, registryPath string) bool {
	return scope[filepath.ToSlash(registryPath)]
}

// runSurfaceSweep evaluates an uploaded plan's scope.files against the
// static surface registry and records the result as an advisory
// plan_surface_sweep audit entry (#763). It catches the class of bug where
// a plan moves one surface of a multi-surface pattern but forgets its
// siblings — e.g. editing an @-mention render surface without its peer, or
// adding an audit kind without the mandated surfaces-doc entry.
//
// Advisory-only and fail-open, matching runScopePrecheck's degradation
// contract verbatim: it guards on RunRepo+AuditRepo, resolves the run
// first, and on any failure (run not resolvable, parse error, audit-append
// failure) WARN-logs and returns without unwinding the upload. The sweep
// itself needs only the plan's scope.files, but gating on a resolvable run
// keeps it from recording an orphan advisory entry against a run that
// doesn't exist (legacy/not-found), exactly as the scope pre-check does.
//
// Returns the computed result payload so handleShipPlan can thread it
// into the plan-review prompt's gate-evidence section (#963); nil on
// every fail-open path (no result was computed). An audit-append failure
// still returns the computed result — the entry is observability, the
// evaluation itself succeeded.
func (s *Server) runSurfaceSweep(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *SurfaceSweepPayload {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}

	// Resolve the run first so the sweep only records against a real,
	// resolvable run — matching runScopePrecheck's degradation contract.
	if _, err := s.cfg.RunRepo.GetRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "surface sweep: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	// Validation already passed in handleShipPlan; a parse failure here is
	// an internal inconsistency — log and skip rather than block.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "surface sweep: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	scopeFiles := make([]string, 0, len(parsedPlan.Scope.Files))
	for _, f := range parsedPlan.Scope.Files {
		scopeFiles = append(scopeFiles, f.Path)
	}

	findings := evaluateSurfaceSweep(scopeFiles, surfacePatterns)

	// Evaluate each decomposition sub-plan's own scope.files too (#1077):
	// an under-scoped slice's coupling gap must surface at the parent plan
	// gate the operator approves, since the fan-out child runs are
	// implement-only and never re-upload a plan. Findings are attributed to
	// their sub-plan via SubPlanTitle; the parent ScannedFiles count is
	// unchanged. evaluateSurfaceSweep stays pure.
	if parsedPlan.Decomposition != nil {
		for _, sp := range parsedPlan.Decomposition.SubPlans {
			if sp.Scope == nil {
				continue
			}
			subFiles := make([]string, 0, len(sp.Scope.Files))
			for _, f := range sp.Scope.Files {
				subFiles = append(subFiles, f.Path)
			}
			for _, f := range evaluateSurfaceSweep(subFiles, surfacePatterns) {
				f.SubPlanTitle = sp.Title
				findings = append(findings, f)
			}
		}
	}

	if findings == nil {
		// Marshal an empty array rather than null so the audit payload's
		// "checked and clean" state is explicit (a missing entry means
		// "never checked").
		findings = []SurfaceSweepFinding{}
	}

	result := &SurfaceSweepPayload{
		Findings:     findings,
		ScannedFiles: len(scopeFiles),
	}
	payload, _ := json.Marshal(result)
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
	return result
}
