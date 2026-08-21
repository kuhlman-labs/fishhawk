package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diagnostics"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// handleGetRunDiagnostics implements GET /v0/runs/{run_id}/diagnostics.
//
// Returns the product-facts-only diagnostic bundle for the run: the
// redaction-safe summary an operator attaches to an upstream Fishhawk
// product report (#1006). Pure read, no egress — the bundle carries
// structured facts only (run id, stage states, the failing stage's
// category + surface, audit sequence range, build versions + git SHAs,
// workflow spec hash, runner kind) and never any diffs, paths, prompts,
// free text, or audit payload bodies. See backend/internal/diagnostics.
//
// Since #1737 the bundle also carries a best-effort wedge_context block
// naming WHY a stuck run is stuck (red required checks, campaign item
// state + blocked dependents, a fan-in conflict marker). Assembling it
// is never allowed to fail the read — see collectWedgeFacts.
func (s *Server) handleGetRunDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"diagnostics endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"diagnostics endpoint requires a configured audit repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
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

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}

	auditEntries, err := s.cfg.AuditRepo.ListForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list audit failed", map[string]any{"error": err.Error()})
		return
	}

	bundle := diagnostics.CollectWithWedge(runRow, stages, auditEntries,
		currentVersionFacts(), s.collectWedgeFacts(r.Context(), runRow, stages))
	s.writeJSON(w, r, http.StatusOK, bundle)
}

// collectWedgeFacts assembles the caller-injected half of the wedge
// block (#1737): the run's red required-check context names and its
// campaign item linkage. Every read here is BEST-EFFORT — an unwired
// repository, a missing linkage, or a read error yields fewer facts and
// NEVER an error, because a wedge fact is a nice-to-have on a bundle
// whose job is to describe the run at all. Turning a 200 diagnostics
// read into a 500 because a campaign list failed would be strictly
// worse for the operator this endpoint exists to serve.
//
// Always returns non-nil, which is what opts the bundle into the wedge
// block; the collector still omits the block when nothing was found.
func (s *Server) collectWedgeFacts(ctx context.Context, runRow *run.Run, stages []*run.Stage) *diagnostics.WedgeFacts {
	w := &diagnostics.WedgeFacts{}
	if runRow == nil {
		return w
	}
	// reviewChecksFailed is already conservative on every gap: a nil or
	// empty RequiredChecksSnapshot (every local run — #2497) or an
	// unwired StageCheckRepo returns nil, so the block degrades to no
	// names rather than fabricating them.
	if reviewStage := firstStageOfType(stages, run.StageTypeReview); reviewStage != nil {
		w.BlockingChecks = s.reviewChecksFailed(ctx, runRow, reviewStage)
	}
	s.addCampaignWedgeFacts(ctx, runRow.ID, w)
	return w
}

// addCampaignWedgeFacts fills in the campaign half: the state of the
// item this run executes, and how many sibling items are blocked
// waiting on it. Silent on every degrade — nil repo, no linkage, read
// error — see collectWedgeFacts.
func (s *Server) addCampaignWedgeFacts(ctx context.Context, runID uuid.UUID, w *diagnostics.WedgeFacts) {
	if s.cfg.CampaignRepo == nil {
		return
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForRun(ctx, runID)
	if err != nil || len(items) == 0 || items[0] == nil {
		return
	}
	item := items[0]
	w.CampaignItemState = string(item.State)

	siblings, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(ctx, item.CampaignID)
	if err != nil {
		return
	}
	for _, sib := range siblings {
		if sib == nil || sib.ID == item.ID || sib.State != campaign.ItemStateBlocked {
			continue
		}
		for _, dep := range sib.DependsOn {
			if dep == item.IssueRef {
				w.BlockedDependents++
				break
			}
		}
	}
}

// firstStageOfType returns the first stage of the given type, or nil.
// The handler already holds the run's stages, so the review stage is
// resolved from that slice rather than re-listing.
func firstStageOfType(stages []*run.Stage, t run.StageType) *run.Stage {
	for _, st := range stages {
		if st != nil && st.Type == t {
			return st
		}
	}
	return nil
}

// currentVersionFacts snapshots this binary's build identity for the
// bundle. The fishhawkd version + git SHA come from internal/version
// (stamped by scripts/dev / release ldflags; "dev"/"unknown" when
// unstamped). The runner's own reported version is not persisted on
// the run row in v0, so only the minimum-runner requirement is carried.
func currentVersionFacts() diagnostics.VersionFacts {
	return diagnostics.VersionFacts{
		Fishhawkd: diagnostics.Component{
			Version: version.Version,
			GitSHA:  version.GitSHA,
		},
		MinRunnerVersion: version.MinRunnerVersion,
	}
}
