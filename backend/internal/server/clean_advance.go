package server

import (
	"encoding/json"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// branchRebasedAdvance is the decoded slice of a branch_rebased payload the
// ADR-112 R1(a)(ii) clean-advance predicate reads. CleanVerified is a pointer
// so a LEGACY row written before the parent check (no clean_verified key) is
// distinguishable from an explicit false — and neither is clean.
type branchRebasedAdvance struct {
	PriorHeadSHA    string `json:"prior_head_sha"`
	NewHeadSHA      string `json:"new_head_sha"`
	MergeCommitSHA  string `json:"merge_commit_sha"`
	AlreadyUpToDate bool   `json:"already_up_to_date"`
	CleanVerified   *bool  `json:"clean_verified"`
}

// isCleanAdvance is the ONE shared CLEAN predicate over a recorded
// branch_rebased row (ADR-112 R1(a)(ii), amending ADR-093 D6): the row
// records a merge THIS advance performed (not the already-up-to-date arm),
// the resolved new head IS the decoded merge commit (no concurrent push
// observed), AND the merge commit's parents were verified as exactly [prior
// head, base tip] (clean_verified true). A legacy row with no clean_verified
// key is NOT clean, so an approval on a pre-change head needs one
// re-approval rather than being carried on an unverified advance.
func isCleanAdvance(p branchRebasedAdvance) bool {
	return !p.AlreadyUpToDate &&
		p.MergeCommitSHA != "" &&
		p.NewHeadSHA == p.MergeCommitSHA &&
		p.CleanVerified != nil && *p.CleanVerified
}

// cleanChainReaches reports whether `to` is reachable from `from` through
// Fishhawk-recorded CLEAN advances only (ADR-112 R1(a)): the chain walk is
// SEEDED at a forge review's commit (`from`), never at a merge intent's
// anchored head. rows are the run's branch_rebased and
// conflict_resolution_pushed audit entries (any other category is ignored);
// they are walked in AUDIT-SEQUENCE order only — no forge-time comparison —
// whatever order they are delivered in.
//
//   - A CLEAN branch_rebased row (isCleanAdvance) whose prior_head_sha is a
//     carrier adds its new_head_sha as a carrier.
//   - A conflict_resolution_pushed row CLEARS every carrier: agent-resolved
//     conflict content is never carried by an approval of an earlier head.
//   - A row whose RESULTING head (branch_rebased new_head_sha,
//     conflict_resolution_pushed head_sha) IS the seed re-establishes the seed
//     alone: the review was submitted at that head, so the row preceded the
//     review and cannot invalidate it.
//
// A fix-up, a vouch, or any push not recorded as a clean branch_rebased row is
// never a carrier edge, so it breaks the chain by construction. An
// undecodable branch_rebased row carries nothing (fail closed).
//
// It returns whether `to` is a carrier and, when it is, the ordered chain
// heads from `from` to `to` (just [from] when from == to).
func cleanChainReaches(rows []*audit.Entry, from, to string) (bool, []string) {
	if from == "" || to == "" {
		return false, nil
	}
	ordered := make([]*audit.Entry, 0, len(rows))
	for _, e := range rows {
		if e != nil && (e.Category == CategoryBranchRebased || e.Category == CategoryConflictResolutionPushed) {
			ordered = append(ordered, e)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })

	// pred maps each carrier to the carrier it was advanced from; the seed
	// maps to "". Membership in pred IS the carrier set.
	pred := map[string]string{from: ""}
	for _, e := range ordered {
		switch e.Category {
		case CategoryBranchRebased:
			var p branchRebasedAdvance
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if _, ok := pred[p.PriorHeadSHA]; ok && isCleanAdvance(p) {
				if _, seen := pred[p.NewHeadSHA]; !seen {
					pred[p.NewHeadSHA] = p.PriorHeadSHA
				}
			}
			if p.NewHeadSHA == from {
				pred = map[string]string{from: ""}
			}
		case CategoryConflictResolutionPushed:
			var p struct {
				HeadSHA string `json:"head_sha"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			pred = map[string]string{}
			if p.HeadSHA == from {
				pred[from] = ""
			}
		}
	}
	if _, ok := pred[to]; !ok {
		return false, nil
	}
	chain := []string{to}
	for cur := to; cur != from; {
		cur = pred[cur]
		chain = append(chain, cur)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return true, chain
}
