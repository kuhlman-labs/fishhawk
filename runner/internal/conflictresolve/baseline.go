package conflictresolve

import (
	"bytes"
	"fmt"
	"sort"
)

// ConflictKind is how git presented a conflicted path.
type ConflictKind int

const (
	// KindContent is a textual conflict: git wrote the file with markers.
	KindContent ConflictKind = iota
	// KindDeleteModify is a delete/modify conflict: one side deleted the
	// path, the other modified it, and git wrote no markers.
	KindDeleteModify
	// KindBinary is a conflict git could not merge textually (a binary file,
	// or one carrying `-merge`). Git leaves OURS in the working tree with no
	// markers, so there is nothing to partition.
	KindBinary
)

// The git file modes a working-tree path may carry into a commit. The gate
// compares these STRINGS rather than an os.FileMode so a captured baseline and
// a later observation are comparable as plain data.
const (
	// ModeRegular is a non-executable regular file.
	ModeRegular = "100644"
	// ModeExecutable is a regular file carrying any execute bit.
	ModeExecutable = "100755"
	// ModeSymlink is a symbolic link.
	ModeSymlink = "120000"
)

// IndexEntry is a stage-0 index entry: the mode and object id git recorded.
type IndexEntry struct {
	Mode string
	OID  string
}

// ConflictedFile is one conflicted path exactly as git presented it, captured
// immediately after the merge stopped.
type ConflictedFile struct {
	Kind ConflictKind
	// MarkerBytes is the working-tree content git wrote, for KindContent.
	MarkerBytes []byte
	// Sides are the full contents a delete/modify resolution may keep. The
	// deletion itself is expressed by the path being absent from the working
	// tree, not by an entry here.
	Sides [][]byte
	// Mode is the working-tree file mode git left on the path, empty when the
	// path is absent. The agent's authority over a conflicted path is over its
	// BYTES only, so the mode the scoped `git add` will stage must still be
	// the one git itself produced.
	Mode string
}

// Baseline is the merge state git produced, captured in ONE read immediately
// after `git merge --no-commit --no-ff` stopped on conflicts and BEFORE the
// agent runs.
//
// MergeHeadSHA and MergeMessage are captured here, alongside HeadSHA, because
// they are inputs to the commit the gate authorizes: a merge commit's second
// parent comes from MERGE_HEAD and its message from the merge message source,
// both of which are on-disk repository metadata an agent can write while
// leaving HEAD, the index and the working tree untouched.
type Baseline struct {
	HeadSHA      string
	MergeHeadSHA string
	MergeMessage string
	// Index holds every NON-CONFLICTED stage-0 entry, keyed by path. Clean
	// base changes git already auto-staged are part of the baseline and are
	// therefore AUTHORIZED.
	Index map[string]IndexEntry
	// Conflicted holds every conflicted path, de-duplicated across stages.
	Conflicted map[string]ConflictedFile
}

// Observed is the same state read back after the agent pass, before any
// `git add` and before the commit.
type Observed struct {
	HeadSHA      string
	MergeHeadSHA string
	MergeMessage string
	// Index holds every stage-0 entry, keyed by path.
	Index map[string]IndexEntry
	// Unmerged is every path still carrying unmerged index stages,
	// de-duplicated across stages.
	Unmerged []string
	// Unstaged is every path whose working-tree content differs from the
	// index, including deletions.
	Unstaged []string
	// Untracked is every untracked path.
	Untracked []string
	// Working holds the working-tree bytes of each path present on disk that
	// the caller read back for the conflicted set.
	Working map[string][]byte
	// WorkingMode holds the file mode of those same paths, read back in the
	// SAME pass as their bytes.
	WorkingMode map[string]string
}

