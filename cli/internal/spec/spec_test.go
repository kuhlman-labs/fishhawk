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

// --- Version routing (ADR-046 / #1381) ---

// minimalSpecAtVersion renders the smallest valid spec at the given
// version, used to exercise the version-routed validator.
func minimalSpecAtVersion(version string) string {
	return "version: \"" + version + "\"\n" + `
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
}

// TestValidateBytes_RoutesV1Spec proves a version: "1.0" spec routes to
// the v1 schema and is accepted (the v1-accepts branch).
func TestValidateBytes_RoutesV1Spec(t *testing.T) {
	if err := spec.ValidateBytes([]byte(minimalSpecAtVersion("1.0"))); err != nil {
		t.Errorf("expected v1 spec to validate, got: %v", err)
	}
}

// TestValidateBytes_RoutesV0Spec proves a version the v0 enum accepts
// ("0.7") routes to v0 and validates (the v0-routes branch).
func TestValidateBytes_RoutesV0Spec(t *testing.T) {
	if err := spec.ValidateBytes([]byte(minimalSpecAtVersion("0.7"))); err != nil {
		t.Errorf("expected v0 spec to validate, got: %v", err)
	}
}

// TestValidateBytes_V1DeploySpec_Accepted proves the cli's v1 schema
// mirror picked up the E23.2 deploy surface (#1382): a version "1.0"
// deploy spec — delegating github_actions executor, deployment artifact,
// and all three pre-flight constraint kinds — validates at the schema
// level. The CLI validates schema-only (no Go domain types / semantic
// binding), so this confirms the embedded mirror carries the new members
// and the CI schema-sync gate is satisfied.
func TestValidateBytes_V1DeploySpec_Accepted(t *testing.T) {
	yml := `
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
          - change_freeze: false
          - required_upstream: [review_merged, ci_green]
        produces:
          - artifact: deployment
        gates:
          - type: approval
            approvers:
              any_of: [release_manager]
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("expected v1 deploy spec to validate at the schema level, got: %v", err)
	}
}

// TestValidateBytes_V1AcceptanceSpec_Accepted proves the cli's v1 schema
// mirror picked up the E31.2 acceptance surface (#1519): a version "1.1"
// spec whose acceptance stage uses an agent executor validates at the
// schema level through the cli's embedded copy. This is the load-bearing
// mirror-sync + version-minor-routing done-means for the cli surface — a
// comment-only schema touch could not satisfy it (the enum member and the
// 1.1 version value must actually be present in the mirror).
func TestValidateBytes_V1AcceptanceSpec_Accepted(t *testing.T) {
	yml := `
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("expected v1.1 acceptance spec to validate at the schema level, got: %v", err)
	}
}

// TestValidateBytes_V13EgressSpec_Accepted proves the cli's embedded v1
// mirror picked up the E31.4 egress allowance (ADR-050 / #1532): a version
// "1.3" acceptance stage declaring egress.target_hosts validates at the
// schema level through the cli's embedded copy — the mirror-sync +
// version-minor done-means for the 1.3 surface (the egress $def and the
// 1.3 version value must actually be present in the mirror).
func TestValidateBytes_V13EgressSpec_Accepted(t *testing.T) {
	yml := `
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
            - staging.example.com:8443
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("expected v1.3 egress spec to validate at the schema level, got: %v", err)
	}
}

// TestValidateBytes_V0AcceptanceSpec_Rejected proves the cli's v0 mirror
// stays frozen: a v0 spec (version 0.7) carrying an acceptance stage is
// rejected at the schema layer, because acceptance is a v1-only type.
func TestValidateBytes_V0AcceptanceSpec_Rejected(t *testing.T) {
	yml := `
version: "0.7"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError (v0 must reject the acceptance type)", err)
	}
}

// TestValidateBytes_V1Deploy_GitHubActionsMissingWorkflowRef_Rejected
// proves the github_actions delegate target requires workflow_ref: the cli
// mirror's nested oneOf rejects a spec that omits it.
func TestValidateBytes_V1Deploy_GitHubActionsMissingWorkflowRef_Rejected(t *testing.T) {
	yml := `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
        produces:
          - artifact: deployment
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
}

