package spec_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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
// unrecognized major (3.0, now that major 2 is routable) fails closed
// with a *SchemaError naming the supported majors (the
// fail-closed-on-unknown-major branch). Anchored on 3.0 because major 2
// left the fail-closed set with workflow-v2 (ADR-067 / #2213).
func TestParse_UnsupportedMajorFailsClosed(t *testing.T) {
	_, err := spec.ParseBytes(minimalSpecAtVersion("3.0"))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	// The message must name every supported major so an operator knows
	// what is routable — proving the list is derived from the routing
	// table (0, 1, AND 2).
	for _, want := range []string{"0", "1", "2"} {
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

// --- workflow-v2 (ADR-067 / #2213) ---

// TestParse_RoutesV2Spec proves a version: "2" spec that is otherwise a
// valid v1-grammar document routes to the v2 schema and is accepted (the
// v2-accepts branch — the embed directive + routing-table entry actually
// dispatch to v2 instead of failing closed).
func TestParse_RoutesV2Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("2"))
	if err != nil {
		t.Fatalf("ParseBytes(version 2): %v", err)
	}
	if s.Version != "2" {
		t.Errorf("version = %q, want 2", s.Version)
	}
}

// TestParse_V2RejectsUndeclaredField proves additionalProperties:false
// survived the v1->v2 copy: a version: "2" spec carrying a top-level
// field the schema does not declare is rejected with a *SchemaError
// naming the offending field. This is the "acceptance is by declaration"
// done-means.
func TestParse_V2RejectsUndeclaredField(t *testing.T) {
	yml := []byte("version: \"2\"\n" + `
bogus_undeclared_field: 1
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)
	_, err := spec.ParseBytes(yml)
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(se.Message, "bogus_undeclared_field") &&
		!strings.Contains(se.Path, "bogus_undeclared_field") {
		t.Errorf("error (path %q, message %q) does not name the offending field", se.Path, se.Message)
	}
}

// TestParse_V2RejectsMinorForm proves the collapsed enum: "2.0" routes to
// the v2 schema by major but is REJECTED by the single-token enum. This
// is the test that fails on a no-op copy — had the v1 minor chain been
// carried over, "2.0" would pass.
func TestParse_V2RejectsMinorForm(t *testing.T) {
	_, err := spec.ParseBytes(minimalSpecAtVersion("2.0"))
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError for the collapsed-enum rejection", err)
	}
	// The failure must be the version enum, not the unsupported-major
	// fail-closed path (major 2 IS routable).
	if strings.Contains(se.Message, "not recognized") {
		t.Errorf("message %q reads as an unsupported-major failure; want a version enum rejection", se.Message)
	}
}

// TestEmbeddedSchemaHashV2 proves the v2 hash accessor returns non-empty
// lowercase hex, stable across calls, and distinct from BOTH the v0 and
// v1 hashes — a copy that forgot the $id/title/version edits would
// collide with v1's hash and fail here.
func TestEmbeddedSchemaHashV2(t *testing.T) {
	h := spec.EmbeddedSchemaHashV2()
	if h == "" {
		t.Fatal("EmbeddedSchemaHashV2() is empty")
	}
	if h != strings.ToLower(h) {
		t.Errorf("EmbeddedSchemaHashV2() = %q is not lowercase", h)
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Errorf("EmbeddedSchemaHashV2() = %q is not hex: %v", h, err)
	}
	if again := spec.EmbeddedSchemaHashV2(); again != h {
		t.Errorf("EmbeddedSchemaHashV2() not stable: %q then %q", h, again)
	}
	if h == spec.EmbeddedSchemaHash() {
		t.Error("v2 hash equals v0 hash; the schemas must differ")
	}
	if h == spec.EmbeddedSchemaHashV1() {
		t.Error("v2 hash equals v1 hash; the structural-copy schemas must still differ by $id/title/version/descriptions")
	}
}

// TestParse_RoutesV16Spec is the v1 no-change pin: a version "1.6" spec
// (the current latest v1 minor) still parses to its declared version,
// asserting the new v2 routing-table entry did not perturb v1 dispatch.
func TestParse_RoutesV16Spec(t *testing.T) {
	s, err := spec.ParseBytes(minimalSpecAtVersion("1.6"))
	if err != nil {
		t.Fatalf("ParseBytes(version 1.6): %v", err)
	}
	if s.Version != "1.6" {
		t.Errorf("version = %q, want 1.6", s.Version)
	}
}

// TestParse_V2ReviewersAgentsRoundTrip pins the replacement surface for the
// removed bare count (E52.3 / #2215): a v2 reviewers block declaring
// `agents` round-trips to an effective count of len(agents), with the
// retained-for-v0/v1 Agent field left at zero — a v2 document can no longer
// populate it, so AgentCount()'s bare-count fallback is unreachable here.
func TestParse_V2ReviewersAgentsRoundTrip(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
            - provider: claudecode
          human: 2
        produces:
          - artifact: plan
            schema: standard_v1
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	rv := s.Workflows["feature_change"].Stages[0].Reviewers
	if rv == nil {
		t.Fatal("Stage.Reviewers = nil, want the declared block")
	}
	if got := rv.AgentCount(); got != 3 {
		t.Errorf("AgentCount() = %d, want 3 (len(agents))", got)
	}
	if rv.Agent != 0 {
		t.Errorf("Reviewers.Agent = %d, want 0 (the removed field is unreachable from a v2 document)", rv.Agent)
	}
	if rv.Human != 2 {
		t.Errorf("Reviewers.Human = %d, want 2", rv.Human)
	}
}

// TestParse_V2NoReviewersBlockLeavesNil pins the parse-layer half of the
// unchanged-default claim: a v2 stage with no reviewers block leaves
// Stage.Reviewers nil, exactly as on v0/v1. The RESOLVED half — what a nil
// block actually means to the consumers — is asserted end to end in
// planreview (TestResolveAuthority_V2ParsedSpec_NilReviewersBlock) and in
// delegation (TestImplementReviewAuthority_NilReviewersBlockFromV2Spec),
// because a nil check here establishes nothing about resolved behavior.
// Note the finding recorded there: no consumer materializes a literal
// {human:1} for a nil block; nil and {human:1} are OBSERVATIONALLY
// EQUIVALENT (both resolve gateless), which is why the documented default
// has never been visible.
func TestParse_V2NoReviewersBlockLeavesNil(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
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
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if rv := s.Workflows["feature_change"].Stages[0].Reviewers; rv != nil {
		t.Errorf("Stage.Reviewers = %+v, want nil for an absent reviewers block", rv)
	}
}

// frozenMajorSchemaHashes pins the canonical-JSON SHA-256 of every FROZEN
// workflow schema major — the digest the production accessors compute in
// computeSchemaHashes and /healthz advertises.
//
// These literals are DERIVED, never hand-written: each was read off a run of
// the accessor it pins, and TestFrozenMajorPinDetectsContentChange/control
// re-derives them from the shipped schema bytes so a transcription slip
// cannot pass. A mistyped constant here is red on a green tree and reads
// exactly like a genuine frozen-major regression, which is why the control
// case exists rather than trust in careful typing.
//
// v2 is deliberately ABSENT: it is the LIVE major the E52 children are still
// editing, so pinning it would be a changelog, not an invariant.
var frozenMajorSchemaHashes = map[int]string{
	0: "8573453693a052f6375cef454310e455d6ffc93f0eb650f624c58562d4d3ff73",
	1: "26848b2499c67863ef9948f669aa88bfa7aeb27165537187188e0ec82eef648c",
}

// frozenMajors is the pin table: one entry per frozen major, pairing the
// pinned digest above with the production accessor that serves it, so the
// pin and the advertised value cannot drift apart.
var frozenMajors = []struct {
	Name     string
	Major    int
	Accessor func() string
}{
	{"v0", 0, spec.EmbeddedSchemaHash},
	{"v1", 1, spec.EmbeddedSchemaHashV1},
}

// TestFrozenMajorsV0AndV1AreImmutable pins the workflow-v0 and workflow-v1
// embedded schemas by content digest. v0 and v1 are SHIPPED majors: nothing
// may edit them, and until #2320 that invariant was asserted only in prose by
// each E52 child and enforced by nothing.
//
// It replaces TestV2DivergesFromV1OnlyByLicensedDeltas (and the licensedV2Deltas
// allow-list, the normalizer and the diff walker that existed only to serve it),
// retired under the #2320 option-1 disposition. That test bought a net against
// an ACCIDENTAL drop during the v1 -> v2 COPY at #2213. The copy is long past —
// five E52 children have deliberately edited v2 — so a 15-entry table of
// intentional divergences had become a changelog rather than an invariant, and
// the guardrail in its own body said to retire it rather than keep appending.
// v0/v1 immutability, by contrast, is permanent. This swaps a decaying check
// for a lasting one at a fraction of the code.
//
// The digest is over CANONICAL JSON (decode then re-marshal, so map keys are
// sorted — see encoding/json.Marshal), which is exactly computeSchemaHashes'
// canonicalization: reformatting or reordering a frozen schema does not fire
// the pin, changing what it SAYS does.
//
// v2's own grammar keeps its direct coverage in the version-gated
// TestParseV2_* / v2removed / v2reuse / v2shape suites, which assert what v2
// SHIPS rather than how closely it still resembles a frozen ancestor.
func TestFrozenMajorsV0AndV1AreImmutable(t *testing.T) {
	for _, fm := range frozenMajors {
		t.Run(fm.Name, func(t *testing.T) {
			want, ok := frozenMajorSchemaHashes[fm.Major]
			if !ok {
				t.Fatalf("no pinned digest for frozen major %d", fm.Major)
			}
			if got := fm.Accessor(); got != want {
				t.Errorf("workflow-%s embedded schema digest = %s, want %s\n"+
					"workflow-%s is a FROZEN major (#2320); run `git diff -- docs/spec/workflow-%s.schema.json` to see what moved, "+
					"justify editing a shipped major in the PR body, then update the pin in frozenMajorSchemaHashes",
					fm.Name, got, want, fm.Name, fm.Name)
			}
		})
	}

	// The copy-pasted-constant mode: pinning one digest twice, or pinning a
	// frozen major to the LIVE v2 digest, would make every assertion above
	// vacuously green.
	t.Run("distinct", func(t *testing.T) {
		v2 := spec.EmbeddedSchemaHashV2()
		seen := make(map[string]string, len(frozenMajors)+1)
		seen[v2] = "v2 (live)"
		for _, fm := range frozenMajors {
			pinned := frozenMajorSchemaHashes[fm.Major]
			if owner, dup := seen[pinned]; dup {
				t.Errorf("pinned digest for %s equals %s (%s); each major has its own schema, so a shared digest means a copy-pasted constant",
					fm.Name, owner, pinned)
				continue
			}
			seen[pinned] = fm.Name
		}
	})
}

// TestFrozenMajorPinDetectsContentChange proves the pins above have TEETH.
// They are magic constants whose correctness compilation cannot enforce, so
// without this test a no-op edit could ship a pin that detects nothing.
//
// Each case re-derives a frozen major's digest from the package's own on-disk
// mirror through the SAME canonicalization the production accessor uses
// (json.Marshal of the decoded tree -> sha256 -> hex, mirroring
// computeSchemaHashes in parse.go), after applying a hermetic IN-MEMORY
// mutation. Nothing on disk is touched.
func TestFrozenMajorPinDetectsContentChange(t *testing.T) {
	cases := []struct {
		name string
		// mutate edits the decoded tree in place. nil is the control.
		mutate func(t *testing.T, tree map[string]any)
		// wantEqual says whether the re-derived digest must MATCH the pin.
		wantEqual bool
	}{
		{
			// A new $defs entry — the "someone grew a frozen schema" mode.
			name: "added_def",
			mutate: func(t *testing.T, tree map[string]any) {
				defs := schemaDefs(t, tree)
				defs["fishhawk_frozen_pin_probe"] = map[string]any{"type": "string"}
			},
		},
		{
			// A dropped $defs entry — the ACCIDENTAL-DROP mode the retired
			// copy-fidelity test originally bought, re-asserted where a drop
			// actually matters: inside a shipped, frozen major.
			name: "removed_def",
			mutate: func(t *testing.T, tree map[string]any) {
				defs := schemaDefs(t, tree)
				if _, ok := defs["stage"]; !ok {
					t.Fatalf("$defs/stage absent; pick another key present in every frozen major")
				}
				delete(defs, "stage")
			},
		},
		{
			// No mutation. This is what proves the pinned constants were
			// DERIVED from the shipped schema bytes rather than invented, and
			// it is the case that fires if a future edit changes a frozen
			// schema and updates only one of the pin or the schema.
			name:      "control",
			wantEqual: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, fm := range frozenMajors {
				t.Run(fm.Name, func(t *testing.T) {
					tree := decodeSchemaMirror(t, "schemas/workflow-"+fm.Name+".schema.json")
					if tc.mutate != nil {
						tc.mutate(t, tree)
					}
					got := canonicalSchemaDigest(t, tree)
					pinned := frozenMajorSchemaHashes[fm.Major]

					if !tc.wantEqual {
						if got == pinned {
							t.Errorf("re-derived digest after %s = %s, same as the pin; the pin does not detect this content change", tc.name, got)
						}
						return
					}
					if got != pinned {
						t.Errorf("re-derived digest for workflow-%s = %s, pinned %s; the pinned constant was not derived from the shipped schema bytes", fm.Name, got, pinned)
					}
					if live := fm.Accessor(); got != live {
						t.Errorf("re-derived digest for workflow-%s = %s, live accessor %s; the mirror and the embedded copy disagree", fm.Name, got, live)
					}
				})
			}
		})
	}
}

// schemaDefs returns a decoded schema's $defs map, failing the test if it is
// missing or not an object — a mutation applied to the wrong node would make
// the mutation cases pass for the wrong reason.
func schemaDefs(t *testing.T, tree map[string]any) map[string]any {
	t.Helper()
	defs, ok := tree["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("$defs missing or not an object in decoded schema")
	}
	return defs
}

// canonicalSchemaDigest re-derives a schema tree's content digest exactly as
// computeSchemaHashes in parse.go does: marshal the decoded value (which sorts
// map keys) and SHA-256 the result, hex-encoded.
func canonicalSchemaDigest(t *testing.T, tree map[string]any) string {
	t.Helper()
	canonical, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("marshal canonical schema JSON: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// decodeSchemaMirror reads and JSON-decodes an embedded schema mirror
// from the package's own schemas dir.
func decodeSchemaMirror(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema mirror %q: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode schema mirror %q: %v", path, err)
	}
	return out
}

// --- Layer A: legacy duplicate-kind constraint characterization (#2218) ------
//
// These are CHARACTERIZATION tests. They were written and run GREEN against
// unmodified HEAD BEFORE the workflow-v2 reshape (#2218 / E52.6) touched any
// production file, and their values are GOLDEN VALUES read off the code as it
// stood — not values predicted from the new design. #2218 normalizes a v2
// `constraints` OBJECT into a ONE-ELEMENT []spec.Constraint and edits ZERO
// constraint consumers, so the v0/v1 path must keep its legacy list
// representation byte-for-byte. The parsed slice is the SOLE input every
// consumer receives, so pinning it exactly — element count, per-element field
// values, and DOCUMENT ORDER — bounds the change's blast radius on the legacy
// path to nothing.
//
// The five consumer folds disagree with each other on a duplicate-kind
// document (concat, min-wins, max-wins, last-wins, first-wins), which is why
// no canonical merge rule could preserve them; see
// backend/internal/server/deploy_legacy_constraints_test.go for the two deploy
// sites' mutually inconsistent answers driven end to end.

func boolPtr(b bool) *bool { return &b }

// legacyDuplicateV1Spec duplicates SIX constraint kinds across an implement
// and a deploy stage: max_files_changed, forbidden_paths and diff_coverage on
// the implement stage; allowed_environments, change_freeze and
// required_upstream on the deploy stage.
const legacyDuplicateV1Spec = `version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
          - max_files_changed: 12
          - forbidden_paths: ["infra/**"]
          - forbidden_paths: [".github/workflows/**"]
          - diff_coverage:
              command: "make cov-a"
              report_path: "cov-a.info"
              min_new_line_coverage: 70
          - diff_coverage:
              command: "make cov-b"
              report_path: "cov-b.info"
              min_new_line_coverage: 85
        produces:
          - artifact: pull_request
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - allowed_environments: [staging]
          - allowed_environments: [prod]
          - change_freeze: true
          - change_freeze: false
          - required_upstream: [review_merged]
          - required_upstream: [ci_green]
        produces:
          - artifact: deployment
`

// legacyDuplicateV0Spec is the v0 sibling. v0 has no deploy stage type and no
// diff_coverage kind, so it duplicates the four post-hoc kinds v0 declares.
const legacyDuplicateV0Spec = `version: "0.7"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
          - max_files_changed: 12
          - forbidden_paths: ["infra/**"]
          - forbidden_paths: [".github/workflows/**"]
          - allowed_paths: ["backend/**"]
          - allowed_paths: ["cli/**"]
          - required_outcomes: [tests_added_or_updated]
          - required_outcomes: [ci_green]
        produces:
          - artifact: pull_request
`

func TestParse_LegacyDuplicateConstraintKinds_PreservedVerbatim(t *testing.T) {
	t.Run("v1", func(t *testing.T) {
		s, err := spec.ParseBytes([]byte(legacyDuplicateV1Spec))
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		stages := s.Workflows["feature_change"].Stages
		if len(stages) != 2 {
			t.Fatalf("stages = %d, want 2", len(stages))
		}

		wantImplement := []spec.Constraint{
			{MaxFilesChanged: 45},
			{MaxFilesChanged: 12},
			{ForbiddenPaths: []string{"infra/**"}},
			{ForbiddenPaths: []string{".github/workflows/**"}},
			{DiffCoverage: &spec.DiffCoverageConstraint{
				Command: "make cov-a", ReportPath: "cov-a.info", MinNewLineCoverage: 70,
			}},
			{DiffCoverage: &spec.DiffCoverageConstraint{
				Command: "make cov-b", ReportPath: "cov-b.info", MinNewLineCoverage: 85,
			}},
		}
		if got := stages[0].Constraints; !reflect.DeepEqual(got, wantImplement) {
			t.Errorf("implement constraints =\n%#v\nwant\n%#v", got, wantImplement)
		}

		wantDeploy := []spec.Constraint{
			{AllowedEnvironments: []string{"staging"}},
			{AllowedEnvironments: []string{"prod"}},
			{ChangeFreeze: boolPtr(true)},
			{ChangeFreeze: boolPtr(false)},
			{RequiredUpstream: []string{"review_merged"}},
			{RequiredUpstream: []string{"ci_green"}},
		}
		got := stages[1].Constraints
		if len(got) != len(wantDeploy) {
			t.Fatalf("deploy constraints = %d entries, want %d", len(got), len(wantDeploy))
		}
		for i := range wantDeploy {
			// ChangeFreeze is a *bool; compare the pointee so the
			// document-order assertion is on VALUES, not addresses.
			if (got[i].ChangeFreeze == nil) != (wantDeploy[i].ChangeFreeze == nil) ||
				(got[i].ChangeFreeze != nil && *got[i].ChangeFreeze != *wantDeploy[i].ChangeFreeze) {
				t.Errorf("deploy constraints[%d].ChangeFreeze mismatch: got %v", i, got[i].ChangeFreeze)
			}
			if !reflect.DeepEqual(got[i].AllowedEnvironments, wantDeploy[i].AllowedEnvironments) {
				t.Errorf("deploy constraints[%d].AllowedEnvironments = %v, want %v",
					i, got[i].AllowedEnvironments, wantDeploy[i].AllowedEnvironments)
			}
			if !reflect.DeepEqual(got[i].RequiredUpstream, wantDeploy[i].RequiredUpstream) {
				t.Errorf("deploy constraints[%d].RequiredUpstream = %v, want %v",
					i, got[i].RequiredUpstream, wantDeploy[i].RequiredUpstream)
			}
		}
	})

	t.Run("v0", func(t *testing.T) {
		s, err := spec.ParseBytes([]byte(legacyDuplicateV0Spec))
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		want := []spec.Constraint{
			{MaxFilesChanged: 45},
			{MaxFilesChanged: 12},
			{ForbiddenPaths: []string{"infra/**"}},
			{ForbiddenPaths: []string{".github/workflows/**"}},
			{AllowedPaths: []string{"backend/**"}},
			{AllowedPaths: []string{"cli/**"}},
			{RequiredOutcomes: []string{"tests_added_or_updated"}},
			{RequiredOutcomes: []string{"ci_green"}},
		}
		if got := s.Workflows["feature_change"].Stages[0].Constraints; !reflect.DeepEqual(got, want) {
			t.Errorf("v0 implement constraints =\n%#v\nwant\n%#v", got, want)
		}
	})
}

// --- Layer B: v2 ≡ v1 equivalence, single-kind (#2218, condition 1a) --------
//
// LAYER B, and deliberately NOT evidence for Layer A: both sides pass through
// the new v2 normalization, so this proves "v2 means what v1 means" and
// nothing about whether the v0/v1 path was preserved.
//
// The parsed-representation deep-equality assertion is valid HERE because the
// fixture declares a SINGLE constraint kind, which both spellings represent as
// a one-element slice. It is deliberately NOT asserted for a MULTI-kind
// fixture: a v1 document expressing several kinds must write them as a list of
// single-key maps (the v0/v1 constraint $def is maxProperties:1) and so parses
// to an N-element slice, while the v2 object denotes exactly one Constraint and
// normalizes to a ONE-element slice. That difference is expected rather than a
// defect, so the multi-kind case asserts equivalence at the ENFORCEMENT level
// instead — see TestV2MultiKindConstraints_EnforcementEquivalentToV1 in
// backend/internal/server/deploy_legacy_constraints_test.go, which drives both
// parsed specs through mergeConstraints, flattenPathConstraints,
// resolveDiffCoverageConfig, the deploy pre-flight gate and
// deployEnvironmentForRun.

const singleKindV1Spec = `version: "1.6"
workflows:
  feature_change:
    drive: true
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
        constraints:
          - max_files_changed: 45
        produces:
          - artifact: pull_request
`

const singleKindV2Spec = `version: "2"
workflows:
  feature_change:
    auto_advance: true
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan]
        constraints:
          max_files_changed: 45
        produces:
          - artifact: pull_request
`

func TestParseV2_SingleKindEquivalentToV1(t *testing.T) {
	v1, err := spec.ParseBytes([]byte(singleKindV1Spec))
	if err != nil {
		t.Fatalf("ParseBytes(v1): %v", err)
	}
	v2, err := spec.ParseBytes([]byte(singleKindV2Spec))
	if err != nil {
		t.Fatalf("ParseBytes(v2): %v", err)
	}
	if !v2.Workflows["feature_change"].Drive {
		t.Error("v2 auto_advance: true did not reach Workflow.Drive")
	}
	// Normalize ONLY the version, the one field the documents legitimately
	// differ in.
	v2.Version = v1.Version
	if !reflect.DeepEqual(v1, v2) {
		t.Errorf("parsed v2 spec differs from v1:\nv1 = %#v\nv2 = %#v", v1, v2)
	}
}

// --- E52.7 / #2219: produced-artifact constraint binding ---

// TestValidate_MixedPreflightPostHocConstraintObject_BindingOrderPinned pins
// which binding rule wins when ONE v2 constraints OBJECT declares BOTH a
// pre-flight and a post-hoc kind — a value only workflow-v2 can express, since
// E52.6 / #2218 normalizes the object into a SINGLE Constraint that trips both
// isPreflight() and isPostHoc() at once. The outcome was previously
// deterministic only by accident of loop order; this test makes it a contract,
// so inserting a rule into that loop cannot silently re-route the diagnosis.
//
// CHARACTERIZATION: cases (a) and (b) were written and run GREEN against the
// loop as it stood BEFORE the produced-artifact rule (#2219) was added, so they
// are a preservation proof, not a description of the new code.
//
// Case (c) is the interaction the new rule creates: a mixed object on a
// non-deploy stage that produces NO diff now has TWO candidate rules. The
// DELIBERATE choice is that the PRE-FLIGHT message wins — a pre-flight deploy
// constraint on a non-deploy stage is wrong regardless of what the stage
// produces, so the older and more specific diagnosis is the more useful one;
// fixing it is a prerequisite for the produced-artifact question even arising.
// The new rule is therefore positioned AFTER both ADR-038 checks, and this
// ordering is a contract, not an artifact of where the branch happened to land.
func TestValidate_MixedPreflightPostHocConstraintObject_BindingOrderPinned(t *testing.T) {
	const preflightMsg = `pre-flight deploy constraint is valid only on a deploy stage, not a "implement" stage (ADR-038)`
	const postHocMsg = "post-hoc diff constraint is not valid on a deploy stage; a delegating deploy produces no reviewable diff (ADR-038)"

	tests := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			// (a) non-deploy stage that DOES produce a diff: the
			// pre-flight-off-deploy rule wins, naming the pre-flight kind.
			name: "non_deploy_producing_pull_request",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        constraints:
          max_files_changed: 5
          allowed_environments: ["prod"]
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/wf/stages/0/constraints/allowed_environments",
			wantMsg:  preflightMsg,
		},
		{
			// (b) deploy stage: the post-hoc-on-deploy rule wins, naming the
			// post-hoc kind. Proves the deploy branch still takes precedence
			// over the produced-artifact rule, which never fires on a deploy
			// stage.
			name: "deploy_stage",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: release
        type: deploy
        executor:
          delegate:
            target: webhook
            url: https://example.com/deploy
        constraints:
          max_files_changed: 5
          allowed_environments: ["prod"]
`,
			wantPath: "/workflows/wf/stages/0/constraints/max_files_changed",
			wantMsg:  postHocMsg,
		},
		{
			// (c) non-deploy stage producing NO diff — the new rule's own
			// territory. The pre-flight message wins by deliberate ordering
			// (see the doc comment).
			name: "non_deploy_producing_no_diff",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        constraints:
          max_files_changed: 5
          allowed_environments: ["prod"]
`,
			wantPath: "/workflows/wf/stages/0/constraints/allowed_environments",
			wantMsg:  preflightMsg,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tt.doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Path != tt.wantPath {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, tt.wantPath)
			}
			if ve.Message != tt.wantMsg {
				t.Errorf("ValidationError.Message = %q, want %q", ve.Message, tt.wantMsg)
			}
		})
	}
}

// v2NonDiffStage renders a workflow-v2 document whose single stage carries the
// given constraints object and, optionally, a produces list. It is the shared
// fixture for the produced-artifact binding: the stage type is a parameter
// because the binding must NOT read it.
func v2ConstraintStage(stageType, constraints, produces string) []byte {
	doc := `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: ` + stageType + `
        executor:
          agent: claude-code
        constraints:
` + constraints
	if produces != "" {
		doc += `        produces:
` + produces
	}
	return []byte(doc)
}

// TestValidate_V2PostHocConstraintOffDiffProducingStage_Rejected is the AC1
// done-means test: at workflow-v2 EVERY post-hoc diff constraint kind is
// rejected on a stage that declares no pull_request artifact, and the error
// names THAT kind in both the path (/constraints/<kind>) and the message.
//
// One case per kind rather than one representative: the reported kind comes
// from Constraint.postHocKindName's fixed switch, so a kind dropped from that
// switch — or from isPostHoc — would leave a hole a single-kind test misses.
func TestValidate_V2PostHocConstraintOffDiffProducingStage_Rejected(t *testing.T) {
	kinds := []struct {
		kind        string
		constraints string
	}{
		{"max_files_changed", "          max_files_changed: 5\n"},
		{"forbidden_paths", "          forbidden_paths: [\"infra/**\"]\n"},
		{"allowed_paths", "          allowed_paths: [\"docs/**\"]\n"},
		{"required_outcomes", "          required_outcomes: [\"ci_green\"]\n"},
		{"diff_coverage", "          diff_coverage:\n            command: \"make cov\"\n            report_path: \"cov.info\"\n            min_new_line_coverage: 80\n"},
	}
	for _, k := range kinds {
		t.Run(k.kind, func(t *testing.T) {
			// type implement so the #1888 implement-only rule cannot be the
			// one firing for the diff_coverage case — the produced-artifact
			// rule is the only candidate.
			_, err := spec.ParseBytes(v2ConstraintStage("implement", k.constraints, ""))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			wantPath := "/workflows/wf/stages/0/constraints/" + k.kind
			if ve.Path != wantPath {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, wantPath)
			}
			for _, want := range []string{
				`post-hoc diff constraint "` + k.kind + `"`,
				"valid only on a stage that produces a diff",
				`stage "apply" declares no pull_request artifact (ADR-067)`,
				"Declare produces: [{artifact: pull_request}] on this stage, or remove the constraint.",
			} {
				if !strings.Contains(ve.Message, want) {
					t.Errorf("ValidationError.Message = %q, want it to contain %q", ve.Message, want)
				}
			}
		})
	}
}

// TestValidate_V2PostHocConstraint_NonEmptyProducesWithoutPullRequest_Rejected
// covers producesDiff's other false path: a produces list that is PRESENT and
// non-empty but carries no pull_request entry. The rejection table above
// exercises the absent-list path; this one walks the loop body and finds no
// match, which is the case a `len(Produces) == 0` shortcut would have missed.
func TestValidate_V2PostHocConstraint_NonEmptyProducesWithoutPullRequest_Rejected(t *testing.T) {
	doc := v2ConstraintStage("plan",
		"          max_files_changed: 5\n",
		"          - artifact: plan\n            schema: standard_v1\n")
	_, err := spec.ParseBytes(doc)
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "declares no pull_request artifact") {
		t.Errorf("ValidationError.Message = %q, want the produced-artifact message", ve.Message)
	}
}

// TestValidate_V2PostHocConstraintOnDiffProducingStage_Accepted is the
// over-breadth guard: the SAME constraints on a stage that declares the
// pull_request artifact parse clean. Without this, a rule that rejected every
// post-hoc constraint at v2 would pass the rejection table above.
func TestValidate_V2PostHocConstraintOnDiffProducingStage_Accepted(t *testing.T) {
	doc := v2ConstraintStage("implement",
		"          max_files_changed: 5\n          forbidden_paths: [\"infra/**\"]\n",
		"          - artifact: pull_request\n")
	if _, err := spec.ParseBytes(doc); err != nil {
		t.Fatalf("ParseBytes: %v, want the constraint accepted on a diff-producing stage", err)
	}
}

// TestValidate_V2ExistingBindingMessagesUnchanged is the AC2 regression block:
// all five pre-existing bindings still fire at version "2" with their message
// text UNCHANGED. The deploy case is the load-bearing one — it proves the
// ADR-038 post-hoc branch still wins on a deploy stage and the new
// produced-artifact message never reaches it (a deploy stage declares no
// pull_request artifact either, so an unguarded new rule would shadow it).
func TestValidate_V2ExistingBindingMessagesUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			name: "preflight_off_deploy",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        constraints:
          allowed_environments: ["prod"]
`,
			wantPath: "/workflows/wf/stages/0/constraints/allowed_environments",
			wantMsg:  `pre-flight deploy constraint is valid only on a deploy stage, not a "implement" stage (ADR-038)`,
		},
		{
			name: "posthoc_on_deploy",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: release
        type: deploy
        executor:
          delegate:
            target: webhook
            url: https://example.com/deploy
        constraints:
          max_files_changed: 5
`,
			wantPath: "/workflows/wf/stages/0/constraints/max_files_changed",
			wantMsg:  "post-hoc diff constraint is not valid on a deploy stage; a delegating deploy produces no reviewable diff (ADR-038)",
		},
		{
			name: "deployment_artifact_off_deploy",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: deployment
`,
			wantPath: "/workflows/wf/stages/0/produces/0/artifact",
			wantMsg:  `deployment artifact is valid only on a deploy stage, not a "implement" stage (ADR-038)`,
		},
		{
			name: "acceptance_artifact_off_acceptance",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: acceptance
`,
			wantPath: "/workflows/wf/stages/0/produces/0/artifact",
			wantMsg:  `acceptance artifact is valid only on an acceptance stage, not a "implement" stage (ADR-049)`,
		},
		{
			name: "egress_off_acceptance",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        egress:
          target_hosts: ["staging.example.com"]
`,
			wantPath: "/workflows/wf/stages/0/egress",
			wantMsg:  `egress allowance is valid only on an acceptance stage, not a "implement" stage (ADR-050)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, tc.wantPath)
			}
			if ve.Message != tc.wantMsg {
				t.Errorf("ValidationError.Message = %q, want the UNCHANGED %q", ve.Message, tc.wantMsg)
			}
		})
	}
}

// TestValidate_ProducedArtifactBinding_NotAppliedBelowMajor2 is the AC3 pin
// for the VERSION GATE. v0/v1 documents legitimately declare post-hoc
// constraints on a stage with no `produces` list at all, so applying the
// artifact-keyed rule below major 2 would newly reject valid specs. The two
// sub-cases are the two majors that must stay type-keyed.
func TestValidate_ProducedArtifactBinding_NotAppliedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(`version: "` + version + `"
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
				t.Fatalf("ParseBytes(version %s): %v, want a post-hoc constraint with no produces list to stay VALID below major 2", version, err)
			}
		})
	}
}

