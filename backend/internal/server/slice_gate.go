package server

import (
	"context"
	"log/slog"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Slice-gate unavailable reasons the server adds beside basedelta's surface
// refusals (surface_undeclared, invalid_surface:<entry>, surface_incomplete).
const sliceGateReasonSurfaceUnreadable = "surface_unreadable"

// resolveSliceGateConfig returns the decomposition-child slice-gate config
// (#5498) served as promptResponse.slice_gate: the repository's DECLARED
// verdict-transfer surface (ADR-124) reduced to the two lists the runner's
// one-sided slice question needs. The slice gate is a SECOND, explicitly
// weaker consumer of that surface (basedelta README): it departs from ADR-124
// D3's inert rule and has no ADR-098 D2 data-read edges or ADR-096 D1(d)
// shared-artifact rule, so SharedDirs are deliberately not served. The
// parent's fan-in merge-candidate pass stays the full verify (ADR-090 D3).
//
// nil (the key omitted, byte-identical to before) unless ALL hold: the stage
// is implement, the run is a decomposition child (DecomposedFrom set), the
// dispatch is not a fix-up, and it is not a merge-candidate verify pass. A
// runner then records slice_gate_not_offered and keeps today's full gate.
//
// Otherwise: a spec that does not parse serves unavailable_reason
// surface_unreadable (warn-logged); a refused declaration
// (basedelta.Surface.Refusal, the same guard Classify reads) serves that
// refusal, bounded to basedelta.MaxReasonLen bytes with control characters
// dropped. A usable declaration serves full_gate_paths = the sorted,
// de-duplicated union of the workflow spec path (a string VALUE, ADR-124
// D5), G, doc-check and opaque, and inert_paths = the inert class.
func (s *Server) resolveSliceGateConfig(ctx context.Context, runRow *run.Run, stage *run.Stage, fixup bool, mergeCandidate *mergeCandidateVerifyTrigger) *sliceGateConfig {
	if runRow == nil || stage == nil || stage.Type != run.StageTypeImplement ||
		runRow.DecomposedFrom == nil || fixup || mergeCandidate != nil {
		return nil
	}
	surface, err := basedelta.ResolveSurface(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for slice_gate config",
			slog.String("run_id", runRow.ID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return &sliceGateConfig{UnavailableReason: sliceGateReasonSurfaceUnreadable}
	}
	return sliceGateConfigFor(surface)
}

// sliceGateConfigFor is the pure projection of a resolved surface onto the
// served config (the golden testdata/wire/slice_gate_prompt.json pins it).
func sliceGateConfigFor(surface *basedelta.Surface) *sliceGateConfig {
	if r := surface.Refusal(); r != "" {
		return &sliceGateConfig{UnavailableReason: boundTransferDetail(r)}
	}
	seen := map[string]bool{}
	var full []string
	for _, list := range [][]string{{basedelta.WorkflowSpecPath}, surface.Global, surface.DocCheck, surface.Opaque} {
		for _, p := range list {
			if !seen[p] {
				seen[p] = true
				full = append(full, p)
			}
		}
	}
	sort.Strings(full)
	var inert []string
	if len(surface.Inert) > 0 {
		inert = append([]string(nil), surface.Inert...)
	}
	return &sliceGateConfig{FullGatePaths: full, InertPaths: inert}
}
