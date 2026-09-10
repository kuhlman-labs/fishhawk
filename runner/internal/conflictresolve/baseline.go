package conflictresolve

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
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
	// ReasonRepoConfigChanged — the EFFECTIVE git configuration moved while the
	// pass was running.
	//
	// The whole configuration is the gate input, not a list of dangerous keys.
	// The enumeration this replaces could only name the keys someone had
	// already thought of (`url.<base>.insteadOf`, `url.<base>.pushInsteadOf`),
	// and `url.<base>.*` alone is an unbounded namespace: the subsection is an
	// arbitrary URL, so there is no finite key set to deny. Capturing the
	// config alongside HEAD/MERGE_HEAD/MERGE_MSG and refusing ANY change during
	// the pass closes every arbitrarily-named key, including ones git has not
	// shipped yet, without naming one of them.
	//
	// Read the residual honestly: this control sees a config the agent CHANGES,
	// never one that was ALREADY poisoned when the pass began. That gap is
	// covered on the push side (gitops.PushCommittedBranch's destination
	// guard), not here.
	ReasonRepoConfigChanged Reason = "conflict_resolution_repo_config_changed"
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
	// Config is the raw `git config --list -z` stream captured with the rest of
	// the baseline. It is stored VERBATIM so ORDER is compared too: git resolves
	// a single-valued key last-one-wins, so a pure reorder of two entries for
	// the same key is a semantic change, and sorting before comparison would
	// hide it.
	Config string
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
	// Config is the same raw stream re-read after the agent. See Baseline.Config.
	Config string
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
	if obs.Config != base.Config {
		add(ReasonRepoConfigChanged, "", configChangeDetail(base.Config, obs.Config))
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

// redactedConfigPart is the fixed stand-in a redacted git-config key part is
// replaced BY. It is a constant, never derived from the input, so nothing an
// agent controls can reach an operator-visible detail through it.
const redactedConfigPart = "<redacted>"

// configRecord is one `git config --list -z` record: its key and its value.
//
// It deliberately does NOT track whether the record carried a value at all, so
// a VALUELESS record (`key\0`, git's boolean-true shape) and an EMPTY-VALUED one
// (`key\n\0`) both yield value "" and are indistinguishable here. That is not a
// hole in the gate: Verify compares the raw config streams BYTE-FOR-BYTE and
// refuses on any difference, so such a pair is still caught. What the conflation
// costs is the DETAIL — configDiffKeys resolves no key-level difference, and
// configChangeDetail falls back to naming the change without a key.
type configRecord struct {
	key   string
	value string
}

// parseConfigStream splits a `git config --list -z` stream into records.
//
// The FRAMING is git's, not a guess: `--list -z` emits `key\nvalue\0` — the
// record is NUL-TERMINATED and the key/value separator INSIDE it is a NEWLINE,
// not `=` (verified with od(1); git-config(1) --list / --null). A parser that
// split records on NUL and then on `=` would treat the whole `key\nvalue` as
// the key, so the detail documented as carrying key NAMES ONLY would in fact
// carry the VALUES — the control meant to prevent credential disclosure would
// be the thing disclosing them. A record with no newline is a VALUELESS key
// (`git config --list` prints those bare); its whole text is the key.
func parseConfigStream(s string) []configRecord {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, "\x00")
	// A well-formed stream ends with a NUL, so the final field is empty.
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	out := make([]configRecord, 0, len(fields))
	for _, f := range fields {
		if nl := strings.IndexByte(f, '\n'); nl >= 0 {
			out = append(out, configRecord{key: f[:nl], value: f[nl+1:]})
			continue
		}
		out = append(out, configRecord{key: f})
	}
	return out
}

// redactConfigKey renders a git config key in the form an operator-visible
// detail may carry: `<section>.<redacted>.<final>`.
//
// The redaction is POSITIVE — the middle is REPLACED by a constant rather than
// filtered — because a key NAME can itself carry a credential. A git config key
// is `section.key` or `section.<subsection>.key`, and the subsection may contain
// any character except a newline (git-config(1)), so
// `url.https://user:password@example.com/.insteadof` is a real, valid shape.
// Section and final-component names are restricted to alphanumerics and `-`, so
// emitting those two verbatim can never carry an embedded URL; replacing
// everything between them means no arbitrary text reaches the detail at all.
// A key with no dot has no safe part to emit, so it collapses to the placeholder.
func redactConfigKey(key string) string {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 {
		return redactedConfigPart
	}
	section, final := key[:first], key[last+1:]
	if first == last {
		return section + "." + final
	}
	return section + "." + redactedConfigPart + "." + final
}

// configDiffKeys returns the sorted set of REDACTED key names that differ
// between two `git config --list -z` streams — added, removed, value-changed,
// or REORDERED relative to the other keys.
//
// Order is part of the comparison because git resolves a single-valued key
// LAST-ONE-WINS: a reorder changes which value wins, so it cannot be compared
// away by a set-equality check. It is compared in two SEMANTIC pieces rather
// than as a raw record index, because a raw index is not a semantic property —
// inserting ONE key shifts the index of every record after it, and an
// index-keyed signature would then name every one of them as changed and bury
// the real edit.
//
//   - Each key's ordered list of VALUES, which is what last-one-wins resolves
//     over for that key.
//   - The ordered sequence of keys RESTRICTED to those present on both sides,
//     which catches a pure permutation of two entries while staying invariant
//     to an insertion elsewhere in the stream.
func configDiffKeys(base, obs string) []string {
	differs := map[string]bool{}
	mark := func(key string) { differs[redactConfigKey(key)] = true }

	values := func(recs []configRecord) map[string][]string {
		m := map[string][]string{}
		for _, rec := range recs {
			m[rec.key] = append(m[rec.key], rec.value)
		}
		return m
	}
	baseRecs, obsRecs := parseConfigStream(base), parseConfigStream(obs)
	a, b := values(baseRecs), values(obsRecs)
	for key, av := range a {
		bv, ok := b[key]
		if !ok || !equalStrings(av, bv) {
			mark(key)
		}
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			mark(key)
		}
	}

	common := func(recs []configRecord, other map[string][]string) []string {
		var out []string
		for _, rec := range recs {
			if _, ok := other[rec.key]; ok {
				out = append(out, rec.key)
			}
		}
		return out
	}
	ax, bx := common(baseRecs, b), common(obsRecs, a)
	if len(ax) == len(bx) {
		for i := range ax {
			if ax[i] != bx[i] {
				mark(ax[i])
				mark(bx[i])
			}
		}
	}

	out := make([]string, 0, len(differs))
	for k := range differs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// configChangeDetail renders the ReasonRepoConfigChanged violation detail. It
// carries redacted key names and a count — never a value, and never the middle
// of a key.
func configChangeDetail(base, obs string) string {
	keys := configDiffKeys(base, obs)
	if len(keys) == 0 {
		// The raw streams differ but no key signature does — the framing itself
		// moved. Report the change rather than swallowing it.
		return "the effective git configuration changed (no key-level difference resolved)"
	}
	return "the effective git configuration changed: " + strconv.Itoa(len(keys)) +
		" key(s) " + strings.Join(keys, ", ") +
		" (redacted to section and final component; values are never reported)"
}
