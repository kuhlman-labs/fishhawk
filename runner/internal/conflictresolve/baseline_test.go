package conflictresolve

import "testing"

const resolvedTwoWay = "alpha\nbeta\nours\ngamma\n"

// fixture builds a baseline and the matching clean observed state: one
// conflicted content file, one untouched index entry, and one entry git
// AUTO-STAGED from the clean side of the base merge (authorized by being in
// the baseline).
func fixture() (Baseline, Observed) {
	base := Baseline{
		HeadSHA:      "head-sha",
		MergeHeadSHA: "merge-head-sha",
		MergeMessage: "Merge remote-tracking branch 'origin/main' into run\n",
		Index: map[string]IndexEntry{
			"untouched.go":  {Mode: "100644", OID: "oid-untouched"},
			"autostaged.go": {Mode: "100644", OID: "oid-autostaged"},
		},
		Conflicted: map[string]ConflictedFile{
			"conflict.go": {Kind: KindContent, MarkerBytes: []byte(twoWay)},
		},
	}
	obs := Observed{
		HeadSHA:      base.HeadSHA,
		MergeHeadSHA: base.MergeHeadSHA,
		MergeMessage: base.MergeMessage,
		Index: map[string]IndexEntry{
			"untouched.go":  {Mode: "100644", OID: "oid-untouched"},
			"autostaged.go": {Mode: "100644", OID: "oid-autostaged"},
		},
		// git ls-files --unmerged reports one record per STAGE, so the caller
		// hands Verify a set that may repeat a path.
		Unmerged:  []string{"conflict.go", "conflict.go", "conflict.go"},
		Unstaged:  []string{"conflict.go"},
		Untracked: nil,
		Working:   map[string][]byte{"conflict.go": []byte(resolvedTwoWay)},
	}
	return base, obs
}

func TestVerify_AcceptsConfinedResolution(t *testing.T) {
	base, obs := fixture()
	if got := Verify(base, obs); len(got) != 0 {
		t.Fatalf("Verify returned %v, want no violations", got)
	}
}