// TestValidate_V2FeatureChangeWithAllPostHocKinds_ValidatesClean is the other
// half of AC3: the shape every real workflow already uses — a plan -> implement
// -> review -> acceptance document whose implement stage declares
// pull_request alongside all four v0 post-hoc kinds — is unaffected. This is
// the assertion that would fail if the "every in-repo workflow already declares
// produces: pull_request" premise were wrong.
func TestValidate_V2FeatureChangeWithAllPostHocKinds_ValidatesClean(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    auto_advance: true
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan]
        constraints:
          max_files_changed: 45
          forbidden_paths: [".github/workflows/**"]
          allowed_paths: ["backend/**", "docs/**"]
          required_outcomes: ["tests_added_or_updated", "ci_green"]
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        produces:
          - artifact: acceptance
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v, want a diff-producing v2 feature_change to validate clean", err)
	}
	if got := len(s.Workflows["feature_change"].Stages); got != 4 {
		t.Errorf("stages = %d, want 4", got)
	}
}

// TestValidate_V2DiffCoverageOrdering_NeitherRuleShadowsTheOther pins the new
// rule's position against the #1888 diff_coverage block in BOTH directions.
// Only asserting one direction would leave the other rule silently
// unreachable at v2.
func TestValidate_V2DiffCoverageOrdering_NeitherRuleShadowsTheOther(t *testing.T) {
	const diffCov = "          diff_coverage:\n            command: \"make cov\"\n            report_path: \"cov.info\"\n            min_new_line_coverage: 80\n"

	// A non-implement stage producing NO diff: the new, more general
	// produced-artifact message wins. Retyping the stage to `implement` would
	// NOT fix it, which is why the general diagnosis is the useful one here.
	t.Run("no_diff_produced_yields_new_message", func(t *testing.T) {
		_, err := spec.ParseBytes(v2ConstraintStage("review", diffCov, ""))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError", err)
		}
		if ve.Path != "/workflows/wf/stages/0/constraints/diff_coverage" {
			t.Errorf("ValidationError.Path = %q, want the v2 kind form", ve.Path)
		}
		if !strings.Contains(ve.Message, "valid only on a stage that produces a diff") {
			t.Errorf("ValidationError.Message = %q, want the produced-artifact message", ve.Message)
		}
	})

	// A stage that DOES produce pull_request but is typed other than
	// implement: the pre-existing #1888 implement-only message still fires,
	// verbatim. This is the direction that proves the new rule did not make
	// the older one unreachable at v2.
	t.Run("diff_produced_wrong_type_yields_1888_message", func(t *testing.T) {
		_, err := spec.ParseBytes(v2ConstraintStage("review", diffCov, "          - artifact: pull_request\n"))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError", err)
		}
		if ve.Path != "/workflows/wf/stages/0/constraints/diff_coverage" {
			t.Errorf("ValidationError.Path = %q, want the v2 kind form", ve.Path)
		}
		const want = `diff_coverage is valid only on an implement stage, not a "review" stage (#1888): the measurement is emitted by the implement runner, and a declared constraint with no measurement is a violation`
		if ve.Message != want {
			t.Errorf("ValidationError.Message = %q, want the UNCHANGED %q", ve.Message, want)
		}
	})
}

