package conflictresolve

import (
	"reflect"
	"testing"
)

const resolvedOurs = "before\nours\nafter\n"

// acceptedPair builds a baseline/observed pair the gate ACCEPTS: one clean
// auto-staged base change and one content conflict resolved by keeping ours.
func acceptedPair() (Baseline, Observed) {
	base := Baseline{
		HeadSHA:      "head1",
		MergeHeadSHA: "merge1",
		MergeMessage: "Merge branch 'main' into feature\n",
		Index: map[string]IndexEntry{
			"clean.go": {Mode: "100644", OID: "aaa"},
			"other.go": {Mode: "100755", OID: "bbb"},
		},
		Conflicted: map[string]ConflictedFile{
			"conf.go": {Kind: ConflictContent, MarkerBytes: []byte(oneBlock), Mode: "100644"},
		},
	}
	obs := Observed{
		HeadSHA:      base.HeadSHA,
		MergeHeadSHA: base.MergeHeadSHA,
		MergeMessage: base.MergeMessage,
		Index: map[string]IndexEntry{
			"clean.go": {Mode: "100644", OID: "aaa"},
			"other.go": {Mode: "100755", OID: "bbb"},
		},
		Unmerged: []string{"conf.go"},
		Unstaged: []string{"conf.go"},
		Working: map[string]FileState{
			"conf.go": {Present: true, Mode: "100644", Bytes: []byte(resolvedOurs)},
		},
	}
	return base, obs
}

func reasonsOf(vs []Violation) []Reason {
	out := make([]Reason, len(vs))
	for i, v := range vs {
		out[i] = v.Reason
	}
	return out
}

func TestVerifyAcceptsAConfinedResolution(t *testing.T) {
	base, obs := acceptedPair()
	if vs := Verify(base, obs); len(vs) != 0 {
		t.Fatalf("Verify = %+v, want no violations", vs)
	}
}