func TestVerify_NamesOneReasonPerRule(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Baseline, *Observed)
		reason Reason
		path   string
	}{
		{
			name:   "head moved",
			mutate: func(_ *Baseline, o *Observed) { o.HeadSHA = "other-head" },
			reason: ReasonHeadMoved,
		},
		{
			name:   "merge head changed",
			mutate: func(_ *Baseline, o *Observed) { o.MergeHeadSHA = "other-merge-head" },
			reason: ReasonMergeHeadChanged,
		},
		{
			name:   "merge message changed",
			mutate: func(_ *Baseline, o *Observed) { o.MergeMessage = "Merge something else\n" },
			reason: ReasonMergeMessageChanged,
		},
		{
			name: "non-conflicted index entry changed",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["autostaged.go"] = IndexEntry{Mode: "100644", OID: "oid-tampered"}
			},
			reason: ReasonIndexEntryChanged,
			path:   "autostaged.go",
		},
		{
			name: "non-conflicted index entry mode changed",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["untouched.go"] = IndexEntry{Mode: "100755", OID: "oid-untouched"}
			},
			reason: ReasonIndexEntryChanged,
			path:   "untouched.go",
		},
		{
			name:   "non-conflicted index entry removed",
			mutate: func(_ *Baseline, o *Observed) { delete(o.Index, "untouched.go") },
			reason: ReasonIndexEntryRemoved,
			path:   "untouched.go",
		},
		{
			name: "newly staged path",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["smuggled.go"] = IndexEntry{Mode: "100644", OID: "oid-smuggled"}
			},
			reason: ReasonNewlyStagedPath,
			path:   "smuggled.go",
		},
		{
			name:   "unmerged entry outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Unmerged = append(o.Unmerged, "elsewhere.go") },
			reason: ReasonUnmergedEntryOutsideSet,
			path:   "elsewhere.go",
		},
		{
			name:   "conflicted path lost its unmerged stages",
			mutate: func(_ *Baseline, o *Observed) { o.Unmerged = nil },
			reason: ReasonConflictedPathNotUnmerged,
			path:   "conflict.go",
		},
		{
			name:   "unstaged change outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Unstaged = append(o.Unstaged, "elsewhere.go") },
			reason: ReasonUnstagedChangeOutsideSet,
			path:   "elsewhere.go",
		},
		{
			name:   "untracked path outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Untracked = []string{"scratch.txt"} },
			reason: ReasonUntrackedPathOutsideSet,
			path:   "scratch.txt",
		},
		{
			name: "resolution edits outside the hunk",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conflict.go"] = []byte("alpha\nbetb\nours\ngamma\n")
			},
			reason: ReasonOutsideHunk,
			path:   "conflict.go",
		},
		{
			name: "resolution leaves a residual marker",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conflict.go"] = []byte(twoWay)
			},
			reason: ReasonResidualMarker,
			path:   "conflict.go",
		},
		{
			name: "textually conflicted path deleted",
			mutate: func(_ *Baseline, o *Observed) {
				delete(o.Working, "conflict.go")
			},
			reason: ReasonConflictedPathMissing,
			path:   "conflict.go",
		},
		{
			name: "captured markers are malformed",
			mutate: func(b *Baseline, _ *Observed) {
				b.Conflicted["conflict.go"] = ConflictedFile{
					Kind:        KindContent,
					MarkerBytes: []byte("alpha\n<<<<<<< HEAD\nours\n"),
				}
			},
			reason: ReasonMalformedMarkers,
			path:   "conflict.go",
		},
		{
			name: "binary conflict is refused whatever the bytes",
			mutate: func(b *Baseline, o *Observed) {
				b.Conflicted["conflict.go"] = ConflictedFile{Kind: KindBinary}
				o.Working["conflict.go"] = []byte("\x00\x01ours")
			},
			reason: ReasonBinaryConflict,
			path:   "conflict.go",
		},
		{
			name: "delete/modify resolved to neither side",
			mutate: func(b *Baseline, o *Observed) {
				b.Conflicted["conflict.go"] = ConflictedFile{
					Kind:  KindDeleteModify,
					Sides: [][]byte{[]byte("modified\n")},
				}
				o.Working["conflict.go"] = []byte("invented\n")
			},
			reason: ReasonDeleteModifyContent,
			path:   "conflict.go",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, obs := fixture()
			tc.mutate(&base, &obs)
			got := Verify(base, obs)
			if len(got) != 1 {
				t.Fatalf("Verify returned %d violations %v, want exactly 1", len(got), got)
			}
			if got[0].Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", got[0].Reason, tc.reason)
			}
			if got[0].Path != tc.path {
				t.Fatalf("path = %q, want %q", got[0].Path, tc.path)
			}
			if got[0].Detail == "" {
				t.Fatal("violation carries no detail")
			}
		})
	}
}

func TestVerify_AcceptsDeleteModifyResolutions(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		content string
	}{
		{"keeps the modified side", true, "modified\n"},
		{"keeps the deletion", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, obs := fixture()
			base.Conflicted["conflict.go"] = ConflictedFile{
				Kind:  KindDeleteModify,
				Sides: [][]byte{[]byte("modified\n")},
			}
			if tc.present {
				obs.Working["conflict.go"] = []byte(tc.content)
			} else {
				delete(obs.Working, "conflict.go")
			}
			if got := Verify(base, obs); len(got) != 0 {
				t.Fatalf("Verify returned %v, want no violations", got)
			}
		})
	}
}

// TestVerify_ReportsAStagedConflictedPathOnce asserts the agent running
// `git add` on a conflicted path draws exactly ONE reason. The path leaves the
// unmerged set and arrives at stage 0, which two rules could both claim; the
// newly-staged sweep defers to the unmerged sweep so the refusal names the
// contract the agent actually broke.
func TestVerify_ReportsAStagedConflictedPathOnce(t *testing.T) {
	base, obs := fixture()
	obs.Unmerged = nil
	obs.Index["conflict.go"] = IndexEntry{Mode: "100644", OID: "oid-staged-by-agent"}
	got := Verify(base, obs)
	if len(got) != 1 || got[0].Reason != ReasonConflictedPathNotUnmerged || got[0].Path != "conflict.go" {
		t.Fatalf("Verify = %v, want one %q on conflict.go", got, ReasonConflictedPathNotUnmerged)
	}
}