// groomingExampleRelPath is the AC4 fixture, read from disk so the test
// exercises the SHIPPED example rather than a copy that can drift from it.
const groomingExampleRelPath = "../../../docs/spec/examples/workflow-v2-backlog-grooming.yaml"

// TestParseV2_BacklogGroomingExample_ValidatesAndProducesNoDiff is the AC4
// cross-boundary test. It drives the real published example through the whole
// chain — YAML -> version routing -> the v2removed sweep -> schema validation
// -> normalizeV2Shapes -> typed decode -> Validate — proving a workflow built
// on plan/implement/review that produces NO code diff is valid at v2.
//
// It also asserts that no stage declares the pull_request artifact. Without
// that guard the example could silently acquire one in a later edit and stop
// exercising the generalization while still passing.
func TestParseV2_BacklogGroomingExample_ValidatesAndProducesNoDiff(t *testing.T) {
	raw, err := os.ReadFile(groomingExampleRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", groomingExampleRelPath, err)
	}
	s, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", groomingExampleRelPath, err)
	}
	if s.Version != "2" {
		t.Errorf("version = %q, want 2", s.Version)
	}
	wf, ok := s.Workflows["backlog_grooming"]
	if !ok {
		t.Fatalf("workflows = %v, want a backlog_grooming workflow", s.Workflows)
	}
	if len(wf.Stages) == 0 {
		t.Fatal("backlog_grooming declares no stages")
	}
	for _, stage := range wf.Stages {
		for _, p := range stage.Produces {
			if p.Artifact == spec.ArtifactPullRequest {
				t.Errorf("stage %q declares the pull_request artifact; the example must stay a NON-code-change workflow or it stops exercising the produced-artifact binding", stage.ID)
			}
		}
	}
}

