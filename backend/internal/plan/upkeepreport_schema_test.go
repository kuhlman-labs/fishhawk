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

// --- advisory source schema rows (#3750) ---

func upkeepAdvisoryExampleMutated(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-advisory-example.json")
	if err != nil {
		t.Fatalf("read advisory example: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode advisory example: %v", err)
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func upkeepAdvisoryAt(m map[string]any, i int) map[string]any {
	return upkeepFindingAt(m, i)["advisory"].(map[string]any)
}

func upkeepFrameAt(m map[string]any, i, k int) map[string]any {
	return upkeepAdvisoryAt(m, i)["call_path"].([]any)[k].(map[string]any)
}

// upkeepCallPathOf sets finding i's call path to n frames: the shipped
// frame 0 (the vulnerable end) followed by n-1 copies of frame 1.
func upkeepCallPathOf(m map[string]any, i, n int) {
	cp := upkeepAdvisoryAt(m, i)["call_path"].([]any)
	out := []any{cp[0]}
	for len(out) < n {
		out = append(out, cp[1])
	}
	upkeepAdvisoryAt(m, i)["call_path"] = out
}

func TestValidateUpkeepReport_AdvisoryExample(t *testing.T) {
	body := upkeepAdvisoryExampleMutated(t, func(map[string]any) {})
	if err := plan.ValidateUpkeepReport(body); err != nil {
		t.Fatalf("ValidateUpkeepReport(advisory example) = %v, want nil", err)
	}
	if err := plan.ValidateArtifact(body); err != nil {
		t.Fatalf("ValidateArtifact(advisory example) = %v, want nil (routed to the upkeep validator)", err)
	}
	r, err := plan.ParseUpkeepReport(body)
	if err != nil {
		t.Fatalf("ParseUpkeepReport(advisory example) = %v", err)
	}
	if len(r.Findings) != 3 || len(r.SourceDegrades) != 1 || r.Findings[0].Advisory == nil {
		t.Errorf("parsed findings=%d source_degrades=%d, want 3 advisory findings and 1 degrade", len(r.Findings), len(r.SourceDegrades))
	}
}

// TestValidateUpkeepReport_AdvisorySchemaRows: each row is refused by the
// SCHEMA (*SchemaError), not the semantic layer.
func TestValidateUpkeepReport_AdvisorySchemaRows(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		pointer string
	}{
		// absent / null / empty are three states: null is "no fix" (valid),
		// absent and empty are refused.
		{"fixed_version absent", func(m map[string]any) { delete(upkeepAdvisoryAt(m, 2), "fixed_version") }, "/findings/2/advisory"},
		{"fixed_version empty", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["fixed_version"] = "" }, "/findings/0/advisory/fixed_version"},
		{"unknown frame key", func(m map[string]any) { upkeepFrameAt(m, 0, 0)["trace_id"] = "x" }, "/findings/0/advisory/call_path/0"},
		{"unknown position key", func(m map[string]any) {
			upkeepFrameAt(m, 0, 0)["position"].(map[string]any)["file"] = "x.go"
		}, "/findings/0/advisory/call_path/0/position"},
		{"frame without module", func(m map[string]any) { delete(upkeepFrameAt(m, 0, 1), "module") }, "/findings/0/advisory/call_path/1"},
		{"reachability outside the enum", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["reachability"] = "reachable" }, "/findings/0/advisory/reachability"},
		{"severity outside the enum", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["severity"] = "critical" }, "/findings/0/advisory/severity"},
		{"scanner outside the enum", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["scanner"] = "trivy" }, "/findings/0/advisory/scanner"},
		{"ecosystem outside the enum", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["ecosystem"] = "pypi" }, "/findings/0/advisory/ecosystem"},
		{"unknown advisory key", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["cvss"] = 9.8 }, "/findings/0/advisory"},
		{"empty advisory_ids", func(m map[string]any) { upkeepAdvisoryAt(m, 0)["advisory_ids"] = []any{} }, "/findings/0/advisory/advisory_ids"},
		{"advisory id with whitespace", func(m map[string]any) {
			upkeepAdvisoryAt(m, 0)["advisory_ids"] = []any{"GO-2024-2687", "CVE 2023"}
		}, "/findings/0/advisory/advisory_ids/1"},
		{"33-frame call path", func(m map[string]any) { upkeepCallPathOf(m, 0, plan.UpkeepMaxCallPathFrames+1) }, "/findings/0/advisory/call_path"},
		{"unknown degrade reason", func(m map[string]any) {
			m["source_degrades"] = []any{map[string]any{"source": "deprecation", "reason": "offline"}}
		}, "/source_degrades/0/reason"},
		{"degrade of an unknown source", func(m map[string]any) {
			m["source_degrades"] = []any{map[string]any{"source": "lint", "reason": "tool_failed"}}
		}, "/source_degrades/0/source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := plan.ValidateUpkeepReport(upkeepAdvisoryExampleMutated(t, tc.mutate))
			var se *plan.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SchemaError", err)
			}
			if !upkeepSchemaErrorAt(se, tc.pointer) {
				t.Errorf("schema error %v does not locate %s", se, tc.pointer)
			}
		})
	}
}

