package server

import (
	"context"
	"log/slog"

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
// child. Children are sorted deterministically by sortDecomposedChildren
// (SliceIndex nil-last ascending, then CreatedAt ascending, then ID string) —
// the same order the run-concerns listing uses — so the rendered prompt is
// stable across repeated builds. Paths are deduped first-wins so two slices amending
// the same path render once.
//
// It is a projection of childApprovedAmendmentRecords, the ONE walk it shares
// with childApprovedAmendmentEntries (the decomposed-parent fix-up scope union,
// #4338), so the review provenance and the fix-up permission set can never
// disagree on which child amendments count.
func (s *Server) childApprovedAmendmentScopePaths(ctx context.Context, parentRunID uuid.UUID) []prompt.ChildAmendedScopePath {
	records := s.childApprovedAmendmentRecords(ctx, parentRunID)
	if len(records) == 0 {
		return nil
	}
	out := make([]prompt.ChildAmendedScopePath, 0, len(records))
	for _, r := range records {
		out = append(out, prompt.ChildAmendedScopePath{
			Path:        r.entry.Path,
			AmendmentID: r.amendmentID,
			ChildRunID:  r.childRunID,
			SliceIndex:  r.sliceIndex,
		})
	}
	return out
}

// childApprovedAmendmentEntries is the operation-preserving projection of the
// same #2820 walk (#4338): every path a decomposed parent's children were
// authorized to touch by an APPROVED mid-stage amendment, carrying the declared
// operation so a child's approved `create` folds into the parent's fix-up scope
// as create rather than being flattened to modify. Same order, same first-wins
// dedup, same fail-open degrades as childApprovedAmendmentScopePaths; nil for a
// run with no children.
func (s *Server) childApprovedAmendmentEntries(ctx context.Context, parentRunID uuid.UUID) []scopeamendment.PathEntry {
	records := s.childApprovedAmendmentRecords(ctx, parentRunID)
	if len(records) == 0 {
		return nil
	}
	out := make([]scopeamendment.PathEntry, 0, len(records))
	for _, r := range records {
		out = append(out, r.entry)
	}
	return out
}

// childAmendmentRecord is one path a decomposed child's APPROVED amendment
// authorized, with its provenance: the full PathEntry (path AND operation), the
// authorizing amendment id, the child run id, and the child's slice index.
type childAmendmentRecord struct {
	entry       scopeamendment.PathEntry
	amendmentID string
	childRunID  string
	sliceIndex  *int
}

// childApprovedAmendmentRecords is the #2820 walk both projections above read:
// the decomposed parent's children (listAllDecomposedChildren, in
// sortDecomposedChildren order), each child's APPROVED amendments, deduped by
// path first-source-wins. Its degrades are the ones documented on
// childApprovedAmendmentScopePaths.
func (s *Server) childApprovedAmendmentRecords(ctx context.Context, parentRunID uuid.UUID) []childAmendmentRecord {
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

	sortDecomposedChildren(children)

	var out []childAmendmentRecord
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
				out = append(out, childAmendmentRecord{
					entry:       p,
					amendmentID: a.ID.String(),
					childRunID:  childRunID,
					sliceIndex:  sliceIdx,
				})
			}
		}
	}
	return out
}