// --- E52.5 / #2217: workflow-v2 budget-unit unification ----------------------
//
// The two tests below are the HEAD-GREEN PRESERVATION BASELINE (approval
// condition 1). They assert ONLY behaviour that exists at unmodified HEAD —
// that a v0/v1 stage budget still parses and that its decoded MaxTokens,
// MaxRuntimeMinutes and Enforcement are carried verbatim — so they compile and
// pass BEFORE any production edit in this slice. Every Budget.Runtime()
// assertion (the new accessor) lives in TestParseV2_BudgetUnitsRoundTrip /
// TestRuntimePrecedence / TestParseV2_AllDurationFieldsShareOneForm, added
// after the accessor exists and making no green-before claim.
//
// The change under #2217 touches only the workflow-v2 GRAMMAR — no runtime
// check reads Stage.Budget on any major — so these v0/v1 documents must keep
// parsing byte-identically. Running them green against HEAD first is what makes
// a later green run a preservation PROOF rather than a fresh assertion.

// TestParseBytes_V0V1StageBudgetUnchanged pins that a v0.7 and a v1.6 stage
// budget declaring the legacy integer-minutes form still parse and decode
// their fields verbatim. HEAD-green: no Runtime() call.
func TestParseBytes_V0V1StageBudgetUnchanged(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run("budget at "+version, func(t *testing.T) {
			s, err := spec.ParseBytes([]byte(`version: "` + version + `"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          max_tokens: 200000
          max_runtime_minutes: 15
          enforcement: advisory
        produces:
          - artifact: pull_request
`))
			if err != nil {
				t.Fatalf("ParseBytes(version %s): %v", version, err)
			}
			b := s.Workflows["feature_change"].Stages[0].Budget
			if b == nil {
				t.Fatal("Stage.Budget = nil, want the declared block")
			}
			if b.MaxTokens != 200000 {
				t.Errorf("MaxTokens = %d, want 200000", b.MaxTokens)
			}
			if b.MaxRuntimeMinutes != 15 {
				t.Errorf("MaxRuntimeMinutes = %d, want 15", b.MaxRuntimeMinutes)
			}
			if b.Enforcement != spec.EnforcementAdvisory {
				t.Errorf("Enforcement = %q, want %q", b.Enforcement, spec.EnforcementAdvisory)
			}
		})
	}
}

