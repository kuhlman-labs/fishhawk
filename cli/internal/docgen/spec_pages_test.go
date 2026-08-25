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

// TestSection4PrimitivesAreDemonstrated asserts each of the six MVP_SPEC
// §4.1 primitives is demonstrated in the generated workflow-spec page at
// BOTH its schema anchor (a `#####` definition heading) AND its example
// key (inside a shipped example's fenced YAML). Dropping either coverage
// reddens the relevant primitive.
func TestSection4PrimitivesAreDemonstrated(t *testing.T) {
	root := testRepoRoot(t)
	gen, err := GeneratePageContent(root, "workflow-spec")
	if err != nil {
		t.Fatalf("generate workflow-spec: %v", err)
	}
	examples := fencedBlocks(gen)
	for _, p := range Section4Primitives {
		t.Run(p.Name, func(t *testing.T) {
			anchor := "##### `" + p.SchemaAnchor + "`"
			if !strings.Contains(gen, anchor) {
				t.Errorf("primitive %q: schema anchor %q not rendered", p.Name, anchor)
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
