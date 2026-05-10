package spec_test

import (
	"errors"
	"os"
	"strings"
	"testing"

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
	if s.Version != "0.2" {
		t.Errorf("version = %q, want 0.2", s.Version)
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
version: "0.2"
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
		Version: "0.2",
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

// --- Parse via io.Reader ---

func TestParse_ReaderRoundTrip(t *testing.T) {
	s, err := spec.Parse(strings.NewReader(`
version: "0.2"
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
