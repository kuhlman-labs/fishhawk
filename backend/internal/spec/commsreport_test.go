package spec_test

// --- E81.5 / #3775 (phase 1, #4011): the comms_report produces artifact -------
//
// The user-report-scan sibling of the E79.2 upkeep family in spec_test.go: the
// same stage-type binding, schema-version rule and one-proposal rule, plus a
// from-disk family over the shipped
// docs/spec/examples/workflow-v2-user-report-scan.yaml.

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// userReportScanExampleRelPath is the shipped user-report-scan declaration,
// read from disk so the tests exercise the SHIPPED bytes rather than a copy.
const userReportScanExampleRelPath = "../../../docs/spec/examples/workflow-v2-user-report-scan.yaml"

// TestValidate_CommsReportArtifact_NonPlanStage_Rejected pins the stage-type
// binding: comms_report is valid ONLY on a `plan`-typed (PROPOSE) stage. Four
// negative rows plus a POSITIVE control asserting a plan-typed stage accepts it
// (which also proves the embedded schema enum admits the new value).
func TestValidate_CommsReportArtifact_NonPlanStage_Rejected(t *testing.T) {
	cases := []struct {
		name      string
		stageType string
		executor  string
	}{
		{name: "implement", stageType: "implement", executor: "          agent: claude-code\n"},
		{name: "review", stageType: "review", executor: "          human: true\n"},
		{
			name:      "deploy",
			stageType: "deploy",
			executor: "          delegate:\n" +
				"            target: webhook\n" +
				"            url: https://example.com/deploy\n",
		},
		{name: "acceptance", stageType: "acceptance", executor: "          agent: claude-code\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `version: "2"
workflows:
  wf:
    stages:
      - id: s
        type: ` + tc.stageType + `
        executor:
` + tc.executor + `        produces:
          - artifact: comms_report
            schema: comms_report_v1
`
			_, err := spec.ParseBytes([]byte(doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if want := "/workflows/wf/stages/0/produces/0/artifact"; ve.Path != want {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, want)
			}
			if !strings.Contains(ve.Message, "comms_report artifact is valid only on a plan stage") {
				t.Errorf("ValidationError.Message = %q, want the comms_report stage-type binding", ve.Message)
			}
			if !strings.Contains(ve.Message, `"`+tc.stageType+`"`) {
				t.Errorf("ValidationError.Message = %q, want it to name the offending stage type %q", ve.Message, tc.stageType)
			}
		})
	}

	t.Run("plan stage accepts", func(t *testing.T) {
		if _, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  user_report_scan:
    stages:
      - id: scan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: comms_report
            schema: comms_report_v1
`)); err != nil {
			t.Fatalf("a plan-typed PROPOSE stage must accept comms_report: %v", err)
		}
	})
}

// TestValidate_CommsReportArtifact_MissingSchema_Rejected pins the
// schema-version rule (MVP_SPEC §4.3): a comms_report-producing stage MUST
// declare schema: comms_report_v1. Rows: schema absent, and a wrong token
// (the upkeep sibling's, which must not pass for this artifact).
func TestValidate_CommsReportArtifact_MissingSchema_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantGot string
	}{
		{name: "absent", schema: "", wantGot: `got ""`},
		{name: "wrong token", schema: "            schema: upkeep_report_v1\n", wantGot: `got "upkeep_report_v1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  wf:
    stages:
      - id: scan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: comms_report
` + tc.schema))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if want := "/workflows/wf/stages/0/produces/0/schema"; ve.Path != want {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, want)
			}
			if !strings.Contains(ve.Message, "must declare schema: comms_report_v1") {
				t.Errorf("ValidationError.Message = %q, want the schema-version rule", ve.Message)
			}
			if !strings.Contains(ve.Message, tc.wantGot) {
				t.Errorf("ValidationError.Message = %q, want it to name %s", ve.Message, tc.wantGot)
			}
		})
	}
}

