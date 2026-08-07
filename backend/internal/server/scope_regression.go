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

// categoryPlanScopeRegression is the audit-log category for the entry
// runScopeRegression writes on a revise pass when it diffs the new plan's
// scoped paths against the revision-base plan (#1257). fishhawk_revise_plan
// regenerates the WHOLE plan artifact, so a narrowly-scoped revision
// constraint can silently DROP files that were present in the
// immediately-prior plan even when it says "keep everything else". The
// entry surfaces that drop to the reviewers and the operator before
// approval, AND is the durable signal the revise handler counts to refund
// the normal revise budget for a regressing pass (countRegressedRevisePasses).
const categoryPlanScopeRegression = "plan_scope_regression"

// ScopeRegressionPayload is the audit-payload shape for a
// plan_scope_regression entry (#1257, refusal semantics #2516).
// RemovedFiles is the set of scoped paths present in the revision-base plan
// but absent from the new plan — EVERY dropped path, declared or not, so its
// meaning is unchanged for existing readers. DeclaredRemovals is the subset
// the new plan declared in scope_removals; UndeclaredRemovals is the
// remainder — the refusable narrowing. AddedFiles is the inverse of removed
// (new minus base), carried for context. All four are marshalled as empty
// arrays (not null) on a clean diff, mirroring surface_sweep's "checked and
// clean vs never checked" rationale. ScannedFiles is the count of the new
// plan's scoped paths.
//
// Regressed is len(UndeclaredRemovals) > 0 (#2516), NOT len(RemovedFiles) —
// it is the bit the plan gate's refusal and the revise budget refund both key
// on, and a DECLARED drop is not a mistake: it neither refuses nor refunds.
type ScopeRegressionPayload struct {
	RemovedFiles       []string `json:"removed_files"`
	DeclaredRemovals   []string `json:"declared_removals"`
	UndeclaredRemovals []string `json:"undeclared_removals"`
	AddedFiles         []string `json:"added_files"`
	ScannedFiles       int      `json:"scanned_files"`
	Regressed          bool     `json:"regressed"`
}