// Baseline-verification reasons.
const (
	// ReasonHeadMoved means HEAD is no longer the commit the merge stopped on.
	ReasonHeadMoved Reason = "conflict_resolution_head_moved"
	// ReasonMergeHeadChanged means MERGE_HEAD changed, which would change the
	// resulting merge commit's SECOND PARENT while every other gate input
	// stayed identical.
	ReasonMergeHeadChanged Reason = "conflict_resolution_merge_head_changed"
	// ReasonMergeMessageChanged means the merge message source changed, which
	// would change the message of the commit `git commit --no-edit` writes.
	ReasonMergeMessageChanged Reason = "conflict_resolution_merge_message_changed"
	// ReasonIndexEntryChanged means a non-conflicted index entry's mode or
	// object id differs from the baseline.
	ReasonIndexEntryChanged Reason = "conflict_resolution_index_entry_changed"
	// ReasonIndexEntryRemoved means a non-conflicted baseline index entry is
	// gone.
	ReasonIndexEntryRemoved Reason = "conflict_resolution_index_entry_removed"
	// ReasonNewlyStagedPath means a stage-0 entry exists for a path the
	// baseline never carried.
	ReasonNewlyStagedPath Reason = "conflict_resolution_newly_staged_path"
	// ReasonUnstagedChangeOutsideSet means a working-tree change touches a
	// path outside the conflicted set.
	ReasonUnstagedChangeOutsideSet Reason = "conflict_resolution_unstaged_change_outside_set"
	// ReasonUntrackedPathOutsideSet means an untracked path exists outside the
	// conflicted set.
	ReasonUntrackedPathOutsideSet Reason = "conflict_resolution_untracked_path_outside_set"
	// ReasonUnmergedEntryOutsideSet means an unmerged index entry exists for a
	// path outside the conflicted set.
	ReasonUnmergedEntryOutsideSet Reason = "conflict_resolution_unmerged_entry_outside_set"
	// ReasonConflictedPathNotUnmerged means a conflicted path lost its
	// unmerged stages — the agent staged or removed it, breaking the
	// runner's sole-writer contract over the index.
	ReasonConflictedPathNotUnmerged Reason = "conflict_resolution_conflicted_path_not_unmerged"
	// ReasonConflictedPathMissing means a textually conflicted path is absent
	// from the working tree. Only a delete/modify conflict may be resolved by
	// deletion.
	ReasonConflictedPathMissing Reason = "conflict_resolution_conflicted_path_missing"
	// ReasonConflictedModeChanged means a conflicted path's FILE MODE differs
	// from the one git left. The resolution contract is bytes-only, and the
	// scoped `git add` stages mode alongside content, so an executable bit or
	// a regular-file-to-symlink swap would otherwise reach the commit without
	// ever appearing in the index or in a content check.
	ReasonConflictedModeChanged Reason = "conflict_resolution_conflicted_mode_changed"
)

// Violation is one refusal: a named reason plus the path it concerns (empty
// for a repository-level rule) and a human-readable detail.
type Violation struct {
	Reason Reason
	Path   string
	Detail string
}

