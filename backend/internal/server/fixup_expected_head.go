package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// fixupHeadWalkCategories are the audit categories the recorded fix-up head
// walk reads after its seed: the recorded base merges (E89.6 / #4574) and the
// operator vouches (#4856), of which only a server-verified fast-forward vouch
// moves the head (isVerifiedVouchAdvance). The acceptance walk
// (acceptanceBaseMergeRows) does not read the vouch category yet; #4874 adds it.
var fixupHeadWalkCategories = []string{CategoryBranchRebased, CategoryBranchReset, CategoryOperatorCommitVouched}

// vouchAdvanceRow is the decoded slice of an operator_commit_vouched payload
// the recorded head walk reads. HeadAdvanceVerified is a pointer so a legacy
// row (no head_advance_verified key) is distinguishable from an explicit false,
// and neither advances. The json tags are the auditcomplete seam constants
// (VouchedSHAField, VouchPriorHeadSHAField, VouchHeadAdvanceVerifiedField);
// TestVouchAdvanceRow_DecodesSeamKeys pins that they agree.
type vouchAdvanceRow struct {
	VouchedSHA          string `json:"vouched_sha"`
	PriorHeadSHA        string `json:"prior_head_sha"`
	HeadAdvanceVerified *bool  `json:"head_advance_verified"`
}

// isVerifiedVouchAdvance is the ONE predicate over a recorded vouch row that
// makes it a head edge (#4856): the handler proved, against the forge, that
// vouched_sha fast-forwards prior_head_sha and that every commit between them
// is already run lineage, and stamped head_advance_verified true. Every other
// vouch row is membership-only.
func isVerifiedVouchAdvance(p vouchAdvanceRow) bool {
	return p.VouchedSHA != "" && p.PriorHeadSHA != "" && p.HeadAdvanceVerified != nil && *p.HeadAdvanceVerified
}

// advanceFixupHeadThroughBaseMerges advances a fix-up's recorded head (the
// newest reported-head ledger entry, seedSHA at chain sequence seedSeq)
// through the run's Fishhawk-recorded base merges and verified vouches
// recorded AFTER it (E89.6 / #4574, #4856), so a fix-up dispatched after
// fishhawk_rebase_run_branch, or after a verified fishhawk_vouch_commit,
// advertises the commit the branch tip now IS instead of failing the runner's
// ADR-035 check with fixup_base_mismatch. The walk itself is
// walkRecordedBaseMerges, which the acceptance head (acceptanceHeadAsOf) also
// walks; only this fix-up read loop, WARN and seed fallback are fix-up-only.
//
// On ANY read error it WARNs and returns seedSHA — the un-advanced recorded
// head, today's answer — and NEVER "": an empty expectation would switch the
// runner to its checkout-only path and skip the provenance comparison
// altogether, which is more permissive than refusing the pass.
func (s *Server) advanceFixupHeadThroughBaseMerges(ctx context.Context, runID, stageID uuid.UUID, seedSeq int64, seedSHA string) string {
	var rows []*audit.Entry
	for _, cat := range fixupHeadWalkCategories {
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
			"prompt: fixup_expected_head_sha advanced through recorded base merges and verified vouches",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("seed_head_sha", seedSHA),
			slog.String("advanced_head_sha", advanced),
		)
	}
	return advanced
}

