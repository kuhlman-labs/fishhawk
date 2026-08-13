package constraint

// MIRROR (#2660): the table below is a port of
// backend/internal/policy/commentonly_test.go, pinning the runner copy of
// the detector to the same behavior. Change them together — a divergence
// here is exactly the runner/backend disagreement the mirror exists to
// prevent.

import "testing"

// goSection renders one `diff --git` section with the standard git
// headers, so every case below differs only in the part it is testing.
func goSection(path, body string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		body
}

const commentOnlyBody = "@@ -1,6 +1,6 @@\n" +
	" package foo\n" +
	" \n" +
	"-// Foo does a thing.\n" +
	"+// Foo does the thing.\n" +
	"+\n" +
	" func Foo() {}\n"

func TestIsGoDirectiveComment(t *testing.T) {
	// The port is pinned by table because go/ast.IsDirective DOES NOT
	// EXIST (verified against go1.26.3: go/ast exports only IsExported
	// and IsGenerated), so the logic is ours and the boundary cases —
	// especially the NEGATIVE `// go:build` with a space, which the
	// compiler reads as an ordinary comment — must be asserted, not
	// assumed.
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"go:build", "//go:build linux", true},
		{"go:generate", "//go:generate stringer -type=T", true},
		{"go:embed", "//go:embed static", true},
		{"go:linkname", "//go:linkname a b", true},
		{"line directive", "//line file.go:1", true},
		{"legacy build tag", "// +build linux", true},
		{"cgo cflags", "// #cgo CFLAGS: -g", true},
		{"cgo include", "// #include <stdio.h>", true},
		{"extern", "//extern name", true},
		{"export", "//export Name", true},

		{"go:build with a space is an ordinary comment", "// go:build linux", false},
		{"plain sentence", "// Foo does the thing.", false},
		{"colon after an uppercase word", "// Note: something", false},
		{"space after the colon", "//todo: fix", false},
		{"bare slashes", "//", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGoDirectiveComment(tc.line); got != tc.want {
				t.Fatalf("isGoDirectiveComment(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestDetectCommentOnlyGo_Pass(t *testing.T) {
	sig := DetectCommentOnlyGo(Diff{
		ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
		Patch:        goSection("backend/foo.go", commentOnlyBody),
	})
	if sig == nil || !sig.CommentOnly {
		t.Fatalf("want comment-only, got %+v", sig)
	}
	if sig.Reason != "" {
		t.Fatalf("a satisfied verdict carries no reason, got %q", sig.Reason)
	}
	if len(sig.Files) != 1 || sig.Files[0] != "backend/foo.go" {
		t.Fatalf("files = %v, want [backend/foo.go]", sig.Files)
	}
}

// TestDetectCommentOnlyGo_Refusals covers ONE case per closed reason
// code — the detector is fail-closed on every input that is not provably
// a comment-only Go change, and each named branch is asserted here.
func TestDetectCommentOnlyGo_Refusals(t *testing.T) {
	cases := []struct {
		name string
		diff Diff
		want string
	}{
		{
			name: "empty diff",
			diff: Diff{},
			want: ReasonEmptyDiff,
		},
		{
			name: "docs only",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "docs/x.md", Status: StatusModified}},
				Patch:        goSection("docs/x.md", commentOnlyBody),
			},
			want: ReasonNoGoSourceChanged,
		},
		{
			name: "non-go testable source changed",
			diff: Diff{
				ChangedFiles: []ChangedFile{
					{Path: "backend/foo.go", Status: StatusModified},
					{Path: "scripts/tool.py", Status: StatusModified},
				},
				Patch: goSection("backend/foo.go", commentOnlyBody),
			},
			want: ReasonNonGoSourceChanged,
		},
		{
			name: "deleted go file",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusDeleted}},
				Patch:        goSection("backend/foo.go", commentOnlyBody),
			},
			want: ReasonUnsupportedStatus,
		},
		{
			name: "renamed go file",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusRenamed}},
				Patch:        goSection("backend/foo.go", commentOnlyBody),
			},
			want: ReasonUnsupportedStatus,
		},
		{
			name: "no patch",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
			},
			want: ReasonPatchAbsent,
		},
		{
			name: "truncated patch",
			diff: Diff{
				ChangedFiles:   []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch:          goSection("backend/foo.go", commentOnlyBody),
				PatchTruncated: true,
			},
			want: ReasonPatchTruncated,
		},
		{
			name: "binary section",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch: "diff --git a/backend/foo.go b/backend/foo.go\n" +
					"index 1111111..2222222 100644\n" +
					"Binary files a/backend/foo.go and b/backend/foo.go differ\n",
			},
			want: ReasonBinaryPatch,
		},
		{
			name: "c-quoted header path",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/fée.go", Status: StatusModified}},
				Patch: "diff --git \"a/backend/f\\303\\251e.go\" \"b/backend/f\\303\\251e.go\"\n" +
					"index 1111111..2222222 100644\n" +
					"--- \"a/backend/f\\303\\251e.go\"\n" +
					"+++ \"b/backend/f\\303\\251e.go\"\n" +
					commentOnlyBody,
			},
			want: ReasonQuotedPatchPath,
		},
		{
			name: "patch section names an unchanged path",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch: goSection("backend/foo.go", commentOnlyBody) +
					goSection("backend/other.go", commentOnlyBody),
			},
			want: ReasonPatchPathUnmatched,
		},
		{
			name: "changed go file absent from the patch",
			diff: Diff{
				ChangedFiles: []ChangedFile{
					{Path: "backend/foo.go", Status: StatusModified},
					{Path: "docs/x.md", Status: StatusModified},
				},
				Patch: goSection("docs/x.md", commentOnlyBody),
			},
			want: ReasonFileMissingFromPatch,
		},
		{
			name: "backtick in the emitted section",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch: goSection("backend/foo.go", "@@ -1,4 +1,4 @@\n"+
					" var q = `select 1`\n"+
					"-// old note\n"+
					"+// new note\n"),
			},
			want: ReasonRawStringDelimiterInSection,
		},
		{
			name: "directive line changed",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch: goSection("backend/foo.go", "@@ -1,3 +1,3 @@\n"+
					"-//go:build linux\n"+
					"+//go:build linux || darwin\n"+
					" \n"),
			},
			want: ReasonDirectiveChanged,
		},
		{
			name: "statement line changed",
			diff: Diff{
				ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
				Patch: goSection("backend/foo.go", "@@ -1,3 +1,3 @@\n"+
					" func Foo() int {\n"+
					"-\treturn 1\n"+
					"+\treturn 2\n"),
			},
			want: ReasonNonCommentLineChanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := DetectCommentOnlyGo(tc.diff)
			if sig == nil {
				t.Fatal("signal is nil; the detector always returns a verdict")
			}
			if sig.CommentOnly {
				t.Fatalf("want refusal %q, got comment-only", tc.want)
			}
			if sig.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", sig.Reason, tc.want)
			}
			if len(sig.Files) != 0 {
				t.Fatalf("a refusal carries no files, got %v", sig.Files)
			}
		})
	}
}

