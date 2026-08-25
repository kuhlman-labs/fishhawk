package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fencedBlocks returns the concatenated contents of all ``` fenced code
// blocks in md — used so the §4 example-key assertion reads the shipped
// example YAML, not the primitive summary table (which also names the
// keys). Removing an example block therefore reddens the check.
func fencedBlocks(md string) string {
	var out strings.Builder
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String()
}

// sectionBetween returns the slice of md from the first occurrence of
// start up to (not including) the next occurrence of end after it; if end
// is absent it returns from start to the end of md, and if start is absent
// it returns "". It is used to scope an assertion to one level-3 heading
// section of a multi-major page.
func sectionBetween(md, start, end string) string {
	i := strings.Index(md, start)
	if i < 0 {
		return ""
	}
	rest := md[i:]
	if j := strings.Index(rest[len(start):], end); j >= 0 {
		return rest[:len(start)+j]
	}
	return rest
}

// TestSection4PrimitivesAreDemonstrated asserts each of the six MVP_SPEC
// §4.1 primitives is demonstrated in the generated workflow-spec page at
// BOTH its schema anchor (a `#####` definition heading) AND its example
// key (inside a shipped example's fenced YAML). The anchor assertion is
// SCOPED to the LIVE workflow-v2 field reference — the page renders v0, v1
// AND v2, and every anchor (e.g. `approvals`) also exists in v1/v0, so an
// unscoped search would stay green if a primitive were dropped from the v2
// rendering alone. Dropping a primitive's node from the v2 schema, or its
// example key from the shipped examples, reddens the relevant primitive.
func TestSection4PrimitivesAreDemonstrated(t *testing.T) {
	root := testRepoRoot(t)
	gen, err := GeneratePageContent(root, "workflow-spec")
	if err != nil {
		t.Fatalf("generate workflow-spec: %v", err)
	}
	// Scope to the workflow-v2 field-reference section: from its heading
	// up to the workflow-v1 heading that follows it.
	v2Section := sectionBetween(gen, "### `workflow-v2`", "### `workflow-v1`")
	if v2Section == "" {
		t.Fatalf("workflow-v2 field-reference section not found in generated page")
	}
	examples := fencedBlocks(gen)
	for _, p := range Section4Primitives {
		t.Run(p.Name, func(t *testing.T) {
			anchor := "##### `" + p.SchemaAnchor + "`"
			if !strings.Contains(v2Section, anchor) {
				t.Errorf("primitive %q: schema anchor %q not rendered in the workflow-v2 field reference", p.Name, anchor)
			}
			if !strings.Contains(examples, p.ExampleKey) {
				t.Errorf("primitive %q: example key %q not demonstrated in any shipped example", p.Name, p.ExampleKey)
			}
		})
	}
}

// TestDeltaTablesCompareShape asserts the major-to-major delta renders a
// shape table with at least one difference between v1 and v2.
func TestDeltaTablesCompareShape(t *testing.T) {
	root := testRepoRoot(t)
	v1, err := os.ReadFile(filepath.Join(root, schemaV1Rel))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := os.ReadFile(filepath.Join(root, schemaV2Rel))
	if err != nil {
		t.Fatal(err)
	}
	delta, err := renderDelta(v1, v2)
	if err != nil {
		t.Fatalf("renderDelta: %v", err)
	}
	if !strings.Contains(delta, "| Field | Change | Detail |") {
		t.Errorf("delta missing table header:\n%s", delta)
	}
	if !strings.Contains(delta, "| added |") && !strings.Contains(delta, "| removed |") &&
		!strings.Contains(delta, "| changed |") {
		t.Errorf("delta between v1 and v2 shows no differences:\n%s", delta)
	}
}
