package planfixture_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/plan"
	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
)

// TestValid_SchemaCompliant is the per-module CI gate: it fails whenever
// Valid() is missing a required field added to the standard_v1 schema.
func TestValid_SchemaCompliant(t *testing.T) {
	b, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := plan.Validate(b); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDecomposed_SchemaCompliant(t *testing.T) {
	b, err := json.Marshal(planfixture.Decomposed())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := plan.Validate(b); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValid_ParityWithExample verifies that every top-level key in
// Valid() is also present in testdata/valid/example.json. When a new
// required field is added to Valid(), this test fails if example.json
// is not updated to match.
func TestValid_ParityWithExample(t *testing.T) {
	b, err := json.Marshal(planfixture.Valid())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	eb, err := os.ReadFile("../testdata/valid/example.json")
	if err != nil {
		t.Fatalf("read example.json: %v", err)
	}
	var example map[string]any
	if err := json.Unmarshal(eb, &example); err != nil {
		t.Fatalf("unmarshal example.json: %v", err)
	}

	for key := range fixture {
		if _, ok := example[key]; !ok {
			t.Errorf("example.json missing key %q — update example.json when planfixture.Valid() adds a required field", key)
		}
	}
}
