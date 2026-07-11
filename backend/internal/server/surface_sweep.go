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
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
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

// CrossSliceClaim records which member files of a lockstep pattern one
// decomposition slice owns. SliceTitle is the sub-plan's title; Files are
// the pattern-member paths present in that slice's declared scope.files,
// slash-normalized and sorted.
type CrossSliceClaim struct {
	SliceTitle string   `json:"slice_title"`
	Files      []string `json:"files"`
}

// CrossSliceCouplingFinding is one cross-slice write-coupling result: a
// registered multi-surface lockstep pattern's member files are partitioned
// across 2+ DISTINCT decomposition slices, so completing the seam would
// force a later slice to modify a file owned by an earlier slice via a
// runtime scope amendment (which can time out, #1035). This is the INVERSE
// of the #1062 same-file-in-two-slices gate: there two slices DECLARE the
// same file; here distinct slices each own different members of a pattern
// that must move in lockstep. The fix is consolidation (one slice owns the
// whole seam), not dual declaration. Slices is sorted by SliceTitle.
type CrossSliceCouplingFinding struct {
	Pattern string            `json:"pattern"`
	Slices  []CrossSliceClaim `json:"slices"`
}

// SurfaceSweepPayload is the audit-payload shape for a plan_surface_sweep
// entry (#763). Findings is marshalled as an empty array (not null) on a
// clean sweep, mirroring scope_precheck's "checked and clean vs never
// checked" rationale. ScannedFiles is the count of scope.files evaluated.
// CrossSliceFindings carries the cross-slice coupling pass (#1102) and is
// likewise an empty array (not null) on a clean sweep.
type SurfaceSweepPayload struct {
	Findings           []SurfaceSweepFinding       `json:"findings"`
	ScannedFiles       int                         `json:"scanned_files"`
	CrossSliceFindings []CrossSliceCouplingFinding `json:"cross_slice_findings"`
	// AppliedExemptions records the plan-declared surface_sweep_exemptions
	// that actually suppressed a would-be missing-sibling finding (#1544).
	// Like Findings/CrossSliceFindings it marshals as an empty array (not
	// null) when nothing was suppressed, so a reader can distinguish "no
	// exemption applied" from a legacy plan predating the field. A
	// non-matching or non-firing declared exemption is a harmless no-op and
	// is NOT recorded here — only an exemption that removed a genuinely
	// absent sibling from the missing set appears.
	AppliedExemptions []AppliedExemption `json:"applied_exemptions"`
}