// shippedBudgetDocs are the repo's own v0.x/v1.x specs that declare stage
// budgets — the exact documents #2217's operator identified as at risk. All
// declare max_tokens on their agent stages; none declares the v2 spellings.
var shippedBudgetDocs = []struct {
	name string
	path string
}{
	{"workflows.yaml", "../../../.fishhawk/workflows.yaml"},
}

// TestParseBytes_ShippedSpecsAndPresetsStillParse pins that this repo's own
// version-1.3 .fishhawk/workflows.yaml and the three embedded presets still
// parse with their stage budgets decoded (MaxTokens / MaxRuntimeMinutes
// populated). HEAD-green: no Runtime() call.
func TestParseBytes_ShippedSpecsAndPresetsStillParse(t *testing.T) {
	assertBudgetsDecoded := func(t *testing.T, name string, raw []byte) {
		t.Helper()
		s, err := spec.ParseBytes(raw)
		if err != nil {
			t.Fatalf("ParseBytes(%s): %v", name, err)
		}
		budgets := 0
		for _, wf := range s.Workflows {
			for _, st := range wf.Stages {
				if st.Budget == nil {
					continue
				}
				budgets++
				if st.Budget.MaxTokens < 1 {
					t.Errorf("%s stage %q: MaxTokens = %d, want the declared value decoded (>=1)", name, st.ID, st.Budget.MaxTokens)
				}
				if st.Budget.MaxRuntimeMinutes < 1 {
					t.Errorf("%s stage %q: MaxRuntimeMinutes = %d, want the declared value decoded (>=1)", name, st.ID, st.Budget.MaxRuntimeMinutes)
				}
			}
		}
		if budgets == 0 {
			t.Errorf("%s: no stage budgets decoded; the preservation test asserts nothing", name)
		}
	}

	for _, doc := range shippedBudgetDocs {
		t.Run(doc.name, func(t *testing.T) {
			raw, err := os.ReadFile(doc.path)
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			assertBudgetsDecoded(t, doc.name, raw)
		})
	}
	for _, p := range []spec.Preset{spec.PresetLow, spec.PresetMedium, spec.PresetHigh} {
		t.Run("preset "+string(p), func(t *testing.T) {
			raw, err := spec.PresetBytes(p)
			if err != nil {
				t.Fatalf("PresetBytes(%q): %v", p, err)
			}
			assertBudgetsDecoded(t, "preset "+string(p), raw)
		})
	}
}

