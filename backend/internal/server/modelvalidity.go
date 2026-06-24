package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// checkModelValidityGate is the gate-time backstop of the model-id validity
// layer (#1339). It validates a single ALREADY-RESOLVED implement model against
// the deployment's snapshot oracle (s.cfg.ModelOracle), keyed by the run
// adapter, with the SAME fail-open semantics as spec.ValidateModels — so the
// plan-approval and fix-up gates reject a definitively-invalid model before the
// allow-list (validity → policy → pricing) without ever hard-failing in
// production today (the wired no-data oracle reports ok=false for every
// provider).
//
// Returns true to proceed. Returns false — after writing a 422 model_invalid —
// ONLY on a definitive authoritative absence: a fresh+ok snapshot exists for
// the adapter and the resolved model is not in it. Fails OPEN (returns true,
// writes nothing) on every ambiguous case: a nil oracle, an empty resolved
// model (today's default spawn), no snapshot for the adapter (ok=false), or a
// stale snapshot (fresh=false).
func (s *Server) checkModelValidityGate(w http.ResponseWriter, r *http.Request, stage *run.Stage, resolvedModel, adapter string) bool {
	if s.cfg.ModelOracle == nil || strings.TrimSpace(resolvedModel) == "" {
		return true
	}
	models, fresh, ok := s.cfg.ModelOracle.Snapshot(r.Context(), adapter)
	if !ok || !fresh {
		// No snapshot or a stale one: absence is not authoritative — fail open.
		return true
	}
	for _, m := range models {
		if m == resolvedModel {
			return true
		}
	}
	// Authoritative absence → hard reject.
	available := append([]string(nil), models...)
	sort.Strings(available)
	avail := "(none)"
	if len(available) > 0 {
		avail = strings.Join(available, ", ")
	}
	s.writeError(w, r, http.StatusUnprocessableEntity, "model_invalid",
		fmt.Sprintf("resolved implement model %q is not a known %q model; available: %s", resolvedModel, adapter, avail),
		map[string]any{
			"stage_id": stage.ID.String(),
			"model":    resolvedModel,
			"adapter":  adapter,
		})
	return false
}