// AppliedExemption records one plan-declared surface_sweep_exemption that
// suppressed a would-be missing-sibling finding (#1544): the Pattern it
// belongs to, the Sibling path it exempted, the plan's stated Reason, and —
// for a decomposition sub-plan scope — the attributing SubPlanTitle. It is
// recorded in the audit payload and rendered to plan reviewers so a bogus
// reason stays challengeable: an exemption is never silent.
type AppliedExemption struct {
	Pattern      string `json:"pattern"`
	Sibling      string `json:"sibling"`
	Reason       string `json:"reason"`
	SubPlanTitle string `json:"sub_plan_title,omitempty"`
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
		// ADR-046 / #1381: workflow-v1 fans out to the same backend + cli
		// mirror set as v0 (scripts/sync-schemas' workflow-v* case). A v1
		// canonical edit without its mirrors red-lines CI's schema-sync gate;
		// self-referential (Triggers == Siblings) so touching any member —
		// including a mirror without the canonical — flags the missing peers.
		Name: "workflow-v1 schema requires every mirror",
		Triggers: []string{
			"docs/spec/workflow-v1.schema.json",
			"backend/internal/spec/schemas/workflow-v1.schema.json",
			"cli/internal/spec/schemas/workflow-v1.schema.json",
		},
		Siblings: []string{
			"docs/spec/workflow-v1.schema.json",
			"backend/internal/spec/schemas/workflow-v1.schema.json",
			"cli/internal/spec/schemas/workflow-v1.schema.json",
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
	{
		// #1101/#1006 case 2: the work-management-v0 schema's canonical and
		// embedded-mirror copies must move in lockstep (scripts/sync-schemas'
		// work-management-v* case routes the canonical to exactly the one
		// backend/internal/workmgmt/schemas mirror). Self-referential
		// (Triggers == Siblings): a field-add touching the canonical without
		// its mirror flags within a slice; canonical and mirror split across
		// decomposition slices flags as a cross-slice coupling finding (#1102).
		Name: "work-management schema requires every mirror",
		Triggers: []string{
			"docs/spec/work-management-v0.schema.json",
			"backend/internal/workmgmt/schemas/work-management-v0.schema.json",
		},
		Siblings: []string{
			"docs/spec/work-management-v0.schema.json",
			"backend/internal/workmgmt/schemas/work-management-v0.schema.json",
		},
	},
}

// surfaceCouplingPatternsForPrompt maps the static surfacePatterns registry
// into the prompt-package wire type so the plan-stage prompt handler can thread
// the sibling map into the plan prompt (#763/#1797). The registry stays the
// SINGLE SOURCE OF TRUTH — this accessor is a pure structural projection with
// no second copy of the coupling knowledge, so a registry edit propagates to
// the prompt with no drift. Called only from the StageTypePlan block of the two
// prompt handlers (handleGetStagePrompt / handleGetStagePromptRender) so the
// signed prompt and the render preview stay byte-identical.
func surfaceCouplingPatternsForPrompt() []prompt.SurfaceCouplingPattern {
	out := make([]prompt.SurfaceCouplingPattern, 0, len(surfacePatterns))
	for _, p := range surfacePatterns {
		out = append(out, prompt.SurfaceCouplingPattern{
			Name:     p.Name,
			Triggers: append([]string(nil), p.Triggers...),
			Siblings: append([]string(nil), p.Siblings...),
		})
	}
	return out
}

// evaluateSurfaceSweep is the pure matcher: for each pattern, if any
// Trigger path is in scope, it reports the Siblings absent from scope as a
// finding. Reporting only absent siblings means a self-referential pattern
// never flags a sibling already present. MissingSiblings is sorted for
// deterministic output. Paths are slash-normalized so a plan listing
// backslash-separated paths still matches.
//
// A plan-declared surface_sweep_exemption naming (pattern.Name, sibling)
// suppresses that sibling from the missing set (#1544): the finding is
// suppressed only when the pattern's missing set is FULLY covered by
// exemptions — a partial exemption still fires a finding listing the
// remaining uncovered siblings. Only an exemption that removes a genuinely
// absent sibling is returned in the second result (an applied exemption);
// a non-matching, non-firing, or already-scoped-sibling exemption is a
// harmless no-op and is not returned. Applied exemptions carry the plan's
// reason so the caller can render it to reviewers as challengeable. Stays
// pure — no receiver, no I/O.
func evaluateSurfaceSweep(scopeFiles []string, patterns []surfacePattern, exemptions []plan.SurfaceSweepExemption) ([]SurfaceSweepFinding, []AppliedExemption) {
	scope := make(map[string]bool, len(scopeFiles))
	for _, f := range scopeFiles {
		scope[filepath.ToSlash(f)] = true
	}

	// Index exemptions by (pattern name, slash-normalized sibling) so a
	// firing pattern can look up an exemption for an absent sibling in O(1),
	// preserving the declared reason.
	type exKey struct{ pattern, sibling string }
	exByKey := make(map[exKey]plan.SurfaceSweepExemption, len(exemptions))
	for _, e := range exemptions {
		exByKey[exKey{e.Pattern, filepath.ToSlash(e.Sibling)}] = e
	}

	var findings []SurfaceSweepFinding
	var applied []AppliedExemption
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
			if pathMatches(scope, sib) {
				continue
			}
			sibNorm := filepath.ToSlash(sib)
			// Sibling absent from scope: a plan-declared exemption naming
			// (this pattern, this sibling) suppresses it from the missing
			// set and is recorded as applied for reviewer visibility.
			if e, ok := exByKey[exKey{p.Name, sibNorm}]; ok {
				applied = append(applied, AppliedExemption{
					Pattern: p.Name,
					Sibling: sibNorm,
					Reason:  e.Reason,
				})
				continue
			}
			missing = append(missing, sibNorm)
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
	return findings, applied
}

// pathMatches reports whether the registry path is present in the scope
// set. It is a thin indirection over exact slash-normalized equality so the
// matcher stays glob-ready: a future registry entry needing prefix/glob
// triggers extends this helper without touching evaluateSurfaceSweep.
func pathMatches(scope map[string]bool, registryPath string) bool {
	return scope[filepath.ToSlash(registryPath)]
}

// evaluateCrossSliceCoupling is the pure cross-slice detector (#1102): for
// each registered pattern it computes the pattern's member-file set
// (Triggers ∪ Siblings, slash-normalized, deduped) and which DECLARING
// decomposition slice owns each present member. When the owned members are
// claimed by 2+ DISTINCT slices the seam is split across the fan-out, so
// completing it would need a runtime scope amendment (which can time out,
// #1035) — it emits one CrossSliceCouplingFinding naming each involved
// slice and the member files it owns.
//
// Only sub-plans that DECLARE a scope are partitioned: an undeclared scope
// inherits the parent's full scope.files and cannot partition unsoundly —
// identical rationale to checkCrossSliceSharedFiles (#1062) and the #1077
// sub-plan sweep. A single slice listing the same member twice collapses to
// one claimant (map semantics). Output is deterministic: slices sorted by
// title, files sorted. Returns nil when nothing is split. Pure — no Server
// receiver, no I/O — exactly like evaluateSurfaceSweep.
func evaluateCrossSliceCoupling(parsedPlan *plan.Plan, patterns []surfacePattern) []CrossSliceCouplingFinding {
	if parsedPlan.Decomposition == nil {
		return nil
	}

	// Per-slice ownership: sliceFiles[title] is the set of that slice's
	// declared, slash-normalized scope.files. Only declaring slices count.
	sliceFiles := make(map[string]map[string]bool)
	for _, sp := range parsedPlan.Decomposition.SubPlans {
		if sp.Scope == nil {
			continue
		}
		files := make(map[string]bool, len(sp.Scope.Files))
		for _, f := range sp.Scope.Files {
			files[filepath.ToSlash(f.Path)] = true
		}
		sliceFiles[sp.Title] = files
	}

	var findings []CrossSliceCouplingFinding
	for _, p := range patterns {
		// Member-file set: Triggers ∪ Siblings, deduped.
		members := make(map[string]bool, len(p.Triggers)+len(p.Siblings))
		for _, m := range p.Triggers {
			members[filepath.ToSlash(m)] = true
		}
		for _, m := range p.Siblings {
			members[filepath.ToSlash(m)] = true
		}

		// Which member files each declaring slice owns.
		owned := make(map[string][]string)
		for title, files := range sliceFiles {
			for member := range members {
				if files[member] {
					owned[title] = append(owned[title], member)
				}
			}
		}
		if len(owned) < 2 {
			continue
		}

		claims := make([]CrossSliceClaim, 0, len(owned))
		for title, files := range owned {
			sort.Strings(files)
			claims = append(claims, CrossSliceClaim{SliceTitle: title, Files: files})
		}
		sort.Slice(claims, func(i, j int) bool { return claims[i].SliceTitle < claims[j].SliceTitle })
		findings = append(findings, CrossSliceCouplingFinding{
			Pattern: p.Name,
			Slices:  claims,
		})
	}
	return findings
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

	// The plan's declared surface_sweep_exemptions apply to the flat parent
	// scope AND every sub-plan scope (#1544): a top-level exemption suppresses
	// the matching missing-sibling finding wherever it would otherwise fire.
	exemptions := parsedPlan.SurfaceSweepExemptions

	findings, applied := evaluateSurfaceSweep(scopeFiles, surfacePatterns, exemptions)

	// Evaluate each decomposition sub-plan's own scope.files too (#1077):
	// an under-scoped slice's coupling gap must surface at the parent plan
	// gate the operator approves, since the fan-out child runs are
	// implement-only and never re-upload a plan. Findings and applied
	// exemptions are attributed to their sub-plan via SubPlanTitle; the
	// parent ScannedFiles count is unchanged. evaluateSurfaceSweep stays pure.
	if parsedPlan.Decomposition != nil {
		for _, sp := range parsedPlan.Decomposition.SubPlans {
			if sp.Scope == nil {
				continue
			}
			subFiles := make([]string, 0, len(sp.Scope.Files))
			for _, f := range sp.Scope.Files {
				subFiles = append(subFiles, f.Path)
			}
			subFindings, subApplied := evaluateSurfaceSweep(subFiles, surfacePatterns, exemptions)
			for _, f := range subFindings {
				f.SubPlanTitle = sp.Title
				findings = append(findings, f)
			}
			for _, a := range subApplied {
				a.SubPlanTitle = sp.Title
				applied = append(applied, a)
			}
		}
	}

	if findings == nil {
		// Marshal an empty array rather than null so the audit payload's
		// "checked and clean" state is explicit (a missing entry means
		// "never checked").
		findings = []SurfaceSweepFinding{}
	}

	if applied == nil {
		// Empty array (not null), same "checked and clean" rationale as
		// findings: an explicit "no exemption applied" rather than a
		// legacy plan that predates the field.
		applied = []AppliedExemption{}
	}

	// Cross-slice coupling pass (#1102): when a registered lockstep
	// pattern's member files are partitioned across distinct decomposition
	// slices, completing the seam would need a runtime scope amendment that
	// can time out (#1035). Pure evaluator, guarded on a decomposition;
	// normalize nil to an empty slice so the payload marshals an array.
	crossSlice := evaluateCrossSliceCoupling(parsedPlan, surfacePatterns)
	if crossSlice == nil {
		crossSlice = []CrossSliceCouplingFinding{}
	}

	result := &SurfaceSweepPayload{
		Findings:           findings,
		ScannedFiles:       len(scopeFiles),
		CrossSliceFindings: crossSlice,
		AppliedExemptions:  applied,
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
