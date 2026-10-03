package plan_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- upkeep_report schema + glue (#3921, slice 2) ---
//
// Every row starts from the SHIPPED example and mutates it as a generic JSON
// document, so the schema layer and the ValidateUpkeepReport glue are both on
// the path. Schema rows assert *SchemaError (the semantic layer deliberately
// does not re-check evidence count, so nothing masks a schema regression);
// semantic rows assert *SemanticError, proving the glue calls
// CheckUpkeepReportSemantics after the schema accepts.

func upkeepExampleBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-example.json")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return b
}

// upkeepExampleMutated decodes the example, applies mutate and re-encodes.
func upkeepExampleMutated(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(upkeepExampleBytes(t), &m); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func upkeepFindingAt(m map[string]any, i int) map[string]any {
	return m["findings"].([]any)[i].(map[string]any)
}

func upkeepEvidenceAt(m map[string]any, i, j int) map[string]any {
	return upkeepFindingAt(m, i)["evidence"].([]any)[j].(map[string]any)
}

func TestValidateUpkeepReport_ShippedExample(t *testing.T) {
	body := upkeepExampleBytes(t)
	if err := plan.ValidateUpkeepReport(body); err != nil {
		t.Fatalf("ValidateUpkeepReport(example) = %v, want nil", err)
	}
	if err := plan.ValidateArtifact(body); err != nil {
		t.Fatalf("ValidateArtifact(example) = %v, want nil (routed to the upkeep validator)", err)
	}
	r, err := plan.ParseUpkeepReport(body)
	if err != nil {
		t.Fatalf("ParseUpkeepReport(example) = %v", err)
	}
	if len(r.Findings) != 3 || len(r.RunRefIDs()) != 2 {
		t.Errorf("parsed findings=%d run refs=%d, want 3 and 2", len(r.Findings), len(r.RunRefIDs()))
	}
	if plan.EmbeddedUpkeepReportSchemaHash() == "" || len(plan.EmbeddedUpkeepReportSchemaHash()) != 64 {
		t.Errorf("EmbeddedUpkeepReportSchemaHash = %q, want a hex sha256", plan.EmbeddedUpkeepReportSchemaHash())
	}
}

// TestValidateArtifact_UpkeepRoutedToUpkeepValidator: an upkeep body that is
// INVALID only under the upkeep schema must be refused by ValidateArtifact
// with the upkeep-schema pointer — never validated as a plan.
func TestValidateArtifact_UpkeepRoutedToUpkeepValidator(t *testing.T) {
	body := upkeepExampleMutated(t, func(m map[string]any) {
		upkeepFindingAt(m, 0)["evidence"] = []any{}
	})
	var se *plan.SchemaError
	err := plan.ValidateArtifact(body)
	if !errors.As(err, &se) || !upkeepSchemaErrorAt(se, "/findings/0/evidence") {
		t.Fatalf("ValidateArtifact = %v, want a *SchemaError at /findings/0/evidence", err)
	}
}

func upkeepSchemaErrorAt(se *plan.SchemaError, pointer string) bool {
	if strings.HasPrefix(se.Path, pointer) {
		return true
	}
	for _, v := range se.Violations {
		if strings.HasPrefix(v.Path, pointer) {
			return true
		}
	}
	return false
}

func TestValidateUpkeepReport_SchemaRows(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		pointer string
	}{
		{"evidence-less finding", func(m map[string]any) { upkeepFindingAt(m, 0)["evidence"] = []any{} }, "/findings/0/evidence"},
		{"unknown source", func(m map[string]any) { upkeepFindingAt(m, 0)["source"] = "lint" }, "/findings/0/source"},
		{"additional property", func(m map[string]any) { m["extra"] = true }, ""},
		{"bad evidence kind", func(m map[string]any) { upkeepEvidenceAt(m, 0, 0)["kind"] = "url" }, "/findings/0/evidence/0"},
		{"empty sources_scanned", func(m map[string]any) { m["sources_scanned"] = []any{} }, "/sources_scanned"},
		{"subject with whitespace", func(m map[string]any) {
			upkeepFindingAt(m, 0)["subject"] = "Test Widget"
			upkeepFindingAt(m, 0)["id"] = "flake:Test Widget"
		}, "/findings/0"},
		{"wrong report_version", func(m map[string]any) { m["report_version"] = "upkeep_report_v2" }, "/report_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := plan.ValidateUpkeepReport(upkeepExampleMutated(t, tc.mutate))
			var se *plan.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SchemaError", err)
			}
			if tc.pointer != "" && !upkeepSchemaErrorAt(se, tc.pointer) {
				t.Errorf("schema error %v does not locate %s", se, tc.pointer)
			}
		})
	}
}

