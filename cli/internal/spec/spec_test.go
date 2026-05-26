package spec_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

const validSpec = `
version: "0.3"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    description: "Default workflow."
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 30
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            sla: 4_hours
`

func TestValidateBytes_HappyPath(t *testing.T) {
	if err := spec.ValidateBytes([]byte(validSpec)); err != nil {
		t.Errorf("expected valid spec to parse, got: %v", err)
	}
}

func TestValidateBytes_EmptyDocument(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n"} {
		err := spec.ValidateBytes([]byte(in))
		var pe *spec.ParseError
		if !errors.As(err, &pe) {
			t.Errorf("ValidateBytes(%q) err = %v, want *ParseError", in, err)
		}
	}
}

func TestValidateBytes_MalformedYAML(t *testing.T) {
	// Unclosed flow sequence — yaml.v3 errors on decode.
	err := spec.ValidateBytes([]byte("key: [unclosed\n"))
	var pe *spec.ParseError
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want *ParseError", err)
	}
}

func TestValidateBytes_MissingRequiredFields(t *testing.T) {
	// Missing `version` (required at the top level).
	noVersion := strings.Replace(validSpec, `version: "0.3"`, "", 1)
	err := spec.ValidateBytes([]byte(noVersion))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	// Should mention `version` somewhere in the leaves.
	joined := strings.Join(messageStrings(ve), " ")
	if !strings.Contains(joined, "version") {
		t.Errorf("ValidationError didn't mention 'version': %s", joined)
	}
}

func TestValidateBytes_InvalidApproverPattern(t *testing.T) {
	// Approver names must match ^[a-z][a-z0-9_]*$.
	bad := strings.Replace(validSpec,
		`any_of: [tech_lead]`,
		`any_of: ["@bad/format"]`, 1)
	err := spec.ValidateBytes([]byte(bad))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
}

func TestValidateBytes_UnknownStageType(t *testing.T) {
	bad := strings.Replace(validSpec,
		`type: implement`,
		`type: bogus`, 1)
	err := spec.ValidateBytes([]byte(bad))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
}

func TestValidateBytes_MultipleLeavesReported(t *testing.T) {
	// Two distinct violations in one doc — the validator should
	// surface both, not just the first one.
	bad := strings.Replace(validSpec,
		`max_files_changed: 30`,
		`max_files_changed: -5`, 1)
	bad = strings.Replace(bad, `type: implement`, `type: bogus`, 1)
	err := spec.ValidateBytes([]byte(bad))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Errors) < 2 {
		t.Errorf("got %d leaf error(s), want >= 2:\n%s", len(ve.Errors), ve.Error())
	}
}

func TestValidationError_ErrorString(t *testing.T) {
	ve := &spec.ValidationError{Errors: []spec.ValidationErrorEntry{
		{Path: "/version", Message: "is required"},
		{Path: "/workflows", Message: "must be an object"},
	}}
	got := ve.Error()
	if !strings.Contains(got, "/version") || !strings.Contains(got, "/workflows") {
		t.Errorf("Error() = %q, want both paths included", got)
	}
}

func TestParseError_ErrorString(t *testing.T) {
	pe := &spec.ParseError{Msg: "empty document"}
	if pe.Error() != "spec: empty document" {
		t.Errorf("Error() = %q", pe.Error())
	}
}

// --- agent_self_retry (ADR-023 / #533) ---

func TestValidateBytes_AgentSelfRetry_True(t *testing.T) {
	yml := `
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
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("expected valid spec with agent_self_retry: true, got: %v", err)
	}
}

func TestValidateBytes_AgentSelfRetry_WrongType(t *testing.T) {
	yml := `
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
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
}

// TestValidateBytes_AgentSelfRetry_RejectedOnHumanExecutor pins the
// contract that agent_self_retry is only allowed inside the agent
// branch of the executor oneOf. The field is declared in the agent
// branch and the executor uses unevaluatedProperties: false, so it
// is rejected when the human branch matches. Catches a future schema
// refactor that loosens unevaluatedProperties and silently changes
// the semantic. (ADR-023.)
func TestValidateBytes_AgentSelfRetry_RejectedOnHumanExecutor(t *testing.T) {
	yml := `
version: "0.3"
workflows:
  trivial:
    stages:
      - id: review
        type: review
        executor:
          human: true
          agent_self_retry: true
        produces: []
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError (agent_self_retry must be rejected on a human executor)", err)
	}
}

func messageStrings(ve *spec.ValidationError) []string {
	out := make([]string, 0, len(ve.Errors))
	for _, e := range ve.Errors {
		out = append(out, e.Path+": "+e.Message)
	}
	return out
}
