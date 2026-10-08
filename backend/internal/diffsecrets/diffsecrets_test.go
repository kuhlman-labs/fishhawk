package diffsecrets

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// Every synthetic credential in this file is BUILT AT RUNTIME (E80.3 / #3760
// condition 7): a literal in committed source would be flagged by this very
// check on this repository's own runs and could trip forge push protection.
// No string literal below matches a redaction.DefaultPatterns regex on its own.

// classicPAT is a runtime-built string matching github-pat-classic.
func classicPAT() string { return "ghp_" + strings.Repeat("A", 36) }

// samples returns one runtime-built matching value per DefaultPatterns entry.
func samples() map[string]string {
	return map[string]string{
		"github-pat-classic":          classicPAT(),
		"github-pat-fine-grained":     "github_pat_" + strings.Repeat("B", 82),
		"github-app-token":            "ghs_" + strings.Repeat("C", 36),
		"github-oauth-token":          "gho_" + strings.Repeat("K", 36),
		"github-user-to-server-token": "ghu_" + strings.Repeat("L", 36),
		"github-refresh-token":        "ghr_" + strings.Repeat("M", 36),
		"gitlab-pat":                  "glpat-" + strings.Repeat("N", 20),
		"openai-api-key":              "sk-" + strings.Repeat("D", 48),
		"openai-project-key":          "sk-proj-" + strings.Repeat("E", 40),
		"anthropic-api-key":           "sk-ant-api03-" + strings.Repeat("F", 40),
		"aws-access-key-id":           "AKIA" + strings.Repeat("G", 16),
		"authorization-bearer":        "Author" + "ization: Bea" + "rer " + strings.Repeat("h", 12),
		"npm-publish-token":           "npm_" + strings.Repeat("I", 36),
		"json-password-field":         `{"pass` + `word"` + `: "` + strings.Repeat("j", 8) + `"}`,
	}
}

func section(path, hunks string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" + hunks
}

// TestScan_AddedLineCarriesNewSideLineNumber: the hunk seeds the new-side
// counter at 20; context, removed and added lines advance it per the unified
// diff rules, so the key on the third NEW-side line reports line 22.
func TestScan_AddedLineCarriesNewSideLineNumber(t *testing.T) {
	patch := section("config/settings.go",
		"@@ -10,3 +20,4 @@ func x() {\n"+
			" context line\n"+ // new 20
			"-removed line\n"+ // old only
			"+added plain\n"+ // new 21
			"+key = \""+classicPAT()+"\"\n"+ // new 22
			" trailing context\n") // new 23
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{{Path: "config/settings.go", Line: 22, Pattern: "github-pat-classic"}}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
	if res.AddedLines != 2 {
		t.Errorf("AddedLines = %d, want 2", res.AddedLines)
	}
}

// TestScan_RemovedAndContextLinesNeverHit: the same key on a `-` line and a
// ` ` line yields zero hits. COUNTERFACTUAL: make the hunk-state switch scan
// every hunk line (accept-any-hunk-line) — this fixture carries the key ONLY
// on a removed and a context line and no added line at all, so any scanned
// non-`+` line produces a hit and the zero-hit assertion goes RED.
func TestScan_RemovedAndContextLinesNeverHit(t *testing.T) {
	patch := section("a.go",
		"@@ -1,2 +1,1 @@\n"+
			"-old = \""+classicPAT()+"\"\n"+
			" ctx = \""+classicPAT()+"\"\n")
	if res := Scan(patch, redaction.DefaultPatterns); len(res.Hits) != 0 {
		t.Fatalf("hits = %+v, want none (removed/context lines are never scanned)", res.Hits)
	}
}

// TestScan_HeaderLookalikeInsideHunkIsContent: inside a hunk, an added line
// whose content begins `++ ` renders `+++ …` and is CONTENT, not a file
// header — it must neither rename the section nor escape the scan.
func TestScan_HeaderLookalikeInsideHunkIsContent(t *testing.T) {
	patch := section("real.go",
		"@@ -1,0 +1,2 @@\n"+
			"+++ b/decoy.go "+classicPAT()+"\n"+ // new 1: content
			"+x := \""+classicPAT()+"\"\n") // new 2
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{
		{Path: "real.go", Line: 1, Pattern: "github-pat-classic"},
		{Path: "real.go", Line: 2, Pattern: "github-pat-classic"},
	}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
}

