package server

// ADR-110 guard (i) (#4626): a repo-wide go/ast literal guard. A repository's
// work-management conventions — its label namespaces and its epic title shape
// — belong in its declaration (.fishhawk/work-management.yaml, resolved per
// repository by the conventions loader), never in product code. This test
// scans every non-test .go file under backend/, cli/ and runner/ and fails on
// a STRING literal that hard-codes one. Every rule is CASE-INSENSITIVE: the
// value is lower-cased before it is classified, so "AREA:", "Type:Feature" or
// a "\[e" regex source is caught too (#4626 finding 4):
//
//	(a) a bare label-namespace prefix: exactly "area:", "type:" or "phase:";
//	(b) a namespaced label: a value matching ^(area|phase|type):\S (so a
//	    "type: " prose/YAML key does not match);
//	(c) escaped epic-title regex source: a value containing the two characters
//	    backslash + "[e" (either case of the e). A bare "[E" in prose or a
//	    format string ("[E%s.<n>]", "[ELIDED") is out of reach by design.
//
// Stated limits: a SPLIT literal such as labelValue(c.Labels, "type") ==
// "epic" (no single BasicLit carries the namespaced label), a bare "[E" in
// prose or a format string, and a namespace spelled without its colon
// (workitems.go's areaNamespace = "area") stay out of reach of a
// single-literal rule.
//
// Remedy: derive the value from the resolved conventions
// (workmgmt.EpicTitleNumber, workmgmt.TypeLabelNamespaces). The exemption
// table below is FROZEN and SHRINK-ONLY: it names the seven sites that remain
// after #4626, each with the remaining-scope item that removes it. An
// exemption that no longer matches fails the test (delete it), and the table
// may not grow past its frozen size.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// literalGuardRoots are the repository-relative module roots scanned.
var literalGuardRoots = []string{"backend", "cli", "runner"}

// literalGuardMinFiles is the anti-vacuity floor: a scan that reads fewer
// files than this has lost its roots (961 non-test files at 194da0fb).
const literalGuardMinFiles = 500

// literalGuardExemptionCeiling is the table's frozen size at #4626. The table
// only shrinks; raising this number is a deliberate, reviewable act.
const literalGuardExemptionCeiling = 7

// literalGuardKey is one exemption: a repository-relative file path and the
// exact (unquoted) literal value allowed in it. The key is the ORIGINAL
// literal, never its lower-cased classification form.
type literalGuardKey struct {
	path    string
	literal string
}

// literalGuardExemptions is the FROZEN, shrink-only exemption table. Each
// value names why the site is still a literal and the remaining ADR-110 scope
// that removes it (#4626 PR body).
var literalGuardExemptions = map[literalGuardKey]string{
	{"backend/internal/plan/commsreport.go", "area:"}:                     "CommsLabelNamespaces is pinned to the comms scan prompt (TestCommsReport_PromptParity); making it conventions-driven needs a backend/internal/prompt/ change and an operator catch-rate re-record",
	{"backend/internal/plan/commsreport.go", "type:"}:                     "CommsLabelNamespaces (see the area: entry)",
	{"backend/internal/plan/commsreport.go", "phase:"}:                    "CommsLabelNamespaces (see the area: entry)",
	{"backend/internal/server/live_validation_filing.go", "area:backend"}: "liveValidationWalkArea: the conventions cannot express a walk-only area; needs a conventions-expressible walk label default plus the repository's declaration edit",
	{"backend/internal/server/product_report.go", "type:feature"}:         "product-report filing type labels; remaining scope",
	{"backend/internal/server/product_report.go", "type:bug"}:             "product-report filing type labels; remaining scope",
	{"backend/internal/intakegroom/epic.go", `^\s*\[E\d+\]`}:              "intakegroom epic detection; remaining scope (its labelValue(\"type\") == \"epic\" check is a split literal no single-literal rule sees, named in remaining scope instead)",
}

// literalGuardLabelRE is rule (b).
var literalGuardLabelRE = regexp.MustCompile(`^(area|phase|type):\S`)

// literalGuardRule classifies one unquoted string value, returning the rule
// it breaks ("a", "b" or "c") or "". It classifies the LOWER-CASED value, so
// every rule is case-insensitive.
func literalGuardRule(v string) string {
	lv := strings.ToLower(v)
	switch {
	case lv == "area:" || lv == "type:" || lv == "phase:":
		return "a"
	case literalGuardLabelRE.MatchString(lv):
		return "b"
	case strings.Contains(lv, `\[e`):
		return "c"
	}
	return ""
}

// literalGuardHit is one flagged literal.
type literalGuardHit struct {
	path    string
	line    int
	literal string
	rule    string
}