// TestValidateBytes_UnsupportedMajorFailsClosed proves a well-formed but
// unrecognized major (2.0) fails closed with a *ValidationError naming
// the supported majors (the fail-closed-on-unknown-major branch).
func TestValidateBytes_UnsupportedMajorFailsClosed(t *testing.T) {
	err := spec.ValidateBytes([]byte(minimalSpecAtVersion("2.0")))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	for _, want := range []string{"0", "1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("error %q does not name supported major %q", joined, want)
		}
	}
}

// TestValidateBytes_AgentVersion_Valid asserts a workflow-v1.4 spec declaring
// agent_version ranges on both the executor and a reviewer passes CLI
// validation (schema + the #1743 semantic range sweep).
func TestValidateBytes_AgentVersion_Valid(t *testing.T) {
	const yml = `
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
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("expected valid agent_version spec to pass, got: %v", err)
	}
}

// TestValidateBytes_AgentVersion_ExecutorMalformed asserts the CLI's semantic
// sweep rejects a malformed executor agent_version range that the schema (a
// plain string) accepts (#1743).
func TestValidateBytes_AgentVersion_ExecutorMalformed(t *testing.T) {
	const yml = `
version: "1.4"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_version: ">=abc"
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(err.Error(), "/executor/agent_version") {
		t.Errorf("error = %q, want it to name /executor/agent_version", err.Error())
	}
}

// TestValidateBytes_AgentVersion_ReviewerMalformed asserts the CLI sweep
// rejects a malformed reviewer agent_version range (#1743).
func TestValidateBytes_AgentVersion_ReviewerMalformed(t *testing.T) {
	const yml = `
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
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(err.Error(), "/reviewers/agents/0/agent_version") {
		t.Errorf("error = %q, want it to name the reviewer agent_version", err.Error())
	}
}

// TestValidate_RequiredOutcomes_VerificationReported pins the
// workflow-v1 enum member added in v1.5 (#1886 / ADR-059) against the
// CLI's embedded mirror — the two mirrors must agree, or a spec the
// backend accepts is rejected by `fishhawk validate` (and vice versa).
// workflow-v0 stays frozen.
func TestValidate_RequiredOutcomes_VerificationReported(t *testing.T) {
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
	if err := spec.ValidateBytes([]byte("version: \"1.5\"\n" + stages)); err != nil {
		t.Fatalf("v1.5 validate: %v", err)
	}

	err := spec.ValidateBytes([]byte("version: \"0.7\"\n" + stages))
	if err == nil {
		t.Fatal("v0 validate = nil, want a rejection (workflow-v0 enum is frozen)")
	}
	if !strings.Contains(err.Error(), "required_outcomes") {
		t.Errorf("v0 error = %q, want it to name required_outcomes", err.Error())
	}
}

// TestValidate_DiffCoverage pins the workflow-v1 constraint kind added in
// v1.6 (#1888 / ADR-059) against the CLI's embedded mirror — the two
// mirrors must agree, or a spec the backend accepts is rejected by
// `fishhawk validate` (and vice versa). workflow-v0 stays frozen.
func TestValidate_DiffCoverage(t *testing.T) {
	const stages = `
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
`
	if err := spec.ValidateBytes([]byte("version: \"1.6\"\n" + stages)); err != nil {
		t.Fatalf("v1.6 validate: %v", err)
	}

	err := spec.ValidateBytes([]byte("version: \"0.7\"\n" + stages))
	if err == nil {
		t.Fatal("v0 validate = nil, want a rejection (workflow-v0 constraint set is frozen)")
	}
	if !strings.Contains(err.Error(), "diff_coverage") {
		t.Errorf("v0 error = %q, want it to name diff_coverage", err.Error())
	}
}

// TestValidate_DiffCoverage_Rejections pins the schema-enforced
// rejections against the CLI mirror too: a mirror missing the enum or the
// range would accept a spec the backend rejects.
func TestValidate_DiffCoverage_Rejections(t *testing.T) {
	cases := map[string]string{
		"unknown format": `
              command: "make coverage"
              report_path: "coverage.lcov"
              format: cobertura
              min_new_line_coverage: 80`,
		"threshold above 100": `
              command: "make coverage"
              report_path: "coverage.lcov"
              min_new_line_coverage: 101`,
		"missing command": `
              report_path: "coverage.lcov"
              min_new_line_coverage: 80`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:` + body + "\n"))
			if err == nil {
				t.Fatal("validate = nil, want a rejection")
			}
		})
	}
}