// scopedPaths returns the slash-normalized, sorted, de-duplicated UNION of
// the plan's top-level scope.files paths, every decomposition sub-plan's
// scope.files paths, AND every split_proposal phase's scope.files paths. The
// observed regression (#1257) dropped files that lived in decomposition
// sub-plan slice scopes, not the flat top-level list, so the diff MUST cover
// sub-plan scopes — diffing only the top-level scope.files would miss the
// exact bug class. Split-phase scopes joined the union with the refusal
// (#2516): under an advisory, reporting a file MOVED into a split phase as
// dropped was a tolerable false positive; under a refusal it would refuse a
// revise that merely rearranged the same file set. This matches the sibling
// plan gate's planGateScopePaths, which already unions all three. Pure: no
// Server receiver, no I/O.
func scopedPaths(p *plan.Plan) []string {
	set := make(map[string]bool)
	for _, f := range p.Scope.Files {
		set[filepath.ToSlash(f.Path)] = true
	}
	if p.Decomposition != nil {
		for _, sp := range p.Decomposition.SubPlans {
			if sp.Scope == nil {
				continue
			}
			for _, f := range sp.Scope.Files {
				set[filepath.ToSlash(f.Path)] = true
			}
		}
	}
	if p.SplitProposal != nil {
		for _, ph := range p.SplitProposal.Phases {
			if ph.Scope == nil {
				continue
			}
			for _, f := range ph.Scope.Files {
				set[filepath.ToSlash(f.Path)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runScopeRegression diffs a revise pass's new plan against the
// revision-base plan and records the result as a plan_scope_regression audit
// entry (#1257). RemovedFiles (base-scoped minus new-scoped) is every dropped
// path; subtracting the paths the new plan DECLARED in scope_removals splits
// it into DeclaredRemovals and UndeclaredRemovals (#2516). AddedFiles is the
// inverse, for context. It runs ONLY on a revise pass — the caller passes a
// non-nil base only when a prior plan_revised entry marks this ship as a
// revise (see handleShipPlan), so a base==nil non-revise ship skips and
// writes nothing.
//
// THIS FUNCTION stays advisory and fail-open, mirroring the sibling gates'
// degradation contract: a nil AuditRepo, a parse failure, or an audit-append
// failure WARN-logs and returns nil (never blocks or unwinds the ship). The
// refusal built on top of it lives in the caller (tryScopeRetry, plan.go),
// which reads Regressed — now keyed on the UNDECLARED removals — and
// re-dispatches the plan stage once rather than admitting the narrowed plan
// to review. The returned payload is also threaded into the plan-review
// prompt's gate-evidence section so BOTH the reviewers and the operator see
// the drop before approving; nil on every fail-open path.
func (s *Server) runScopeRegression(ctx context.Context, runID, stageID uuid.UUID, base *plan.Plan, newBody []byte) *ScopeRegressionPayload {
	// Non-revise ship (no revision base) → nothing to diff.
	if base == nil {
		return nil
	}
	if s.cfg.AuditRepo == nil {
		return nil
	}

	// Validation already passed in handleShipPlan; a parse failure here is
	// an internal inconsistency — log and skip rather than block.
	parsedNew, err := plan.Parse(newBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope regression: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	baseScoped := scopedPaths(base)
	newScoped := scopedPaths(parsedNew)
	newSet := make(map[string]bool, len(newScoped))
	for _, p := range newScoped {
		newSet[p] = true
	}
	baseSet := make(map[string]bool, len(baseScoped))
	for _, p := range baseScoped {
		baseSet[p] = true
	}

	// removed = base minus new (the regression); added = new minus base.
	// Both inherit baseScoped/newScoped's sorted order. Empty arrays (not
	// nil) so the audit payload marshals [] on a clean diff.
	removed := []string{}
	for _, p := range baseScoped {
		if !newSet[p] {
			removed = append(removed, p)
		}
	}
	added := []string{}
	for _, p := range newScoped {
		if !baseSet[p] {
			added = append(added, p)
		}
	}

	// Declared narrowing (#2516): subtract the paths the new plan DECLARED in
	// scope_removals from the dropped set. A declaration only counts when the
	// path was actually dropped — an entry naming a still-scoped path is a
	// harmless no-op, exactly as an unmatched surface_sweep_exemption is —
	// and paths are slash-normalized on both sides so a declaration written
	// with OS separators still matches. Both lists inherit removed's sorted
	// order and marshal as [] (not null).
	declaredSet := make(map[string]bool, len(parsedNew.ScopeRemovals))
	for _, sr := range parsedNew.ScopeRemovals {
		declaredSet[filepath.ToSlash(sr.Path)] = true
	}
	declaredRemovals := []string{}
	undeclaredRemovals := []string{}
	for _, p := range removed {
		if declaredSet[p] {
			declaredRemovals = append(declaredRemovals, p)
			continue
		}
		undeclaredRemovals = append(undeclaredRemovals, p)
	}

	result := &ScopeRegressionPayload{
		RemovedFiles:       removed,
		DeclaredRemovals:   declaredRemovals,
		UndeclaredRemovals: undeclaredRemovals,
		AddedFiles:         added,
		ScannedFiles:       len(newScoped),
		// A DECLARED drop is not a regression: it neither triggers the plan
		// gate's refusal nor refunds the revise budget (#2516).
		Regressed: len(undeclaredRemovals) > 0,
	}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanScopeRegression,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		// Fail-open: the entry is BOTH the reviewer/operator signal and the
		// budget-refund counter, so without it neither fires — but a failed
		// append must never unwind the ship. Return nil so the gate-evidence
		// threading omits the (unrecorded) block.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "scope regression: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
		return nil
	}
	return result
}