// scanLiteralGuardFile walks f's STRING BasicLits and returns every hit.
func scanLiteralGuardFile(fset *token.FileSet, f *ast.File, path string) []literalGuardHit {
	var hits []literalGuardHit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if rule := literalGuardRule(v); rule != "" {
			hits = append(hits, literalGuardHit{path: path, line: fset.Position(lit.Pos()).Line, literal: v, rule: rule})
		}
		return true
	})
	return hits
}

// TestNoRepositorySpecificConventionLiterals is the guard itself.
func TestNoRepositorySpecificConventionLiterals(t *testing.T) {
	if len(literalGuardExemptions) > literalGuardExemptionCeiling {
		t.Fatalf("literal-guard exemption table has %d entries, past its frozen ceiling %d: the table only shrinks — derive the new site from the conventions instead (ADR-110 guard (i), #4626)",
			len(literalGuardExemptions), literalGuardExemptionCeiling)
	}
	root := filepath.Join(pkgSrcDir, "..", "..", "..")
	fset := token.NewFileSet()
	scanned := 0
	used := map[literalGuardKey]bool{}
	var violations []literalGuardHit
	for _, r := range literalGuardRoots {
		err := filepath.WalkDir(filepath.Join(root, r), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "db" || name == "testdata" || (strings.HasPrefix(name, ".") && p != filepath.Join(root, r)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			scanned++
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, h := range scanLiteralGuardFile(fset, f, rel) {
				key := literalGuardKey{path: h.path, literal: h.literal}
				if _, ok := literalGuardExemptions[key]; ok {
					used[key] = true
					continue
				}
				violations = append(violations, h)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", r, err)
		}
	}
	t.Logf("scanned %d non-test .go files", scanned)
	if scanned <= literalGuardMinFiles {
		t.Fatalf("scanned %d non-test .go files, want more than %d: the guard has lost its roots and would pass vacuously", scanned, literalGuardMinFiles)
	}
	for _, v := range violations {
		t.Errorf("%s:%d: rule (%s) repository-specific convention literal %q: derive it from the resolved conventions (workmgmt.EpicTitleNumber / workmgmt.TypeLabelNamespaces) — the exemption table is frozen (ADR-110 guard (i), #4626)",
			v.path, v.line, v.rule, v.literal)
	}
	var stale []string
	for key := range literalGuardExemptions {
		if !used[key] {
			stale = append(stale, key.path+" "+strconv.Quote(key.literal))
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("stale literal-guard exemption %s: the literal no longer appears there — delete the entry (the table only shrinks) and lower literalGuardExemptionCeiling", s)
	}
}

// TestLiteralGuardScanner_DetectsEachRule self-tests the scanner on in-memory
// sources, so a scanner that silently flags nothing cannot keep the guard
// green: each banned shape is flagged under its rule, and each negative is not.
func TestLiteralGuardScanner_DetectsEachRule(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // rule, or "" for not flagged
	}{
		{"a: area prefix", `"area:"`, "a"},
		{"a: type prefix", `"type:"`, "a"},
		{"a: phase prefix", `"phase:"`, "a"},
		{"b: phase label", `"phase:build"`, "b"},
		{"b: type label", `"type:feature"`, "b"},
		{"b: area label in a raw string", "`area:backend`", "b"},
		{"c: escaped epic regex (raw)", "`\\[E\\d+`", "c"},
		{"c: escaped epic regex (interpreted)", `"^\\s*\\[E(\\d+)"`, "c"},
		{"a: upper-case area prefix", `"AREA:"`, "a"},
		{"b: mixed-case type label", `"Type:Feature"`, "b"},
		{"c: lower-case escaped epic regex", "`^\\s*\\[e(\\d+)`", "c"},
		{"negative: yaml key prose", `"type: "`, ""},
		{"negative: format string", `"[E%s.<n>]"`, ""},
		{"negative: bare bracket prose", `"[ELIDED"`, ""},
		{"negative: other namespace", `"autonomy:medium"`, ""},
		{"negative: namespace mid-string", `"the area:backend label"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\n\nvar x = " + tc.src + "\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", src, 0)
			if err != nil {
				t.Fatalf("parse %q: %v", src, err)
			}
			hits := scanLiteralGuardFile(fset, f, "x.go")
			switch {
			case tc.want == "" && len(hits) != 0:
				t.Errorf("flagged %+v, want no hit", hits)
			case tc.want != "" && (len(hits) != 1 || hits[0].rule != tc.want):
				t.Errorf("hits = %+v, want exactly one rule (%s) hit", hits, tc.want)
			}
		})
	}
}

// parseRepoOwnWorkMgmtDeclaration parses THIS repository's own
// .fishhawk/work-management.yaml, read-only, anchored on pkgSrcDir (#4179).
// Each call parses fresh, so a test may mutate the returned maps.
func parseRepoOwnWorkMgmtDeclaration(t *testing.T) workmgmt.Conventions {
	t.Helper()
	path := filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "work-management.yaml")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	conv, err := workmgmt.Parse(f)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return conv
}
