package docgen

import (
	"os"
	"path/filepath"
	"testing"
)

// accountedSchemas maps a schema logical name to its repo-relative path.
var accountedSchemas = map[string]string{
	"workflow-v0":      schemaV0Rel,
	"workflow-v1":      schemaV1Rel,
	"workflow-v2":      schemaV2Rel,
	"plan-standard-v1": planSchemaRel,
}

// TestSchemaAccountingIsExhaustive requires that every accountable node
// (top-level property, $defs entry, nested $defs property, by JSON
// pointer) is rendered exactly once OR named in DeliberateExclusions,
// that nothing NON-accountable is rendered, and that every exclusion
// entry still resolves (a stale exclusion is a failure). Counts are
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
		renderedSet := map[string]int{}
		for _, p := range sr.Rendered {
			renderedSet[p]++
		}
		accSet := map[string]bool{}
		for _, p := range acc {
			accSet[p] = true
		}
		excl := DeliberateExclusions[name]

		// Every accountable node rendered exactly once or excluded.
		for _, p := range acc {
			switch {
			case renderedSet[p] == 1:
			case renderedSet[p] > 1:
				t.Errorf("%s: node %s rendered %d times, want exactly once", name, p, renderedSet[p])
			case excl[p] != "":
				// deliberately excluded — fine
			default:
				t.Errorf("%s: accountable node %s is neither rendered nor in DeliberateExclusions", name, p)
			}
		}
		// Nothing non-accountable rendered.
		for _, p := range sr.Rendered {
			if !accSet[p] {
				t.Errorf("%s: rendered non-accountable pointer %s", name, p)
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
		t.Logf("%s: %d accountable nodes, %d rendered, %d excluded", name, len(acc), len(sr.Rendered), len(excl))
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
