package spec_test

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// readFixture loads a testdata file relative to the package dir.
func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + path)
	if err != nil {
		t.Fatalf("read fixture %q: %v", path, err)
	}
	return b
}

// --- Happy paths ---

func TestParse_CanonicalFeatureChange(t *testing.T) {
	s, err := spec.ParseBytes(readFixture(t, "valid/feature-change.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Version != "0.3" {
		t.Errorf("version = %q, want 0.3", s.Version)
	}
	if got, want := len(s.Workflows), 1; got != want {
		t.Errorf("workflows count = %d, want %d", got, want)
	}
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal(`workflows["feature_change"] missing`)
	}
	if got, want := len(wf.Stages), 3; got != want {
		t.Fatalf("stage count = %d, want %d", got, want)
	}
	plan := wf.Stages[0]
	if plan.ID != "plan" || plan.Type != spec.StageTypePlan {
		t.Errorf("first stage = %+v, want id=plan type=plan", plan)
	}
	if plan.Executor.Agent != "claude-code" {
		t.Errorf("plan.executor.agent = %q, want claude-code", plan.Executor.Agent)
	}
	review := wf.Stages[2]
	if !review.Executor.Human {
		t.Errorf("review stage executor.human should be true")
	}
}

func TestParse_Minimal(t *testing.T) {
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := len(s.Workflows), 1; got != want {
		t.Errorf("workflows = %d, want %d", got, want)
	}
	if len(s.Roles) != 0 {
		t.Errorf("roles should be empty, got %v", s.Roles)
	}
}

// --- on_ci_failure / retry policy (#277) ---

func TestParse_OnCIFailure_Absent_NilPointer(t *testing.T) {
	// No `on_ci_failure` block → Workflow.OnCIFailure is nil. The
	// nil-vs-zero distinction is load-bearing: nil = "use the
	// default of 1 retry"; an explicit `max_retries: 0` = "opt out
	// of auto-retries." The dispatcher reads these differently.
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf, ok := s.Workflows["trivial"]
	if !ok {
		t.Fatal("trivial workflow missing from parsed spec")
	}
	if wf.OnCIFailure != nil {
		t.Errorf("OnCIFailure = %+v, want nil for an unset block", wf.OnCIFailure)
	}
}

