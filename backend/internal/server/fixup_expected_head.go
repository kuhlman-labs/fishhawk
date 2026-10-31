package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// advanceFixupHeadThroughBaseMerges advances a fix-up's recorded head (the
// newest reported-head ledger entry, seedSHA at chain sequence seedSeq)
// through the run's Fishhawk-recorded base merges recorded AFTER it (E89.6 /
// #4574), so a fix-up dispatched after fishhawk_rebase_run_branch advertises
// the merge commit the branch tip now IS instead of failing the runner's
// ADR-035 check with fixup_base_mismatch. The walk itself is
// walkRecordedBaseMerges.
//
// On ANY read error it WARNs and returns seedSHA — the un-advanced recorded
// head, today's answer — and NEVER "": an empty expectation would switch the
// runner to its checkout-only path and skip the provenance comparison
// altogether, which is more permissive than refusing the pass.
func (s *Server) advanceFixupHeadThroughBaseMerges(ctx context.Context, runID, stageID uuid.UUID, seedSeq int64, seedSHA string) string {
	var rows []*audit.Entry
	for _, cat := range []string{CategoryBranchRebased, CategoryBranchReset} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"prompt: list base-merge audit entries failed; advertising the un-advanced recorded head",
				slog.String("field", "fixup_expected_head_sha"),
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			return seedSHA
		}
		rows = append(rows, entries...)
	}
	advanced := walkRecordedBaseMerges(rows, seedSeq, seedSHA)
	if advanced != seedSHA {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"prompt: fixup_expected_head_sha advanced through recorded base merges",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("seed_head_sha", seedSHA),
			slog.String("advanced_head_sha", advanced),
		)
	}
	return advanced
}

// walkRecordedBaseMerges is the pure fix-up base walk (E89.6 / #4574). rows
// are the run's branch_rebased and branch_reset entries (any other category
// is ignored); only those recorded AFTER the seed (Sequence > seedSeq) take
// part, walked in AUDIT-SEQUENCE order whatever order they are delivered in.
//
//   - A branch_rebased row advances the head to its merge commit ONLY when it
//     is a CLEAN advance under the shared ADR-112 predicate (isCleanAdvance:
//     a merge this advance performed, new head == merge commit, parents
//     verified as [prior head, base tip]) AND its prior_head_sha is the head
//     walked so far. Every other branch_rebased row, an undecodable one
//     included, is skipped: it leaves the head where it was.
//   - A branch_reset row moves the head to its reset_to_sha when that sha is
//     already on the walked chain (the seed or a merge commit walked through).
//     A reset anywhere else, or an undecodable reset row, returns the SEED:
//     today's answer, never something more permissive.
//
// Threat model. The merge commit's provenance is the GitHub App's own
// merges-API call, recorded by the server, so it is run lineage in a way a
// pushed commit is not. clean_verified is what closes the lease-recheck ->
// merge TOCTOU window: the lease re-check runs BEFORE the merge, so a foreign
// push landing there becomes the merge commit's first parent, and only the
// verified parents [prior head, base tip] prove the merge carries no foreign
// commit. A concurrent push after the merge (new head != merge commit), an
// unverified or legacy advance (clean_verified false or absent), and a rebase
// of a head the walk never reached (prior head mismatch) all leave the
// expected head at the recorded head, so the runner's unchanged tip equality
// check still refuses the fix-up (ADR-035 preserved).
func walkRecordedBaseMerges(rows []*audit.Entry, seedSeq int64, seed string) string {
	ordered := make([]*audit.Entry, 0, len(rows))
	for _, e := range rows {
		if e == nil {
			continue
		}
		if e.Sequence <= seedSeq {
			continue
		}
		ordered = append(ordered, e)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })

	head := seed
	onChain := map[string]bool{seed: true}
	for _, e := range ordered {
		switch e.Category {
		case CategoryBranchRebased:
			var p branchRebasedAdvance
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if isCleanAdvance(p) && p.PriorHeadSHA == head {
				head = p.MergeCommitSHA
				onChain[head] = true
			}
		case CategoryBranchReset:
			var p struct {
				ResetToSHA string `json:"reset_to_sha"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || !onChain[p.ResetToSHA] {
				return seed
			}
			head = p.ResetToSHA
		}
	}
	return head
}