// TestValidateUpkeepReport_AdvisoryAccepted: shapes the schema and the rules
// must ACCEPT, including the 32-frame call-path boundary (a longer trace keeps
// index 0 and truncates the far end) and an explicit null fixed_version.
func TestValidateUpkeepReport_AdvisoryAccepted(t *testing.T) {
	for name, mutate := range map[string]func(m map[string]any){
		"32-frame call path":                func(m map[string]any) { upkeepCallPathOf(m, 0, plan.UpkeepMaxCallPathFrames) },
		"fixed_version null on govulncheck": func(m map[string]any) { upkeepAdvisoryAt(m, 0)["fixed_version"] = nil },
		"pnpm call_path empty array":        func(m map[string]any) { upkeepAdvisoryAt(m, 2)["call_path"] = []any{} },
		"no source_degrades key":            func(m map[string]any) { delete(m, "source_degrades") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := plan.ValidateUpkeepReport(upkeepAdvisoryExampleMutated(t, mutate)); err != nil {
				t.Fatalf("ValidateUpkeepReport = %v, want nil", err)
			}
		})
	}
}

// TestValidateUpkeepReport_AdvisorySemanticRowsThroughGlue: one schema-valid
// row per rule l-t, so only CheckUpkeepReportSemantics can refuse it.
func TestValidateUpkeepReport_AdvisorySemanticRowsThroughGlue(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		pointer string
	}{
		{"l advisory object missing", func(m map[string]any) { delete(upkeepFindingAt(m, 0), "advisory") }, "/findings/0/advisory"},
		{"m subject not derived", func(m map[string]any) {
			upkeepFindingAt(m, 0)["subject"] = "GO-2024-2687:golang.org/x/text"
			upkeepFindingAt(m, 0)["id"] = "advisory:GO-2024-2687:golang.org/x/text"
		}, "/findings/0/subject"},
		{"n scanner/ecosystem", func(m map[string]any) { upkeepAdvisoryAt(m, 2)["ecosystem"] = "go" }, "/findings/2/advisory/scanner"},
		{"o reachability not derived", func(m map[string]any) { upkeepAdvisoryAt(m, 1)["reachability"] = "called" }, "/findings/1/advisory/reachability"},
		{"p severity over the cap", func(m map[string]any) { upkeepAdvisoryAt(m, 1)["severity"] = "high" }, "/findings/1/advisory/severity"},
		{"q no manifest", func(m map[string]any) {
			upkeepFindingAt(m, 1)["evidence"] = []any{map[string]any{"kind": "file", "path": "runner/main.go"}}
		}, "/findings/1/evidence"},
		{"r degraded source listed as scanned", func(m map[string]any) {
			m["sources_scanned"] = []any{"advisory", "deprecation"}
		}, "/source_degrades/0"},
		{"s fixed_version is a range", func(m map[string]any) { upkeepAdvisoryAt(m, 1)["fixed_version"] = ">=0.3.8 <0.4.0" }, "/findings/1/advisory/fixed_version"},
		{"t advisory source unaccounted", func(m map[string]any) {
			m["sources_scanned"] = []any{"flake"}
			m["findings"] = []any{}
		}, "/sources_scanned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := plan.ValidateUpkeepReport(upkeepAdvisoryExampleMutated(t, tc.mutate))
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

// TestUpkeepSources_MatchSchemaEnum pins plan.UpkeepSources() (the Go-side
// owner of the closed source set) to the EMBEDDED schema's $defs.source.enum
// and to the source alternation in the finding id pattern. Adding a source in
// one place and not the others fails here, naming the other sites to update:
// docs/spec/upkeep-report-v1.schema.json (+ scripts/sync-schemas), the
// upkeep-dispositions `source` enum in docs/api/v0.openapi.yaml, and
// docs/spec/upkeep-report-v1.md.
func TestUpkeepSources_MatchSchemaEnum(t *testing.T) {
	raw, err := os.ReadFile("schemas/upkeep-report-v1.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	var s struct {
		Defs struct {
			Source struct {
				Enum []string `json:"enum"`
			} `json:"source"`
			Finding struct {
				Properties struct {
					ID struct {
						Pattern string `json:"pattern"`
					} `json:"id"`
				} `json:"properties"`
			} `json:"finding"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode embedded schema: %v", err)
	}
	got := plan.UpkeepSources()
	if strings.Join(got, ",") != strings.Join(s.Defs.Source.Enum, ",") {
		t.Errorf("plan.UpkeepSources() = %v, schema $defs.source.enum = %v; update both together (and the OpenAPI upkeep-dispositions source enum)", got, s.Defs.Source.Enum)
	}
	if want := "^(" + strings.Join(got, "|") + "):"; !strings.HasPrefix(s.Defs.Finding.Properties.ID.Pattern, want) {
		t.Errorf("finding id pattern %q does not start with %q; the id pattern must name every source", s.Defs.Finding.Properties.ID.Pattern, want)
	}
	found := false
	for _, src := range got {
		found = found || src == plan.UpkeepSourceAdvisory
	}
	if !found {
		t.Errorf("plan.UpkeepSources() = %v, want it to include %q", got, plan.UpkeepSourceAdvisory)
	}
	got[0] = "mutated"
	if plan.UpkeepSources()[0] != plan.UpkeepSourceFlake {
		t.Error("plan.UpkeepSources() shares its backing array; a caller mutated the closed set")
	}
}