// TestScan_CompareReconstructedSectionUsesGitHeaderPath: a GitHub compare
// patch is rebuilt as `diff --git a/<p> b/<p>` + hunks with NO ---/+++ lines,
// so the git header is the only path source.
func TestScan_CompareReconstructedSectionUsesGitHeaderPath(t *testing.T) {
	patch := "diff --git a/one.go b/one.go\n@@ -1 +1,2 @@\n line\n+v := \"" + classicPAT() + "\"\n" +
		"diff --git a/dir with space/two.go b/dir with space/two.go\n@@ -0,0 +5 @@\n+w := \"" + classicPAT() + "\"\n"
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{
		{Path: "one.go", Line: 2, Pattern: "github-pat-classic"},
		{Path: "dir with space/two.go", Line: 5, Pattern: "github-pat-classic"},
	}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
}

// TestScan_QuotedPath: git C-quotes a path carrying a quote, backslash,
// control or non-ASCII byte; both the +++ header and the git header forms
// decode back to the real name.
func TestScan_QuotedPath(t *testing.T) {
	plus := "diff --git \"a/sp\\303\\251c.go\" \"b/sp\\303\\251c.go\"\n" +
		"--- \"a/sp\\303\\251c.go\"\n+++ \"b/sp\\303\\251c.go\"\n@@ -0,0 +1 @@\n+k := \"" + classicPAT() + "\"\n"
	header := "diff --git a/plain.go \"b/q\\\"uote.go\"\n@@ -0,0 +3 @@\n+k := \"" + classicPAT() + "\"\n"
	res := Scan(plus+header, redaction.DefaultPatterns)
	want := []Hit{
		{Path: "spéc.go", Line: 1, Pattern: "github-pat-classic"},
		{Path: "q\"uote.go", Line: 3, Pattern: "github-pat-classic"},
	}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
}

// TestScan_UnresolvedPathStillReported: a hunk with no resolvable header is
// still scanned; the hit carries the placeholder path rather than being
// dropped.
func TestScan_UnresolvedPathStillReported(t *testing.T) {
	patch := "@@ -0,0 +1 @@\n+k := \"" + classicPAT() + "\"\n"
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{{Path: UnresolvedPath, Line: 1, Pattern: "github-pat-classic"}}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
}

// TestScan_MalformedHunkHeaderStillScansAddedLines (carried from #3760 into
// E80.4 / #3761): a `@@` header whose new-side start cannot be parsed still
// opens a hunk, so the `+` line under it is scanned and reported against its
// path with UnknownLine, and Note renders it "(line unknown)".
// COUNTERFACTUAL: restore the old `if start, ok := hunkNewStart(line); ok {
// state = stateHunk; … }` branch — this fixture's section has NO well-formed
// header before the key-bearing `+` line (only `@@ garbage @@`), so the
// section is still in header state when that line arrives, header state
// discards it, and zero hits are reported where the test asserts one.
func TestScan_MalformedHunkHeaderStillScansAddedLines(t *testing.T) {
	patch := section("conf/app.go",
		"@@ garbage @@\n"+
			" context\n"+
			"+k := \""+classicPAT()+"\"\n")
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{{Path: "conf/app.go", Line: UnknownLine, Pattern: "github-pat-classic"}}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
	note := Note(GroupHits(res.Hits)[0])
	if !strings.Contains(note, "conf/app.go:(line unknown)") {
		t.Errorf("note does not render the unknown line:\n%s", note)
	}
	if strings.Contains(note, "conf/app.go:0") {
		t.Errorf("note renders the UnknownLine sentinel as a line number:\n%s", note)
	}
}

