package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// accountedSchemas maps a schema logical name to its repo-relative path.
var accountedSchemas = map[string]string{
	"workflow-v0":      schemaV0Rel,
	"workflow-v1":      schemaV1Rel,
	"workflow-v2":      schemaV2Rel,
	"plan-standard-v1": planSchemaRel,
}

// pointersFromMarkdown reconstructs the set of JSON pointers a
// RenderSchemaDoc body ACTUALLY EMITTED, by parsing the output itself:
// each level-5 "name" definition heading yields /$defs/<name>, and each
// properties-table data row yields a /properties/<key> (under the
// top-level fields heading) or /$defs/<name>/properties/<key> (under a
// definition heading) pointer. It is the authority TestSchemaAccountingIsExhaustive
// trusts INSTEAD of SchemaRender.Rendered — so deleting a markdown row or a
// definition heading, while the renderer still self-reports the pointer in
// Rendered, drops the pointer here and reddens the accounting.
func pointersFromMarkdown(md string) map[string]int {
	out := map[string]int{}
	section := "" // "toplevel" | "defs"
	curDef := ""  // current $defs name while section=="defs"
	for _, line := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(line, "#### ") && strings.HasSuffix(line, "— top-level fields"):
			section, curDef = "toplevel", ""
			continue
		case strings.HasPrefix(line, "#### ") && strings.HasSuffix(line, "— definitions"):
			section, curDef = "defs", ""
			continue
		case strings.HasPrefix(line, "##### "):
			name := unbacktick(strings.TrimPrefix(line, "#####"))
			if name != "" {
				curDef = name
				out["/$defs/"+name]++
			}
			continue
		}
		key, ok := firstTableCellKey(line)
		if !ok {
			continue
		}
		switch section {
		case "toplevel":
			out["/properties/"+key]++
		case "defs":
			if curDef != "" {
				out["/$defs/"+curDef+"/properties/"+key]++
			}
		}
	}
	return out
}

// firstTableCellKey returns the code-span key in the first cell of a
// properties-table DATA row, or ok=false for any non-row line (the header,
// the |---| separator, prose). A data row has exactly five cells and a
// first cell wrapped in a code span.
func firstTableCellKey(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return "", false
	}
	cells := splitTableRow(line)
	if len(cells) != 5 {
		return "", false
	}
	first := strings.TrimSpace(cells[0])
	if first == "Field" || strings.HasPrefix(first, "---") {
		return "", false
	}
	key := unbacktick(first)
	if key == "" || key == first { // first cell was not a code span
		return "", false
	}
	return key, true
}

// splitTableRow splits a markdown table row on unescaped `|`, honouring the
// `\|` cell escape proseCell emits, and drops the empty cells the leading
// and trailing border pipes produce.
func splitTableRow(line string) []string {
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	cells = append(cells, cur.String())
	if len(cells) >= 2 {
		cells = cells[1 : len(cells)-1]
	}
	return cells
}

// unbacktick strips a surrounding single-backtick code span (the shape
// codeSpan emits for a backtick-free, space-free key) and trims space; it
// returns the input trimmed when there is no code span to strip.
func unbacktick(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	return strings.TrimSpace(s)
}

// TestSchemaAccountingIsExhaustive requires that every accountable node
// (top-level property, $defs entry, nested $defs property, by JSON
// pointer) appears in the rendered Markdown exactly once OR is named in
// DeliberateExclusions, that nothing NON-accountable appears, and that
// every exclusion entry still resolves (a stale exclusion is a failure).
//
// The evidence is derived FROM THE RENDERED MARKDOWN (pointersFromMarkdown)
// — the definition headings and property rows the reader will actually see
// — NOT from SchemaRender.Rendered, which the renderer populates itself
// alongside but independently of the Markdown writes. Deleting a Markdown
// row or a definition heading while the renderer keeps claiming the pointer
// in Rendered therefore reddens this, restoring the load-bearing
// counterfactual (drop rendered schema coverage -> fail). Counts are
// logged so coverage magnitude is visible.
func TestSchemaAccountingIsExhaustive(t *testing.T) {
	root := testRepoRoot(t)
	for name, rel := range accountedSchemas {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		acc, err := AccountableNodes(data)
		if err != nil {
			t.Fatalf("%s: accountable: %v", name, err)
		}
		sr, err := RenderSchemaDoc(name, data)
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		renderedSet := pointersFromMarkdown(sr.Markdown)
		accSet := map[string]bool{}
		for _, p := range acc {
			accSet[p] = true
		}
		excl := DeliberateExclusions[name]

		// Every accountable node appears in the Markdown exactly once or
		// is excluded.
		for _, p := range acc {
			switch {
			case renderedSet[p] == 1:
			case renderedSet[p] > 1:
				t.Errorf("%s: node %s appears in the rendered Markdown %d times, want exactly once", name, p, renderedSet[p])
			case excl[p] != "":
				// deliberately excluded — fine
			default:
				t.Errorf("%s: accountable node %s appears in neither the rendered Markdown nor DeliberateExclusions", name, p)
			}
		}
		// Nothing non-accountable appears in the Markdown.
		for p := range renderedSet {
			if !accSet[p] {
				t.Errorf("%s: rendered Markdown carries non-accountable pointer %s", name, p)
			}
		}
		// Every exclusion entry still resolves.
		for ptr, reason := range excl {
			if reason == "" {
				t.Errorf("%s: exclusion %s has an empty reason", name, ptr)
			}
			ok, err := PointerResolves(data, ptr)
			if err != nil {
				t.Fatalf("%s: resolve %s: %v", name, ptr, err)
			}
			if !ok {
				t.Errorf("%s: stale DeliberateExclusions entry %s does not resolve in the schema", name, ptr)
			}
		}
		t.Logf("%s: %d accountable nodes, %d rendered (from Markdown), %d excluded", name, len(acc), len(renderedSet), len(excl))
	}
}

// TestFieldOrderIsRequiredFirstThenDocumentOrder pins the ordering
// against a fixture whose OPTIONAL field is declared BEFORE its two
// required ones, so required-first and document order disagree: deleting
// the required-first sort reddens this.
func TestFieldOrderIsRequiredFirstThenDocumentOrder(t *testing.T) {
	fixture := []byte(`{
      "type": "object",
      "required": ["beta", "gamma"],
      "properties": {
        "alpha_optional": {"type": "string"},
        "beta": {"type": "string"},
        "gamma": {"type": "string"}
      }
    }`)
	root, err := rootMapping(fixture)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	props := mapVal(root, "properties")
	got := orderedProps(props, requiredSet(root))
	want := []string{"beta", "gamma", "alpha_optional"}
	if len(got) != len(want) {
		t.Fatalf("orderedProps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedProps = %v, want %v (required-first then document order)", got, want)
		}
	}

	// And it surfaces in the rendered table: the required rows precede
	// the optional one.
	sr, err := RenderSchemaDoc("fixture", fixture)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	iBeta := indexOfCell(sr.Markdown, "beta")
	iAlpha := indexOfCell(sr.Markdown, "alpha_optional")
	if iBeta < 0 || iAlpha < 0 || iBeta > iAlpha {
		t.Errorf("rendered order wrong: beta at %d, alpha_optional at %d (want beta first)", iBeta, iAlpha)
	}
}

func indexOfCell(md, field string) int {
	return indexOf(md, "`"+field+"`")
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