func TestVerify(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Baseline, *Observed)
		want   []Reason
		wantAt string // expected Path on the first violation
	}{
		{
			name:   "head moved",
			mutate: func(_ *Baseline, o *Observed) { o.HeadSHA = "head2" },
			want:   []Reason{ReasonHeadMoved},
		},
		{
			name:   "merge head rewritten",
			mutate: func(_ *Baseline, o *Observed) { o.MergeHeadSHA = "merge2" },
			want:   []Reason{ReasonMergeHeadChanged},
		},
		{
			name:   "merge message rewritten",
			mutate: func(_ *Baseline, o *Observed) { o.MergeMessage = "something else\n" },
			want:   []Reason{ReasonMergeMessageChanged},
		},
		{
			name: "non conflicted index entry content changed",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["clean.go"] = IndexEntry{Mode: "100644", OID: "ccc"}
			},
			want:   []Reason{ReasonIndexEntryChanged},
			wantAt: "clean.go",
		},
		{
			name: "non conflicted index entry mode changed",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["clean.go"] = IndexEntry{Mode: "100755", OID: "aaa"}
			},
			want:   []Reason{ReasonIndexEntryChanged},
			wantAt: "clean.go",
		},
		{
			name:   "non conflicted index entry removed",
			mutate: func(_ *Baseline, o *Observed) { delete(o.Index, "clean.go") },
			want:   []Reason{ReasonIndexEntryRemoved},
			wantAt: "clean.go",
		},
		{
			name: "newly staged path",
			mutate: func(_ *Baseline, o *Observed) {
				o.Index["new.go"] = IndexEntry{Mode: "100644", OID: "ddd"}
			},
			want:   []Reason{ReasonNewlyStagedPath},
			wantAt: "new.go",
		},
		{
			name:   "unstaged change outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Unstaged = append(o.Unstaged, "clean.go") },
			want:   []Reason{ReasonUnstagedOutsideSet},
			wantAt: "clean.go",
		},
		{
			name:   "untracked path outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Untracked = []string{"scratch.txt"} },
			want:   []Reason{ReasonUntrackedOutsideSet},
			wantAt: "scratch.txt",
		},
		{
			name:   "unmerged entry outside the conflicted set",
			mutate: func(_ *Baseline, o *Observed) { o.Unmerged = append(o.Unmerged, "sneaky.go") },
			want:   []Reason{ReasonUnmergedOutsideSet},
			wantAt: "sneaky.go",
		},
		{
			name: "agent staged the conflicted path",
			mutate: func(_ *Baseline, o *Observed) {
				o.Unmerged = nil
				o.Index["conf.go"] = IndexEntry{Mode: "100644", OID: "eee"}
			},
			// The staged conflicted path draws the precise reason, NOT the
			// generic newly-staged one.
			want:   []Reason{ReasonConflictedNotUnmerged},
			wantAt: "conf.go",
		},
		{
			name:   "conflicted path deleted outright",
			mutate: func(_ *Baseline, o *Observed) { o.Working["conf.go"] = FileState{} },
			want:   []Reason{ReasonConflictedPathMissing},
			wantAt: "conf.go",
		},
		{
			name: "executable bit set on a legitimately resolved file",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conf.go"] = FileState{Present: true, Mode: "100755", Bytes: []byte(resolvedOurs)}
			},
			want:   []Reason{ReasonConflictedModeChanged},
			wantAt: "conf.go",
		},
		{
			name: "resolved file swapped for a symlink",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conf.go"] = FileState{Present: true, Mode: "120000", Bytes: []byte(resolvedOurs)}
			},
			want:   []Reason{ReasonConflictedModeChanged},
			wantAt: "conf.go",
		},
		{
			name: "edit outside the conflicted hunk",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conf.go"] = FileState{Present: true, Mode: "100644", Bytes: []byte("beforeX\nours\nafter\n")}
			},
			want:   []Reason{ReasonOutsideHunk},
			wantAt: "conf.go",
		},
		{
			name: "residual marker in the assembled result",
			mutate: func(_ *Baseline, o *Observed) {
				o.Working["conf.go"] = FileState{Present: true, Mode: "100644", Bytes: []byte("before\n<<<<<<< HEAD\nours\nafter\n")}
			},
			want:   []Reason{ReasonResidualMarker},
			wantAt: "conf.go",
		},
		{
			name: "binary conflict is refused outright",
			mutate: func(b *Baseline, o *Observed) {
				b.Conflicted["conf.go"] = ConflictedFile{Kind: ConflictBinary, Mode: "100644"}
				o.Working["conf.go"] = FileState{Present: true, Mode: "100644", Bytes: []byte("\x00\x01")}
			},
			want:   []Reason{ReasonBinaryConflict},
			wantAt: "conf.go",
		},
		{
			name: "delete modify resolved with invented content",
			mutate: func(b *Baseline, o *Observed) {
				b.Conflicted["conf.go"] = ConflictedFile{
					Kind:  ConflictDeleteModify,
					Mode:  "100644",
					Sides: Sides{TheirsPresent: true, Theirs: []byte("theirs\n")},
				}
				o.Working["conf.go"] = FileState{Present: true, Mode: "100644", Bytes: []byte("invented\n")}
			},
			want:   []Reason{ReasonDeleteModifyContent},
			wantAt: "conf.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, obs := acceptedPair()
			tc.mutate(&base, &obs)
			vs := Verify(base, obs)
			if got := reasonsOf(vs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Verify reasons = %v, want %v (violations %+v)", got, tc.want, vs)
			}
			if vs[0].Path != tc.wantAt {
				t.Errorf("Verify violation path = %q, want %q", vs[0].Path, tc.wantAt)
			}
		})
	}
}

