package conflictresolve

import (
	"reflect"
	"strings"
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

// --- the repository-config gate input (#3338) ---

// cfgStream renders records into git's REAL `git config --list -z` framing:
// `key\nvalue\0` per record. Every fixture below is built through it so no test
// silently encodes the WRONG framing and then agrees with a wrong parser.
func cfgStream(kv ...string) string {
	var b strings.Builder
	for i := 0; i < len(kv); i += 2 {
		b.WriteString(kv[i])
		b.WriteByte('\n')
		b.WriteString(kv[i+1])
		b.WriteByte(0)
	}
	return b.String()
}

// TestVerifyRepoConfigChanged pins the gate input itself: a config that moved
// during the pass is a violation with its OWN named reason, and an identical
// config is not. The pair is otherwise the ACCEPTED pair, so the discrimination
// is on the config alone.
func TestVerifyRepoConfigChanged(t *testing.T) {
	base, obs := acceptedPair()
	base.Config = cfgStream("user.name", "test")
	obs.Config = base.Config
	if vs := Verify(base, obs); len(vs) != 0 {
		t.Fatalf("an unchanged config must not violate: %+v", vs)
	}

	obs.Config = cfgStream("user.name", "test", "url.https://decoy.example/.insteadof", "https://origin.example/")
	vs := Verify(base, obs)
	if len(vs) != 1 {
		t.Fatalf("Verify = %+v, want exactly one violation", vs)
	}
	if vs[0].Reason != ReasonRepoConfigChanged {
		t.Errorf("reason = %q, want %q", vs[0].Reason, ReasonRepoConfigChanged)
	}
	if vs[0].Path != "" {
		t.Errorf("Path = %q, want empty (a repository-level rule)", vs[0].Path)
	}
	if !strings.Contains(vs[0].Detail, "url."+redactedConfigPart+".insteadof") {
		t.Errorf("detail does not name the redacted key: %q", vs[0].Detail)
	}
}

// TestConfigChangeDetailRedactsKeyAndValue is the disclosure control. A git
// config key NAME can itself carry a credential — the subsection of
// `url.<base>.insteadOf` is an arbitrary URL — so the detail is built
// POSITIVELY (section + constant + final component) rather than by filtering
// values out. The fixture plants a distinctive secret in BOTH halves and the
// assertion is that NEITHER reaches the detail.
func TestConfigChangeDetailRedactsKeyAndValue(t *testing.T) {
	const keySecret = "s3cr3t-user:s3cr3t-token"
	const valueSecret = "v4lue-s3cr3t-token"
	base := cfgStream("user.name", "test")
	obs := cfgStream("user.name", "test",
		"url.https://"+keySecret+"@example.com/.insteadof", "https://"+valueSecret+"@example.com/")

	detail := configChangeDetail(base, obs)
	for _, secret := range []string{keySecret, valueSecret, "s3cr3t", "example.com"} {
		if strings.Contains(detail, secret) {
			t.Errorf("detail leaked %q: %q", secret, detail)
		}
	}
	if !strings.Contains(detail, "url."+redactedConfigPart+".insteadof") {
		t.Errorf("detail must still name the redacted key shape: %q", detail)
	}
}

// TestConfigDiffKeys is the helper's table: one case per way two streams can
// differ, plus the identical control.
func TestConfigDiffKeys(t *testing.T) {
	cases := []struct {
		name      string
		base, obs string
		want      []string
	}{
		{"identical", cfgStream("user.name", "a"), cfgStream("user.name", "a"), nil},
		{"key added", cfgStream("user.name", "a"), cfgStream("user.name", "a", "core.bare", "false"),
			[]string{"core.bare"}},
		{"key removed", cfgStream("user.name", "a", "core.bare", "false"), cfgStream("user.name", "a"),
			[]string{"core.bare"}},
		{"value changed", cfgStream("user.name", "a"), cfgStream("user.name", "b"),
			[]string{"user.name"}},
		{
			// git resolves a single-valued key LAST-ONE-WINS, so a pure reorder
			// changes which value wins and must be reported as a difference
			// rather than compared away by a set-equality check.
			"pure reorder", cfgStream("user.name", "a", "core.bare", "false"),
			cfgStream("core.bare", "false", "user.name", "a"),
			[]string{"core.bare", "user.name"},
		},
		{"multi-valued key reordered", cfgStream("remote.origin.fetch", "a", "remote.origin.fetch", "b"),
			cfgStream("remote.origin.fetch", "b", "remote.origin.fetch", "a"),
			[]string{"remote." + redactedConfigPart + ".fetch"}},
		{"valueless record", cfgStream("user.name", "a"), cfgStream("user.name", "a") + "core.bare\x00",
			[]string{"core.bare"}},
		{"two-component key keeps both parts", "", cfgStream("user.name", "a"),
			[]string{"user.name"}},
		{"dotted subsection is replaced wholesale", "",
			cfgStream("url.https://x.example/.insteadof", "y"),
			[]string{"url." + redactedConfigPart + ".insteadof"}},
		{"dotless key collapses to the placeholder", "", cfgStream("bare", "a"),
			[]string{redactedConfigPart}},
		// A VALUELESS record (`key\0`, git's boolean-true shape) and an
		// EMPTY-VALUED one (`key\n\0`) both parse to value "", so no key-level
		// difference resolves. The streams still DIFFER, which is why Verify's
		// byte-for-byte comparison refuses regardless; this case pins that the
		// key signature is what goes silent.
		{"valueless vs empty-valued resolves no key", "core.bare\x00", "core.bare\n\x00", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := configDiffKeys(tc.base, tc.obs)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("configDiffKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConfigChangeDetailFallback pins configChangeDetail's no-key-resolved
// branch, which is reachable because parseConfigStream conflates a VALUELESS
// record (`key\0`, git's boolean-true shape) with an EMPTY-VALUED one
// (`key\n\0`): both parse to value "", so configDiffKeys returns nothing while
// the raw streams still differ. The detail must report the change rather than
// swallow it — the alternative would be an empty key list rendered as though
// nothing had happened.
//
// The gate itself is unaffected either way: Verify compares the raw streams
// byte-for-byte and refuses on any difference (TestVerifyRepoConfigChanged).
// What this pins is the DETAIL an operator reads.
func TestConfigChangeDetailFallback(t *testing.T) {
	cases := []struct{ name, base, obs string }{
		{"valueless becomes empty-valued", "core.bare\x00", "core.bare\n\x00"},
		{"empty-valued becomes valueless", "core.bare\n\x00", "core.bare\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.base == tc.obs {
				t.Fatalf("the fixture streams are identical, so nothing changed: %q", tc.base)
			}
			if keys := configDiffKeys(tc.base, tc.obs); len(keys) != 0 {
				t.Fatalf("configDiffKeys resolved %v — the fallback branch is not reached", keys)
			}
			detail := configChangeDetail(tc.base, tc.obs)
			const want = "the effective git configuration changed (no key-level difference resolved)"
			if detail != want {
				t.Errorf("configChangeDetail = %q, want %q", detail, want)
			}
		})
	}
}

// TestConfigStreamFraming is the FRAMING regression pin. git's `--list -z`
// emits `key\nvalue\0` — the record is NUL-terminated and the key/value
// separator inside it is a NEWLINE, not `=`. A parser that split records on NUL
// and then on `=` would fold the VALUE into the key, so the detail documented
// as key-names-only would carry values instead.
//
// The two fixture keys are in DIFFERENT sections deliberately: they must stay
// distinguishable after redaction, so a wrong split produces two visibly wrong
// key names rather than collapsing into one placeholder that happens to match.
// Both values carry an `=` so an `=`-splitting parser has something to hit.
func TestConfigStreamFraming(t *testing.T) {
	stream := "alpha.one\nv=1\x00beta.two\nv=2\x00"
	if stream != cfgStream("alpha.one", "v=1", "beta.two", "v=2") {
		t.Fatalf("the byte-exact fixture and cfgStream disagree on git's framing")
	}
	got := configDiffKeys("", stream)
	want := []string{"alpha.one", "beta.two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("configDiffKeys = %v, want %v — the key/value split is not git's `key\\nvalue` framing", got, want)
	}
	detail := configChangeDetail("", stream)
	for _, leak := range []string{"v=1", "v=2"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail leaked a VALUE %q: %q", leak, detail)
		}
	}
}

// --- the conflicted set's effective attributes as a gate input (#3339) ---

// attrStream renders triplets into git's REAL `git check-attr -z --all`
// framing: `<path>\0<attr>\0<value>\0` per record. Every fixture below is built
// through it so no test silently encodes the WRONG framing and then agrees with
// a wrong parser.
func attrStream(triplets ...string) string {
	var b strings.Builder
	for i := 0; i+3 <= len(triplets); i += 3 {
		b.WriteString(triplets[i])
		b.WriteByte(0)
		b.WriteString(triplets[i+1])
		b.WriteByte(0)
		b.WriteString(triplets[i+2])
		b.WriteByte(0)
	}
	return b.String()
}

// TestVerifyAttributesChanged pins the gate input: attributes that moved during
// the pass are a violation with their OWN named reason, an identical stream is
// not, and the detail names the differing attribute by NAME while never leaking
// its value. The pair is otherwise the ACCEPTED pair, so the discrimination is
// on the attributes alone.
func TestVerifyAttributesChanged(t *testing.T) {
	base, obs := acceptedPair()
	base.Attributes = attrStream("conf.go", "text", "set")
	obs.Attributes = base.Attributes
	if vs := Verify(base, obs); len(vs) != 0 {
		t.Fatalf("an unchanged attribute stream must not violate: %+v", vs)
	}

	obs.Attributes = attrStream("conf.go", "text", "set", "conf.go", "filter", "mangle")
	vs := Verify(base, obs)
	if len(vs) != 1 {
		t.Fatalf("Verify = %+v, want exactly one violation", vs)
	}
	if vs[0].Reason != ReasonAttributesChanged {
		t.Errorf("reason = %q, want %q", vs[0].Reason, ReasonAttributesChanged)
	}
	if vs[0].Path != "" {
		t.Errorf("Path = %q, want empty (a repository-level rule)", vs[0].Path)
	}
	if !strings.Contains(vs[0].Detail, "conf.go: filter") {
		t.Errorf("detail does not name the differing attribute: %q", vs[0].Detail)
	}
	if strings.Contains(vs[0].Detail, "mangle") {
		t.Errorf("detail leaked the attribute VALUE: %q", vs[0].Detail)
	}
}

// TestAttributeChangeDetailNamesOnly is the disclosure control for the attribute
// detail: a git attribute VALUE is agent-writable arbitrary text, so a secret
// planted in a value must never reach the operator-visible detail. Only the
// attribute NAME (git-restricted to `-`, `_`, `.` and alphanumerics) is emitted.
func TestAttributeChangeDetailNamesOnly(t *testing.T) {
	const valueSecret = "hunter2-SECRET-VALUE"
	base := attrStream("conf.go", "text", "set")
	obs := attrStream("conf.go", "text", "set", "conf.go", "marker", valueSecret)
	detail := attributeChangeDetail(base, obs)
	for _, leak := range []string{valueSecret, "hunter2"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail leaked a VALUE %q: %q", leak, detail)
		}
	}
	if !strings.Contains(detail, "conf.go: marker") {
		t.Errorf("detail must name the attribute NAME: %q", detail)
	}
}

// TestAttributeNamesFor pins the names-only rendering the staged-content refusal
// detail reuses: sorted NAMES for a path, the "none" fallback, and never a value.
func TestAttributeNamesFor(t *testing.T) {
	stream := attrStream(
		"conf.go", "text", "set",
		"conf.go", "eol", "crlf",
		"conf.go", "marker", "hunter2-SECRET-VALUE",
		"other.go", "diff", "golang",
	)
	if got := AttributeNamesFor(stream, "conf.go"); got != "eol, marker, text" {
		t.Errorf("AttributeNamesFor(conf.go) = %q, want the sorted names", got)
	}
	if strings.Contains(AttributeNamesFor(stream, "conf.go"), "hunter2") {
		t.Error("a value reached the names output")
	}
	if got := AttributeNamesFor(stream, "nope.go"); got != "none" {
		t.Errorf("AttributeNamesFor(absent path) = %q, want none", got)
	}
	if got := AttributeNamesFor("", "conf.go"); got != "none" {
		t.Errorf("AttributeNamesFor(empty stream) = %q, want none", got)
	}
}

// TestParseAttrStream pins that EVERY field boundary is taken from the NUL
// framing, never whitespace or a newline — the #3338 root cause. A value
// carrying spaces, a newline and an `=` must parse intact, a trailing partial
// triplet must be dropped rather than panic, and the empty stream is nil.
func TestParseAttrStream(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []attrRecord
	}{
		{"empty", "", nil},
		{"well_formed", attrStream("p", "a", "v"), []attrRecord{{"p", "a", "v"}}},
		{
			"value_with_spaces_newline_equals",
			attrStream("p", "filter", "sed s/a b/c=d/\nx"),
			[]attrRecord{{"p", "filter", "sed s/a b/c=d/\nx"}},
		},
		{
			"trailing_partial_dropped",
			attrStream("p", "a", "v") + "q\x00b\x00",
			[]attrRecord{{"p", "a", "v"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAttrStream(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAttrStream = %+v, want %+v", got, tc.want)
			}
		})
	}
}
