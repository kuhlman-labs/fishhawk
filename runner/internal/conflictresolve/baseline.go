package conflictresolve

import (
	"bytes"
	"sort"
)

// Baseline-side reasons. Each rule the gate can break earns its OWN named
// reason: the runner reports exactly one string per violation and the operator
// reads it out of the terminal failure report.
const (
	// ReasonHeadMoved — HEAD is the merge commit's FIRST parent.
	ReasonHeadMoved Reason = "conflict_resolution_head_moved"
	// ReasonMergeHeadChanged — MERGE_HEAD is the merge commit's SECOND parent,
	// so rewriting it changes the commit's ancestry with every other gate input
	// unchanged.
	ReasonMergeHeadChanged Reason = "conflict_resolution_merge_head_changed"
	// ReasonMergeMessageChanged — the merge message source is the commit's
	// message, so it is a gate input too.
	ReasonMergeMessageChanged Reason = "conflict_resolution_merge_message_changed"
	// ReasonIndexEntryChanged — a non-conflicted index entry's mode or OID moved.
	ReasonIndexEntryChanged Reason = "conflict_resolution_index_entry_changed"
	// ReasonIndexEntryRemoved — a non-conflicted index entry disappeared.
	ReasonIndexEntryRemoved Reason = "conflict_resolution_index_entry_removed"
	// ReasonNewlyStagedPath — a stage-0 entry absent from the baseline index.
	ReasonNewlyStagedPath Reason = "conflict_resolution_newly_staged_path"
	// ReasonUnstagedOutsideSet — a working-tree edit to a path git did not mark
	// conflicted.
	ReasonUnstagedOutsideSet Reason = "conflict_resolution_unstaged_change_outside_set"
	// ReasonUntrackedOutsideSet — a new untracked path.
	ReasonUntrackedOutsideSet Reason = "conflict_resolution_untracked_path_outside_set"
	// ReasonUnmergedOutsideSet — an unmerged entry that was not conflicted when
	// the baseline was captured.
	ReasonUnmergedOutsideSet Reason = "conflict_resolution_unmerged_entry_outside_set"
	// ReasonConflictedNotUnmerged — a conflicted path the agent staged or
	// committed, so it is no longer unmerged and the runner's own scoped
	// `git add` is no longer the only thing that stages it.
	ReasonConflictedNotUnmerged Reason = "conflict_resolution_conflicted_path_not_unmerged"
	// ReasonConflictedPathMissing — a content-conflicted path deleted outright.
	ReasonConflictedPathMissing Reason = "conflict_resolution_conflicted_path_missing"
	// ReasonConflictedModeChanged — the resolution contract is BYTES-ONLY, and
	// the runner's scoped `git add` stages mode alongside content, so a mode
	// flip on a legitimately resolved file is refused.
	ReasonConflictedModeChanged Reason = "conflict_resolution_conflicted_mode_changed"
	// ReasonBinaryConflict — git leaves OURS on disk with no markers, so there
	// is no hunk boundary to confine anything to.
	ReasonBinaryConflict Reason = "conflict_resolution_binary_conflict"
	// ReasonDeleteModifyContent — a delete/modify resolution must be one side's
	// full content or the deletion.
	ReasonDeleteModifyContent Reason = "conflict_resolution_delete_modify_content"
)

// ConflictKind is how git presented one conflicted path.
type ConflictKind string

const (
	// ConflictContent is the ordinary case: git wrote marker lines.
	ConflictContent ConflictKind = "content"
	// ConflictDeleteModify is one side deleting a path the other modified.
	ConflictDeleteModify ConflictKind = "delete_modify"
	// ConflictBinary is a conflict git could not present with markers.
	ConflictBinary ConflictKind = "binary"
)

// Sides carries the full content of each side of a delete/modify conflict. The
// Present flags distinguish an absent side from an empty one.
type Sides struct {
	Ours          []byte
	OursPresent   bool
	Theirs        []byte
	TheirsPresent bool
}

// ConflictedFile is one conflicted path exactly as git left it.
type ConflictedFile struct {
	Kind        ConflictKind
	MarkerBytes []byte
	Sides       Sides
	// Mode is the octal string git reports (100644 / 100755 / 120000 / 160000).
	Mode string
}

// IndexEntry is one stage-0 index entry.
type IndexEntry struct {
	Mode string
	OID  string
}

// FileState is a working-tree path read back after the agent ran.
type FileState struct {
	Present bool
	Mode    string
	Bytes   []byte
}

// Baseline is the mechanical snapshot of the merge state git produced, captured
// in ONE read immediately after the merge stopped on conflicts and before the
// agent was invoked.
type Baseline struct {
	HeadSHA      string
	MergeHeadSHA string
	MergeMessage string
	// Index holds every NON-conflicted stage-0 entry. Clean base changes git
	// already auto-staged in other files live here and are AUTHORIZED.
	Index      map[string]IndexEntry
	Conflicted map[string]ConflictedFile
}