// TestParseV2_BudgetUnitsRoundTrip is the DONE-MEANS behavioural test: a v2
// stage budget spelling limit_usd, the Go-duration max_runtime and max_tokens
// parses and decodes each field, with Runtime() resolving the duration. The
// 90s value is chosen deliberately — it is INEXPRESSIBLE in the old
// integer-minutes form, so a green result proves the parser genuinely changed
// rather than merely still working.
func TestParseV2_BudgetUnitsRoundTrip(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          limit_usd: 8.5
          max_runtime: 90s
          max_tokens: 200000
          enforcement: advisory
        produces:
          - artifact: pull_request
`))
	if err != nil {
		t.Fatalf("ParseBytes(v2 budget): %v", err)
	}
	b := s.Workflows["feature_change"].Stages[0].Budget
	if b == nil {
		t.Fatal("Stage.Budget = nil, want the declared block")
	}
	if b.LimitUSD != 8.5 {
		t.Errorf("LimitUSD = %v, want 8.5", b.LimitUSD)
	}
	if b.MaxRuntime.Duration != 90*time.Second {
		t.Errorf("MaxRuntime = %v, want 90s", b.MaxRuntime.Duration)
	}
	if b.MaxTokens != 200000 {
		t.Errorf("MaxTokens = %d, want 200000", b.MaxTokens)
	}
	if b.Runtime() != 90*time.Second {
		t.Errorf("Runtime() = %v, want 90s", b.Runtime())
	}
	if b.Enforcement != spec.EnforcementAdvisory {
		t.Errorf("Enforcement = %q, want %q", b.Enforcement, spec.EnforcementAdvisory)
	}
	// The v2 spelling must NOT populate the legacy minutes field.
	if b.MaxRuntimeMinutes != 0 {
		t.Errorf("MaxRuntimeMinutes = %d, want 0 — a v2 document spells the runtime cap as max_runtime", b.MaxRuntimeMinutes)
	}
}

// TestParseV2_AllDurationFieldsShareOneForm is approval CONDITION 2: acceptance
// criterion 1 is a CROSS-FIELD claim, so ONE v2 document declares all four
// duration surfaces — policy.max_stage_runtime, executor.timeout,
// executor.verify.timeout and budget.max_runtime — with DISTINCT values, and
// each must decode to its exact time.Duration through the one
// time.ParseDuration code path. budget.max_runtime keeps a sub-minute value,
// inexpressible in the integer-minutes form, so it proves the parser changed.
func TestParseV2_AllDurationFieldsShareOneForm(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "2"
workflows:
  feature_change:
    policy:
      max_stage_runtime: 30m
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          timeout: 1h
          verify:
            command: "make test"
            timeout: 10m
        budget:
          max_runtime: 90s
        produces:
          - artifact: pull_request
`))
	if err != nil {
		t.Fatalf("ParseBytes(all duration fields): %v", err)
	}
	wf := s.Workflows["feature_change"]
	if wf.Policy == nil || wf.Policy.MaxStageRuntime.Duration != 30*time.Minute {
		t.Errorf("policy.max_stage_runtime = %v, want 30m", wf.Policy)
	}
	st := wf.Stages[0]
	if st.Executor.Timeout.Duration != time.Hour {
		t.Errorf("executor.timeout = %v, want 1h", st.Executor.Timeout.Duration)
	}
	if st.Executor.Verify == nil || st.Executor.Verify.Timeout.Duration != 10*time.Minute {
		t.Errorf("executor.verify.timeout = %v, want 10m", st.Executor.Verify)
	}
	if st.Budget == nil || st.Budget.MaxRuntime.Duration != 90*time.Second {
		t.Errorf("budget.max_runtime = %v, want 90s", st.Budget)
	}
	if st.Budget.Runtime() != 90*time.Second {
		t.Errorf("budget.Runtime() = %v, want 90s", st.Budget.Runtime())
	}
}

