package server

import (
	"context"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// childApprovedAmendmentScopePaths returns a decomposed PARENT run's view of the
// paths its fan-out CHILDREN were authorized to touch via an APPROVED mid-stage
// scope amendment (#2820). A child slice's amendment lives under the CHILD run
// id, so a path an operator approved on a slice reaches the parent's
// consolidated implement-review diff with no provenance on the parent — it reads
// as an unexplained residual and the reviewer flags scope drift (#2237, #2239).
// This resolves the children's APPROVED amendments and returns per-path records
// carrying the authorizing amendment id, child run id, and slice index, so the
// review prompt can render them as in-scope (NOT drift) and the provenance fold
// can account for them instead of reporting a residual.
//
// It mirrors approvedAmendmentScopePaths' best-effort, fail-open posture: a
// provenance lookup must NEVER block a review. A nil RunRepo or
// ScopeAmendmentRepo, a listAllDecomposedChildren error, or a per-child
// ListByRun error all WARN-log and contribute nothing (the last only for the
// failing child — its siblings still resolve). An ordinary (non-decomposed) run
// has no children → returns nil with no allocation, keeping the prompt
// byte-identical.
//
// Children are discriminated by DecomposedFrom (via listAllDecomposedChildren),
// NOT ParentRunID: run.ChildParamsFrom sets parent_run_id for every child kind
// including recovery children, so ParentRunID would wrongly sweep in a recovery
// child. Children are sorted deterministically (SliceIndex nil-last ascending,
// then CreatedAt ascending, then ID string) so the rendered prompt is stable
// across repeated builds. Paths are deduped first-wins so two slices amending
// the same path render once.
func (s *Server) childApprovedAmendmentScopePaths(ctx context.Context, parentRunID uuid.UUID) []prompt.ChildAmendedScopePath {
	if s.cfg.RunRepo == nil || s.cfg.ScopeAmendmentRepo == nil {
		return nil
	}
	// listAllDecomposedChildren pages past the 100-row cap (consolidate.go), so a
	// wide fan-out is fully covered rather than silently truncated.
	children, err := s.listAllDecomposedChildren(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"implement review: list decomposition children failed — child-scope-amendment provenance contributes nothing for this run",
			slog.String("run_id", parentRunID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(children) == 0 {
		// Not a decomposed parent — an ordinary run pays nothing and renders
		// nothing (byte-identical prompt).
		return nil
	}

	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]
		ai, bi := a.SliceIndex, b.SliceIndex
		switch {
		case ai != nil && bi != nil:
			if *ai != *bi {
				return *ai < *bi
			}
		case ai != nil && bi == nil:
			return true // known slice index sorts before an unknown one
		case ai == nil && bi != nil:
			return false
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID.String() < b.ID.String()
	})

	var out []prompt.ChildAmendedScopePath
	seen := make(map[string]struct{})
	for _, child := range children {
		items, lerr := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, child.ID)
		if lerr != nil {
			// Best-effort per child: this child contributes nothing, but its
			// siblings still resolve — never an all-or-nothing early return.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"implement review: list child scope amendments failed — this child contributes nothing to child-scope-amendment provenance",
				slog.String("run_id", parentRunID.String()),
				slog.String("child_run_id", child.ID.String()),
				slog.String("error", lerr.Error()),
			)
			continue
		}
		var sliceIdx *int
		if child.SliceIndex != nil {
			v := *child.SliceIndex
			sliceIdx = &v
		}
		childRunID := child.ID.String()
		for _, a := range items {
			if a.Status != scopeamendment.StatusApproved {
				continue // pending / denied confer nothing
			}
			for _, p := range a.Paths {
				if _, ok := seen[p.Path]; ok {
					continue // first-source-wins dedup
				}
				seen[p.Path] = struct{}{}
				out = append(out, prompt.ChildAmendedScopePath{
					Path:        p.Path,
					AmendmentID: a.ID.String(),
					ChildRunID:  childRunID,
					SliceIndex:  sliceIdx,
				})
			}
		}
	}
	return out
}