// walkRecordedBaseMerges is the pure recorded base walk shared by the fix-up
// seed (E89.6 / #4574) and the acceptance head (acceptanceHeadAsOf, #4874). rows
// are the run's branch_rebased, branch_reset and operator_commit_vouched
// entries (any other category is ignored; the acceptance resolvers deliver no
// vouch rows until #4874 adds them); only those recorded AFTER the seed
// (Sequence > seedSeq) take part, walked in AUDIT-SEQUENCE order whatever order
// they are delivered in.
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
//   - An operator_commit_vouched row (#4856) advances the head to its
//     vouched_sha ONLY when it is a server-verified fast-forward vouch
//     (isVerifiedVouchAdvance) AND its prior_head_sha is the head walked so
//     far; the vouched sha then joins the walked chain, so a later reset to it
//     stays on the chain. Every other vouch row leaves the head where it was:
//     a legacy row, the rebase verb's attribution row
//     (writeRebaseLineageAttribution never writes the advance keys), an
//     unverified row, an undecodable row, and a verified row whose prior is
//     not the walked head. This is an ADR-112-style step in the recorded walk;
//     any other vouch stays membership-only (ADR-035).
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
//
// A vouch is operator-authored and the handler records it for ANY well-formed
// sha, so the row alone proves nothing. Only the handler's forge-verified stamp
// (probeVouchHeadAdvance: the vouched commit is a fast-forward of the recorded
// head, and every commit between them is already run lineage, counting only
// run-reported pushes, recorded clean merges and EARLIER verified vouches)
// makes it an edge. Commits are immutable and the audit chain is append-only,
// so the stamp's ancestry and closure proof cannot go stale; a push after the
// vouched tip still fails the runner's unchanged equality check, and a vouch
// whose stamped prior is no longer the walked head (a newer seed, a rebase in
// between) is skipped.
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
		case CategoryOperatorCommitVouched:
			var p vouchAdvanceRow
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if isVerifiedVouchAdvance(p) && p.PriorHeadSHA == head {
				head = p.VouchedSHA
				onChain[head] = true
			}
		}
	}
	return head
}

// newestReportedHeadEntry is the fix-up seed selection over reported-head
// ledger rows: the newest by TIMESTAMP, then by chain sequence within an equal
// timestamp, among rows carrying a decodable, non-empty head_sha. It is pure;
// prompt.go newestReportedHead and recordedFixupHead both select through it, so
// the dispatch-time fix-up expectation and the vouch probe's prior head share
// one seed rule. Returns (nil, "") when no row qualifies.
func newestReportedHeadEntry(entries []*audit.Entry) (*audit.Entry, string) {
	var newest *audit.Entry
	var newestSHA string
	for _, e := range entries {
		if e == nil {
			continue
		}
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.HeadSHA == "" {
			continue
		}
		if newest == nil || e.Timestamp.After(newest.Timestamp) ||
			(e.Timestamp.Equal(newest.Timestamp) && e.Sequence > newest.Sequence) {
			newest = e
			newestSHA = payload.HeadSHA
		}
	}
	return newest, newestSHA
}

// recordedFixupHead is the STRICT twin of resolveFixupExpectedHeadSHA (#4856):
// the same seed rule (newestReportedHeadEntry over lineageLedgerCategories) and
// the same walk (walkRecordedBaseMerges over fixupHeadWalkCategories), but it
// RETURNS the first read error instead of degrading to the seed, so the vouch
// probe never stamps a prior head computed from a partial read. It answers
// ("", nil) when the AuditRepo is unconfigured or the run has no seed.
func (s *Server) recordedFixupHead(ctx context.Context, runID uuid.UUID) (string, error) {
	if s.cfg.AuditRepo == nil {
		return "", nil
	}
	var reported []*audit.Entry
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return "", fmt.Errorf("list reported-head rows (%s): %w", cat, err)
		}
		reported = append(reported, entries...)
	}
	rows, err := s.fixupHeadWalkRows(ctx, runID)
	if err != nil {
		return "", err
	}
	seed, seedSHA := newestReportedHeadEntry(reported)
	if seed == nil {
		return "", nil
	}
	return walkRecordedBaseMerges(rows, seed.Sequence, seedSHA), nil
}

// fixupHeadWalkRows reads every fixupHeadWalkCategories category for the run,
// returning the first read error.
func (s *Server) fixupHeadWalkRows(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	var rows []*audit.Entry
	for _, cat := range fixupHeadWalkCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return nil, fmt.Errorf("list recorded head walk rows (%s): %w", cat, err)
		}
		rows = append(rows, entries...)
	}
	return rows, nil
}
