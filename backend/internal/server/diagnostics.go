package server

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

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

	bundle := diagnostics.Collect(runRow, stages, auditEntries, currentVersionFacts())
	s.writeJSON(w, r, http.StatusOK, bundle)
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