// TestScan_MalformedHunkHeaderMidHunkMarksLineUnknown: a malformed `@@` inside
// a hunk keeps hunk state with the line unknown, and the NEXT well-formed
// header restores real numbering. COUNTERFACTUAL: make the assignment
// `lineKnown = ok || true` (keep hunk state but treat the line as known) — a
// failed parse seeds the counter at 0, so the FIRST `+` line after the
// malformed header would still read 0; the fixture therefore carries a SECOND
// `+` line there, which the mutation numbers 1 instead of UnknownLine, and the
// hit list goes RED. The third hit must restart at 40, pinning that the next
// well-formed header restores real numbering.
func TestScan_MalformedHunkHeaderMidHunkMarksLineUnknown(t *testing.T) {
	patch := section("a.go",
		"@@ -1,1 +1,2 @@\n"+
			"+one := \""+classicPAT()+"\"\n"+ // new 1
			"@@ -9 +nonsense @@\n"+
			"+two := \""+classicPAT()+"\"\n"+ // unknown
			"+two2 := \""+classicPAT()+"\"\n"+ // unknown (1 if the line were "known")
			"@@ -30 +40 @@\n"+
			"+three := \""+classicPAT()+"\"\n") // new 40
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{
		{Path: "a.go", Line: 1, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: UnknownLine, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: UnknownLine, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: 40, Pattern: "github-pat-classic"},
	}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
	// A malformed header in one file never bleeds into the next file's
	// well-formed hunk: that file's own `@@` re-establishes the line.
	two := section("x.go", "@@ bad @@\n+a := \""+classicPAT()+"\"\n") +
		section("y.go", "@@ -0,0 +3 @@\n+b := \""+classicPAT()+"\"\n")
	got := Scan(two, redaction.DefaultPatterns).Hits
	wantTwo := []Hit{
		{Path: "x.go", Line: UnknownLine, Pattern: "github-pat-classic"},
		{Path: "y.go", Line: 3, Pattern: "github-pat-classic"},
	}
	if !reflect.DeepEqual(got, wantTwo) {
		t.Fatalf("two-section hits = %+v, want %+v", got, wantTwo)
	}
}

// TestScan_CredentialShapedFileNameIsRedacted: the path itself passes through
// the pattern set, so a file NAMED like a credential cannot carry it onto a
// hit.
func TestScan_CredentialShapedFileNameIsRedacted(t *testing.T) {
	name := "fixtures/" + classicPAT() + ".txt"
	patch := section(name, "@@ -0,0 +1 @@\n+k := \""+classicPAT()+"\"\n")
	res := Scan(patch, redaction.DefaultPatterns)
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %+v, want 1", res.Hits)
	}
	if strings.Contains(res.Hits[0].Path, classicPAT()) {
		t.Fatalf("hit path %q carries the credential bytes", res.Hits[0].Path)
	}
	if res.Hits[0].Path != "fixtures/[REDACTED:github-pat-classic].txt" {
		t.Errorf("hit path = %q", res.Hits[0].Path)
	}
}

// TestScan_DeletedFileAndBinarySectionsNoPanic: a deletion (+++ /dev/null), a
// binary section and a header-only section parse without panicking, and the
// deleted file's removed key is not a hit; a following section is still
// attributed correctly.
func TestScan_DeletedFileAndBinarySectionsNoPanic(t *testing.T) {
	patch := "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n-k := \"" + classicPAT() + "\"\n" +
		"diff --git a/img.png b/img.png\nBinary files a/img.png and b/img.png differ\n" +
		"diff --git a/mode.sh b/mode.sh\nold mode 100644\nnew mode 100755\n" +
		"diff --git a/next.go b/next.go\n@@ -0,0 +7 @@\n+k := \"" + classicPAT() + "\"\n" +
		"@@ malformed hunk header\n" +
		"\\ No newline at end of file\n"
	res := Scan(patch, redaction.DefaultPatterns)
	want := []Hit{{Path: "next.go", Line: 7, Pattern: "github-pat-classic"}}
	if !reflect.DeepEqual(res.Hits, want) {
		t.Fatalf("hits = %+v, want %+v", res.Hits, want)
	}
	for _, in := range []string{"", "\n", "diff --git \n", "+++ \n@@ -0,0 +x @@\n+k\n", "diff --git \"a/unterminated\n"} {
		_ = Scan(in, redaction.DefaultPatterns)
	}
}