func TestParse_OnCIFailure_Default(t *testing.T) {
	// `max_retries: 1` round-trips cleanly. Same shape the
	// dispatcher will read at run-create time when evaluating
	// whether to fire a follow-up implement workflow_dispatch on
	// CI failure (#276).
	yml := []byte(`
version: "0.3"
workflows:
  feature_change:
    description: "x"
    on_ci_failure:
      max_retries: 1
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.OnCIFailure == nil {
		t.Fatal("OnCIFailure should round-trip non-nil")
	}
	if wf.OnCIFailure.MaxRetries != 1 {
		t.Errorf("MaxRetries = %d, want 1", wf.OnCIFailure.MaxRetries)
	}
}

func TestParse_OnCIFailure_ExplicitZero_OptsOut(t *testing.T) {
	// `max_retries: 0` is the explicit opt-out — the dispatcher
	// won't fire any auto-retries even on CI failure. Distinct
	// from the absent-block case (nil pointer → DefaultMaxRetries).
	yml := []byte(`
version: "0.3"
workflows:
  human_led_change:
    description: "x"
    on_ci_failure:
      max_retries: 0
    stages:
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - source: pull_request
            required: true
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["human_led_change"]
	if wf.OnCIFailure == nil {
		t.Fatal("OnCIFailure should round-trip non-nil even when value=0")
	}
	if wf.OnCIFailure.MaxRetries != 0 {
		t.Errorf("MaxRetries = %d, want 0", wf.OnCIFailure.MaxRetries)
	}
}

func TestParse_OnCIFailure_ExceedsCap_Rejected(t *testing.T) {
	// max_retries: 6 violates the schema's maximum: 5. The schema-
	// validation pass surfaces it as a ValidationError naming the
	// failing field — the dispatcher never gets a chance to fire
	// six retries because we refuse the spec before it lands on a
	// run row.
	yml := []byte(`
version: "0.3"
workflows:
  feature_change:
    description: "x"
    on_ci_failure:
      max_retries: 6
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	// The error trail names the offending field so a customer can
	// fix their spec without grepping the schema source.
	if !strings.Contains(se.Path, "max_retries") && !strings.Contains(se.Message, "maximum") {
		t.Errorf("error should name the offending field / constraint: %s", se.Error())
	}
}

// --- Periodic budgets (ADR-030 / #688) ---

func TestParse_Budgets_RoundTrip(t *testing.T) {
	// A workflow with a budgets entry decodes into Workflow.Budgets
	// with every field populated. version 0.4 advertises the field.
	yml := []byte(`
version: "0.4"
workflows:
  feature_change:
    description: "x"
    budgets:
      - period: weekly
        limit_usd: 50
        enforcement: blocking
        warn_at: 0.8
      - period: monthly
        limit_usd: 200.5
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if got, want := len(wf.Budgets), 2; got != want {
		t.Fatalf("budgets count = %d, want %d", got, want)
	}
	b0 := wf.Budgets[0]
	if b0.Period != spec.BudgetPeriodWeekly {
		t.Errorf("budgets[0].period = %q, want weekly", b0.Period)
	}
	if b0.LimitUSD != 50 {
		t.Errorf("budgets[0].limit_usd = %v, want 50", b0.LimitUSD)
	}
	if b0.Enforcement != spec.EnforcementBlocking {
		t.Errorf("budgets[0].enforcement = %q, want blocking", b0.Enforcement)
	}
	if b0.WarnAt == nil || *b0.WarnAt != 0.8 {
		t.Errorf("budgets[0].warn_at = %v, want 0.8", b0.WarnAt)
	}
	// Second entry omits enforcement + warn_at: enforcement is the
	// zero value (caller defaults to advisory) and WarnAt is nil.
	b1 := wf.Budgets[1]
	if b1.Period != spec.BudgetPeriodMonthly {
		t.Errorf("budgets[1].period = %q, want monthly", b1.Period)
	}
	if b1.LimitUSD != 200.5 {
		t.Errorf("budgets[1].limit_usd = %v, want 200.5", b1.LimitUSD)
	}
	if b1.WarnAt != nil {
		t.Errorf("budgets[1].warn_at = %v, want nil for an omitted field", b1.WarnAt)
	}
}

func TestParse_Budgets_Absent_NilSlice(t *testing.T) {
	// No budgets block → Workflow.Budgets is nil; the admission gate
	// and advisory wiring are no-ops for such a workflow.
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if wf := s.Workflows["trivial"]; wf.Budgets != nil {
		t.Errorf("Budgets = %v, want nil for an absent block", wf.Budgets)
	}
}

func TestParse_Budgets_UnknownPeriod_Rejected(t *testing.T) {
	// period is a closed enum (weekly|monthly); an unknown value is a
	// schema error refused before the spec lands on a run row.
	_, err := spec.ParseBytes([]byte(`
version: "0.4"
workflows:
  feature_change:
    budgets:
      - period: daily
        limit_usd: 10
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_Budgets_MissingLimit_Rejected(t *testing.T) {
	// limit_usd is required on a budget entry; its absence is a
	// schema error.
	_, err := spec.ParseBytes([]byte(`
version: "0.4"
workflows:
  feature_change:
    budgets:
      - period: weekly
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_Budgets_WarnAtOutOfRange_Rejected(t *testing.T) {
	// warn_at must be a fraction in [0,1]; >1 is a schema error.
	_, err := spec.ParseBytes([]byte(`
version: "0.4"
workflows:
  feature_change:
    budgets:
      - period: monthly
        limit_usd: 100
        warn_at: 1.5
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- Drive mode (#1023) ---

func TestParse_Drive(t *testing.T) {
	// `drive` is an optional workflow-level boolean, default false.
	// Absent and explicit-false are indistinguishable on the struct
	// (both false) — by design: unlike on_ci_failure there is no
	// nil-vs-zero distinction to preserve, the per-run override at
	// POST /v0/runs is a separate knob.
	const tmpl = `
version: "0.3"
workflows:
  feature_change:
    description: "x"
%s    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	cases := []struct {
		name  string
		drive string // injected workflow-level line; "" = absent
		want  bool
	}{
		{name: "absent_defaults_false", drive: "", want: false},
		{name: "explicit_false", drive: "    drive: false\n", want: false},
		{name: "explicit_true", drive: "    drive: true\n", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := spec.ParseBytes([]byte(strings.ReplaceAll(tmpl, "%s", tc.drive)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := s.Workflows["feature_change"].Drive; got != tc.want {
				t.Errorf("Drive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParse_Drive_NonBoolean_Rejected(t *testing.T) {
	// The schema types `drive` as boolean; a string is a schema error.
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  feature_change:
    drive: "yes"
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- Executor model override (#1013) ---

func TestParse_ExecutorModel(t *testing.T) {
	// `executor.model` is an optional per-stage model override in the agent
	// branch. Absent decodes to the empty string (one rung of the
	// implement-model ladder; empty falls through to the next-lower rung).
	const tmpl = `
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
%s`
	cases := []struct {
		name  string
		model string // injected executor line; "" = absent
		want  string
	}{
		{name: "absent_defaults_empty", model: "", want: ""},
		{name: "explicit_model", model: "          model: claude-opus-4-8\n", want: "claude-opus-4-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := spec.ParseBytes([]byte(strings.ReplaceAll(tmpl, "%s", tc.model)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := s.Workflows["feature_change"].Stages[0].Executor.Model; got != tc.want {
				t.Errorf("Executor.Model = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParse_ExecutorModel_OnHuman_Rejected confirms model lives in the agent
// branch of the executor oneOf only: declaring it on a human executor trips
// unevaluatedProperties.
func TestParse_ExecutorModel_OnHuman_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
          model: claude-opus-4-8
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- YAML errors ---

func TestParse_EmptyDocument(t *testing.T) {
	_, err := spec.ParseBytes([]byte("\n   \n"))
	var ye *spec.YAMLError
	if !errors.As(err, &ye) {
		t.Fatalf("err = %v, want *YAMLError", err)
	}
}

func TestParse_MalformedYAML(t *testing.T) {
	_, err := spec.ParseBytes([]byte("version: '0.1'\n  bad: indent: again"))
	var ye *spec.YAMLError
	if !errors.As(err, &ye) {
		t.Fatalf("err = %v, want *YAMLError", err)
	}
}

// --- Schema errors ---

func TestParse_MissingVersion(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
workflows:
  trivial:
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_WrongVersion(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "9.9"
workflows:
  trivial:
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_UnknownStageType(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: x
        type: deploy
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_BothExecutorKinds(t *testing.T) {
	// Schema's oneOf rejects {agent, human} together.
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: x
        type: plan
        executor:
          agent: claude-code
          human: true
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_StageIDPattern(t *testing.T) {
	// Stage IDs must be snake_case (^[a-z][a-z0-9_]*$).
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: NotSnakeCase
        type: plan
        executor: { agent: claude-code }
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_UnknownArtifactKind(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: x
        type: implement
        executor: { agent: claude-code }
        produces:
          - artifact: design_doc
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- Semantic (post-schema) errors ---

func TestParse_DuplicateStageIDs(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: same
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
      - id: same
        type: implement
        executor: { agent: claude-code }
        produces:
          - artifact: pull_request
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "duplicate") {
		t.Errorf("message = %q, expected to mention 'duplicate'", ve.Message)
	}
}

func TestParse_DanglingFromStage(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor: { agent: claude-code }
        inputs:
          - artifact: plan
            from_stage: nonexistent
        produces:
          - artifact: pull_request
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "from_stage") {
		t.Errorf("message = %q, expected to mention 'from_stage'", ve.Message)
	}
}

func TestParse_ForwardFromStage(t *testing.T) {
	// from_stage may reference only earlier stages.
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: first
        type: implement
        executor: { agent: claude-code }
        inputs:
          - artifact: plan
            from_stage: second
        produces:
          - artifact: pull_request
      - id: second
        type: review
        executor: { human: true }
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
}

func TestParse_UndefinedApproverRole(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  trivial:
    stages:
      - id: review
        type: review
        executor: { human: true }
        gates:
          - type: approval
            approvers:
              any_of: [maintainer]   # not defined
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "maintainer") {
		t.Errorf("message = %q, expected to name the missing role", ve.Message)
	}
}

func TestParse_PlanMissingSchema(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            # schema: standard_v1     ← deliberately omitted
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "standard_v1") {
		t.Errorf("message = %q, expected to mention standard_v1", ve.Message)
	}
}

// --- Validate(*Spec) standalone ---

func TestValidate_NilSpec(t *testing.T) {
	if err := spec.Validate(nil); err == nil {
		t.Fatal("Validate(nil) should error")
	}
}

func TestValidate_BuiltProgrammatically(t *testing.T) {
	// Confirms callers can build a Spec in-memory and run only the
	// semantic layer without going through Parse.
	s := &spec.Spec{
		Version: "0.3",
		Roles: map[string]spec.Role{
			"founder": {Members: []string{"@kuhlman-labs"}},
		},
		Workflows: map[string]spec.Workflow{
			"trivial": {
				Stages: []spec.Stage{
					{
						ID:       "plan",
						Type:     spec.StageTypePlan,
						Executor: spec.Executor{Agent: "claude-code"},
						Produces: []spec.Produces{
							{Artifact: spec.ArtifactPlan, Schema: "standard_v1"},
						},
					},
				},
			},
		},
	}
	if err := spec.Validate(s); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// --- Timeout policy (#452) ---

func TestParse_TimeoutPolicy(t *testing.T) {
	s, err := spec.ParseBytes(readFixture(t, "valid/timeout-policy.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal(`workflows["feature_change"] missing`)
	}
	if wf.Policy == nil {
		t.Fatal("Policy should be non-nil")
	}
	if got, want := wf.Policy.MaxStageRuntime.Duration, 30*time.Minute; got != want {
		t.Errorf("Policy.MaxStageRuntime = %v, want %v", got, want)
	}
	if len(wf.Stages) == 0 {
		t.Fatal("no stages")
	}
	planStage := wf.Stages[0]
	if got, want := planStage.Executor.Timeout.Duration, 10*time.Minute; got != want {
		t.Errorf("plan stage Executor.Timeout = %v, want %v", got, want)
	}
	// implement stage has no explicit timeout.
	if len(wf.Stages) < 2 {
		t.Fatal("expected at least 2 stages")
	}
	implStage := wf.Stages[1]
	if implStage.Executor.Timeout.Duration != 0 {
		t.Errorf("implement stage Executor.Timeout = %v, want 0", implStage.Executor.Timeout.Duration)
	}
}

func TestParse_NoTimeout_BackwardCompat(t *testing.T) {
	// Existing specs without policy or executor.timeout must still parse.
	s, err := spec.ParseBytes(readFixture(t, "valid/feature-change.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.Policy != nil {
		t.Errorf("Policy = %+v, want nil for spec without policy block", wf.Policy)
	}
	for _, st := range wf.Stages {
		if st.Executor.Timeout.Duration != 0 {
			t.Errorf("stage %q Executor.Timeout = %v, want 0", st.ID, st.Executor.Timeout.Duration)
		}
	}
}

func TestParse_VerifyMaxIterations_RoundTrip(t *testing.T) {
	// executor.verify.max_iterations round-trips into VerifyConfig.
	yml := []byte(`
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test"
            max_iterations: 3
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := s.Workflows["feature_change"].Stages[0].Executor.Verify
	if v == nil {
		t.Fatal("Verify should round-trip non-nil")
	}
	if v.MaxIterations != 3 {
		t.Errorf("MaxIterations = %d, want 3", v.MaxIterations)
	}
}

func TestParse_VerifyMaxIterations_DefaultsZero(t *testing.T) {
	// A verify block without max_iterations defaults to 0 (single-shot).
	yml := []byte(`
version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test"
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := s.Workflows["feature_change"].Stages[0].Executor.Verify
	if v == nil {
		t.Fatal("Verify should round-trip non-nil")
	}
	if v.MaxIterations != 0 {
		t.Errorf("MaxIterations = %d, want 0 when absent", v.MaxIterations)
	}
}

func TestResolveStageTimeout(t *testing.T) {
	const def = 15 * time.Minute

	makeDur := func(d time.Duration) spec.Duration {
		return spec.Duration{Duration: d}
	}

	cases := []struct {
		name    string
		policy  *spec.Policy
		stageTO spec.Duration
		want    time.Duration
	}{
		{
			name:    "stage timeout wins over workflow policy and default",
			policy:  &spec.Policy{MaxStageRuntime: makeDur(30 * time.Minute)},
			stageTO: makeDur(10 * time.Minute),
			want:    10 * time.Minute,
		},
		{
			name:    "workflow policy wins over default when stage timeout is zero",
			policy:  &spec.Policy{MaxStageRuntime: makeDur(20 * time.Minute)},
			stageTO: makeDur(0),
			want:    20 * time.Minute,
		},
		{
			name:    "default wins when both stage and policy are zero",
			policy:  nil,
			stageTO: makeDur(0),
			want:    def,
		},
		{
			name:    "zero stage timeout falls through to workflow policy",
			policy:  &spec.Policy{MaxStageRuntime: makeDur(45 * time.Minute)},
			stageTO: makeDur(0),
			want:    45 * time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := spec.Workflow{Policy: tc.policy}
			st := spec.Stage{Executor: spec.Executor{Timeout: tc.stageTO}}
			got := spec.ResolveStageTimeout(wf, st, def)
			if got != tc.want {
				t.Errorf("ResolveStageTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveReviewTimeout asserts the review-budget-floor resolution ladder
// (#1494): a non-empty, parseable spec reviewers.review_timeout WINS over the
// deployment default; an empty string, an unparseable string, a zero duration,
// and a nil reviewers block all fall back to the default. The unparseable and
// zero branches are the fail-closed guards — they must yield the default, never
// a zero Floor (which would silently kill reviewers on tiny prompts).
func TestResolveReviewTimeout(t *testing.T) {
	const def = 300 * time.Second

	cases := []struct {
		name      string
		reviewers *spec.ReviewersConfig
		want      time.Duration
	}{
		{
			name:      "spec review_timeout wins over the deployment default",
			reviewers: &spec.ReviewersConfig{ReviewTimeout: "47s"},
			want:      47 * time.Second,
		},
		{
			name:      "empty review_timeout falls back to the default",
			reviewers: &spec.ReviewersConfig{ReviewTimeout: ""},
			want:      def,
		},
		{
			name:      "unparseable review_timeout falls back to the default",
			reviewers: &spec.ReviewersConfig{ReviewTimeout: "not-a-duration"},
			want:      def,
		},
		{
			name:      "zero-duration review_timeout falls back to the default",
			reviewers: &spec.ReviewersConfig{ReviewTimeout: "0s"},
			want:      def,
		},
		{
			name:      "nil reviewers block falls back to the default",
			reviewers: nil,
			want:      def,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := spec.ResolveReviewTimeout(tc.reviewers, def)
			if got != tc.want {
				t.Errorf("ResolveReviewTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- agent_self_retry (ADR-023 / #533) ---

func TestParse_AgentSelfRetry_Absent(t *testing.T) {
	// Omitted field defaults to false — the zero value.
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ex := s.Workflows["trivial"].Stages[0].Executor
	if ex.AgentSelfRetry {
		t.Errorf("AgentSelfRetry = true, want false when field is absent")
	}
}

func TestParse_AgentSelfRetry_True(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_self_retry: true
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ex := s.Workflows["trivial"].Stages[0].Executor
	if !ex.AgentSelfRetry {
		t.Errorf("AgentSelfRetry = false, want true")
	}
}

func TestParse_AgentSelfRetry_ExplicitFalse(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_self_retry: false
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ex := s.Workflows["trivial"].Stages[0].Executor
	if ex.AgentSelfRetry {
		t.Errorf("AgentSelfRetry = true, want false when explicitly set to false")
	}
}

func TestParse_AgentSelfRetry_WrongType(t *testing.T) {
	// "yes" is a string, not a boolean — schema rejects it.
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_self_retry: "yes"
        produces:
          - artifact: pull_request
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- reviewers field (ADR-027 / #560) ---

func TestParse_Reviewers_Absent_NilPointer(t *testing.T) {
	// No `reviewers` block → Stage.Reviewers is nil. The nil pointer is
	// load-bearing: callers treat nil as {Human:1} (pre-ADR-027 behavior).
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	st := s.Workflows["trivial"].Stages[0]
	if st.Reviewers != nil {
		t.Errorf("Reviewers = %+v, want nil when block is absent", st.Reviewers)
	}
}

func TestParse_Reviewers_ExplicitAgentAndHuman(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 1
          human: 1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	st := s.Workflows["trivial"].Stages[0]
	if st.Reviewers == nil {
		t.Fatal("Reviewers should be non-nil when block is present")
	}
	if st.Reviewers.Agent != 1 {
		t.Errorf("Reviewers.Agent = %d, want 1", st.Reviewers.Agent)
	}
	if st.Reviewers.Human != 1 {
		t.Errorf("Reviewers.Human = %d, want 1", st.Reviewers.Human)
	}
}

func TestParse_Reviewers_AgentOnly_Gating(t *testing.T) {
	// agent>0 && human==0 → gating authority mode.
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 2
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv == nil {
		t.Fatal("Reviewers should be non-nil")
	}
	if rv.Agent != 2 {
		t.Errorf("Reviewers.Agent = %d, want 2", rv.Agent)
	}
	if rv.Human != 0 {
		t.Errorf("Reviewers.Human = %d, want 0 (omitted → zero)", rv.Human)
	}
}

func TestParse_Reviewers_AgentsList_Heterogeneous(t *testing.T) {
	// #955: the heterogeneous agents list parses with per-reviewer
	// provider+model, and AgentCount() returns its length.
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
          human: 1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv == nil {
		t.Fatal("Reviewers should be non-nil")
	}
	if len(rv.Agents) != 2 {
		t.Fatalf("Reviewers.Agents len = %d, want 2", len(rv.Agents))
	}
	if rv.Agents[0].Provider != "anthropic" || rv.Agents[0].Model != "claude-opus-4-8" {
		t.Errorf("Agents[0] = %+v, want {anthropic claude-opus-4-8}", rv.Agents[0])
	}
	if rv.Agents[1].Provider != "codex" || rv.Agents[1].Model != "" {
		t.Errorf("Agents[1] = %+v, want {codex} with empty model (provider default)", rv.Agents[1])
	}
	if got := rv.AgentCount(); got != 2 {
		t.Errorf("AgentCount() = %d, want 2 (len(Agents))", got)
	}
}

func TestParse_Reviewers_AgentsList_SupersedesBareCount(t *testing.T) {
	// #955 supersession rule: when both `agents` and the bare `agent`
	// integer are present, the list wins — AgentCount() == len(Agents).
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 5
          agents:
            - provider: claudecode
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv.Agent != 5 {
		t.Errorf("Reviewers.Agent = %d, want 5 (bare count still parsed)", rv.Agent)
	}
	if got := rv.AgentCount(); got != 1 {
		t.Errorf("AgentCount() = %d, want 1 (agents list supersedes the bare count)", got)
	}
}

func TestParse_Reviewers_AgentsList_UnknownProvider_Rejected(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: banana
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for unknown provider", err)
	}
}

func TestParse_Reviewers_AgentsList_ReasoningEffort_RoundTrip(t *testing.T) {
	// #1493: a workflow-v1 reviewers.agents entry carrying reasoning_effort
	// parses into AgentReviewer.ReasoningEffort and survives a re-marshal. The
	// field is workflow-v1-only, so the spec is pinned at version "1.0".
	yml := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              reasoning_effort: high
            - provider: anthropic
          human: 1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv == nil || len(rv.Agents) != 2 {
		t.Fatalf("Reviewers.Agents = %+v, want 2 entries", rv)
	}
	if rv.Agents[0].Provider != "codex" || rv.Agents[0].ReasoningEffort != "high" {
		t.Errorf("Agents[0] = %+v, want {codex reasoning_effort=high}", rv.Agents[0])
	}
	// An absent reasoning_effort stays empty (falls back to the deployment
	// default at the seam).
	if rv.Agents[1].ReasoningEffort != "" {
		t.Errorf("Agents[1].ReasoningEffort = %q, want empty (absent)", rv.Agents[1].ReasoningEffort)
	}

	// Re-marshal preserves the field (omitempty keeps the absent one absent).
	out, err := yaml.Marshal(rv.Agents[0])
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !strings.Contains(string(out), "reasoning_effort: high") {
		t.Errorf("re-marshalled agent = %q, want it to preserve reasoning_effort: high", out)
	}
	absent, err := yaml.Marshal(rv.Agents[1])
	if err != nil {
		t.Fatalf("re-marshal absent: %v", err)
	}
	if strings.Contains(string(absent), "reasoning_effort") {
		t.Errorf("re-marshalled agent with no effort = %q, want reasoning_effort omitted", absent)
	}
}

func TestParse_Reviewers_AgentsList_Optional_RoundTrip(t *testing.T) {
	// #1495: a workflow-v1 reviewers.agents entry carrying optional parses
	// into AgentReviewer.Optional and survives a re-marshal; an absent optional
	// defaults to false (the deployment SHOULD run it — loud degradation). The
	// field is additive within workflow-v1.x; pinned at version "1.0".
	yml := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              optional: true
            - provider: anthropic
          human: 1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv == nil || len(rv.Agents) != 2 {
		t.Fatalf("Reviewers.Agents = %+v, want 2 entries", rv)
	}
	if rv.Agents[0].Provider != "codex" || !rv.Agents[0].Optional {
		t.Errorf("Agents[0] = %+v, want {codex optional=true}", rv.Agents[0])
	}
	// An absent optional defaults to false (the done-means: default-false is
	// honored, not merely accepted).
	if rv.Agents[1].Optional {
		t.Errorf("Agents[1].Optional = true, want false (absent → default false)")
	}

	// Re-marshal preserves optional:true; omitempty keeps the absent one absent.
	out, err := yaml.Marshal(rv.Agents[0])
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !strings.Contains(string(out), "optional: true") {
		t.Errorf("re-marshalled agent = %q, want it to preserve optional: true", out)
	}
	absent, err := yaml.Marshal(rv.Agents[1])
	if err != nil {
		t.Fatalf("re-marshal absent: %v", err)
	}
	if strings.Contains(string(absent), "optional") {
		t.Errorf("re-marshalled agent with default optional = %q, want optional omitted", absent)
	}
}

func TestParse_Reviewers_AgentsList_ReasoningEffort_InvalidEnum_Rejected(t *testing.T) {
	// #1493: the schema enum (low|medium|high|xhigh|max) is the sole guard
	// before the value reaches the codex CLI as -c model_reasoning_effort, so
	// an out-of-enum value must be rejected by spec validation.
	yml := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              reasoning_effort: turbo
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for out-of-enum reasoning_effort", err)
	}
}

func TestParse_Reviewers_ReviewTimeout_RoundTrip(t *testing.T) {
	// #1494: a workflow-v1 reviewers.review_timeout parses onto
	// ReviewersConfig.ReviewTimeout under DisallowUnknownFields and survives a
	// re-marshal. The field is workflow-v1-only, so the spec is pinned at
	// version "1.0". An absent field stays empty (falls back to the deployment
	// default at the seam).
	withTimeout := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 1
          review_timeout: 5m
`)
	s, err := spec.ParseBytes(withTimeout)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rv := s.Workflows["trivial"].Stages[0].Reviewers
	if rv == nil || rv.ReviewTimeout != "5m" {
		t.Fatalf("Reviewers.ReviewTimeout = %+v, want \"5m\"", rv)
	}
	out, err := yaml.Marshal(rv)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !strings.Contains(string(out), "review_timeout: 5m") {
		t.Errorf("re-marshalled reviewers = %q, want it to preserve review_timeout: 5m", out)
	}

	absent := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 1
`)
	sa, err := spec.ParseBytes(absent)
	if err != nil {
		t.Fatalf("Parse absent: %v", err)
	}
	if rv := sa.Workflows["trivial"].Stages[0].Reviewers; rv == nil || rv.ReviewTimeout != "" {
		t.Errorf("absent review_timeout = %+v, want empty string", rv)
	}
}

func TestParse_Reviewers_ReviewTimeout_InvalidPattern_Rejected(t *testing.T) {
	// #1494: the schema duration pattern (^([0-9]+(ns|us|ms|s|m|h))+$) is the
	// guard at parse time, so a malformed duration string must be rejected by
	// spec validation rather than reaching ResolveReviewTimeout.
	yml := []byte(`
version: "1.0"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 1
          review_timeout: 5minutes
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for malformed review_timeout", err)
	}
}

func TestReviewersConfig_AgentCount_CountFormUnchanged(t *testing.T) {
	// Back-compat: without an agents list, AgentCount is the bare count.
	if got := (spec.ReviewersConfig{Agent: 3}).AgentCount(); got != 3 {
		t.Errorf("AgentCount() = %d, want 3", got)
	}
	if got := (spec.ReviewersConfig{}).AgentCount(); got != 0 {
		t.Errorf("AgentCount() zero-value = %d, want 0", got)
	}
}

func TestParse_Reviewers_NegativeAgent_Rejected(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: -1
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for negative agent count", err)
	}
}

func TestParse_Reviewers_NegativeHuman_Rejected(t *testing.T) {
	yml := []byte(`
version: "0.3"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          human: -1
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for negative human count", err)
	}
}

// --- approvals gate predicate (E39.2 / #1707) ---

// TestApprovalsGateParsesAndValidates is the cross-layer done-means: the
// approvals fixture (all five predicate fields populated) round-trips
// through the backend embedded workflow-v1 schema AND the semantic
// Validate pass, and every decoded Approvals field carries its declared
// value. Proves the block is wired end-to-end (schema accept -> JSON
// coerce -> struct decode), not merely structurally present.
func TestApprovalsGateParsesAndValidates(t *testing.T) {
	s, err := spec.ParseBytes(readFixture(t, "valid/approvals.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal(`workflows["feature_change"] missing`)
	}
	if len(wf.Stages) != 1 || len(wf.Stages[0].Gates) != 1 {
		t.Fatalf("want 1 stage with 1 gate, got %d stages", len(wf.Stages))
	}
	g := wf.Stages[0].Gates[0]
	if g.Type != spec.GateTypeApproval {
		t.Errorf("gate type = %q, want approval", g.Type)
	}
	// The legacy approvers form is nil — this gate uses approvals.
	if g.Approvers != nil {
		t.Errorf("Approvers = %+v, want nil for an approvals-only gate", g.Approvers)
	}
	a := g.Approvals
	if a == nil {
		t.Fatal("Approvals should be non-nil for an approvals gate")
	}
	if a.Count == nil || *a.Count != 2 {
		t.Errorf("Approvals.Count = %v, want 2", a.Count)
	}
	if len(a.Not) != 2 || a.Not[0] != "author" || a.Not[1] != "agent" {
		t.Errorf("Approvals.Not = %v, want [author agent]", a.Not)
	}
	if a.MinPermission != "write" {
		t.Errorf("Approvals.MinPermission = %q, want write", a.MinPermission)
	}
	if a.MemberOf != "my-org/reviewers" {
		t.Errorf("Approvals.MemberOf = %q, want my-org/reviewers", a.MemberOf)
	}
	if len(a.Members) != 2 || a.Members[0] != "alice" || a.Members[1] != "bob" {
		t.Errorf("Approvals.Members = %v, want [alice bob]", a.Members)
	}
}

// TestParse_ApprovalGate_NeitherPredicate_Rejected asserts the relaxed
// required invariant: an approval gate declaring NEITHER approvers nor
// approvals is rejected by the schema (the gate approval-branch inner
// oneOf must match exactly one). Guards against the gate becoming a
// no-op when `required` dropped `approvers`.
func TestParse_ApprovalGate_NeitherPredicate_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for an approval gate with no predicate", err)
	}
}

// TestParse_ApprovalGate_EmptyApprovals_Rejected pins binding condition
// (1): count is REQUIRED inside the approvals object, so `approvals: {}`
// fails validation. An empty predicate is a no-op and must be refused.
func TestParse_ApprovalGate_EmptyApprovals_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals: {}
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for approvals:{} (missing required count)", err)
	}
}

// TestParse_ApprovalGate_BothPredicates_Rejected pins binding condition
// (2): a single gate must not declare BOTH the legacy approvers form and
// the new approvals block. The mutual exclusion is enforced in the schema
// (the gate approval-branch inner oneOf), so a both-declared gate matches
// two subschemas and is rejected.
func TestParse_ApprovalGate_BothPredicates_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvers:
              any_of: [founder]
            approvals:
              count: 1
              not: [author, agent]
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for a gate declaring both approvers and approvals", err)
	}
}

// TestParse_ApprovalGate_ApproversOnly_BackCompat is the back-compat
// proof: the legacy approvers-only form still validates under the v1
// schema after approvals was added — no new required field was
// introduced. A count-only approvals gate (the ADR-055 minimum) also
// validates, confirming the other predicates stay optional.
func TestParse_ApprovalGate_ApproversOnly_BackCompat(t *testing.T) {
	approversOnly := []byte(`
version: "1.0"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvers:
              any_of: [founder]
`)
	s, err := spec.ParseBytes(approversOnly)
	if err != nil {
		t.Fatalf("Parse approvers-only: %v", err)
	}
	g := s.Workflows["feature_change"].Stages[0].Gates[0]
	if g.Approvers == nil || len(g.Approvers.AnyOf) != 1 || g.Approvers.AnyOf[0] != "founder" {
		t.Errorf("Approvers = %+v, want any_of=[founder]", g.Approvers)
	}
	if g.Approvals != nil {
		t.Errorf("Approvals = %+v, want nil for an approvers-only gate", g.Approvals)
	}

	countOnly := []byte(`
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
`)
	sc, err := spec.ParseBytes(countOnly)
	if err != nil {
		t.Fatalf("Parse count-only approvals: %v", err)
	}
	a := sc.Workflows["feature_change"].Stages[0].Gates[0].Approvals
	if a == nil || a.Count == nil || *a.Count != 1 {
		t.Fatalf("count-only Approvals = %+v, want Count=1 with optional predicates absent", a)
	}
	if len(a.Not) != 0 || a.MinPermission != "" || a.MemberOf != "" || len(a.Members) != 0 {
		t.Errorf("count-only Approvals should leave optional predicates empty, got %+v", a)
	}
}

// --- operator_agent delegation knobs (ADR-040 / #1026) ---

func TestParse_OperatorAgent_RoundTrip(t *testing.T) {
	// The fixture declares a workflow-level block and a per-gate
	// override on the plan stage's approval gate; the implement
	// stage's gate has no block. Exercises both placements plus the
	// EffectiveOperatorAgent precedence (gate wins wholesale, else
	// workflow, else nil).
	s, err := spec.ParseBytes(readFixture(t, "valid/operator-agent.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Version != "0.5" {
		t.Errorf("version = %q, want 0.5", s.Version)
	}
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal(`workflows["feature_change"] missing`)
	}
	wfBlock := wf.OperatorAgent
	if wfBlock == nil {
		t.Fatal("workflow-level OperatorAgent should be non-nil")
	}
	if wfBlock.MayApprove != spec.ConditionCleanDualApproval {
		t.Errorf("workflow MayApprove = %q, want clean_dual_approval", wfBlock.MayApprove)
	}
	if wfBlock.MayRouteFixup != spec.ConditionConvergentConcerns {
		t.Errorf("workflow MayRouteFixup = %q, want convergent_concerns", wfBlock.MayRouteFixup)
	}
	if wfBlock.MayRetry != spec.ConditionInfraFlake {
		t.Errorf("workflow MayRetry = %q, want infra_flake", wfBlock.MayRetry)
	}
	if wfBlock.MayMerge != spec.ConditionGatesResolvedCIGreen {
		t.Errorf("workflow MayMerge = %q, want gates_resolved_ci_green", wfBlock.MayMerge)
	}
	if wfBlock.MayWaive != "" {
		t.Errorf("workflow MayWaive = %q, want empty (not delegated)", wfBlock.MayWaive)
	}
	wantPage := []string{spec.PageEventReviewerReject, spec.PageEventBudgetOverride}
	if len(wfBlock.MustPageHuman) != 2 || wfBlock.MustPageHuman[0] != wantPage[0] || wfBlock.MustPageHuman[1] != wantPage[1] {
		t.Errorf("workflow MustPageHuman = %v, want %v", wfBlock.MustPageHuman, wantPage)
	}
	// model_policy (#1421) round-trips into ModelPolicy with the exact
	// declared strategy/defaults/allowed — asserts the SHIPPED contract,
	// not just that the field parsed.
	mp := wfBlock.ModelPolicy
	if mp == nil {
		t.Fatal("workflow-level ModelPolicy should be non-nil")
	}
	if mp.Strategy != spec.ModelPolicyExplicitDefaults {
		t.Errorf("ModelPolicy.Strategy = %q, want explicit_defaults", mp.Strategy)
	}
	if mp.Defaults == nil {
		t.Fatal("ModelPolicy.Defaults should be non-nil")
	}
	if mp.Defaults.Plan != "claude-opus-4-8" || mp.Defaults.Implement != "claude-sonnet-4-6" || mp.Defaults.Review != "gpt-5.5" {
		t.Errorf("ModelPolicy.Defaults = %+v, want {plan:claude-opus-4-8 implement:claude-sonnet-4-6 review:gpt-5.5}", *mp.Defaults)
	}
	wantAllowed := []string{"claude-opus-4-8", "claude-sonnet-4-6", "gpt-5.5"}
	if len(mp.Allowed) != len(wantAllowed) {
		t.Fatalf("ModelPolicy.Allowed = %v, want %v", mp.Allowed, wantAllowed)
	}
	for i, want := range wantAllowed {
		if mp.Allowed[i] != want {
			t.Errorf("ModelPolicy.Allowed[%d] = %q, want %q", i, mp.Allowed[i], want)
		}
	}

	planGate := &wf.Stages[0].Gates[0]
	if planGate.OperatorAgent == nil {
		t.Fatal("plan gate OperatorAgent should be non-nil")
	}
	eff := wf.EffectiveOperatorAgent(planGate)
	if eff != planGate.OperatorAgent {
		t.Errorf("EffectiveOperatorAgent(plan gate) = %+v, want the gate-level block", eff)
	}
	if eff.MayWaive != spec.ConditionSoloLow {
		t.Errorf("gate MayWaive = %q, want solo_low", eff.MayWaive)
	}
	// The gate block wins WHOLESALE — knobs the gate omits are not
	// inherited from the workflow block. The workflow delegates
	// may_retry; the gate block doesn't, so the effective view must
	// not carry it.
	if eff.MayRetry != "" {
		t.Errorf("gate-effective MayRetry = %q, want empty (no cross-level merge)", eff.MayRetry)
	}
	// model_policy (#1421) inherits the same wholesale-override semantics:
	// the gate block declares no model_policy, so the workflow-level
	// policy is NOT merged into the effective view — fails if model_policy
	// were ever inherited across levels.
	if eff.ModelPolicy != nil {
		t.Errorf("gate-effective ModelPolicy = %+v, want nil (no cross-level merge)", eff.ModelPolicy)
	}

	implGate := &wf.Stages[1].Gates[0]
	if implGate.OperatorAgent != nil {
		t.Fatalf("implement gate OperatorAgent = %+v, want nil", implGate.OperatorAgent)
	}
	if got := wf.EffectiveOperatorAgent(implGate); got != wf.OperatorAgent {
		t.Errorf("EffectiveOperatorAgent(implement gate) = %+v, want the workflow-level block", got)
	}
	if got := wf.EffectiveOperatorAgent(nil); got != wf.OperatorAgent {
		t.Errorf("EffectiveOperatorAgent(nil) = %+v, want the workflow-level block", got)
	}
}

func TestParse_OperatorAgent_Absent_Nil(t *testing.T) {
	// No operator_agent block anywhere → nil at every level, and the
	// precedence helper resolves to nil. Nil is load-bearing:
	// fail-closed, nothing is delegated.
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["trivial"]
	if wf.OperatorAgent != nil {
		t.Errorf("OperatorAgent = %+v, want nil for an absent block", wf.OperatorAgent)
	}
	if got := wf.EffectiveOperatorAgent(nil); got != nil {
		t.Errorf("EffectiveOperatorAgent = %+v, want nil (fail-closed)", got)
	}
}

func TestParse_OperatorAgent_UnknownCondition_Rejected(t *testing.T) {
	// Each knob is a closed single-value enum; an unknown condition is
	// refused at parse with a JSON Pointer into the offending knob.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_approve: anything_goes
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(se.Path, "operator_agent/may_approve") {
		t.Errorf("Path = %q, want a JSON Pointer into operator_agent/may_approve", se.Path)
	}
}

func TestParse_OperatorAgent_UnknownKnob_Rejected(t *testing.T) {
	// additionalProperties: false closes the knob set — a knob the
	// backend can't evaluate must never parse.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_deploy: anything
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_OperatorAgent_UnknownPageEvent_Rejected(t *testing.T) {
	// must_page_human items are a closed enum too.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      must_page_human: [solar_flare]
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_OperatorAgent_RouteFixupMinSeverity_RoundTrip(t *testing.T) {
	// route_fixup_min_severity (#1964) round-trips into
	// OperatorAgent.RouteFixupMinSeverity, and is accepted at both a 0.x
	// and a 1.x version (additive-optional at every advertised version).
	cases := []struct {
		name     string
		version  string
		severity string
	}{
		{"v0.x accepts high", "0.5", "high"},
		{"v1.x accepts low", "1.0", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := spec.ParseBytes([]byte(`
version: "` + tc.version + `"
workflows:
  feature_change:
    operator_agent:
      may_route_fixup: convergent_concerns
      route_fixup_min_severity: ` + tc.severity + `
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			wf := s.Workflows["feature_change"]
			if wf.OperatorAgent == nil {
				t.Fatal("OperatorAgent should be non-nil")
			}
			if wf.OperatorAgent.RouteFixupMinSeverity != tc.severity {
				t.Errorf("RouteFixupMinSeverity = %q, want %q", wf.OperatorAgent.RouteFixupMinSeverity, tc.severity)
			}
		})
	}
}

func TestParse_OperatorAgent_RouteFixupMinSeverity_UnknownValue_Rejected(t *testing.T) {
	// route_fixup_min_severity is a closed low/medium/high enum; an
	// out-of-enum value is refused at parse with a JSON Pointer into the
	// offending field (#1964).
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      route_fixup_min_severity: cosmetic
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(se.Path, "operator_agent/route_fixup_min_severity") {
		t.Errorf("Path = %q, want a JSON Pointer into operator_agent/route_fixup_min_severity", se.Path)
	}
}

func TestParse_OperatorAgent_ModelPolicy_Absent_Nil(t *testing.T) {
	// An operator_agent block WITHOUT model_policy (#1421) parses with a
	// nil ModelPolicy — the byte-identical-to-today absence posture. The
	// minimal fixtures have no operator_agent at all, so declare one here
	// that omits only model_policy.
	s, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      may_approve: clean_dual_approval
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.OperatorAgent == nil {
		t.Fatal("OperatorAgent should be non-nil")
	}
	if wf.OperatorAgent.ModelPolicy != nil {
		t.Errorf("ModelPolicy = %+v, want nil for an absent model_policy", wf.OperatorAgent.ModelPolicy)
	}
}

func TestParse_OperatorAgent_ModelPolicy_UnknownStrategy_Rejected(t *testing.T) {
	// strategy is a closed enum (#1421); an unknown value is refused at
	// parse with a JSON Pointer into the offending field.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      model_policy:
        strategy: vibes
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(se.Path, "model_policy/strategy") {
		t.Errorf("Path = %q, want a JSON Pointer into model_policy/strategy", se.Path)
	}
}

func TestParse_OperatorAgent_ModelPolicy_UnknownSubKey_Rejected(t *testing.T) {
	// additionalProperties:false closes the model_policy object (#1421) —
	// an unknown sub-key must never parse.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      model_policy:
        strategy: explicit_defaults
        cheapest: true
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_OperatorAgent_ModelPolicy_UnknownDefaultsKey_Rejected(t *testing.T) {
	// additionalProperties:false closes the defaults object too (#1421):
	// only plan/implement/review are addressable stages.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      model_policy:
        defaults:
          deploy: claude-opus-4-8
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_OperatorAgent_ClarificationRequestPageEvent_Accepted(t *testing.T) {
	// clarification_request (#1057) joins the closed must_page_human set:
	// the planner parking the plan stage at awaiting_input always pages the
	// human and is never absorbed by a delegation.
	if spec.PageEventClarificationRequest != "clarification_request" {
		t.Fatalf("PageEventClarificationRequest = %q, want clarification_request", spec.PageEventClarificationRequest)
	}
	s, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    operator_agent:
      must_page_human: [clarification_request]
    stages:
      - id: x
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.OperatorAgent == nil || len(wf.OperatorAgent.MustPageHuman) != 1 ||
		wf.OperatorAgent.MustPageHuman[0] != spec.PageEventClarificationRequest {
		t.Errorf("MustPageHuman = %v, want [clarification_request]", wf.OperatorAgent)
	}
}

func TestParse_OperatorAgent_ExplicitRejectClasses_Accepted(t *testing.T) {
	// #1378 / workflow-v0.7: the explicit advisory_reviewer_reject and
	// gating_reviewer_reject page-event classes join the closed
	// must_page_human set. Assert the wire-string constants first, then
	// that a 0.7 spec listing both explicit tokens parses.
	if spec.PageEventAdvisoryReviewerReject != "advisory_reviewer_reject" {
		t.Fatalf("PageEventAdvisoryReviewerReject = %q, want advisory_reviewer_reject", spec.PageEventAdvisoryReviewerReject)
	}
	if spec.PageEventGatingReviewerReject != "gating_reviewer_reject" {
		t.Fatalf("PageEventGatingReviewerReject = %q, want gating_reviewer_reject", spec.PageEventGatingReviewerReject)
	}
	s, err := spec.ParseBytes(readFixture(t, "valid/operator-agent-explicit-reject-classes.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Version != "0.7" {
		t.Errorf("version = %q, want 0.7", s.Version)
	}
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal(`workflows["feature_change"] missing`)
	}
	want := []string{spec.PageEventAdvisoryReviewerReject, spec.PageEventGatingReviewerReject}
	got := wf.OperatorAgent.MustPageHuman
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("MustPageHuman = %v, want %v", got, want)
	}
}

func TestParse_OperatorAgent_OnCheckGate_Rejected(t *testing.T) {
	// operator_agent lives on the approval branch of the gate oneOf
	// only; unevaluatedProperties rejects it on a check gate.
	_, err := spec.ParseBytes([]byte(`
version: "0.5"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor: { human: true }
        gates:
          - type: check
            operator_agent:
              may_merge: gates_resolved_ci_green
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- test_conventions (#1004) ---

func TestParse_TestConventions_RoundTrip(t *testing.T) {
	// The fixture declares two conventions (Python + Ruby). They decode
	// into Spec.TestConventions — and because ParseBytes round-trips
	// through json.DisallowUnknownFields, this only passes if the struct
	// field exists alongside the schema property (the load-bearing
	// coupling the #1004 plan calls out).
	s, err := spec.ParseBytes(readFixture(t, "valid/test-conventions.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := len(s.TestConventions), 2; got != want {
		t.Fatalf("TestConventions count = %d, want %d", got, want)
	}
	py := s.TestConventions[0]
	if py.Match != "src/**/*.py" {
		t.Errorf("TestConventions[0].Match = %q, want src/**/*.py", py.Match)
	}
	if len(py.Candidates) != 1 || py.Candidates[0] != "tests/test_{name}.py" {
		t.Errorf("TestConventions[0].Candidates = %v, want [tests/test_{name}.py]", py.Candidates)
	}
	rb := s.TestConventions[1]
	if rb.Match != "lib/**/*.rb" {
		t.Errorf("TestConventions[1].Match = %q, want lib/**/*.rb", rb.Match)
	}
	if len(rb.Candidates) != 1 || rb.Candidates[0] != "spec/{relpath}_spec.rb" {
		t.Errorf("TestConventions[1].Candidates = %v, want [spec/{relpath}_spec.rb]", rb.Candidates)
	}
}

func TestParse_TestConventions_Absent_NilSlice(t *testing.T) {
	// No test_conventions block → Spec.TestConventions is nil; the sweep
	// falls back to its built-in defaults.
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.TestConventions != nil {
		t.Errorf("TestConventions = %v, want nil for an absent block", s.TestConventions)
	}
}

func TestParse_TestConventions_MissingCandidates_Rejected(t *testing.T) {
	// candidates is required on a convention entry; its absence is a
	// schema error refused before the spec lands on a run row.
	_, err := spec.ParseBytes([]byte(`
version: "0.3"
test_conventions:
  - match: "src/**/*.py"
workflows:
  feature_change:
    stages:
      - id: x
        type: implement
        executor: { agent: claude-code }
        produces:
          - artifact: pull_request
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- Parse via io.Reader ---

func TestParse_ReaderRoundTrip(t *testing.T) {
	s, err := spec.Parse(strings.NewReader(`
version: "0.3"
workflows:
  t:
    stages:
      - id: i
        type: implement
        executor: { agent: claude-code }
        produces:
          - artifact: pull_request
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Workflows["t"].Stages[0].ID != "i" {
		t.Errorf("unexpected parse result: %+v", s)
	}
}

func TestParse_Decomposition_RoundTrip(t *testing.T) {
	// A workflow with decomposition.max_parallel decodes onto
	// Workflow.Decomposition through the real ParseBytes path
	// (DisallowUnknownFields), proving the schema + Go type stay in
	// lockstep. version 0.6 advertises the field.
	yml := []byte(`
version: "0.6"
workflows:
  feature_change:
    decomposition:
      max_parallel: 3
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.Decomposition == nil {
		t.Fatal("Decomposition = nil, want decoded block")
	}
	if got, want := wf.Decomposition.MaxParallel, 3; got != want {
		t.Errorf("Decomposition.MaxParallel = %d, want %d", got, want)
	}
}

func TestParse_Decomposition_Absent_NilPointer(t *testing.T) {
	// No decomposition block → Workflow.Decomposition is nil, so
	// EffectiveMaxParallel falls through to the global default.
	s, err := spec.ParseBytes(readFixture(t, "valid/minimal.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if wf := s.Workflows["trivial"]; wf.Decomposition != nil {
		t.Errorf("Decomposition = %v, want nil for an absent block", wf.Decomposition)
	}
}

func TestParse_Decomposition_NegativeMaxParallel_Rejected(t *testing.T) {
	// max_parallel has minimum 0 in the schema; a negative value is a
	// schema error refused before the spec lands on a run row.
	_, err := spec.ParseBytes([]byte(`
version: "0.6"
workflows:
  feature_change:
    decomposition:
      max_parallel: -1
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`))
	if err == nil {
		t.Fatal("Parse: want error for negative max_parallel, got nil")
	}
}

func TestEffectiveMaxParallel(t *testing.T) {
	knob := func(n int) *spec.Decomposition { return &spec.Decomposition{MaxParallel: n} }
	tests := []struct {
		name          string
		decomposition *spec.Decomposition
		globalDefault int
		want          int
	}{
		{
			// Per-workflow knob > 0 wins over the global default.
			name:          "knob wins over global",
			decomposition: knob(2),
			globalDefault: 9,
			want:          2,
		},
		{
			// Knob 0 (explicitly unlimited / unset) falls through to global.
			name:          "knob zero falls through to global",
			decomposition: knob(0),
			globalDefault: 5,
			want:          5,
		},
		{
			// Absent block (nil) falls through to global.
			name:          "nil block falls through to global",
			decomposition: nil,
			globalDefault: 4,
			want:          4,
		},
		{
			// Both zero → 0, the unlimited sentinel.
			name:          "both zero is unlimited",
			decomposition: knob(0),
			globalDefault: 0,
			want:          0,
		},
		{
			// Knob set with a zero global still wins.
			name:          "knob with zero global",
			decomposition: knob(7),
			globalDefault: 0,
			want:          7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := spec.Workflow{Decomposition: tt.decomposition}
			if got := wf.EffectiveMaxParallel(tt.globalDefault); got != tt.want {
				t.Errorf("EffectiveMaxParallel(%d) = %d, want %d", tt.globalDefault, got, tt.want)
			}
		})
	}
}

func TestEffectiveMaxParallel_NilReceiver(t *testing.T) {
	// A nil *Workflow degrades to the global default (defensive: the
	// resolver is called on the orchestrator's looked-up workflow).
	var wf *spec.Workflow
	if got := wf.EffectiveMaxParallel(6); got != 6 {
		t.Errorf("EffectiveMaxParallel on nil receiver = %d, want 6", got)
	}
}

// --- Version routing (ADR-046 / #1381) ---

// minimalSpecAtVersion renders the smallest valid spec body at the
// given version string, used to exercise the version-routed validator
// without coupling to a testdata fixture's frozen version.
func minimalSpecAtVersion(version string) []byte {
	return []byte("version: \"" + version + "\"\n" + `
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`)
}

// TestParse_RoutesV1Spec proves a version: "1.0" spec routes to the v1
// schema and is accepted (the v1-accepts branch).
func TestParse_RoutesV1Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("1.0"))
	if err != nil {
		t.Fatalf("ParseBytes(version 1.0): %v", err)
	}
	if s.Version != "1.0" {
		t.Errorf("version = %q, want 1.0", s.Version)
	}
}

// TestParse_RoutesV0Spec proves a version the v0 enum accepts ("0.7",
// the current latest) routes to v0 AND validates against it (the
// v0-routes branch — confirmed in the v0 enum so the pass is unambiguous).
func TestParse_RoutesV0Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("0.7"))
	if err != nil {
		t.Fatalf("ParseBytes(version 0.7): %v", err)
	}
	if s.Version != "0.7" {
		t.Errorf("version = %q, want 0.7", s.Version)
	}
}

// TestParse_UnsupportedMajorFailsClosed proves a well-formed but
// unrecognized major (2.0) fails closed with a *SchemaError naming the
// supported majors (the fail-closed-on-unknown-major branch).
func TestParse_UnsupportedMajorFailsClosed(t *testing.T) {
	_, err := spec.ParseBytes(minimalSpecAtVersion("2.0"))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	// The message must name the supported majors so an operator knows
	// what is routable.
	for _, want := range []string{"0", "1"} {
		if !strings.Contains(se.Message, want) {
			t.Errorf("message %q does not name supported major %q", se.Message, want)
		}
	}
}

// --- v1 deploy surface (E23.2 / #1382, ADR-038 / #925) ---

// v1DeploySpec is the canonical gated delegating deploy spec exercised by
// the happy-path and schema-shape tests. The deploy stage delegates to a
// github_actions workflow_dispatch, produces a deployment artifact,
// carries all three pre-flight constraint kinds, and is gated by an
// approval gate — the full type<->executor<->constraint binding in one
// spec.
const v1DeploySpec = `
version: "1.0"
roles:
  release_manager:
    members: ["@kuhlman-labs"]
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          - allowed_environments: [production]
          - change_freeze: true
          - required_upstream: [review_merged, ci_green]
        produces:
          - artifact: deployment
        gates:
          - type: approval
            approvers:
              any_of: [release_manager]
`

// TestParse_V1DeployStage_Valid drives a full version "1.0" deploy spec
// through the real ParseBytes path (version routing -> v1 JSON Schema ->
// YAML decode -> semantic Validate) and asserts every decoded member of
// the new deploy surface round-trips. This is the end-to-end / cross-layer
// test: the seam being added is the type<->executor<->constraint binding
// spread across the schema and the validator, so a single spec crossing
// all three layers is the right shape.
func TestParse_V1DeployStage_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(v1DeploySpec))
	if err != nil {
		t.Fatalf("ParseBytes(v1 deploy): %v", err)
	}
	if s.Version != "1.0" {
		t.Errorf("version = %q, want 1.0", s.Version)
	}
	st := s.Workflows["release"].Stages[0]
	if st.Type != spec.StageTypeDeploy {
		t.Errorf("stage type = %q, want deploy", st.Type)
	}
	// Delegating executor round-trips with target + workflow_ref + git_ref.
	if st.Executor.Delegate == nil {
		t.Fatal("Executor.Delegate = nil, want decoded delegate block")
	}
	d := st.Executor.Delegate
	if d.Target != spec.DelegateTargetGitHubActions {
		t.Errorf("Delegate.Target = %q, want github_actions", d.Target)
	}
	if d.WorkflowRef != "deploy.yml" {
		t.Errorf("Delegate.WorkflowRef = %q, want deploy.yml", d.WorkflowRef)
	}
	if d.GitRef != "main" {
		t.Errorf("Delegate.GitRef = %q, want main", d.GitRef)
	}
	if st.Executor.Agent != "" || st.Executor.Human {
		t.Errorf("deploy executor should carry neither agent nor human, got agent=%q human=%v", st.Executor.Agent, st.Executor.Human)
	}
	// deployment artifact round-trips.
	if len(st.Produces) != 1 || st.Produces[0].Artifact != spec.ArtifactDeployment {
		t.Errorf("Produces = %+v, want a single deployment artifact", st.Produces)
	}
	// All three pre-flight constraint kinds round-trip.
	if len(st.Constraints) != 3 {
		t.Fatalf("constraints count = %d, want 3", len(st.Constraints))
	}
	if got := st.Constraints[0].AllowedEnvironments; len(got) != 1 || got[0] != "production" {
		t.Errorf("constraints[0].AllowedEnvironments = %v, want [production]", got)
	}
	if cf := st.Constraints[1].ChangeFreeze; cf == nil || !*cf {
		t.Errorf("constraints[1].ChangeFreeze = %v, want non-nil true", cf)
	}
	if got := st.Constraints[2].RequiredUpstream; len(got) != 2 || got[0] != "review_merged" || got[1] != "ci_green" {
		t.Errorf("constraints[2].RequiredUpstream = %v, want [review_merged ci_green]", got)
	}
}

// TestParse_V1Deploy_WithoutDelegate_Rejected asserts rule (1): a deploy
// stage that uses an agent executor (schema-valid on its own) is rejected
// by the semantic validator because deploy must delegate.
func TestParse_V1Deploy_WithoutDelegate_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          agent: claude-code
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "delegating executor") {
		t.Errorf("message = %q, want it to mention the delegating-executor requirement", ve.Message)
	}
}

// TestParse_V1NonDeploy_WithDelegate_Rejected asserts rule (2): a
// non-deploy stage carrying a delegating executor is rejected.
func TestParse_V1NonDeploy_WithDelegate_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: implement
        type: implement
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "delegating executor") {
		t.Errorf("message = %q, want it to flag the delegating executor on a non-deploy stage", ve.Message)
	}
}

// TestParse_V1PreflightConstraint_OnNonDeploy_Rejected asserts rule (3),
// and specifically the binding condition's falsifying case: a non-deploy
// stage carrying a `change_freeze: false` constraint is rejected. The
// `*bool` presence model is load-bearing — a plain bool zero-value could
// not tell "present and false" from "absent" and would miss this.
func TestParse_V1PreflightConstraint_OnNonDeploy_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - change_freeze: false
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "pre-flight deploy constraint") {
		t.Errorf("message = %q, want it to flag the pre-flight constraint on a non-deploy stage", ve.Message)
	}
}

// TestParse_V1PostHocConstraint_OnDeploy_Rejected asserts rule (4): a
// deploy stage carrying a post-hoc diff constraint is rejected.
func TestParse_V1PostHocConstraint_OnDeploy_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: webhook
            url: https://example.com/deploy
        constraints:
          - max_files_changed: 5
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "post-hoc diff constraint") {
		t.Errorf("message = %q, want it to flag the post-hoc constraint on a deploy stage", ve.Message)
	}
}

// TestParse_V1DeploymentArtifact_OnNonDeploy_Rejected asserts rule (5): a
// non-deploy stage declaring the deployment artifact is rejected.
func TestParse_V1DeploymentArtifact_OnNonDeploy_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: deployment
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "deployment artifact is valid only on a deploy stage") {
		t.Errorf("message = %q, want it to flag the deployment artifact on a non-deploy stage", ve.Message)
	}
}

// TestValidate_V1Deploy_DelegateWithAgent_Rejected exercises the second
// half of rule (1) — a deploy stage that sets BOTH a delegate and an agent
// executor. The JSON Schema's executor oneOf rejects {delegate, agent}
// together, so this branch is unreachable via ParseBytes; it guards
// programmatic Spec builders, so it is driven through Validate directly.
func TestValidate_V1Deploy_DelegateWithAgent_Rejected(t *testing.T) {
	s := &spec.Spec{
		Version: "1.0",
		Workflows: map[string]spec.Workflow{
			"release": {
				Stages: []spec.Stage{
					{
						ID:   "deploy",
						Type: spec.StageTypeDeploy,
						Executor: spec.Executor{
							Agent: "claude-code",
							Delegate: &spec.DelegateConfig{
								Target:      spec.DelegateTargetGitHubActions,
								WorkflowRef: "deploy.yml",
							},
						},
					},
				},
			},
		},
	}
	err := spec.Validate(s)
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "must not use an agent or human") {
		t.Errorf("message = %q, want it to flag the agent/human executor on a deploy stage", ve.Message)
	}
}

// TestParse_V1Deploy_WebhookTarget_Valid is a schema-shape test: a deploy
// stage delegating to a webhook target (url) parses and validates.
func TestParse_V1Deploy_WebhookTarget_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: webhook
            url: https://example.com/deploy
        produces:
          - artifact: deployment
`))
	if err != nil {
		t.Fatalf("ParseBytes(webhook delegate): %v", err)
	}
	d := s.Workflows["release"].Stages[0].Executor.Delegate
	if d == nil || d.Target != spec.DelegateTargetWebhook {
		t.Fatalf("Delegate = %+v, want target webhook", d)
	}
	if d.URL != "https://example.com/deploy" {
		t.Errorf("Delegate.URL = %q, want https://example.com/deploy", d.URL)
	}
}

// TestParse_V1Deploy_GitHubActionsMissingWorkflowRef_Rejected is a
// schema-shape test: the github_actions delegate target requires
// workflow_ref, so omitting it is a *SchemaError (caught at the schema
// layer, before the semantic validator).
func TestParse_V1Deploy_GitHubActionsMissingWorkflowRef_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

// --- v1.1 acceptance surface (E31.2 / #1519, ADR-049) ---

// TestParse_V1AcceptanceStage_AgentExecutor_Valid drives a version "1.1"
// spec whose acceptance stage uses an agent executor through the real
// ParseBytes path (version routing -> v1 JSON Schema -> YAML decode ->
// semantic Validate). Acceptance is a runner-hosted advisory agent stage
// (ADR-049 #3): it rides the ordinary agent executor branch with no
// acceptance-specific binding, so the happy path is that it simply
// parses and validates.
func TestParse_V1AcceptanceStage_AgentExecutor_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`))
	if err != nil {
		t.Fatalf("ParseBytes(v1.1 acceptance, agent): %v", err)
	}
	if s.Version != "1.1" {
		t.Errorf("version = %q, want 1.1", s.Version)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if st.Type != spec.StageTypeAcceptance {
		t.Errorf("stage type = %q, want acceptance", st.Type)
	}
	if st.Executor.Agent != "claude-code" {
		t.Errorf("Executor.Agent = %q, want claude-code", st.Executor.Agent)
	}
}

// TestParse_V1AcceptanceStage_HumanExecutor_Valid asserts an acceptance
// stage may also use a human executor — acceptance is bound to the
// agent/human executor branches, never the delegating one.
func TestParse_V1AcceptanceStage_HumanExecutor_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          human: true
`))
	if err != nil {
		t.Fatalf("ParseBytes(v1.1 acceptance, human): %v", err)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if st.Type != spec.StageTypeAcceptance {
		t.Errorf("stage type = %q, want acceptance", st.Type)
	}
	if !st.Executor.Human {
		t.Error("Executor.Human = false, want true")
	}
}

// TestParse_V1Acceptance_WithDelegate_Rejected asserts the type-generic
// non-deploy executor branch fires for acceptance: a delegating executor
// is valid only on a deploy stage, so an acceptance stage carrying one is
// rejected with no acceptance-specific validator code.
func TestParse_V1Acceptance_WithDelegate_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "delegating executor") {
		t.Errorf("message = %q, want it to flag the delegating executor on a non-deploy (acceptance) stage", ve.Message)
	}
}

// TestParse_V1Acceptance_WithPreflightConstraint_Rejected asserts the
// type-generic pre-flight-constraint branch fires for acceptance: a
// pre-flight deploy constraint (change_freeze) is valid only on a deploy
// stage, so an acceptance stage carrying one is rejected.
func TestParse_V1Acceptance_WithPreflightConstraint_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        constraints:
          - allowed_environments: [production]
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "pre-flight deploy constraint") {
		t.Errorf("message = %q, want it to flag the pre-flight constraint on a non-deploy (acceptance) stage", ve.Message)
	}
}

// TestParse_V1Acceptance_WithDeploymentArtifact_Rejected asserts the
// type-generic deployment-artifact branch fires for acceptance: the
// deployment artifact is deploy-only, so an acceptance stage declaring it
// is rejected.
func TestParse_V1Acceptance_WithDeploymentArtifact_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        produces:
          - artifact: deployment
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "deployment artifact is valid only on a deploy stage") {
		t.Errorf("message = %q, want it to flag the deployment artifact on a non-deploy (acceptance) stage", ve.Message)
	}
}

// TestParse_V0Acceptance_Rejected proves the v0 enums stay frozen: a v0
// spec (version 0.7, the current latest) carrying an acceptance stage is
// rejected at the SCHEMA layer (a *SchemaError, before the semantic
// validator), because acceptance is a v1-only type.
func TestParse_V0Acceptance_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "0.7"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError (v0 must reject the acceptance type at the schema layer)", err)
	}
}

// --- v1.2 acceptance artifact (E31.3 / #1531, ADR-049) ---

// TestParse_V12AcceptanceArtifact_OnAcceptanceStage_Valid drives a version
// "1.2" spec whose acceptance stage declares the acceptance produces
// artifact through the real ParseBytes path (version routing -> v1 JSON
// Schema with the widened produces enum -> YAML decode -> semantic
// Validate). This is the spec-grammar-acceptance-artifact done-means: it
// fails if the enum, the ArtifactAcceptance constant, or the mirror sync is
// missing.
func TestParse_V12AcceptanceArtifact_OnAcceptanceStage_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "1.2"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        produces:
          - artifact: acceptance
`))
	if err != nil {
		t.Fatalf("ParseBytes(v1.2 acceptance artifact): %v", err)
	}
	if s.Version != "1.2" {
		t.Errorf("version = %q, want 1.2", s.Version)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if st.Type != spec.StageTypeAcceptance {
		t.Errorf("stage type = %q, want acceptance", st.Type)
	}
	if len(st.Produces) != 1 || st.Produces[0].Artifact != spec.ArtifactAcceptance {
		t.Errorf("Produces = %+v, want a single acceptance artifact", st.Produces)
	}
}

// TestParse_V12AcceptanceArtifact_OnImplementStage_Rejected asserts the new
// binding fires: the acceptance artifact is acceptance-stage-only, so an
// implement stage declaring it is rejected with the ADR-049 message.
func TestParse_V12AcceptanceArtifact_OnImplementStage_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: acceptance
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "acceptance artifact is valid only on an acceptance stage") {
		t.Errorf("message = %q, want it to flag the acceptance artifact on a non-acceptance (implement) stage", ve.Message)
	}
}

// TestParse_V12AcceptanceArtifact_OnDeployStage_Rejected asserts the same
// binding fires on the other non-acceptance stage type: a deploy stage
// (otherwise valid with its delegating executor) declaring the acceptance
// artifact is rejected with the ADR-049 message.
func TestParse_V12AcceptanceArtifact_OnDeployStage_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.2"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        produces:
          - artifact: acceptance
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "acceptance artifact is valid only on an acceptance stage") {
		t.Errorf("message = %q, want it to flag the acceptance artifact on a non-acceptance (deploy) stage", ve.Message)
	}
}

// TestParse_RoutesV12Spec proves a bare version "1.2" spec routes to the v1
// schema (minor is not routing-significant) and validates — the additive
// 1.2 minor-bump routing done-means.
func TestParse_RoutesV12Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("1.2"))
	if err != nil {
		t.Fatalf("ParseBytes(version 1.2): %v", err)
	}
	if s.Version != "1.2" {
		t.Errorf("version = %q, want 1.2", s.Version)
	}
}

// TestParse_RoutesV11Spec proves a bare version "1.1" spec routes to the
// v1 schema (minor is not routing-significant) and validates — the
// additive-minor-bump routing done-means.
func TestParse_RoutesV11Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("1.1"))
	if err != nil {
		t.Fatalf("ParseBytes(version 1.1): %v", err)
	}
	if s.Version != "1.1" {
		t.Errorf("version = %q, want 1.1", s.Version)
	}
}

// TestParse_V13Egress_OnAcceptanceStage_Valid asserts the v1.3 egress
// allowance (ADR-050 / #1532) parses and validates on an acceptance stage
// and that the declared hosts decode faithfully — including a host:port
// entry — through the typed StageEgress.
func TestParse_V13Egress_OnAcceptanceStage_Valid(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - staging.example.com
            - preview.internal.example.com:8443
`))
	if err != nil {
		t.Fatalf("ParseBytes(v1.3 egress): %v", err)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if st.Egress == nil {
		t.Fatal("Egress = nil, want decoded StageEgress")
	}
	want := []string{"staging.example.com", "preview.internal.example.com:8443"}
	if len(st.Egress.TargetHosts) != len(want) {
		t.Fatalf("TargetHosts = %v, want %v", st.Egress.TargetHosts, want)
	}
	for i, h := range want {
		if st.Egress.TargetHosts[i] != h {
			t.Errorf("TargetHosts[%d] = %q, want %q", i, st.Egress.TargetHosts[i], h)
		}
	}
}

// TestParse_V13Egress_OnImplementStage_Rejected asserts the new binding
// fires: the egress allowance is acceptance-stage-only, so an implement
// stage declaring it is rejected with the ADR-050 message.
func TestParse_V13Egress_OnImplementStage_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - staging.example.com
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "egress allowance is valid only on an acceptance stage") {
		t.Errorf("message = %q, want it to flag egress on a non-acceptance (implement) stage", ve.Message)
	}
}

// TestParse_V13Egress_OnDeployStage_Rejected asserts the same binding on
// the other non-acceptance stage type: a deploy stage (otherwise valid
// with its delegating executor) declaring egress is rejected.
func TestParse_V13Egress_OnDeployStage_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.3"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        egress:
          target_hosts:
            - staging.example.com
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "egress allowance is valid only on an acceptance stage") {
		t.Errorf("message = %q, want it to flag egress on a non-acceptance (deploy) stage", ve.Message)
	}
}

// TestParse_V13Egress_EmptyHosts_SchemaRejected asserts the schema floor:
// a declared egress block must carry at least one host (minItems 1) — an
// empty allowance is a contradiction, not a default-deny declaration.
func TestParse_V13Egress_EmptyHosts_SchemaRejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts: []
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError (minItems)", err)
	}
}

// TestParse_V13Egress_URLEntry_SchemaRejected asserts entries are hosts,
// not URLs: a scheme-carrying entry fails the schema pattern so egress
// declarations cannot smuggle scheme/path semantics into the allow-list.
func TestParse_V13Egress_URLEntry_SchemaRejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - https://staging.example.com
`))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError (host pattern)", err)
	}
}

// TestParse_RoutesV13Spec proves a bare version "1.3" spec routes to the
// v1 schema (minor is not routing-significant) and validates — the
// additive 1.3 minor-bump routing done-means.
func TestParse_RoutesV13Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("1.3"))
	if err != nil {
		t.Fatalf("ParseBytes(version 1.3): %v", err)
	}
	if s.Version != "1.3" {
		t.Errorf("version = %q, want 1.3", s.Version)
	}
}

// TestEmbeddedSchemaHashV1 proves the v1 hash advertised on /healthz is
// a non-empty hex string distinct from the v0 hash (the two schemas
// differ by $id/title/version enum, so their hashes must differ).
func TestEmbeddedSchemaHashV1(t *testing.T) {
	h := spec.EmbeddedSchemaHashV1()
	if h == "" {
		t.Fatal("EmbeddedSchemaHashV1() is empty")
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("EmbeddedSchemaHashV1() = %q is not hex: %v", h, err)
	}
	if h == spec.EmbeddedSchemaHash() {
		t.Error("v1 hash equals v0 hash; the structural-copy schemas must still differ by $id/title/version")
	}
}

// TestParse_AgentVersion_RoundTrip asserts a workflow-v1.4 spec declaring
// agent_version on BOTH the executor's agent branch and a reviewers.agents
// entry parses into the struct fields and passes semantic validation
// (E32.13 / #1743). The field is workflow-v1-only, so the spec is pinned at
// version "1.4".
func TestParse_AgentVersion_RoundTrip(t *testing.T) {
	yml := []byte(`
version: "1.4"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          agent_version: ">=2.1 <2.2"
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              agent_version: ">=0.30 <0.31"
            - provider: anthropic
          human: 1
`)
	s, err := spec.ParseBytes(yml)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if got := st.Executor.AgentVersion; got != ">=2.1 <2.2" {
		t.Errorf("Executor.AgentVersion = %q, want %q", got, ">=2.1 <2.2")
	}
	if st.Reviewers == nil || len(st.Reviewers.Agents) != 2 {
		t.Fatalf("Reviewers.Agents = %+v, want 2 entries", st.Reviewers)
	}
	if got := st.Reviewers.Agents[0].AgentVersion; got != ">=0.30 <0.31" {
		t.Errorf("Agents[0].AgentVersion = %q, want %q", got, ">=0.30 <0.31")
	}
	// An absent reviewer agent_version stays empty (no constraint).
	if got := st.Reviewers.Agents[1].AgentVersion; got != "" {
		t.Errorf("Agents[1].AgentVersion = %q, want empty (absent)", got)
	}

	// Re-marshal preserves the executor field; omitempty keeps the absent
	// reviewer field absent.
	out, err := yaml.Marshal(st.Executor)
	if err != nil {
		t.Fatalf("re-marshal executor: %v", err)
	}
	if !strings.Contains(string(out), "agent_version: '>=2.1 <2.2'") &&
		!strings.Contains(string(out), `agent_version: ">=2.1 <2.2"`) {
		t.Errorf("re-marshalled executor = %q, want it to preserve agent_version", out)
	}
	absent, err := yaml.Marshal(st.Reviewers.Agents[1])
	if err != nil {
		t.Fatalf("re-marshal absent reviewer: %v", err)
	}
	if strings.Contains(string(absent), "agent_version") {
		t.Errorf("re-marshalled reviewer with no agent_version = %q, want it omitted", absent)
	}
}

// TestParse_AgentVersion_ExecutorMalformedRange_Rejected asserts a malformed
// executor agent_version range — schema-valid as a plain string — is caught
// by the semantic validator (#1743).
func TestParse_AgentVersion_ExecutorMalformedRange_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.4"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_version: ">=abc"
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Path, "/executor/agent_version") {
		t.Errorf("ValidationError.Path = %q, want it to name /executor/agent_version", ve.Path)
	}
}

// TestParse_AgentVersion_ReviewerMalformedRange_Rejected asserts a malformed
// reviewer agent_version range is caught by the semantic validator (#1743).
func TestParse_AgentVersion_ReviewerMalformedRange_Rejected(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`
version: "1.4"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              agent_version: "2.1"
          human: 1
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Path, "/reviewers/agents/0/agent_version") {
		t.Errorf("ValidationError.Path = %q, want it to name the reviewer agent_version", ve.Path)
	}
}

// TestParse_RequiredOutcomes_VerificationReported pins the workflow-v1
// enum member added in v1.5 (#1886 / ADR-059) against the BACKEND's
// embedded mirror. workflow-v0 stays frozen: the same declaration under
// a 0.x version must still fail at the schema layer.
func TestParse_RequiredOutcomes_VerificationReported(t *testing.T) {
	const stages = `
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - required_outcomes:
              - verification_reported
`
	s, err := spec.ParseBytes([]byte("version: \"1.5\"\n" + stages))
	if err != nil {
		t.Fatalf("v1.5 parse: %v", err)
	}
	got := s.Workflows["feature_change"].Stages[0].Constraints
	if len(got) != 1 || len(got[0].RequiredOutcomes) != 1 ||
		got[0].RequiredOutcomes[0] != "verification_reported" {
		t.Fatalf("parsed constraints = %+v, want required_outcomes [verification_reported]", got)
	}

	// workflow-v0 is frozen — the outcome is not in its enum.
	_, err = spec.ParseBytes([]byte("version: \"0.7\"\n" + stages))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("v0 err = %v, want *SchemaError (workflow-v0 enum is frozen)", err)
	}
}

// diffCoverageStages is the v1.6 `diff_coverage` declaration the parse
// tests below share.
const diffCoverageStages = `
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:
              command: "make coverage"
              report_path: "coverage.lcov"
              format: lcov
              min_new_line_coverage: 85
              base_ref: release
`

// TestParse_DiffCoverage pins the workflow-v1 constraint kind added in
// v1.6 (#1888 / ADR-059) against the BACKEND's embedded mirror, asserting
// the parsed constraint carries the DECLARED values rather than merely a
// non-nil struct — a field wired to a zero default is exactly what a
// presence-only check cannot catch. workflow-v0 stays frozen.
func TestParse_DiffCoverage(t *testing.T) {
	s, err := spec.ParseBytes([]byte("version: \"1.6\"\n" + diffCoverageStages))
	if err != nil {
		t.Fatalf("v1.6 parse: %v", err)
	}
	got := s.Workflows["feature_change"].Stages[0].Constraints
	if len(got) != 1 || got[0].DiffCoverage == nil {
		t.Fatalf("parsed constraints = %+v, want one diff_coverage entry", got)
	}
	dc := got[0].DiffCoverage
	if dc.Command != "make coverage" {
		t.Errorf("Command = %q, want %q", dc.Command, "make coverage")
	}
	if dc.ReportPath != "coverage.lcov" {
		t.Errorf("ReportPath = %q, want %q", dc.ReportPath, "coverage.lcov")
	}
	if dc.Format != "lcov" {
		t.Errorf("Format = %q, want lcov", dc.Format)
	}
	if dc.MinNewLineCoverage != 85 {
		t.Errorf("MinNewLineCoverage = %d, want 85 (the DECLARED threshold, not a zero default)",
			dc.MinNewLineCoverage)
	}
	if dc.BaseRef != "release" {
		t.Errorf("BaseRef = %q, want release", dc.BaseRef)
	}

	// workflow-v0 is frozen — diff_coverage is not in its closed
	// constraint set (additionalProperties: false).
	_, err = spec.ParseBytes([]byte("version: \"0.7\"\n" + diffCoverageStages))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("v0 err = %v, want *SchemaError (workflow-v0 constraint set is frozen)", err)
	}
}

// TestParse_DiffCoverage_OmittedOptionalFields pins that the two optional
// fields are genuinely optional: format defaults to lcov (supplied by the
// schema, surfaced as an empty string the runner reads as lcov) and an
// omitted base_ref parses to empty, which the RUNNER resolves to the run's
// base branch.
func TestParse_DiffCoverage_OmittedOptionalFields(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 0
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dc := s.Workflows["feature_change"].Stages[0].Constraints[0].DiffCoverage
	if dc == nil {
		t.Fatal("DiffCoverage = nil, want the declared constraint")
	}
	if dc.BaseRef != "" {
		t.Errorf("BaseRef = %q, want empty (runner resolves the run's base branch)", dc.BaseRef)
	}
	if dc.MinNewLineCoverage != 0 {
		t.Errorf("MinNewLineCoverage = %d, want 0 (a declared zero is legal)", dc.MinNewLineCoverage)
	}
}

// TestParse_DiffCoverage_Rejected covers every schema- and
// validator-enforced rejection: each required field missing, the format
// enum, both ends of the 0..100 range, and the deploy-stage binding.
func TestParse_DiffCoverage_Rejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing command", `
              report_path: "coverage.lcov"
              min_new_line_coverage: 80`},
		{"missing report_path", `
              command: "make coverage"
              min_new_line_coverage: 80`},
		{"missing min_new_line_coverage", `
              command: "make coverage"
              report_path: "coverage.lcov"`},
		{"empty command", `
              command: ""
              report_path: "coverage.lcov"
              min_new_line_coverage: 80`},
		{"empty report_path", `
              command: "make coverage"
              report_path: ""
              min_new_line_coverage: 80`},
		{"unknown format", `
              command: "make coverage"
              report_path: "coverage.lcov"
              format: cobertura
              min_new_line_coverage: 80`},
		{"threshold above 100", `
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 101`},
		{"negative threshold", `
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: -1`},
		{"unknown field", `
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 80
              exclude: "vendor/**"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:` + tc.body + "\n"))
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SchemaError", err)
			}
		})
	}
}

// TestParse_DiffCoverage_RejectedOnDeployStage pins the type<->constraint
// binding: diff_coverage is a post-hoc diff constraint, so a delegating
// deploy stage — which produces no reviewable diff — rejects it exactly
// like its four siblings (ADR-038).
func TestParse_DiffCoverage_RejectedOnDeployStage(t *testing.T) {
	_, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  ship:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          - diff_coverage:
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 80
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "post-hoc diff constraint") {
		t.Errorf("ValidationError.Message = %q, want the post-hoc binding message", ve.Message)
	}
}

// TestParse_DiffCoverage_RejectedOffImplementStage pins the stage-type
// binding the runner actually implements: ONLY the implement stage measures
// diff coverage. Because an absent signal on a DECLARED constraint is by
// design a violation, a spec that declared it on, say, an acceptance or
// review stage would earn a guaranteed false category-B failure on every
// run — the false-RED this opt-in gate exists to avoid. Reject it at parse
// time instead, where the spec author can act on it.
func TestParse_DiffCoverage_RejectedOffImplementStage(t *testing.T) {
	for _, stageType := range []string{"plan", "review", "acceptance"} {
		t.Run(stageType, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: s
        type: ` + stageType + `
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 80
`))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Message, "only on an implement stage") {
				t.Errorf("ValidationError.Message = %q, want the implement-only binding message", ve.Message)
			}
			if !strings.Contains(ve.Path, "diff_coverage") {
				t.Errorf("ValidationError.Path = %q, want it to name diff_coverage", ve.Path)
			}
		})
	}
}

// TestParse_DiffCoverage_ReportPathMustStayInRepo pins the semantic check
// the JSON Schema cannot express: the runner joins report_path onto the
// checkout, so an absolute path or a `..` escape would read a file outside
// the tree the measurement claims to describe.
func TestParse_DiffCoverage_ReportPathMustStayInRepo(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "../../outside.lcov", "a/../../escape.lcov"} {
		t.Run(bad, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:
              command: "make coverage"
              report_path: "` + bad + `"
              min_new_line_coverage: 80
`))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Path, "diff_coverage/report_path") {
				t.Errorf("ValidationError.Path = %q, want it to name report_path", ve.Path)
			}
		})
	}
}

// TestParse_DiffCoverage_NoRegression is the opt-in pin: a v1 spec that
// does NOT declare the constraint parses exactly as before, with a nil
// DiffCoverage on every constraint entry.
func TestParse_DiffCoverage_NoRegression(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "1.5"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 20
          - required_outcomes:
              - tests_added_or_updated
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for i, c := range s.Workflows["feature_change"].Stages[0].Constraints {
		if c.DiffCoverage != nil {
			t.Errorf("constraint %d DiffCoverage = %+v, want nil", i, c.DiffCoverage)
		}
	}
}
