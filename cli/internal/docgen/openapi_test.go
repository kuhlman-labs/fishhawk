package docgen

import (
	"strings"
	"testing"
)

// TestAPICoversEveryOperation requires each (method, path) operation in
// the OpenAPI document — the coverage AUTHORITY, taken from LoadOperations
// independently of the renderer — to appear exactly once in the rendered
// table. Dropping an operation from the rendering reddens this even though
// the authority still lists it. The count is logged.
func TestAPICoversEveryOperation(t *testing.T) {
	root := testRepoRoot(t)
	authority, err := LoadOperations(root)
	if err != nil {
		t.Fatalf("load operations: %v", err)
	}
	if len(authority) == 0 {
		t.Fatal("no operations loaded from the OpenAPI document")
	}
	table, rendered, err := RenderAPI(root)
	if err != nil {
		t.Fatalf("render API: %v", err)
	}
	// The renderer's own op list must equal the authority.
	if len(rendered) != len(authority) {
		t.Errorf("RenderAPI returned %d ops, authority has %d", len(rendered), len(authority))
	}
	for _, op := range authority {
		token := op.RowToken()
		if n := strings.Count(table, token); n != 1 {
			t.Errorf("operation %s %s rendered %d times, want exactly once (token %q)",
				op.Method, op.Path, n, token)
		}
	}
	t.Logf("API: %d operations covered", len(authority))
}