// TestVerify_NamesNonConflictedIndexEntryChanged is the counterfactual vehicle
// for the index comparison: the tampered entry is seeded BY CONSTRUCTION.
func TestVerify_NamesNonConflictedIndexEntryChanged(t *testing.T) {
	base, obs := fixture()
	obs.Index["autostaged.go"] = IndexEntry{Mode: "100644", OID: "oid-tampered"}
	got := Verify(base, obs)
	if len(got) != 1 || got[0].Reason != ReasonIndexEntryChanged || got[0].Path != "autostaged.go" {
		t.Fatalf("Verify = %v, want one %q on autostaged.go", got, ReasonIndexEntryChanged)
	}
}

// TestVerify_NamesHeadMoved is the counterfactual vehicle for the HEAD
// comparison.
func TestVerify_NamesHeadMoved(t *testing.T) {
	base, obs := fixture()
	obs.HeadSHA = "advanced-head"
	got := Verify(base, obs)
	if len(got) != 1 || got[0].Reason != ReasonHeadMoved {
		t.Fatalf("Verify = %v, want one %q", got, ReasonHeadMoved)
	}
}

// TestVerify_NamesMergeHeadChanged is the counterfactual vehicle for the
// MERGE_HEAD comparison (approval condition 1). MERGE_HEAD feeds the merge
// commit's SECOND PARENT, and rewriting it leaves HEAD, the index and the
// working tree byte-identical — so nothing else in the gate can refuse it.
func TestVerify_NamesMergeHeadChanged(t *testing.T) {
	base, obs := fixture()
	obs.MergeHeadSHA = "substituted-second-parent"
	got := Verify(base, obs)
	if len(got) != 1 || got[0].Reason != ReasonMergeHeadChanged {
		t.Fatalf("Verify = %v, want one %q", got, ReasonMergeHeadChanged)
	}
}

// TestVerify_NamesMergeMessageChanged is the counterfactual vehicle for the
// merge-message-source comparison (approval condition 1).
func TestVerify_NamesMergeMessageChanged(t *testing.T) {
	base, obs := fixture()
	obs.MergeMessage = "Merge branch 'attacker' into run\n"
	got := Verify(base, obs)
	if len(got) != 1 || got[0].Reason != ReasonMergeMessageChanged {
		t.Fatalf("Verify = %v, want one %q", got, ReasonMergeMessageChanged)
	}
}

func TestVerify_ReportsEveryBrokenRuleInDeterministicOrder(t *testing.T) {
	base, obs := fixture()
	obs.HeadSHA = "advanced-head"
	obs.Index["untouched.go"] = IndexEntry{Mode: "100644", OID: "oid-tampered"}
	obs.Index["zz-smuggled.go"] = IndexEntry{Mode: "100644", OID: "oid-smuggled"}
	obs.Index["aa-smuggled.go"] = IndexEntry{Mode: "100644", OID: "oid-smuggled"}
	obs.Untracked = []string{"scratch.txt"}

	want := []Violation{
		{Reason: ReasonHeadMoved},
		{Reason: ReasonIndexEntryChanged, Path: "untouched.go"},
		{Reason: ReasonNewlyStagedPath, Path: "aa-smuggled.go"},
		{Reason: ReasonNewlyStagedPath, Path: "zz-smuggled.go"},
		{Reason: ReasonUntrackedPathOutsideSet, Path: "scratch.txt"},
	}
	for i := 0; i < 5; i++ {
		got := Verify(base, obs)
		if len(got) != len(want) {
			t.Fatalf("Verify returned %d violations %v, want %d", len(got), got, len(want))
		}
		for j := range got {
			if got[j].Reason != want[j].Reason || got[j].Path != want[j].Path {
				t.Fatalf("violation %d = {%q %q}, want {%q %q}", j, got[j].Reason, got[j].Path, want[j].Reason, want[j].Path)
			}
		}
	}
}