func TestVerifyOrdersViolationsDeterministically(t *testing.T) {
	base, obs := acceptedPair()
	obs.HeadSHA = "head2"
	obs.MergeHeadSHA = "merge2"
	obs.MergeMessage = "rewritten\n"
	obs.Untracked = []string{"zeta.txt", "alpha.txt"}
	delete(obs.Index, "clean.go")

	want := []Reason{
		// Repository-level rules first (empty path sorts first), then paths.
		ReasonHeadMoved,
		ReasonMergeHeadChanged,
		ReasonMergeMessageChanged,
		ReasonUntrackedOutsideSet, // alpha.txt
		ReasonIndexEntryRemoved,   // clean.go
		ReasonUntrackedOutsideSet, // zeta.txt
	}
	wantPaths := []string{"", "", "", "alpha.txt", "clean.go", "zeta.txt"}

	for i := 0; i < 5; i++ {
		vs := Verify(base, obs)
		if got := reasonsOf(vs); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: reasons = %v, want %v", i, got, want)
		}
		for j, v := range vs {
			if v.Path != wantPaths[j] {
				t.Fatalf("run %d: violation %d path = %q, want %q", i, j, v.Path, wantPaths[j])
			}
		}
	}
}

func TestVerifyCarriesDetail(t *testing.T) {
	base, obs := acceptedPair()
	obs.HeadSHA = "head2"
	vs := Verify(base, obs)
	if len(vs) != 1 || vs[0].Detail != "HEAD head1 -> head2" {
		t.Fatalf("Verify = %+v, want a head_moved violation naming both shas", vs)
	}
}

func TestAcceptConflictedFile(t *testing.T) {
	deleteModify := ConflictedFile{
		Kind: ConflictDeleteModify,
		Mode: "100644",
		Sides: Sides{
			Ours: []byte("ours side\n"), OursPresent: true,
			Theirs: []byte("theirs side\n"), TheirsPresent: true,
		},
	}
	oursOnly := ConflictedFile{
		Kind:  ConflictDeleteModify,
		Mode:  "100644",
		Sides: Sides{Ours: []byte("ours side\n"), OursPresent: true},
	}
	theirsOnly := ConflictedFile{
		Kind:  ConflictDeleteModify,
		Mode:  "100644",
		Sides: Sides{Theirs: []byte("theirs side\n"), TheirsPresent: true},
	}

	cases := []struct {
		name string
		file ConflictedFile
		got  FileState
		want Reason
	}{
		{
			"delete modify accepts the deletion",
			deleteModify,
			FileState{},
			ReasonNone,
		},
		{
			"delete modify accepts our full content",
			deleteModify,
			FileState{Present: true, Mode: "100644", Bytes: []byte("ours side\n")},
			ReasonNone,
		},
		{
			"delete modify accepts their full content",
			deleteModify,
			FileState{Present: true, Mode: "100644", Bytes: []byte("theirs side\n")},
			ReasonNone,
		},
		{
			"delete modify refuses invented content",
			deleteModify,
			FileState{Present: true, Mode: "100644", Bytes: []byte("half of each\n")},
			ReasonDeleteModifyContent,
		},
		{
			// An ABSENT side must never be matched by an empty candidate: the
			// Present flags carry that distinction. Both flags get their own
			// case, since each guards its own comparison.
			"delete modify refuses an absent theirs side's empty content",
			oursOnly,
			FileState{Present: true, Mode: "100644", Bytes: nil},
			ReasonDeleteModifyContent,
		},
		{
			"delete modify refuses an absent ours side's empty content",
			theirsOnly,
			FileState{Present: true, Mode: "100644", Bytes: nil},
			ReasonDeleteModifyContent,
		},
		{
			"binary conflict refused even when the bytes look clean",
			ConflictedFile{Kind: ConflictBinary, Mode: "100644"},
			FileState{Present: true, Mode: "100644", Bytes: []byte("anything\n")},
			ReasonBinaryConflict,
		},
		{
			"content conflict deleted outright",
			ConflictedFile{Kind: ConflictContent, MarkerBytes: []byte(oneBlock), Mode: "100644"},
			FileState{},
			ReasonConflictedPathMissing,
		},
		{
			"content conflict resolved inside the hunk",
			ConflictedFile{Kind: ConflictContent, MarkerBytes: []byte(oneBlock), Mode: "100644"},
			FileState{Present: true, Mode: "100644", Bytes: []byte(resolvedOurs)},
			ReasonNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptConflictedFile(tc.file, tc.got); got != tc.want {
				t.Errorf("AcceptConflictedFile = %q, want %q", got, tc.want)
			}
		})
	}
}
