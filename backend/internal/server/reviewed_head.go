package server

import (
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// Reviewed-head unknown reasons (ADR-117 A1.7, E94.3 / #4694): why an
// implement-review round recorded reviewed_head.status = head_unknown.
const (
	// reviewedHeadReasonNoRoundSource: the dispatch context carried no
	// recognised round source, so the head's kind cannot be named.
	reviewedHeadReasonNoRoundSource = "no_round_source"
	// reviewedHeadReasonNoVerifyHead: a trace round whose bundle's
	// authoritative verify_run carried no head_sha (or no verify_run at all).
	reviewedHeadReasonNoVerifyHead = "no_verify_head"
	// reviewedHeadReasonNoHead: a compare-sourced round dispatched with an
	// empty head.
	reviewedHeadReasonNoHead = "no_head"
	// reviewedHeadReasonNoVerifyTree: a trace round with a verify head but no
	// verify tree_sha, so the wip_tree identity is incomplete.
	reviewedHeadReasonNoVerifyTree = "no_verify_tree"
	// reviewedHeadReasonCompareNoCommits: the consolidated compare reported no
	// commits ahead of base, so it named no head.
	reviewedHeadReasonCompareNoCommits = "compare_reported_no_commits"
	// reviewedHeadReasonCompareCommitsTruncated: the forge capped the
	// consolidated compare's commit listing, so its head is the last LISTED
	// commit, not the tip (forge.ComparePatchResult.CommitsTruncated).
	reviewedHeadReasonCompareCommitsTruncated = "compare_commits_truncated"
)

// reviewedHeadKindForOrigin maps a round origin to the kind of head its sha
// names; "" for an unrecognised origin.
func reviewedHeadKindForOrigin(origin string) string {
	switch origin {
	case reviewRoundOriginTrace:
		return planreview.ReviewedHeadKindWipTree
	case reviewRoundOriginFixupPush:
		return planreview.ReviewedHeadKindPushedCommit
	case reviewRoundOriginConsolidated:
		return planreview.ReviewedHeadKindCompareHead
	}
	return ""
}

// buildReviewedHead builds an implement-review round's reviewed_head record
// (ADR-117 A1.7). It is pure: src/ok are the round source the dispatch context
// carries (reviewRoundSourceFrom), headSHA/treeSHA/changeID are the values
// runImplementReviewsForTree was called with, reviewed is the diff the round
// ACTUALLY reviewed (the #1725 fix-up delta on a delta re-review, else the
// full diff) and delta marks a delta re-review.
//
// The head by origin: a trace round's sha is src.HeadSHA, the bundle's
// AUTHORITATIVE verify head (bundle.ExtractVerifyHead) — never headSHA, which
// is the #797 dedup key (the FIRST verify head); a fixup_push or consolidated
// round's sha is headSHA, the compare head it was dispatched against, with
// base_sha = src.BaseSHA. A round with no usable head records head_unknown
// with a reason and an empty sha, and STILL records its rendered-diff sets.
//
// The rendered-diff sets are plain paths, deduplicated in ChangedFiles order,
// pairwise disjoint, and their union is every distinct changed path: an empty
// patch puts every path in omitted_files (patch_rendered=false); a complete
// patch puts every path in files; a truncated patch uses
// classifyTruncatedPatchFiles, the same classification the truncation audit
// and prompt notice render from.
func buildReviewedHead(src reviewRoundSource, ok bool, headSHA, treeSHA, changeID string, reviewed policy.Diff, delta bool) *planreview.ReviewedHead {
	rh := &planreview.ReviewedHead{
		Status:       planreview.ReviewedHeadRecorded,
		DiffMode:     planreview.ReviewedHeadDiffFull,
		TreeSHA:      treeSHA,
		ChangeID:     changeID,
		Files:        []string{},
		PartialFiles: []string{},
		OmittedFiles: []string{},
	}
	if delta {
		rh.DiffMode = planreview.ReviewedHeadDiffDelta
	}

	var full, partial, omitted []string
	switch {
	case reviewed.Patch == "":
		for _, f := range reviewed.ChangedFiles {
			omitted = append(omitted, f.Path)
		}
	case reviewed.PatchTruncated:
		rh.PatchRendered = true
		rh.Truncated = true
		if trunc, tok := deriveReviewDiffTruncation(reviewed); tok {
			rh.InventoryBestEffort = trunc.BestEffort
		}
		full, partial, omitted = classifyTruncatedPatchFiles(reviewed)
	default:
		rh.PatchRendered = true
		for _, f := range reviewed.ChangedFiles {
			full = append(full, f.Path)
		}
	}
	seen := make(map[string]bool, len(reviewed.ChangedFiles))
	rh.Files = reviewedHeadAppendDistinct(rh.Files, full, seen)
	rh.PartialFiles = reviewedHeadAppendDistinct(rh.PartialFiles, partial, seen)
	rh.OmittedFiles = reviewedHeadAppendDistinct(rh.OmittedFiles, omitted, seen)
	rh.ChangedFileCount = len(seen)

	kind := ""
	if ok {
		kind = reviewedHeadKindForOrigin(src.Origin)
	}
	reason := ""
	if kind != "" {
		rh.Kind = kind
		rh.Origin = src.Origin
		if kind == planreview.ReviewedHeadKindWipTree {
			rh.SHA = src.HeadSHA
		} else {
			rh.SHA = headSHA
			rh.BaseSHA = src.BaseSHA
		}
	}
	switch {
	case kind == "":
		reason = reviewedHeadReasonNoRoundSource
	case src.HeadUnresolvedReason != "":
		reason = src.HeadUnresolvedReason
	case rh.SHA == "" && kind == planreview.ReviewedHeadKindWipTree:
		reason = reviewedHeadReasonNoVerifyHead
	case rh.SHA == "":
		reason = reviewedHeadReasonNoHead
	case kind == planreview.ReviewedHeadKindWipTree && treeSHA == "":
		reason = reviewedHeadReasonNoVerifyTree
	}
	if reason != "" {
		rh.Status = planreview.ReviewedHeadUnknown
		rh.UnknownReason = reason
		rh.SHA = ""
	}
	return rh
}

// reviewedHeadAppendDistinct appends each path of from not already in seen, in
// order, recording it in seen — so the three sets stay pairwise disjoint even
// when ChangedFiles repeats a path.
func reviewedHeadAppendDistinct(dst, from []string, seen map[string]bool) []string {
	for _, p := range from {
		if seen[p] {
			continue
		}
		seen[p] = true
		dst = append(dst, p)
	}
	return dst
}