// TestScan_EveryDefaultPatternFires: one runtime-built sample per
// DefaultPatterns entry is detected under that pattern's name.
// COUNTERFACTUAL: pass a pattern set missing one entry (the subtest below
// drops github-pat-classic) — that pattern's sample, which matches NO other
// default regex, then yields no hit under its name, so the row goes RED; the
// drop arm asserts exactly that, pinning that detection rides the passed set.
func TestScan_EveryDefaultPatternFires(t *testing.T) {
	s := samples()
	if len(s) != len(redaction.DefaultPatterns) {
		t.Fatalf("samples cover %d patterns, DefaultPatterns has %d — add a sample", len(s), len(redaction.DefaultPatterns))
	}
	for _, p := range redaction.DefaultPatterns {
		sample, ok := s[p.Name]
		if !ok {
			t.Errorf("no sample for pattern %q", p.Name)
			continue
		}
		patch := section("f.go", "@@ -0,0 +1 @@\n+v := "+sample+"\n")
		if !hasPattern(Scan(patch, redaction.DefaultPatterns).Hits, p.Name) {
			t.Errorf("pattern %q: sample not detected", p.Name)
		}
	}
	t.Run("missing pattern set misses its sample", func(t *testing.T) {
		var reduced []redaction.Pattern
		for _, p := range redaction.DefaultPatterns {
			if p.Name != "github-pat-classic" {
				reduced = append(reduced, p)
			}
		}
		patch := section("f.go", "@@ -0,0 +1 @@\n+v := "+classicPAT()+"\n")
		if hits := Scan(patch, reduced).Hits; len(hits) != 0 {
			t.Errorf("reduced set hits = %+v, want none", hits)
		}
	})
}

func hasPattern(hits []Hit, name string) bool {
	for _, h := range hits {
		if h.Pattern == name {
			return true
		}
	}
	return false
}

// TestScan_OneHitPerLinePerPattern: two matches of one pattern on one line
// are one hit.
func TestScan_OneHitPerLinePerPattern(t *testing.T) {
	patch := section("f.go", "@@ -0,0 +1 @@\n+a, b := \""+classicPAT()+"\", \""+classicPAT()+"\"\n")
	if hits := Scan(patch, redaction.DefaultPatterns).Hits; len(hits) != 1 {
		t.Fatalf("hits = %+v, want 1", hits)
	}
}

func TestGroupHits_FoldsByPathAndPatternWithSortedLines(t *testing.T) {
	hits := []Hit{
		{Path: "b.go", Line: 9, Pattern: "aws-access-key-id"},
		{Path: "a.go", Line: 7, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: 3, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: 7, Pattern: "github-pat-classic"},
		{Path: "a.go", Line: 5, Pattern: "aws-access-key-id"},
	}
	want := []Group{
		{Path: "a.go", Pattern: "aws-access-key-id", Lines: []int{5}},
		{Path: "a.go", Pattern: "github-pat-classic", Lines: []int{3, 7}},
		{Path: "b.go", Pattern: "aws-access-key-id", Lines: []int{9}},
	}
	if got := GroupHits(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %+v, want %+v", got, want)
	}
	if got := GroupHits(nil); len(got) != 0 {
		t.Errorf("GroupHits(nil) = %+v, want empty", got)
	}
}

// TestCheckKey_Injective pins the FACT injectivity rests on (no pattern name
// contains '|') and that distinct (pattern, path) pairs — including paths
// carrying '|' — never collide.
func TestCheckKey_Injective(t *testing.T) {
	paths := []string{"a.go", "a|b.go", "|", "x/y.go", "github-pat-classic|a.go"}
	seen := map[string][2]string{}
	for _, p := range redaction.DefaultPatterns {
		if strings.Contains(p.Name, "|") {
			t.Fatalf("pattern name %q contains '|' — CheckKey would no longer be injective", p.Name)
		}
		for _, path := range paths {
			k := CheckKey(p.Name, path)
			if prev, dup := seen[k]; dup {
				t.Fatalf("CheckKey collision: %v and %v -> %q", prev, [2]string{p.Name, path}, k)
			}
			seen[k] = [2]string{p.Name, path}
			if !strings.HasPrefix(k, CheckName+"|"+p.Name+"|") {
				t.Errorf("key %q lacks its check/pattern prefix", k)
			}
		}
	}
	g := Group{Path: "a.go", Pattern: "aws-access-key-id"}
	if g.Key() != CheckKey("aws-access-key-id", "a.go") {
		t.Errorf("Group.Key = %q", g.Key())
	}
}