// Observed is the same shape read back after the agent, plus the status sets the
// gate needs to catch work outside the conflicted paths. Working carries one
// entry per baseline-conflicted path.
type Observed struct {
	HeadSHA      string
	MergeHeadSHA string
	MergeMessage string
	Index        map[string]IndexEntry
	Unmerged     []string
	Unstaged     []string
	Untracked    []string
	Working      map[string]FileState
}

// Violation is one broken rule. Path is empty for the repository-level rules.
type Violation struct {
	Reason Reason
	Path   string
	Detail string
}

// AcceptConflictedFile decides ONE conflicted path.
//
// A binary conflict is refused unconditionally. A delete/modify conflict accepts
// the deletion or either side's full content. A content conflict is refused when
// the path is gone and otherwise delegates to AcceptResolution.
func AcceptConflictedFile(f ConflictedFile, got FileState) Reason {
	switch f.Kind {
	case ConflictBinary:
		return ReasonBinaryConflict
	case ConflictDeleteModify:
		if !got.Present {
			return ReasonNone
		}
		if f.Sides.OursPresent && bytes.Equal(got.Bytes, f.Sides.Ours) {
			return ReasonNone
		}
		if f.Sides.TheirsPresent && bytes.Equal(got.Bytes, f.Sides.Theirs) {
			return ReasonNone
		}
		return ReasonDeleteModifyContent
	default:
		if !got.Present {
			return ReasonConflictedPathMissing
		}
		return AcceptResolution(f.MarkerBytes, got.Bytes)
	}
}

// Verify returns one Violation per rule broken, in deterministic order: the
// repository-level rules first, then the path-keyed rules sorted by path and
// reason. An empty result is the accept verdict — and ONLY then may the runner
// perform its scoped `git add` and single `git commit --no-edit`.
func Verify(base Baseline, obs Observed) []Violation {
	var vs []Violation
	add := func(r Reason, path, detail string) {
		vs = append(vs, Violation{Reason: r, Path: path, Detail: detail})
	}

	if obs.HeadSHA != base.HeadSHA {
		add(ReasonHeadMoved, "", "HEAD "+base.HeadSHA+" -> "+obs.HeadSHA)
	}
	if obs.MergeHeadSHA != base.MergeHeadSHA {
		add(ReasonMergeHeadChanged, "", "MERGE_HEAD "+base.MergeHeadSHA+" -> "+obs.MergeHeadSHA)
	}
	if obs.MergeMessage != base.MergeMessage {
		add(ReasonMergeMessageChanged, "", "merge message rewritten")
	}

	for path, want := range base.Index {
		got, ok := obs.Index[path]
		if !ok {
			add(ReasonIndexEntryRemoved, path, "")
			continue
		}
		if got != want {
			add(ReasonIndexEntryChanged, path, want.Mode+" "+want.OID+" -> "+got.Mode+" "+got.OID)
		}
	}
	for path := range obs.Index {
		if _, ok := base.Index[path]; ok {
			continue
		}
		// A conflicted path appearing at stage 0 is the agent having staged it;
		// ReasonConflictedNotUnmerged names that case precisely.
		if _, ok := base.Conflicted[path]; ok {
			continue
		}
		add(ReasonNewlyStagedPath, path, "")
	}

	for _, path := range obs.Unstaged {
		if _, ok := base.Conflicted[path]; !ok {
			add(ReasonUnstagedOutsideSet, path, "")
		}
	}
	for _, path := range obs.Untracked {
		if _, ok := base.Conflicted[path]; !ok {
			add(ReasonUntrackedOutsideSet, path, "")
		}
	}
	for _, path := range obs.Unmerged {
		if _, ok := base.Conflicted[path]; !ok {
			add(ReasonUnmergedOutsideSet, path, "")
		}
	}

	unmerged := make(map[string]bool, len(obs.Unmerged))
	for _, path := range obs.Unmerged {
		unmerged[path] = true
	}
	for path, f := range base.Conflicted {
		if !unmerged[path] {
			add(ReasonConflictedNotUnmerged, path, "")
		}
		got := obs.Working[path]
		if got.Present && got.Mode != f.Mode {
			add(ReasonConflictedModeChanged, path, f.Mode+" -> "+got.Mode)
		}
		if r := AcceptConflictedFile(f, got); r != ReasonNone {
			add(r, path, "")
		}
	}

	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Path != vs[j].Path {
			return vs[i].Path < vs[j].Path
		}
		return vs[i].Reason < vs[j].Reason
	})
	return vs
}
