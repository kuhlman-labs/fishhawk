package server

import (
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// ADR-117 A1.7 reviewed-head record (E94.3 / #4694): buildReviewedHead's pure
// classification and head resolution. Fixture repository acme/widgets
// (ADR-110); the paths below are that repository's.

// reviewedHeadTruncatedDiff is a REAL runner-truncated diff carrying both
// decorations deriveReviewDiffTruncation renders: A's header survives and is
// not the last, B's header is the LAST surviving one (its tail may be cut),
// and C has no header at all.
func reviewedHeadTruncatedDiff() policy.Diff {
	return policy.Diff{
		PatchTruncated: true,
		Patch: gitHeader("widgets/a.go") + "@@ -1 +1 @@\n-x\n+y\n" +
			gitHeader("widgets/b.go") + "@@ -1 +1 @@\n-x\n+",
		ChangedFiles: []policy.ChangedFile{
			{Path: "widgets/a.go", Status: policy.StatusModified},
			{Path: "widgets/b.go", Status: policy.StatusModified},
			{Path: "widgets/c.go", Status: policy.StatusAdded},
		},
	}
}

// TestReviewedHead_RenderedDiffSetsFromRealTruncation pins the three plain-path
// sets against the SAME truncation the audit and prompt render: A in files, B
// (the last surviving header) in partial_files, C (no header) in omitted_files —
// plain paths, pairwise disjoint, union == ChangedFiles in order.
//
// COUNTERFACTUAL CF-A2: trace.go `partial = append(partial, f.Path)` →
// `full = append(full, f.Path)` puts B in files → RED.
func TestReviewedHead_RenderedDiffSetsFromRealTruncation(t *testing.T) {
	diff := reviewedHeadTruncatedDiff()
	// The decorated audit list the same diff derives (the existing #2875
	// surface) names exactly B and C, so both decorations are in play.
	trunc, ok := deriveReviewDiffTruncation(diff)
	if !ok || !reflect.DeepEqual(trunc.OmittedFiles, []string{
		"widgets/b.go (may be cut — its tail may be missing)",
		"widgets/c.go (no hunks shown)",
	}) {
		t.Errorf("decorated truncation = %+v ok=%v, want B cut + C no hunks", trunc, ok)
	}

	src := reviewRoundSource{Origin: reviewRoundOriginTrace, HeadSHA: "verify-head"}
	rh := buildReviewedHead(src, true, "dedup-head", "tree-1", "change-1", diff, false)

	if !reflect.DeepEqual(rh.Files, []string{"widgets/a.go"}) {
		t.Errorf("files = %v, want [widgets/a.go]", rh.Files)
	}
	if !reflect.DeepEqual(rh.PartialFiles, []string{"widgets/b.go"}) {
		t.Errorf("partial_files = %v, want [widgets/b.go] (the last surviving header)", rh.PartialFiles)
	}
	if !reflect.DeepEqual(rh.OmittedFiles, []string{"widgets/c.go"}) {
		t.Errorf("omitted_files = %v, want [widgets/c.go]", rh.OmittedFiles)
	}
	if !rh.PatchRendered || !rh.Truncated || rh.InventoryBestEffort {
		t.Errorf("patch_rendered/truncated/best_effort = %v/%v/%v, want true/true/false (runner cap)", rh.PatchRendered, rh.Truncated, rh.InventoryBestEffort)
	}
	if rh.ChangedFileCount != 3 || rh.DiffMode != planreview.ReviewedHeadDiffFull {
		t.Errorf("changed_file_count = %d diff_mode = %q, want 3 / full", rh.ChangedFileCount, rh.DiffMode)
	}
	if rh.Status != planreview.ReviewedHeadRecorded || rh.Kind != planreview.ReviewedHeadKindWipTree || rh.SHA != "verify-head" {
		t.Errorf("head = %+v, want recorded wip_tree verify-head (never the dedup head)", rh)
	}
	assertReviewedHeadSetsPartition(t, rh, diff)

	// A forge-truncated (best-effort) inventory is flagged.
	diff.PatchTruncationReason = "oversized compare"
	if rh := buildReviewedHead(src, true, "", "tree-1", "", diff, false); !rh.InventoryBestEffort {
		t.Error("forge-truncated diff: inventory_best_effort = false, want true")
	}
}

// assertReviewedHeadSetsPartition asserts the three sets are plain, pairwise
// disjoint and that their union is exactly diff's distinct ChangedFiles paths.
func assertReviewedHeadSetsPartition(t *testing.T, rh *planreview.ReviewedHead, diff policy.Diff) {
	t.Helper()
	seen := map[string]int{}
	for _, set := range [][]string{rh.Files, rh.PartialFiles, rh.OmittedFiles} {
		for _, p := range set {
			seen[p]++
		}
	}
	want := map[string]bool{}
	for _, f := range diff.ChangedFiles {
		want[f.Path] = true
	}
	if len(seen) != len(want) {
		t.Errorf("union = %v, want exactly %v", seen, want)
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("path %q appears in %d sets, want exactly 1", p, n)
		}
		if !want[p] {
			t.Errorf("path %q is not a changed path (decorated or foreign)", p)
		}
	}
}