func TestValidateUpkeepReport_ParseErrors(t *testing.T) {
	for name, body := range map[string][]byte{"empty": nil, "whitespace": []byte("  \n"), "not json": []byte("{ nope")} {
		t.Run(name, func(t *testing.T) {
			var pe *plan.ParseError
			if err := plan.ValidateUpkeepReport(body); !errors.As(err, &pe) {
				t.Errorf("err = %v, want *ParseError", err)
			}
		})
	}
}

// TestValidateUpkeepReport_SemanticRowsThroughGlue: one row per rule a-k,
// each schema-valid, so only CheckUpkeepReportSemantics can refuse it.
func TestValidateUpkeepReport_SemanticRowsThroughGlue(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		pointer string
	}{
		{"a id not derived", func(m map[string]any) { upkeepFindingAt(m, 0)["id"] = "flake:TestOther" }, "/findings/0/id"},
		{"b duplicate id", func(m map[string]any) {
			fs := m["findings"].([]any)
			m["findings"] = append(fs, fs[0])
		}, "/findings/3/id"},
		{"c source not scanned", func(m map[string]any) { m["sources_scanned"] = []any{"flake", "toolchain_drift"} }, "/findings/2/source"},
		{"d control rune in subject", func(m map[string]any) {
			upkeepFindingAt(m, 0)["subject"] = "Test\u0007"
			upkeepFindingAt(m, 0)["id"] = "flake:Test\u0007"
		}, "/findings/0/subject"},
		{"e malformed run_id", func(m map[string]any) { upkeepEvidenceAt(m, 0, 0)["run_id"] = "not-a-uuid" }, "/findings/0/evidence/0/run_id"},
		{"e nil run_id", func(m map[string]any) { upkeepEvidenceAt(m, 0, 0)["run_id"] = "00000000-0000-0000-0000-000000000000" }, "/findings/0/evidence/0/run_id"},
		{"e malformed stage_id", func(m map[string]any) { upkeepEvidenceAt(m, 0, 0)["stage_id"] = "stage-1" }, "/findings/0/evidence/0/stage_id"},
		{"f flake without a run ref", func(m map[string]any) {
			upkeepFindingAt(m, 0)["evidence"] = []any{map[string]any{"kind": "file", "path": "a.go"}}
		}, "/findings/0/evidence"},
		{"g drift on one path", func(m map[string]any) { upkeepEvidenceAt(m, 1, 1)["path"] = "go.work" }, "/findings/1/evidence"},
		{"h deprecation without a file ref", func(m map[string]any) {
			upkeepFindingAt(m, 2)["evidence"] = []any{map[string]any{"kind": "run", "run_id": "6f1c2a3e-8b4d-4e5f-9a6b-7c8d9e0f1a2b"}}
		}, "/findings/2/evidence"},
		{"i label with whitespace", func(m map[string]any) {
			upkeepFindingAt(m, 0)["proposed_issue"].(map[string]any)["labels"] = []any{"area: x"}
		}, "/findings/0/proposed_issue/labels/0"},
		{"j parent_epic zero", func(m map[string]any) {
			upkeepFindingAt(m, 1)["proposed_issue"].(map[string]any)["parent_epic"] = "#0"
		}, "/findings/1/proposed_issue/parent_epic"},
		{"k too many distinct run ids", func(m map[string]any) {
			// 201 distinct run ids over five flakes (schema: <= 50 evidence each).
			var fs []any
			n := 0
			for f := 0; f < 5; f++ {
				var ev []any
				for j := 0; j < 41 && n < plan.UpkeepMaxDistinctRunRefs+1; j++ {
					ev = append(ev, map[string]any{"kind": "run", "run_id": fmt.Sprintf("00000000-0000-4000-8000-%012d", n+1)})
					n++
				}
				fs = append(fs, map[string]any{
					"id": fmt.Sprintf("flake:T%d", f), "source": "flake", "subject": fmt.Sprintf("T%d", f),
					"evidence":       ev,
					"proposed_issue": map[string]any{"title": "t", "body": "b", "type": "bug", "labels": []any{}},
				})
			}
			m["findings"] = fs
		}, "/findings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := plan.ValidateUpkeepReport(upkeepExampleMutated(t, tc.mutate))
			var sem *plan.SemanticError
			if !errors.As(err, &sem) {
				t.Fatalf("err = %v, want *SemanticError", err)
			}
			if !strings.Contains(sem.Message, tc.pointer+":") {
				t.Errorf("semantic error %q does not name %s", sem.Message, tc.pointer)
			}
		})
	}
}