func TestNote_NamesLocationAndClassNotValue(t *testing.T) {
	g := Group{Path: "config/settings.go", Pattern: "github-pat-classic", Lines: []int{2, 9}}
	note := Note(g)
	for _, want := range []string{"github-pat-classic", "config/settings.go:2", "config/settings.go:9", "not by a model reviewer", "human waive", "cannot waive or defer"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	for _, p := range redaction.DefaultPatterns {
		if p.Regex.MatchString(note) {
			t.Errorf("note matches pattern %q:\n%s", p.Name, note)
		}
	}

	many := Group{Path: "f.go", Pattern: "aws-access-key-id"}
	for i := 1; i <= noteLineCap+3; i++ {
		many.Lines = append(many.Lines, i)
	}
	n := Note(many)
	if !strings.Contains(n, "and 3 more line(s)") || strings.Contains(n, "f.go:"+strconv.Itoa(noteLineCap+1)) {
		t.Errorf("capped note wrong:\n%s", n)
	}
}

// TestHit_CarriesNoBytes reflects over every string field of every Hit and
// Group produced from a key-bearing patch: no field value contains the key.
func TestHit_CarriesNoBytes(t *testing.T) {
	var b strings.Builder
	for name, v := range samples() {
		b.WriteString(section(name+".go", "@@ -0,0 +1 @@\n+v := "+v+"\n"))
	}
	res := Scan(b.String(), redaction.DefaultPatterns)
	if len(res.Hits) < len(redaction.DefaultPatterns) {
		t.Fatalf("hits = %d, want >= %d", len(res.Hits), len(redaction.DefaultPatterns))
	}
	check := func(v reflect.Value) {
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.Kind() != reflect.String {
				continue
			}
			for _, secret := range samples() {
				if strings.Contains(f.String(), secret) {
					t.Errorf("%s.%s carries credential bytes: %q", v.Type().Name(), v.Type().Field(i).Name, f.String())
				}
			}
		}
	}
	for _, h := range res.Hits {
		check(reflect.ValueOf(h))
	}
	for _, g := range GroupHits(res.Hits) {
		check(reflect.ValueOf(g))
		for _, secret := range samples() {
			if strings.Contains(Note(g), secret) || strings.Contains(g.Key(), secret) {
				t.Errorf("note/key for %s carries credential bytes", g.Path)
			}
		}
	}
}

// TestUnquoteC_GitEscapes pins the git C-quote decoder: every named escape,
// an octal byte, and the malformed forms (unterminated, dangling backslash,
// short or non-octal escape), which fail rather than guess.
func TestUnquoteC_GitEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want string
		n    int
		ok   bool
	}{
		{`"a\a\b\t\n\v\f\r\"\\z" tail`, "a\a\b\t\n\v\f\r\"\\z", 22, true},
		{`"\303\251"`, "é", 10, true},
		{`"unterminated`, "", 0, false},
		{`"dangling\`, "", 0, false},
		{`"\30"`, "", 0, false},
		{`"\9xx"`, "", 0, false},
		{`"\777"`, "", 0, false},
		{`noquote`, "", 0, false},
	}
	for _, c := range cases {
		got, n, ok := unquoteC(c.in)
		if got != c.want || n != c.n || ok != c.ok {
			t.Errorf("unquoteC(%q) = (%q, %d, %v), want (%q, %d, %v)", c.in, got, n, ok, c.want, c.n, c.ok)
		}
	}
}

// TestHeaderPaths pins the two header path resolvers' edge forms.
func TestHeaderPaths(t *testing.T) {
	for in, want := range map[string]string{
		`a/x.go b/x.go`:             "x.go",
		`a/old.go b/new.go`:         "new.go",
		`"a/q\"1.go" "b/q\"1.go"`:   `q"1.go`,
		`"a/q\"1.go" b/plain.go`:    "plain.go",
		`"a/unterminated`:           "",
		`"a/ok.go" "b/unterminated`: "",
		`a/x.go "b/unterminated`:    "",
		`nothing-to-split`:          "",
	} {
		if got := gitHeaderPath(in); got != want {
			t.Errorf("gitHeaderPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]any{
		"b/x.go":                 {"x.go", true},
		"x.go\t2026-01-01 00:00": {"x.go", true},
		"/dev/null":              {"", false},
		"":                       {"", false},
		`"b/sp ace.go"`:          {"sp ace.go", true},
		`"b/unterminated`:        {"", false},
	} {
		got, ok := plusPath(in)
		if got != want[0] || ok != want[1] {
			t.Errorf("plusPath(%q) = (%q, %v), want %v", in, got, ok, want)
		}
	}
	if _, ok := hunkNewStart("@@ -1 @@"); ok {
		t.Errorf("hunkNewStart accepted a header with no new side")
	}
	if n, ok := hunkNewStart("@@ -1 +7 @@"); !ok || n != 7 {
		t.Errorf("hunkNewStart(+7) = %d, %v", n, ok)
	}
}