// TestReviewedHead_EmptyPatchAllOmitted: a diff with no patch text rendered
// nothing, so every changed path is omitted and patch_rendered is false.
func TestReviewedHead_EmptyPatchAllOmitted(t *testing.T) {
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{
		{Path: "widgets/a.go", Status: policy.StatusModified},
		{Path: "widgets/b.go", Status: policy.StatusModified},
		{Path: "widgets/a.go", Status: policy.StatusModified}, // a repeated path is recorded once
	}}
	rh := buildReviewedHead(reviewRoundSource{Origin: reviewRoundOriginFixupPush, BaseSHA: "base-1"}, true, "pushed-1", "", "", diff, false)
	if rh.PatchRendered || rh.Truncated {
		t.Errorf("patch_rendered/truncated = %v/%v, want false/false", rh.PatchRendered, rh.Truncated)
	}
	if !reflect.DeepEqual(rh.OmittedFiles, []string{"widgets/a.go", "widgets/b.go"}) || len(rh.Files) != 0 || len(rh.PartialFiles) != 0 {
		t.Errorf("sets = files %v partial %v omitted %v, want everything omitted once", rh.Files, rh.PartialFiles, rh.OmittedFiles)
	}
	if rh.ChangedFileCount != 2 {
		t.Errorf("changed_file_count = %d, want 2 distinct paths", rh.ChangedFileCount)
	}
	if rh.Status != planreview.ReviewedHeadRecorded || rh.Kind != planreview.ReviewedHeadKindPushedCommit || rh.SHA != "pushed-1" || rh.BaseSHA != "base-1" {
		t.Errorf("head = %+v, want recorded pushed_commit pushed-1 base base-1", rh)
	}

	// A complete patch puts every path in files.
	full := policy.Diff{
		Patch:        gitHeader("widgets/a.go") + "@@ -1 +1 @@\n-x\n+y\n",
		ChangedFiles: []policy.ChangedFile{{Path: "widgets/a.go", Status: policy.StatusModified}},
	}
	rh = buildReviewedHead(reviewRoundSource{Origin: reviewRoundOriginConsolidated, BaseSHA: "b"}, true, "h", "", "", full, true)
	if !reflect.DeepEqual(rh.Files, []string{"widgets/a.go"}) || !rh.PatchRendered || rh.Truncated || rh.DiffMode != planreview.ReviewedHeadDiffDelta {
		t.Errorf("complete delta patch = %+v, want files [widgets/a.go], rendered, not truncated, diff_mode delta", rh)
	}
	if rh.Kind != planreview.ReviewedHeadKindCompareHead || rh.SHA != "h" {
		t.Errorf("consolidated head = kind %q sha %q, want compare_head / h", rh.Kind, rh.SHA)
	}
}