// Verify checks the observed state against the baseline and returns one
// violation per rule broken. An empty result authorizes the scoped
// `git add` + single `git commit --no-edit`; anything else refuses.
//
// Verify is deterministic: per-path violations are reported in sorted path
// order, so a refusal message is stable across runs.
func Verify(base Baseline, obs Observed) []Violation {
	var out []Violation
	add := func(r Reason, path, detail string) {
		out = append(out, Violation{Reason: r, Path: path, Detail: detail})
	}

	if obs.HeadSHA != base.HeadSHA {
		add(ReasonHeadMoved, "", fmt.Sprintf("HEAD %s != baseline %s", obs.HeadSHA, base.HeadSHA))
	}
	if obs.MergeHeadSHA != base.MergeHeadSHA {
		add(ReasonMergeHeadChanged, "", fmt.Sprintf("MERGE_HEAD %s != baseline %s", obs.MergeHeadSHA, base.MergeHeadSHA))
	}
	if obs.MergeMessage != base.MergeMessage {
		add(ReasonMergeMessageChanged, "", "merge message source differs from baseline")
	}

	conflicted := make(map[string]bool, len(base.Conflicted))
	for p := range base.Conflicted {
		conflicted[p] = true
	}

	for _, path := range sortedKeys(base.Index) {
		want := base.Index[path]
		got, ok := obs.Index[path]
		switch {
		case !ok:
			add(ReasonIndexEntryRemoved, path, "baseline index entry is gone")
		case got != want:
			add(ReasonIndexEntryChanged, path, fmt.Sprintf("index entry %s %s != baseline %s %s", got.Mode, got.OID, want.Mode, want.OID))
		}
	}
	for _, path := range sortedKeys(obs.Index) {
		if _, ok := base.Index[path]; ok {
			continue
		}
		if conflicted[path] {
			// Reported once, by the unmerged sweep below.
			continue
		}
		add(ReasonNewlyStagedPath, path, "staged path absent from the baseline index")
	}

	unmerged := make(map[string]bool, len(obs.Unmerged))
	for _, path := range obs.Unmerged {
		unmerged[path] = true
	}
	for _, path := range sortedUnique(obs.Unmerged) {
		if !conflicted[path] {
			add(ReasonUnmergedEntryOutsideSet, path, "unmerged index entry outside the conflicted set")
		}
	}
	for _, path := range sortedKeys(base.Conflicted) {
		if !unmerged[path] {
			add(ReasonConflictedPathNotUnmerged, path, "conflicted path no longer has unmerged index stages")
		}
	}

	for _, path := range sortedUnique(obs.Unstaged) {
		if !conflicted[path] {
			add(ReasonUnstagedChangeOutsideSet, path, "working-tree change outside the conflicted set")
		}
	}
	for _, path := range sortedUnique(obs.Untracked) {
		if !conflicted[path] {
			add(ReasonUntrackedPathOutsideSet, path, "untracked path outside the conflicted set")
		}
	}

	for _, path := range sortedKeys(base.Conflicted) {
		cf := base.Conflicted[path]
		content, present := obs.Working[path]
		if present {
			// The mode is checked SEPARATELY from the content: a conflicted
			// path may legitimately carry unstaged working-tree changes, so
			// the non-conflicted index sweep above never sees it, and an
			// accepted byte-level resolution says nothing about the metadata
			// the scoped `git add` will stage with it.
			if mode := obs.WorkingMode[path]; mode != cf.Mode {
				add(ReasonConflictedModeChanged, path, fmt.Sprintf("file mode %s != baseline %s", displayMode(mode), displayMode(cf.Mode)))
			}
		}
		if r := acceptConflict(cf, content, present); r != ReasonNone {
			add(r, path, "resolution refused by the hunk-level partition check")
		}
	}
	return out
}

// acceptConflict dispatches one conflicted path to the check its kind admits.
func acceptConflict(cf ConflictedFile, content []byte, present bool) Reason {
	switch cf.Kind {
	case KindBinary:
		// Git left OURS on disk with no markers, so there is no hunk boundary
		// to confine an edit to. Refuse rather than accept whatever bytes the
		// agent wrote.
		return ReasonBinaryConflict
	case KindDeleteModify:
		if !present {
			// Keeping the deletion is one of the two sides.
			return ReasonNone
		}
		for _, side := range cf.Sides {
			if bytes.Equal(content, side) {
				return ReasonNone
			}
		}
		return ReasonDeleteModifyContent
	default:
		if !present {
			return ReasonConflictedPathMissing
		}
		return AcceptResolution(cf.MarkerBytes, content)
	}
}

// displayMode renders a captured mode for a refusal message, naming an
// UNREADABLE or absent mode explicitly rather than printing an empty string.
func displayMode(mode string) string {
	if mode == "" {
		return "(none)"
	}
	return mode
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