// TestRuntimePrecedence pins Budget.Runtime()'s resolution order: the v2
// Go-duration MaxRuntime wins when set, else the legacy minutes convert, else
// zero means unset.
func TestRuntimePrecedence(t *testing.T) {
	cases := []struct {
		name string
		b    spec.Budget
		want time.Duration
	}{
		{"max_runtime wins over minutes", spec.Budget{MaxRuntime: spec.Duration{Duration: 90 * time.Second}, MaxRuntimeMinutes: 5}, 90 * time.Second},
		{"minutes-only converts", spec.Budget{MaxRuntimeMinutes: 15}, 15 * time.Minute},
		{"neither set is zero", spec.Budget{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.Runtime(); got != tc.want {
				t.Errorf("Runtime() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseV2_BudgetAC4PrimaryNotRequired pins the AC-4 decision behaviourally:
// a v2 budget declaring ONLY max_tokens is valid, and one declaring ONLY
// limit_usd is valid — limit_usd is primary but NOT required.
func TestParseV2_BudgetAC4PrimaryNotRequired(t *testing.T) {
	docFor := func(budget string) string {
		return `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          ` + budget + `
        produces:
          - artifact: pull_request
`
	}
	for _, budget := range []string{"max_tokens: 200000", "limit_usd: 8.5"} {
		t.Run(budget, func(t *testing.T) {
			if _, err := spec.ParseBytes([]byte(docFor(budget))); err != nil {
				t.Errorf("ParseBytes(budget only %s) = %v, want nil", budget, err)
			}
		})
	}
}

// TestParseV2_BudgetMaxTokensDescriptionStatesSecondaryLever is approval
// CONDITION / AC-4: the choice must be explicit IN the shipped schema text, so
// the assertion reads the embedded mirror rather than trusting behaviour alone.
func TestParseV2_BudgetMaxTokensDescriptionStatesSecondaryLever(t *testing.T) {
	v2 := decodeSchemaMirror(t, "schemas/workflow-v2.schema.json")
	defs, _ := v2["$defs"].(map[string]any)
	budget, _ := defs["budget"].(map[string]any)
	props, _ := budget["properties"].(map[string]any)
	maxTokens, _ := props["max_tokens"].(map[string]any)
	desc, _ := maxTokens["description"].(string)
	if !strings.Contains(desc, "SECONDARY") {
		t.Errorf("max_tokens.description = %q, want it to state the OPTIONAL SECONDARY-lever decision", desc)
	}
	if !strings.Contains(desc, "limit_usd") {
		t.Errorf("max_tokens.description = %q, want it to name limit_usd as the primary lever", desc)
	}
}

// TestParseV2_BudgetMaxRuntimeRejectsNonDuration proves the schema pattern
// rejects a max_runtime that is not a Go duration string — a bare integer and
// a spaced form time.ParseDuration would reject.
func TestParseV2_BudgetMaxRuntimeRejectsNonDuration(t *testing.T) {
	for _, bad := range []string{`"15"`, `"30 minutes"`} {
		t.Run(bad, func(t *testing.T) {
			doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          max_runtime: ` + bad + `
        produces:
          - artifact: pull_request
`
			if _, err := spec.ParseBytes([]byte(doc)); err == nil {
				t.Errorf("ParseBytes(max_runtime %s) = nil, want the schema pattern rejection", bad)
			}
		})
	}
}

// TestParseV2_BudgetSpellingsDoNotLeakBelowMajor2 proves the v2 spellings are
// partitioned by major: a v0/v1 stage budget declaring max_runtime or limit_usd
// is rejected by those schemas' additionalProperties:false.
func TestParseV2_BudgetSpellingsDoNotLeakBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		for _, field := range []string{"max_runtime: 90s", "limit_usd: 8.5"} {
			t.Run(version+"/"+field, func(t *testing.T) {
				doc := `version: "` + version + `"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          ` + field + `
        produces:
          - artifact: pull_request
`
				if _, err := spec.ParseBytes([]byte(doc)); err == nil {
					t.Errorf("ParseBytes(version %s, %s) = nil, want rejection by additionalProperties:false", version, field)
				}
			})
		}
	}
}

// --- workflow-v2 same-document reuse (E52.4 / #2216) -----------------------

// TestParseV2_DeclaredReviewersBlockTakenWhole is the governance-critical
// direction of the block-level reviewers rule, and it lives in the EXTERNAL
// test package because it asserts the AUTHORITY, not just the field:
// planreview imports spec, so only spec_test can import it back.
//
// The hazard a key-wise merge would create: file defaults declaring
// {human: 1, agents: [a, b]} merged into a stage declaring {agents: [c]}
// resolves to {human: 1, agents: [c]}. The stage's agents correctly replace
// the default's, but `human: 1` is SUPPLEMENTED from a block the author never
// wrote on that stage — and planreview.ResolveAuthority keys on
// AgentCount() > 0 && Human == 0, so the supplemented human silently converts
// a GATING review into an arbitrable ADVISORY one. Nothing in the document
// the author wrote would say so.
//
// So a declared `reviewers` block is taken WHOLE from exactly one rung:
// declared or inherited, never blended. The authority is what an operator is
// governed by; the field is only the mechanism, which is why both are
// asserted and the authority is the point.
func TestParseV2_DeclaredReviewersBlockTakenWhole(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
  reviewers:
    human: 1
    agents:
      - provider: claudecode
      - provider: anthropic
workflows:
  wf:
    stages:
      - id: propose
        type: plan
        reviewers:
          agents:
            - provider: codex
`
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	rev := s.Workflows["wf"].Stages[0].Reviewers
	if rev == nil {
		t.Fatal("propose.reviewers = nil, want the authored block")
	}
	if rev.Human != 0 {
		t.Errorf("propose.reviewers.human = %d, want 0 — NO human key supplemented from the file default", rev.Human)
	}
	if got := rev.AgentCount(); got != 1 {
		t.Fatalf("propose.reviewers agent count = %d (%+v), want exactly the one authored agent", got, rev.Agents)
	}
	if got := string(rev.Agents[0].Provider); got != "codex" {
		t.Errorf("propose.reviewers.agents[0].provider = %q, want codex", got)
	}
	// THE ASSERTION THAT MATTERS: the review authority the operator is
	// governed by. A key-wise regression flips this to AuthorityAdvisory.
	if got := planreview.ResolveAuthority(*rev); got != planreview.AuthorityGating {
		t.Errorf("planreview.ResolveAuthority = %v, want %v — a supplemented human would silently make this gating stage advisory",
			got, planreview.AuthorityGating)
	}
}

// TestParseV2_DefaultsCarryingDocumentDecodesWithNoStructChange proves the
// reuse keys are a GRAMMAR-only addition: a document declaring `defaults` and
// `extends` decodes into the unchanged Spec / Workflow / Stage structs, under
// DisallowUnknownFields, with no field added for either key.
func TestParseV2_DefaultsCarryingDocumentDecodesWithNoStructChange(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 20m
workflows:
  base:
    stages:
      - id: propose
        type: plan
  derived:
    extends: base
    defaults:
      executor:
        timeout: 40m
`
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	st := s.Workflows["derived"].Stages[0]
	if st.Executor.Agent != "claude-code" {
		t.Errorf("executor.agent = %q, want the file default claude-code", st.Executor.Agent)
	}
	if got := st.Executor.Timeout.Duration; got != 40*time.Minute {
		t.Errorf("executor.timeout = %v, want the workflow default 40m over the file default 20m", got)
	}
}