// TestValidate_CommsReportWithOtherProposal_SameStage_Rejected pins the
// one-proposal-per-propose-stage rule against ALL THREE sibling proposals
// (plan, grooming_report, upkeep_report), in BOTH declaration orders. Every
// entry carries its own correct schema, so only a one-proposal rule can refuse
// these documents, and each row asserts the EXACT path and message so the
// block that refused it is identified:
//
//   - comms first: the comms block refuses at /produces/0. On the upkeep row
//     the extended upkeep block would ALSO refuse — at /produces/1 with the
//     upkeep message — so this row's exact path and comms-first message is
//     what makes a deleted comms conflict loop observable there.
//   - plan / grooming_report first: the comms block refuses at /produces/1
//     (neither the plan nor the grooming block checks for comms_report).
//   - upkeep first: the EXTENDED upkeep block refuses at /produces/0, naming
//     comms_report — the row that pins the upkeep-list extension.
func TestValidate_CommsReportWithOtherProposal_SameStage_Rejected(t *testing.T) {
	const (
		comms    = "          - artifact: comms_report\n            schema: comms_report_v1\n"
		plan     = "          - artifact: plan\n            schema: standard_v1\n"
		grooming = "          - artifact: grooming_report\n            schema: grooming_report_v1\n"
		upkeep   = "          - artifact: upkeep_report\n            schema: upkeep_report_v1\n"
		path0    = "/workflows/wf/stages/0/produces/0/artifact"
		path1    = "/workflows/wf/stages/0/produces/1/artifact"
	)
	cases := []struct {
		name     string
		produces string
		wantMsg  string
		wantPath string
	}{
		{name: "comms first, plan", produces: comms + plan, wantMsg: "declares both the comms_report and plan artifacts", wantPath: path0},
		{name: "plan first, comms", produces: plan + comms, wantMsg: "declares both the comms_report and plan artifacts", wantPath: path1},
		{name: "comms first, grooming_report", produces: comms + grooming, wantMsg: "declares both the comms_report and grooming_report artifacts", wantPath: path0},
		{name: "grooming_report first, comms", produces: grooming + comms, wantMsg: "declares both the comms_report and grooming_report artifacts", wantPath: path1},
		{name: "comms first, upkeep_report", produces: comms + upkeep, wantMsg: "declares both the comms_report and upkeep_report artifacts", wantPath: path0},
		{name: "upkeep_report first, comms", produces: upkeep + comms, wantMsg: "declares both the upkeep_report and comms_report artifacts", wantPath: path0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  wf:
    stages:
      - id: scan
        type: plan
        executor:
          agent: claude-code
        produces:
` + tc.produces))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Message, tc.wantMsg) {
				t.Errorf("ValidationError.Message = %q, want it to contain %q", ve.Message, tc.wantMsg)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, tc.wantPath)
			}
		})
	}
}

// TestParseBytes_V0V1_CommsReportArtifact_Rejected pins that the FROZEN majors
// keep rejecting the new artifact: v0 and v1's own produces enums do not admit
// comms_report, so a v0.7 / v1.6 document declaring it fails on its own schema
// (a *SchemaError), never reaching the version-agnostic binding. Each version
// carries a POSITIVE CONTROL: the plan/standard_v1 twin parses cleanly, so the
// refusal is attributable to the produces enum.
func TestParseBytes_V0V1_CommsReportArtifact_Rejected(t *testing.T) {
	doc := func(version, artifact, schema string) []byte {
		return []byte(`version: "` + version + `"
workflows:
  wf:
    stages:
      - id: scan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: ` + artifact + `
            schema: ` + schema + `
`)
	}
	for _, version := range []string{"0.7", "1.6"} {
		t.Run("frozen major "+version, func(t *testing.T) {
			_, err := spec.ParseBytes(doc(version, "comms_report", "comms_report_v1"))
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("version %s: err = %v, want *SchemaError (the frozen major must reject the artifact)", version, err)
			}
		})
		t.Run("frozen major "+version+" positive control", func(t *testing.T) {
			if _, err := spec.ParseBytes(doc(version, "plan", "standard_v1")); err != nil {
				t.Fatalf("version %s: the plan/standard_v1 twin must parse, got %v — the refusal above is not attributable to the produces enum", version, err)
			}
		})
	}
}

// TestStageProducesCommsReport pins the pure per-stage predicate: true only for
// a stage whose produces list declares comms_report, false for a plan stage, a
// grooming stage, an upkeep stage and the zero Stage.
func TestStageProducesCommsReport(t *testing.T) {
	cases := []struct {
		name string
		st   spec.Stage
		want bool
	}{
		{
			name: "comms stage",
			st: spec.Stage{ID: "scan", Type: spec.StageTypePlan, Produces: []spec.Produces{
				{Artifact: spec.ArtifactCommsReport, Schema: spec.CommsReportSchemaVersion},
			}},
			want: true,
		},
		{
			name: "plan artifact stage",
			st: spec.Stage{ID: "plan", Type: spec.StageTypePlan, Produces: []spec.Produces{
				{Artifact: spec.ArtifactPlan, Schema: "standard_v1"},
			}},
			want: false,
		},
		{
			name: "grooming stage",
			st: spec.Stage{ID: "groom", Type: spec.StageTypePlan, Produces: []spec.Produces{
				{Artifact: spec.ArtifactGroomingReport, Schema: spec.GroomingReportSchemaVersion},
			}},
			want: false,
		},
		{
			name: "upkeep stage",
			st: spec.Stage{ID: "scan", Type: spec.StageTypePlan, Produces: []spec.Produces{
				{Artifact: spec.ArtifactUpkeepReport, Schema: spec.UpkeepReportSchemaVersion},
			}},
			want: false,
		},
		{name: "zero Stage", st: spec.Stage{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spec.StageProducesCommsReport(tc.st); got != tc.want {
				t.Errorf("StageProducesCommsReport = %v, want %v", got, tc.want)
			}
		})
	}
}

// parseShippedUserReportScanExample reads and parses the shipped declaration.
func parseShippedUserReportScanExample(t *testing.T) spec.Workflow {
	t.Helper()
	raw, err := os.ReadFile(userReportScanExampleRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", userReportScanExampleRelPath, err)
	}
	s, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", userReportScanExampleRelPath, err)
	}
	wf, ok := s.Workflows["user_report_scan"]
	if !ok {
		t.Fatalf("workflows = %v, want a user_report_scan workflow", s.Workflows)
	}
	return wf
}

// shippedScanStage returns the shipped workflow's single stage, failing the
// test unless the workflow declares exactly one.
func shippedScanStage(t *testing.T, wf spec.Workflow) spec.Stage {
	t.Helper()
	if len(wf.Stages) != 1 {
		t.Fatalf("user_report_scan declares %d stages, want exactly 1 (the scan; no apply stage)", len(wf.Stages))
	}
	return wf.Stages[0]
}

// TestShippedUserReportScanExample_ProposeStageShape reads the shipped bytes
// from disk and asserts the workflow has exactly one stage, that it is the
// plan-typed comms_report_v1 producer, and that no stage produces a
// pull_request (the workflow is a no-diff one).
func TestShippedUserReportScanExample_ProposeStageShape(t *testing.T) {
	wf := parseShippedUserReportScanExample(t)
	scan := shippedScanStage(t, wf)
	if !spec.StageProducesCommsReport(scan) {
		t.Fatalf("stage %q does not produce comms_report", scan.ID)
	}
	if scan.Type != spec.StageTypePlan {
		t.Errorf("comms_report is produced by a %q stage, want a plan (PROPOSE) stage", scan.Type)
	}
	for _, p := range scan.Produces {
		if p.Artifact == spec.ArtifactCommsReport && p.Schema != spec.CommsReportSchemaVersion {
			t.Errorf("comms_report schema = %q, want %q", p.Schema, spec.CommsReportSchemaVersion)
		}
		if p.Artifact == spec.ArtifactPullRequest {
			t.Errorf("stage %q declares the pull_request artifact; the user-report scan must stay a NON-code-change workflow", scan.ID)
		}
	}
}

// TestShippedUserReportScanExample_NoEgress pins ADR-029's rule of two on the
// shipped declaration: the scan stage reads untrusted user-authored text, so
// it declares neither an egress allowance nor a permissions block.
func TestShippedUserReportScanExample_NoEgress(t *testing.T) {
	scan := shippedScanStage(t, parseShippedUserReportScanExample(t))
	if scan.Egress != nil {
		t.Errorf("scan stage Egress = %+v, want nil — untrusted input and network access are never granted together (ADR-029)", scan.Egress)
	}
	if scan.Permissions != nil {
		t.Errorf("scan stage Permissions = %+v, want nil (ADR-029)", scan.Permissions)
	}
}

// TestShippedUserReportScanExample_TriggerRouting pins the non-diff routing
// form the weekly schedule requires: applies_to lists scheduled and on_demand
// and refuses a diff-shaped change, and the declared cadence is present.
func TestShippedUserReportScanExample_TriggerRouting(t *testing.T) {
	wf := parseShippedUserReportScanExample(t)
	if wf.AppliesTo == nil {
		t.Fatal("user_report_scan declares no applies_to; the non-diff routing form is missing")
	}
	for _, tc := range []struct {
		trigger spec.TriggerForm
		want    bool
	}{
		{spec.TriggerScheduled, true},
		{spec.TriggerOnDemand, true},
		{spec.TriggerDiff, false},
	} {
		got, err := wf.AppliesTo.Match(spec.Change{Trigger: tc.trigger})
		if err != nil {
			t.Fatalf("Match(trigger=%s): %v", tc.trigger, err)
		}
		if got != tc.want {
			t.Errorf("Match(trigger=%s) = %v, want %v", tc.trigger, got, tc.want)
		}
	}
	if wf.Schedule == nil || wf.Schedule.Cron == "" {
		t.Errorf("schedule = %+v, want a declared weekly cadence", wf.Schedule)
	}
}

// TestShippedUserReportScanExample_ApprovalGate pins the captain gate: exactly
// one approval gate on the scan stage, requiring one approval and excluding
// both the author and the agent, so no agent approves its own drafts.
func TestShippedUserReportScanExample_ApprovalGate(t *testing.T) {
	scan := shippedScanStage(t, parseShippedUserReportScanExample(t))
	seen := 0
	for gi := range scan.Gates {
		g := scan.Gates[gi]
		if g.Type != spec.GateTypeApproval {
			continue
		}
		seen++
		if g.Approvals == nil {
			t.Errorf("scan gate %d declares no `approvals` block", gi)
			continue
		}
		if g.Approvals.Count == nil || *g.Approvals.Count != 1 {
			t.Errorf("scan gate %d approvals.count = %v, want 1", gi, g.Approvals.Count)
		}
		for _, role := range []string{"author", "agent"} {
			if !containsRole(g.Approvals.Not, role) {
				t.Errorf("scan gate %d approvals.not = %v, want it to exclude %q", gi, g.Approvals.Not, role)
			}
		}
	}
	if seen != 1 {
		t.Errorf("found %d approval gates on the scan stage, want 1", seen)
	}
}

// TestShippedUserReportScanExample_AutonomyLow pins the declared tier: a scan
// over untrusted user text delegates none of the run-driving classes.
func TestShippedUserReportScanExample_AutonomyLow(t *testing.T) {
	wf := parseShippedUserReportScanExample(t)
	if wf.Autonomy != spec.TierLow {
		t.Errorf("user_report_scan autonomy = %q, want %q", wf.Autonomy, spec.TierLow)
	}
}

// TestShippedUserReportScanExample_SpecLayerRequiresNoCharter pins, POSITIVELY,
// the issue's decision that the spec layer does not demand a charter for a
// comms_report workflow: WorkflowRequiresCharter stays grooming-only, and the
// comms charter is enforced when the scan prompt is served (phase 4, #4014).
// A later change to the predicate must therefore be a deliberate decision.
func TestShippedUserReportScanExample_SpecLayerRequiresNoCharter(t *testing.T) {
	wf := parseShippedUserReportScanExample(t)
	if spec.WorkflowRequiresCharter(wf) {
		t.Error("WorkflowRequiresCharter(user_report_scan) = true, want false — the comms charter is enforced at prompt serve (phase 4), not at validate/admission (E81.5)")
	}
}
