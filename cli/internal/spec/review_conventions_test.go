package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The CLI half of the review_conventions parity proof (ADR-068 / E55.2 /
// #2243). The shared corpus docs/spec/review-conventions-fixtures.json is
// mirrored into testdata/ by scripts/sync-schemas and run here through the
// REAL `fishhawk validate` path (ValidateBytes: YAML -> version-routed schema
// -> v2 reuse resolution -> semantic sweep); backend/internal/spec runs the
// SAME rows through ParseBytes (TestReviewConventionsCorpus_BackendMatches).
// This file is `package spec` (internal) so the path-reason branches YAML
// cannot reach — an empty path the schema refuses first, and invalid UTF-8 a
// YAML scalar cannot carry — are pinned directly.

const rcCorpusPath = "testdata/review-conventions-fixtures.json"

// rcCorpusRow is one corpus row. Message is a pointer so an ABSENT message on
// a semantic row is distinguishable from an empty one and fails the loader.
type rcCorpusRow struct {
	Name    string  `json:"name"`
	Note    string  `json:"note"`
	Doc     string  `json:"doc"`
	Valid   *bool   `json:"valid"`
	Layer   string  `json:"layer"`
	Path    string  `json:"path"`
	Message *string `json:"message"`
}

// loadRCCorpus reads and SHAPE-CHECKS the corpus, failing on an empty or
// malformed one so the CLI runner can never pass vacuously — the same guard
// the backend loader applies.
func loadRCCorpus(t *testing.T) []rcCorpusRow {
	t.Helper()
	raw, err := os.ReadFile(rcCorpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus struct {
		Rows []rcCorpusRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(corpus.Rows) == 0 {
		t.Fatal("review-conventions corpus has no rows — the parity proof would pass vacuously")
	}
	seen := map[string]bool{}
	kinds := map[string]int{}
	for i, r := range corpus.Rows {
		if r.Name == "" || seen[r.Name] {
			t.Fatalf("row %d: name %q is empty or duplicated", i, r.Name)
		}
		seen[r.Name] = true
		if strings.TrimSpace(r.Doc) == "" {
			t.Fatalf("row %q: empty doc", r.Name)
		}
		if r.Valid == nil {
			t.Fatalf("row %q: missing `valid`", r.Name)
		}
		if *r.Valid {
			if r.Layer != "" || r.Path != "" || r.Message != nil {
				t.Fatalf("row %q: a valid row carries layer/path/message", r.Name)
			}
			kinds["valid"]++
			continue
		}
		if !strings.HasPrefix(r.Path, "/") {
			t.Fatalf("row %q: rejection path %q is not a JSON pointer", r.Name, r.Path)
		}
		switch r.Layer {
		case "semantic":
			if r.Message == nil || *r.Message == "" {
				t.Fatalf("row %q: a semantic row needs its exact message", r.Name)
			}
		case "schema":
			if r.Message != nil {
				t.Fatalf("row %q: a schema row asserts a path only, not a message", r.Name)
			}
		default:
			t.Fatalf("row %q: unknown layer %q", r.Name, r.Layer)
		}
		kinds[r.Layer]++
	}
	for _, k := range []string{"valid", "semantic", "schema"} {
		if kinds[k] == 0 {
			t.Fatalf("review-conventions corpus has no %s row", k)
		}
	}
	return corpus.Rows
}

// rcEntries returns ValidateBytes' entries, failing on any other error type.
func rcEntries(t *testing.T, err error) []ValidationErrorEntry {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T %v, want *ValidationError", err, err)
	}
	return ve.Errors
}

// TestReviewConventionsCorpus_CLIMatches runs every corpus row through
// ValidateBytes: a valid row validates; a semantic row yields EXACTLY ONE
// entry equal to the row's path and message (the backend returns that same
// single error); a schema row yields entries all at or under the row's path.
func TestReviewConventionsCorpus_CLIMatches(t *testing.T) {
	for _, row := range loadRCCorpus(t) {
		t.Run(row.Name, func(t *testing.T) {
			err := ValidateBytes([]byte(row.Doc))
			if *row.Valid {
				if err != nil {
					t.Fatalf("valid row rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s row validated, want a rejection at %s", row.Layer, row.Path)
			}
			entries := rcEntries(t, err)
			switch row.Layer {
			case "semantic":
				if len(entries) != 1 {
					t.Fatalf("got %d entries, want exactly 1:\n%v", len(entries), err)
				}
				if entries[0].Path != row.Path {
					t.Errorf("path = %q, want %q", entries[0].Path, row.Path)
				}
				if entries[0].Message != *row.Message {
					t.Errorf("message mismatch\n got: %s\nwant: %s", entries[0].Message, *row.Message)
				}
			case "schema":
				// EVERY leaf must sit at or under the row's path: the row
				// triggers one rule, so a leaf elsewhere means the document
				// is also wrong somewhere the row does not claim.
				if len(entries) == 0 {
					t.Fatalf("schema rejection carried no entries, want one at or under %q", row.Path)
				}
				for _, e := range entries {
					if e.Path != row.Path && !strings.HasPrefix(e.Path, row.Path+"/") {
						t.Errorf("schema entry %q (%s) is not at or under %q", e.Path, e.Message, row.Path)
					}
				}
			}
		})
	}
}

// TestReviewConventionsCorpus_MirrorMatchesCanonical holds this module's
// testdata/ copy byte-identical to the canonical docs/spec/ corpus.
// scripts/sync-schemas writes the mirror, but a DROPPED cp line would leave a
// stale mirror that the schema-sync drift check cannot see (re-running the
// sync changes nothing), so the parity proof would silently run old rows.
func TestReviewConventionsCorpus_MirrorMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile("../../../docs/spec/review-conventions-fixtures.json")
	if err != nil {
		t.Fatalf("read canonical corpus: %v", err)
	}
	mirror, err := os.ReadFile(rcCorpusPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !bytes.Equal(canonical, mirror) {
		t.Fatalf("%s differs from docs/spec/review-conventions-fixtures.json — run scripts/sync-schemas (and check it still carries the review-conventions cp lines)", rcCorpusPath)
	}
}

// rcConstDecl matches a single-line exported review-convention message or
// path constant declaration.
var rcConstDecl = regexp.MustCompile(`(?m)^const ((?:Msg|MsgFmt|PathFmt)\w*ReviewConvention\w*) = (".*")$`)

// TestReviewConventionsMessageParity holds every review-convention message and
// path constant BYTE-IDENTICAL across the two modules, which cannot share a
// constant (the TestEscalationMessageParity idiom, made exhaustive): the
// declaration set is extracted from BOTH source files, must be the same set,
// each declaration must be the same line, and each CLI constant's VALUE must
// be what that line declares — so a renamed, dropped or edited constant on
// either side fails here rather than surfacing as `fishhawk validate` and the
// backend reporting different text for one spec error.
func TestReviewConventionsMessageParity(t *testing.T) {
	read := func(path string) map[string]string {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out := map[string]string{}
		for _, m := range rcConstDecl.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = m[0]
		}
		return out
	}
	backend := read("../../../backend/internal/spec/review_conventions.go")
	cli := read("review_conventions.go")

	// The CLI's own values, keyed by name: the parity anchor for the
	// regex-extracted lines.
	values := map[string]string{
		"PathFmtReviewConvention":                  PathFmtReviewConvention,
		"PathFmtStageReviewConventions":            PathFmtStageReviewConventions,
		"PathFmtStageReviewConventionItem":         PathFmtStageReviewConventionItem,
		"MsgFmtReviewConventionPathInvalid":        MsgFmtReviewConventionPathInvalid,
		"MsgReviewConventionPathEmpty":             MsgReviewConventionPathEmpty,
		"MsgReviewConventionPathAbsolute":          MsgReviewConventionPathAbsolute,
		"MsgReviewConventionPathBackslash":         MsgReviewConventionPathBackslash,
		"MsgReviewConventionPathInvalidUTF8":       MsgReviewConventionPathInvalidUTF8,
		"MsgFmtReviewConventionPathControlChar":    MsgFmtReviewConventionPathControlChar,
		"MsgReviewConventionPathEmptySegment":      MsgReviewConventionPathEmptySegment,
		"MsgFmtReviewConventionPathDotSegment":     MsgFmtReviewConventionPathDotSegment,
		"MsgReviewConventionChangeKindUnsupported": MsgReviewConventionChangeKindUnsupported,
		"MsgFmtReviewConventionStageType":          MsgFmtReviewConventionStageType,
		"MsgFmtReviewConventionUnknown":            MsgFmtReviewConventionUnknown,
		"MsgFmtReviewConventionNoAgents":           MsgFmtReviewConventionNoAgents,
		"MsgFmtReviewConventionUnreferenced":       MsgFmtReviewConventionUnreferenced,
	}
	if len(backend) != len(values) || len(cli) != len(values) {
		t.Fatalf("extracted %d backend / %d cli declarations, want %d each — a constant was added, dropped, or made multi-line on one side", len(backend), len(cli), len(values))
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := `const ` + name + ` = ` + strconv.Quote(values[name])
		if cli[name] != want {
			t.Errorf("cli review_conventions.go: %s is not declared as the single line\n%s", name, want)
		}
		if backend[name] != want {
			t.Errorf("backend/internal/spec/review_conventions.go does not declare %s verbatim;\nwant the line: %s\n got: %s\n"+
				"The two modules cannot share a constant, so the copies must stay byte-identical or `fishhawk validate` and the backend report different text for the same spec error.",
				name, want, backend[name])
		}
	}
}

// TestReviewConventionPathReason_UnreachableFromYAML pins the reasons the
// corpus cannot reach through ValidateBytes, plus the U+2029 sibling of the
// corpus's U+2028 row and the canonical spellings that must pass.
func TestReviewConventionPathReason_UnreachableFromYAML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", MsgReviewConventionPathEmpty},
		{"docs/\xff\xfe.md", MsgReviewConventionPathInvalidUTF8},
		{"docs/a\u2029b.md", fmt.Sprintf(MsgFmtReviewConventionPathControlChar, '\u2029')},
		{"docs/conventions/backend.md", ""},
		{".fishhawk/review/backend.md", ""},
		{"README.md", ""},
	}
	for _, c := range cases {
		if got := reviewConventionPathReason(c.in); got != c.want {
			t.Errorf("reviewConventionPathReason(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReviewConventions_CollectsOneEntryPerSite pins the collect-mode shape
// the corpus's one-rule rows cannot: each declaration and each stage reports
// at most ONE entry (the backend's first-error-per-site), declarations are
// reported in sorted name order so the list leads with the backend's single
// error, and two violating sites yield two entries.
func TestReviewConventions_CollectsOneEntryPerSite(t *testing.T) {
	doc := `version: "2"
review_conventions:
  zeta:
    path: /z.md
  alpha:
    path: /a.md
    applies_to:
      change_kind: [refactor]
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
          conventions: [alpha, zeta, security, ops]
`
	entries := rcEntries(t, ValidateBytes([]byte(doc)))
	want := []ValidationErrorEntry{
		{Path: "/review_conventions/alpha/path", Message: fmt.Sprintf(MsgFmtReviewConventionPathInvalid, "alpha", "/a.md", MsgReviewConventionPathAbsolute)},
		{Path: "/review_conventions/zeta/path", Message: fmt.Sprintf(MsgFmtReviewConventionPathInvalid, "zeta", "/z.md", MsgReviewConventionPathAbsolute)},
		{Path: "/workflows/feature_change/stages/0/reviewers/conventions/2", Message: fmt.Sprintf(MsgFmtReviewConventionUnknown, "plan", "security", "security")},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d:\n%v", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry[%d] = %+v\nwant %+v", i, entries[i], want[i])
		}
	}
}

// TestReviewConventions_ShapeTolerance pins the skip-on-shape-mismatch
// branches the schema keeps unreachable from ValidateBytes, by driving the
// checks over hand-built raw trees: a non-map review_conventions node, a
// non-map entry, a non-string path and a non-map reviewers node each yield no
// entry rather than a panic or an invented rejection.
func TestReviewConventions_ShapeTolerance(t *testing.T) {
	var errs []ValidationErrorEntry
	checkReviewConventionDeclarations(map[string]any{"review_conventions": []any{"x"}}, &errs)
	checkReviewConventionDeclarations(map[string]any{"review_conventions": map[string]any{
		"a": "not-a-map",
		"b": map[string]any{"path": 7},
	}}, &errs)
	checkStageReviewConventions(map[string]any{"reviewers": "not-a-map"}, "wf", 0, nil, &errs)
	checkReviewConventionsReferenced(map[string]any{"review_conventions": "not-a-map"}, &errs)
	if len(errs) != 0 {
		t.Fatalf("shape-mismatched nodes produced entries: %+v", errs)
	}

	// A declared entry with no readable workflows is unreferenced — the
	// reference rung still runs, as the backend's does.
	checkReviewConventionsReferenced(map[string]any{"review_conventions": map[string]any{
		"a": map[string]any{"path": "docs/a.md"},
	}}, &errs)
	if len(errs) != 1 || errs[0].Path != "/review_conventions/a" {
		t.Fatalf("unreferenced with no workflows: got %+v, want one entry at /review_conventions/a", errs)
	}
}