// TestReviewedHead_HeadUnknownReasons: one case per head_unknown reason. Each
// records an empty sha and STILL records its rendered-diff sets.
func TestReviewedHead_HeadUnknownReasons(t *testing.T) {
	diff := policy.Diff{
		Patch:        gitHeader("widgets/a.go") + "@@ -1 +1 @@\n-x\n+y\n",
		ChangedFiles: []policy.ChangedFile{{Path: "widgets/a.go", Status: policy.StatusModified}},
	}
	cases := []struct {
		name           string
		src            reviewRoundSource
		ok             bool
		head, tree     string
		wantReason     string
		wantKindOrigin bool
	}{
		{name: "no round source", ok: false, head: "h", tree: "t", wantReason: reviewedHeadReasonNoRoundSource},
		{name: "unrecognised origin", src: reviewRoundSource{Origin: "bogus", HeadSHA: "h"}, ok: true, head: "h", tree: "t", wantReason: reviewedHeadReasonNoRoundSource},
		{name: "compare reported no commits", src: reviewRoundSource{Origin: reviewRoundOriginConsolidated, BaseSHA: "b", HeadUnresolvedReason: reviewedHeadReasonCompareNoCommits}, ok: true, wantReason: reviewedHeadReasonCompareNoCommits, wantKindOrigin: true},
		{name: "compare commits truncated", src: reviewRoundSource{Origin: reviewRoundOriginConsolidated, BaseSHA: "b", HeadUnresolvedReason: reviewedHeadReasonCompareCommitsTruncated}, ok: true, head: "listed-head", wantReason: reviewedHeadReasonCompareCommitsTruncated, wantKindOrigin: true},
		{name: "trace with no verify head", src: reviewRoundSource{Origin: reviewRoundOriginTrace}, ok: true, head: "dedup-head", tree: "t", wantReason: reviewedHeadReasonNoVerifyHead, wantKindOrigin: true},
		{name: "fix-up with no head", src: reviewRoundSource{Origin: reviewRoundOriginFixupPush, BaseSHA: "b"}, ok: true, wantReason: reviewedHeadReasonNoHead, wantKindOrigin: true},
		{name: "trace with no verify tree", src: reviewRoundSource{Origin: reviewRoundOriginTrace, HeadSHA: "verify-head"}, ok: true, head: "dedup-head", wantReason: reviewedHeadReasonNoVerifyTree, wantKindOrigin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := buildReviewedHead(tc.src, tc.ok, tc.head, tc.tree, "", diff, false)
			if rh.Status != planreview.ReviewedHeadUnknown || rh.UnknownReason != tc.wantReason {
				t.Errorf("status/reason = %q/%q, want head_unknown/%s", rh.Status, rh.UnknownReason, tc.wantReason)
			}
			if rh.SHA != "" {
				t.Errorf("sha = %q, want empty on head_unknown", rh.SHA)
			}
			if tc.wantKindOrigin != (rh.Kind != "" && rh.Origin == tc.src.Origin) {
				t.Errorf("kind/origin = %q/%q, want present=%v", rh.Kind, rh.Origin, tc.wantKindOrigin)
			}
			if !reflect.DeepEqual(rh.Files, []string{"widgets/a.go"}) || !rh.PatchRendered {
				t.Errorf("head_unknown round must still record its sets: %+v", rh)
			}
		})
	}
}

// TestClassifyTruncatedPatchFiles_DecoratedListRenderedFromClassifier pins that
// the decorated #2875 list is rendered from the classifier in ChangedFiles
// order, interleaving the two decorations exactly as before A1.7.
func TestClassifyTruncatedPatchFiles_DecoratedListRenderedFromClassifier(t *testing.T) {
	diff := policy.Diff{
		PatchTruncated: true,
		Patch:          gitHeader("widgets/b.go") + "@@ -1 +1 @@\n-x\n+y\n" + gitHeader("widgets/d.go") + "@@",
		ChangedFiles: []policy.ChangedFile{
			{Path: "widgets/a.go"}, {Path: "widgets/b.go"}, {Path: "widgets/d.go"}, {Path: "widgets/e.go"},
		},
	}
	full, partial, omitted := classifyTruncatedPatchFiles(diff)
	if !reflect.DeepEqual(full, []string{"widgets/b.go"}) || !reflect.DeepEqual(partial, []string{"widgets/d.go"}) || !reflect.DeepEqual(omitted, []string{"widgets/a.go", "widgets/e.go"}) {
		t.Errorf("classify = full %v partial %v omitted %v", full, partial, omitted)
	}
	want := []string{
		"widgets/a.go (no hunks shown)",
		"widgets/d.go (may be cut — its tail may be missing)",
		"widgets/e.go (no hunks shown)",
	}
	if got := truncatedPatchOmittedFiles(diff); !reflect.DeepEqual(got, want) {
		t.Errorf("decorated list = %v, want %v (ChangedFiles order)", got, want)
	}
}
