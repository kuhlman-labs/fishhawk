package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- fishhawk_validate (E45.65 / #3579) ---
//
// Every test here drives the handler in-process: the verb makes no HTTP call,
// so there is no fake backend and no transport — the seam under test is the
// input ladder, the spec.ParseBytes error classification, and the
// pointer-derived workflow/stage fields.

// validV2Spec is a minimal, schema-valid workflow-v2 document the negative
// cases below are derived from by ONE edit each, so a red lands on the edited
// rule and not on an unrelated shape defect.
const validV2Spec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
`

// groomingV2Spec is a minimal grooming_report-producing workflow modelled on
// docs/spec/examples/workflow-v2-backlog-grooming.yaml, kept beside a plain
// feature_change so the charter_required_by assertion has a negative control
// in the same document.
const groomingV2Spec = validV2Spec + `  backlog_grooming:
    applies_to:
      trigger: [scheduled, on_demand]
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
      - id: apply
        type: implement
        executor:
          agent: claude-code
        needs: [groom]
      - id: confirm
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
              not: [agent]
`

func callValidate(t *testing.T, in ValidateSpecInput) (ValidateSpecOutput, error) {
	t.Helper()
	r := &runResolver{getenv: envFuncFromMap(nil)}
	_, out, err := r.validateSpec(context.Background(), nil, in)
	return out, err
}

func mustValidate(t *testing.T, in ValidateSpecInput) ValidateSpecOutput {
	t.Helper()
	out, err := callValidate(t, in)
	if err != nil {
		t.Fatalf("validateSpec returned a tool error: %v", err)
	}
	return out
}

func TestValidateSpec_Inline_ValidPreset(t *testing.T) {
	data, err := spec.PresetBytes(spec.PresetMedium)
	if err != nil {
		t.Fatal(err)
	}
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: string(data)})
	if !out.Valid {
		t.Fatalf("Valid = false; diagnostics = %+v", out.Diagnostics)
	}
	if out.Source != "inline" {
		t.Errorf("Source = %q, want inline", out.Source)
	}
	if out.Path != "" {
		t.Errorf("Path = %q, want empty for inline input", out.Path)
	}
	if !strings.HasPrefix(out.Version, "2") {
		t.Errorf("Version = %q, want a 2.x version", out.Version)
	}
	if len(out.Diagnostics) != 0 {
		t.Errorf("Diagnostics = %+v, want empty", out.Diagnostics)
	}
	if out.Hint != "" {
		t.Errorf("Hint = %q, want empty on a valid spec", out.Hint)
	}
	if len(out.CharterRequiredBy) != 0 {
		t.Errorf("CharterRequiredBy = %v, want empty for the feature_change preset", out.CharterRequiredBy)
	}
	if strings.TrimSpace(out.Checked) == "" {
		t.Error("Checked is empty")
	}
	if len(out.NotChecked) != 3 {
		t.Errorf("NotChecked = %v, want three entries (charter rule, model ids, deployment wiring)", out.NotChecked)
	}
	for _, want := range []string{"charter", "model", "fishhawk_doctor"} {
		if !strings.Contains(strings.Join(out.NotChecked, "\n"), want) {
			t.Errorf("NotChecked missing %q: %v", want, out.NotChecked)
		}
	}
}

// TestValidateSpec_Inline_SchemaError: a schema violation is a valid:false
// RESULT (nil tool error) whose diagnostic is classified schema and carries the
// validator's pointer. RED when the *spec.SchemaError classification arm is
// deleted (the kind falls through to other).
func TestValidateSpec_Inline_SchemaError(t *testing.T) {
	bad := strings.Replace(validV2Spec, "type: implement", "type: not_a_stage_type", 1)
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: bad})
	if out.Valid {
		t.Fatal("Valid = true for a schema-violating spec")
	}
	if len(out.Diagnostics) != 1 {
		t.Fatalf("Diagnostics = %+v, want exactly one", out.Diagnostics)
	}
	d := out.Diagnostics[0]
	if d.Kind != "schema" {
		t.Errorf("Kind = %q, want schema (message %q)", d.Kind, d.Message)
	}
	if d.Path == "" {
		t.Error("Path is empty; a schema diagnostic must carry the JSON pointer")
	}
	if d.Workflow != "feature_change" {
		t.Errorf("Workflow = %q, want feature_change (path %q)", d.Workflow, d.Path)
	}
	if d.Message == "" {
		t.Error("Message is empty")
	}
	if out.Version != "" {
		t.Errorf("Version = %q, want empty on an invalid spec", out.Version)
	}
}

func TestValidateSpec_Inline_YAMLError(t *testing.T) {
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: "version: [unclosed"})
	if out.Valid {
		t.Fatal("Valid = true for non-YAML input")
	}
	if len(out.Diagnostics) != 1 || out.Diagnostics[0].Kind != "yaml" {
		t.Fatalf("Diagnostics = %+v, want one yaml diagnostic", out.Diagnostics)
	}
	if out.Diagnostics[0].Path != "" {
		t.Errorf("Path = %q, want empty for a yaml failure", out.Diagnostics[0].Path)
	}
}

// TestValidateSpec_Inline_ValidationError_DuplicateStageID is condition (1)'s
// first vehicle: a duplicate stage id is a semantic (validation) failure, and
// the diagnostic must name the offending STAGE id, not just the pointer. On a
// workflow-v2 document the reuse pass rejects it FIRST, at
// /workflows/feature_change/stages with NO index, so the id is recovered from
// the rejection message and stage_index stays absent; the indexed v1 shape is
// covered by TestLocateStage_PointerShapes.
func TestValidateSpec_Inline_ValidationError_DuplicateStageID(t *testing.T) {
	// Rename the review stage to a second "implement": the type stays review
	// so only the id rule trips.
	bad := strings.Replace(validV2Spec, "- id: review\n", "- id: implement\n", 1)
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: bad})
	if out.Valid {
		t.Fatal("Valid = true for a duplicate stage id")
	}
	if len(out.Diagnostics) != 1 {
		t.Fatalf("Diagnostics = %+v, want exactly one", out.Diagnostics)
	}
	d := out.Diagnostics[0]
	if d.Kind != "validation" {
		t.Errorf("Kind = %q, want validation (message %q)", d.Kind, d.Message)
	}
	if d.Workflow != "feature_change" {
		t.Errorf("Workflow = %q, want feature_change (path %q)", d.Workflow, d.Path)
	}
	if d.Path != "/workflows/feature_change/stages" {
		t.Errorf("Path = %q, want the v2 reuse pass's index-less /workflows/feature_change/stages", d.Path)
	}
	if d.StageIndex != nil {
		t.Errorf("StageIndex = %d, want absent for an index-less pointer", *d.StageIndex)
	}
	if d.Stage != "implement" {
		t.Errorf("Stage = %q, want the offending id implement (path %q, message %q)", d.Stage, d.Path, d.Message)
	}
	if !strings.Contains(d.Message, "duplicate stage id") {
		t.Errorf("Message = %q, want the duplicate-stage-id rule", d.Message)
	}
}

// TestValidateSpec_Inline_ValidationError_BadNeeds is condition (1)'s second
// vehicle: a `needs` naming no declared stage expands to an inputs.from_stage
// that fails resolution at /workflows/<wf>/stages/<idx>/inputs/<j>/from_stage;
// stage must be the id of the stage carrying the bad reference.
func TestValidateSpec_Inline_ValidationError_BadNeeds(t *testing.T) {
	bad := strings.Replace(validV2Spec,
		"        inputs:\n          - artifact: plan\n            from_stage: plan\n",
		"        needs: [nonexistent]\n", 1)
	if bad == validV2Spec {
		t.Fatal("fixture edit did not apply")
	}
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: bad})
	if out.Valid {
		t.Fatal("Valid = true for a needs reference to an undeclared stage")
	}
	if len(out.Diagnostics) != 1 {
		t.Fatalf("Diagnostics = %+v, want exactly one", out.Diagnostics)
	}
	d := out.Diagnostics[0]
	if d.Kind != "validation" {
		t.Errorf("Kind = %q, want validation (message %q)", d.Kind, d.Message)
	}
	if !strings.HasSuffix(d.Path, "/from_stage") {
		t.Errorf("Path = %q, want a .../from_stage pointer", d.Path)
	}
	if d.Workflow != "feature_change" {
		t.Errorf("Workflow = %q, want feature_change", d.Workflow)
	}
	if d.StageIndex == nil || *d.StageIndex != 1 {
		t.Errorf("StageIndex = %v, want 1 (path %q)", d.StageIndex, d.Path)
	}
	if d.Stage != "implement" {
		t.Errorf("Stage = %q, want implement — the stage carrying the bad needs (path %q)", d.Stage, d.Path)
	}
	if !strings.Contains(d.Message, "nonexistent") {
		t.Errorf("Message = %q, want it to name the missing referent", d.Message)
	}
}

// TestValidateSpec_StageEmptyWhenNoStagesSegment pins the documented empty
// case: a workflow-level pointer carries the workflow but no stage.
func TestValidateSpec_StageEmptyWhenNoStagesSegment(t *testing.T) {
	// A `paths` applies_to rule on a workflow with no plan stage is refused
	// at /workflows/<wf>/applies_to/paths.
	bad := strings.Replace(validV2Spec, "  feature_change:\n", "  feature_change:\n    applies_to:\n      paths: [\"docs/**\"]\n", 1)
	bad = strings.Replace(bad, "type: plan", "type: implement", 1)
	bad = strings.Replace(bad, "        produces:\n          - artifact: plan\n            schema: standard_v1\n", "", 1)
	bad = strings.Replace(bad, "        inputs:\n          - artifact: plan\n            from_stage: plan\n", "", 1)
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: bad})
	if out.Valid {
		t.Fatal("Valid = true; fixture should be refused")
	}
	d := out.Diagnostics[0]
	if d.Workflow != "feature_change" {
		t.Errorf("Workflow = %q, want feature_change (path %q, message %q)", d.Workflow, d.Path, d.Message)
	}
	if d.Path != "/workflows/feature_change/applies_to/paths" {
		t.Fatalf("Path = %q, want the workflow-level applies_to/paths rejection (message %q)", d.Path, d.Message)
	}
	if d.StageIndex != nil || d.Stage != "" {
		t.Errorf("StageIndex/Stage = %v/%q, want absent/empty for a workflow-level pointer %q", d.StageIndex, d.Stage, d.Path)
	}
}

// TestValidateSpec_UnsupportedVersion_CarriesStaleHint: the /version
// SchemaError is the one shape a stale fishhawk-mcp binary uniquely causes,
// so the annotateStaleSpecError text rides along as hint. RED when the Hint
// assignment is deleted.
func TestValidateSpec_UnsupportedVersion_CarriesStaleHint(t *testing.T) {
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: strings.Replace(validV2Spec, `version: "2"`, `version: "9.0"`, 1)})
	if out.Valid {
		t.Fatal("Valid = true for an unsupported version")
	}
	if len(out.Diagnostics) != 1 || out.Diagnostics[0].Kind != "schema" || out.Diagnostics[0].Path != "/version" {
		t.Fatalf("Diagnostics = %+v, want one schema diagnostic at /version", out.Diagnostics)
	}
	if !strings.Contains(out.Hint, "/mcp") {
		t.Errorf("Hint = %q, want the stale-binary reconnect hint naming /mcp", out.Hint)
	}
	if out.Diagnostics[0].Workflow != "" {
		t.Errorf("Workflow = %q, want empty for a /version pointer", out.Diagnostics[0].Workflow)
	}
}

// TestValidateSpec_CharterRequiredBy: a grooming_report-producing workflow is
// listed; the feature_change beside it is not. RED when the population loop is
// deleted.
func TestValidateSpec_CharterRequiredBy(t *testing.T) {
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: groomingV2Spec})
	if !out.Valid {
		t.Fatalf("Valid = false; diagnostics = %+v", out.Diagnostics)
	}
	if len(out.CharterRequiredBy) != 1 || out.CharterRequiredBy[0] != "backlog_grooming" {
		t.Errorf("CharterRequiredBy = %v, want [backlog_grooming]", out.CharterRequiredBy)
	}
}

// writeRepoSpec lays out a fake checkout: <dir>/.git plus the spec at
// <dir>/.fishhawk/workflows.yaml, and returns the spec's absolute path.
func writeRepoSpec(t *testing.T, dir, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, specFileName)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateSpec_WorkingDir_Discovers(t *testing.T) {
	dir := t.TempDir()
	want := writeRepoSpec(t, dir, validV2Spec)
	// Start the walk from a nested dir so the .git-bounded walk-up is
	// exercised, not just a direct read.
	nested := filepath.Join(dir, "sub", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	out := mustValidate(t, ValidateSpecInput{WorkingDir: nested})
	if !out.Valid {
		t.Fatalf("Valid = false; diagnostics = %+v", out.Diagnostics)
	}
	if out.Source != "file" {
		t.Errorf("Source = %q, want file", out.Source)
	}
	if out.Path != want {
		t.Errorf("Path = %q, want %q", out.Path, want)
	}
}

func TestValidateSpec_SpecFile_Explicit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.yaml")
	if err := os.WriteFile(p, []byte(validV2Spec), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustValidate(t, ValidateSpecInput{SpecFile: p})
	if !out.Valid || out.Source != "file" || out.Path != p {
		t.Errorf("got Valid=%v Source=%q Path=%q, want true/file/%q (diagnostics %+v)", out.Valid, out.Source, out.Path, p, out.Diagnostics)
	}
}

func TestValidateSpec_SpecFile_Missing_FailsClosed(t *testing.T) {
	_, err := callValidate(t, ValidateSpecInput{SpecFile: filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "spec_file") {
		t.Fatalf("err = %v, want a tool error naming spec_file", err)
	}
}

// TestValidateSpec_InlineWinsOverWorkingDir: the dir holds an INVALID file and
// the inline bytes are valid; the verb must report the inline verdict. RED
// when the inline-first arm is deleted (the discovered invalid file decides).
func TestValidateSpec_InlineWinsOverWorkingDir(t *testing.T) {
	dir := t.TempDir()
	writeRepoSpec(t, dir, "version: [unclosed")
	out := mustValidate(t, ValidateSpecInput{WorkflowSpec: validV2Spec, WorkingDir: dir})
	if !out.Valid {
		t.Fatalf("Valid = false — the working_dir file decided instead of the inline bytes; diagnostics = %+v", out.Diagnostics)
	}
	if out.Source != "inline" || out.Path != "" {
		t.Errorf("Source/Path = %q/%q, want inline/empty", out.Source, out.Path)
	}
}

// TestValidateSpec_NoInput_FailsClosed: with none of the three inputs the verb
// refuses with a tool error naming all three. RED when the refusal is
// deleted.
func TestValidateSpec_NoInput_FailsClosed(t *testing.T) {
	_, err := callValidate(t, ValidateSpecInput{})
	if err == nil {
		t.Fatal("err = nil, want a tool error for no input")
	}
	for _, want := range []string{"workflow_spec", "working_dir", "spec_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %s", err, want)
		}
	}
}

func TestValidateSpec_WorkingDir_NoSpec_FailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := callValidate(t, ValidateSpecInput{WorkingDir: dir})
	if err == nil {
		t.Fatal("err = nil, want a tool error for a checkout with no spec")
	}
	for _, want := range []string{dir, specFileName, "workflow_spec", "spec_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

// TestLocateStage_PointerShapes pins the pure pointer→(workflow, index, id)
// derivation on the shapes the validator emits, including the RFC 6901
// unescape and the documented empty cases.
func TestLocateStage_PointerShapes(t *testing.T) {
	two := 2
	cases := []struct {
		pointer   string
		input     string
		wantWF    string
		wantIndex *int
		wantStage string
	}{
		{"/version", validV2Spec, "", nil, ""},
		{"/workflows/feature_change", validV2Spec, "feature_change", nil, ""},
		{"/workflows/feature_change/applies_to/paths", validV2Spec, "feature_change", nil, ""},
		{"/workflows/feature_change/stages/2/id", validV2Spec, "feature_change", &two, "review"},
		{"/workflows/feature_change/stages/9/id", validV2Spec, "feature_change", intPtr(9), ""},
		{"/workflows/feature_change/stages/x/id", validV2Spec, "feature_change", nil, ""},
		{"/workflows/absent/stages/0/id", validV2Spec, "absent", intPtr(0), ""},
		{"/workflows/a~1b/stages/0/id", "workflows:\n  a/b:\n    stages:\n      - id: only\n", "a/b", intPtr(0), "only"},
		// An extends-deriving workflow: the reported index addresses the
		// MERGED stage list, so the author's own list is not read by index.
		{"/workflows/child/stages/0/id", "workflows:\n  child:\n    extends: feature_change\n    stages:\n      - id: extra\n", "child", intPtr(0), ""},
		{"/workflows/feature_change/stages/0/id", "not: [yaml", "feature_change", intPtr(0), ""},
	}
	for _, tc := range cases {
		wf, idx, stage := locateStage(tc.pointer, []byte(tc.input))
		if wf != tc.wantWF || stage != tc.wantStage || !intPtrEq(idx, tc.wantIndex) {
			t.Errorf("locateStage(%q) = (%q, %v, %q), want (%q, %v, %q)", tc.pointer, wf, fmtIntPtr(idx), stage, tc.wantWF, fmtIntPtr(tc.wantIndex), tc.wantStage)
		}
	}
}

func TestDuplicateStageIDFromMessage(t *testing.T) {
	cases := map[string]string{
		`duplicate stage id "implement" declared at positions 1 and 2; …`: "implement",
		`duplicate stage id "a b" (also at /workflows/x/stages/0/id)`:     "a b",
		`duplicate stage id implement`:                                    "",
		`from_stage "x" does not match any stage id in workflow "y"`:      "",
		``: "",
	}
	for msg, want := range cases {
		if got := duplicateStageIDFromMessage(msg); got != want {
			t.Errorf("duplicateStageIDFromMessage(%q) = %q, want %q", msg, got, want)
		}
	}
}

func intPtrEq(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestValidateToolDescription_StatesResiduals pins the SHIPPED description:
// it must name the doctor's default-branch limitation and each not-checked
// residual, plus the skill resource — a prose surface no compiler enforces.
func TestValidateToolDescription_StatesResiduals(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_validate")), " ")
	for _, want := range []string{
		"BEFORE committing",
		"fishhawk_start_run",
		"POST /v0/runs",
		"fishhawk_doctor",
		"DEFAULT BRANCH",
		"charter",
		"model",
		"fishhawk://onboarding-skill",
		"never a tool error",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_validate description missing %q:\n%s", want, desc)
		}
	}
}
