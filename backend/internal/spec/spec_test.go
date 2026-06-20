package spec_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