// TestDetectCommentOnlyGo_AddedLineLookingLikeAHeader pins the bounded
// header state machine: `++ ` at the start of an ADDED line is source
// text, not a `+++` header, so the section's path resolution and the
// changed-line inspection must both read it as a changed line.
func TestDetectCommentOnlyGo_AddedLineLookingLikeAHeader(t *testing.T) {
	sig := DetectCommentOnlyGo(Diff{
		ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
		Patch: goSection("backend/foo.go", "@@ -1,3 +1,3 @@\n"+
			" func Foo() {\n"+
			"++ b/evil.go\n"),
	})
	if sig.CommentOnly {
		t.Fatal("an added line beginning with `++ ` is a changed source line, not a header")
	}
	if sig.Reason != ReasonNonCommentLineChanged {
		t.Fatalf("reason = %q, want %q", sig.Reason, ReasonNonCommentLineChanged)
	}
}

// TestDetectCommentOnlyGo_KnownFalsePositive_RawStringDelimitersOutsideContext
// records the residual this design KNOWINGLY accepts (#2660, per the
// operator's ratification): a changed blank or `//`-shaped line that
// actually sits inside a Go raw-string literal whose backtick delimiters
// lie entirely OUTSIDE the emitted context is admitted as comment-only.
// Behavioral emptiness is not decidable from a unified diff, and the
// trace bundle carries no file contents.
//
// The cost is one vacuous satisfaction of `tests_added_or_updated` PER
// OCCURRENCE — it is NOT bounded at one ever, and this test does not
// claim it is. Closing it properly needs a runner-side AST or lexer
// signal, where the working tree exists. If a future change makes this
// case return false, that is an IMPROVEMENT: update this test rather
// than reverting the change.
func TestDetectCommentOnlyGo_KnownFalsePositive_RawStringDelimitersOutsideContext(t *testing.T) {
	// The real file reads:
	//
	//	var tmpl = `
	//	// this line is string data, not a comment
	//	`
	//
	// but the emitted hunk's context window excludes both backticks.
	sig := DetectCommentOnlyGo(Diff{
		ChangedFiles: []ChangedFile{{Path: "backend/foo.go", Status: StatusModified}},
		Patch: goSection("backend/foo.go", "@@ -40,3 +40,3 @@\n"+
			" // heading\n"+
			"-// this line is string data, not a comment\n"+
			"+// this line is DIFFERENT string data, still not a comment\n"),
	})
	if sig == nil || !sig.CommentOnly {
		t.Fatalf("KNOWN FALSE POSITIVE: this case is admitted today; got %+v", sig)
	}
}
