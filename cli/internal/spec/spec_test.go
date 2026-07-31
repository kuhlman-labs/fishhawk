package spec_test

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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
	// Anchored on 3.0 because major 2 left the fail-closed set with
	// workflow-v2 (ADR-067 / #2213).
	err := spec.ValidateBytes([]byte(minimalSpecAtVersion("3.0")))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	for _, want := range []string{"0", "1", "2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("error %q does not name supported major %q", joined, want)
		}
	}
}

// TestValidateBytes_RoutesV2Spec proves a version: "2" spec routes to the
// cli's embedded v2 schema mirror and validates (the v2-accepts branch —
// the embed directive + routing-table entry dispatch to v2).
func TestValidateBytes_RoutesV2Spec(t *testing.T) {
	if err := spec.ValidateBytes([]byte(minimalSpecAtVersion("2"))); err != nil {
		t.Errorf("expected v2 spec to validate, got: %v", err)
	}
}

// TestValidateBytes_V2RejectsUndeclaredField proves additionalProperties:
// false survived the v1->v2 copy in the cli mirror: a version: "2" spec
// carrying an undeclared top-level field is rejected naming the field.
func TestValidateBytes_V2RejectsUndeclaredField(t *testing.T) {
	yml := "version: \"2\"\n" + `
bogus_undeclared_field: 1
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	if !strings.Contains(joined, "bogus_undeclared_field") {
		t.Errorf("error %q does not name the offending field", joined)
	}
}

// TestValidateBytes_V2RejectsMinorForm proves the collapsed enum in the
// cli mirror: "2.0" routes to v2 by major but is rejected by the single-
// token enum (the test that fails on a no-op copy of the v1 minor chain).
func TestValidateBytes_V2RejectsMinorForm(t *testing.T) {
	err := spec.ValidateBytes([]byte(minimalSpecAtVersion("2.0")))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for the collapsed-enum rejection", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	if strings.Contains(joined, "not recognized") {
		t.Errorf("error %q reads as an unsupported-major failure; want a version enum rejection", joined)
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

// TestValidateBytes_Authority_Valid is the positive control (E53.2 / #2225):
// a v2 document declaring reviewers.authority with at least one agent reviewer
// passes `fishhawk validate`.
func TestValidateBytes_Authority_Valid(t *testing.T) {
	for _, authority := range []string{"advisory", "gating"} {
		t.Run(authority, func(t *testing.T) {
			yml := `
version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: ` + authority + `
          agents:
            - provider: claudecode
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("expected valid authority spec to pass, got: %v", err)
			}
		})
	}
}

// TestValidateBytes_Authority_NoAgents_Rejected is the CLI parity check for
// the backend's step-5 semantic rule (E53.2 / #2225): a declared authority
// (EITHER value) with no agent reviewers is rejected with the SAME actionable
// message the backend produces, so `fishhawk validate` and the backend cannot
// diverge. Asserts the message text — stage name and fix — not just a non-nil
// error.
func TestValidateBytes_Authority_NoAgents_Rejected(t *testing.T) {
	for _, authority := range []string{"advisory", "gating"} {
		t.Run(authority, func(t *testing.T) {
			yml := `
version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          authority: ` + authority + `
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`
			err := spec.ValidateBytes([]byte(yml))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError for a declared authority with no agents", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "/reviewers/authority") {
				t.Errorf("error = %q, want it to name /reviewers/authority", msg)
			}
			if !strings.Contains(msg, `stage "plan"`) {
				t.Errorf("error = %q, want it to name the stage \"plan\"", msg)
			}
			if !strings.Contains(msg, authority) {
				t.Errorf("error = %q, want it to name the declared value %q", msg, authority)
			}
			if !strings.Contains(msg, "reviewers.agents") {
				t.Errorf("error = %q, want it to name the fix (reviewers.agents)", msg)
			}
		})
	}
}

// TestValidateBytes_Authority_NoAgents_Inherited_Rejected is the CLI parity
// mirror of TestValidateBytes_Authority_NoAgents_Rejected for the INHERITED form
// (E53.2 / #2225): a defaults.reviewers block carrying `authority` and no
// `agents`, folded WHOLE onto an inheriting stage, is rejected with the SAME
// actionable message. decodeAndResolve resolves same-document reuse BEFORE the
// semantic walk, so the inherited case must reject exactly as the stage-declared
// case does — this pins that by-construction claim instead of leaving it reasoned.
func TestValidateBytes_Authority_NoAgents_Inherited_Rejected(t *testing.T) {
	for _, authority := range []string{"advisory", "gating"} {
		t.Run(authority, func(t *testing.T) {
			yml := `
version: "2"
defaults:
  executor:
    agent: claude-code
  reviewers:
    authority: ` + authority + `
workflows:
  feature_change:
    stages:
      - id: inherits
        type: plan
        produces:
          - artifact: plan
            schema: standard_v1
`
			err := spec.ValidateBytes([]byte(yml))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError for an inherited authority with no agents", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "/reviewers/authority") {
				t.Errorf("error = %q, want it to name /reviewers/authority", msg)
			}
			if !strings.Contains(msg, `stage "inherits"`) {
				t.Errorf("error = %q, want it to name the inheriting stage", msg)
			}
			if !strings.Contains(msg, authority) {
				t.Errorf("error = %q, want it to name the declared value %q", msg, authority)
			}
			if !strings.Contains(msg, "reviewers.agents") {
				t.Errorf("error = %q, want it to name the fix (reviewers.agents)", msg)
			}
		})
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

// --- workflow-v2 removed back-compat forms (E52.3 / #2215) ---
//
// `fishhawk validate` is where a spec author most often meets these two
// rejections, so the CLI must not degrade to the generic schema message.
// The messages are byte-identical to the backend's; these assertions and
// their backend counterparts are what keep the two copies in lockstep.

// v2SpecWithPageEvent renders a v2 document listing the given page event in
// a workflow-level `must_page_human` array.
//
// The array is deliberately NOT nested under an `operator_agent` block: at v2
// that block is itself removed (E52.10 / #2222) and the sweep reports the
// outer removal first (see
// TestValidateBytes_V2OperatorAgentRemovalPrecedesInnerForms). A bare
// `must_page_human` at a level the v2 schema does not declare it is exactly
// what a hand migration of the removed block leaves behind, and it is what
// the sweep's deliberately-over-triggering, NOT-position-aware contract
// exists to catch. The below-major-2 non-firing case uses
// legacySpecWithPageEvent instead, which keeps the operator_agent wrapper
// v0/v1 still declare.
func v2SpecWithPageEvent(event string) string {
	return `version: "2"
workflows:
  feature_change:
    must_page_human:
      - ` + event + `
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
}

// legacySpecWithPageEvent renders a v0/v1 document listing the given page
// event under the workflow-level operator_agent block those majors still
// declare. It is the below-major-2 counterpart to v2SpecWithPageEvent, which
// cannot be reused there: v2 removed operator_agent, so the v2 fixture puts
// must_page_human at workflow level, where v0/v1 reject it as an undeclared
// property before any sweep runs.
func legacySpecWithPageEvent(version, event string) string {
	return `version: "` + version + `"
workflows:
  feature_change:
    operator_agent:
      must_page_human:
        - ` + event + `
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
}

// specWithReviewersAgentCount renders a document at the given version whose
// plan stage carries the bare `reviewers.agent` integer.
func specWithReviewersAgentCount(version string) string {
	return `version: "` + version + `"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 2
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`
}

// TestValidateBytes_V2RejectsBareReviewerReject asserts the CLI rejects the
// removed page-event token at v2 with a message naming BOTH replacements —
// not the generic "value must be one of" enum message.
func TestValidateBytes_V2RejectsBareReviewerReject(t *testing.T) {
	err := spec.ValidateBytes([]byte(v2SpecWithPageEvent("reviewer_reject")))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	for _, want := range []string{"advisory_reviewer_reject", "gating_reviewer_reject", "must_page_human/0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("error %q does not name %q", joined, want)
		}
	}
}

// TestValidateBytes_V2RejectsReviewersAgentCount asserts the CLI rejects the
// removed bare integer at v2 with a message naming reviewers.agents[].
func TestValidateBytes_V2RejectsReviewersAgentCount(t *testing.T) {
	err := spec.ValidateBytes([]byte(specWithReviewersAgentCount("2")))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	for _, want := range []string{"reviewers.agents", "len(agents)", "/reviewers/agent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("error %q does not name %q", joined, want)
		}
	}
}

// TestValidateBytes_LegacyFormsAcceptedBelowMajor2 is the non-firing branch
// of the CLI's version gate: both removed forms are still valid at 1.6 and
// 0.7, so the sweep must not fire below major 2.
func TestValidateBytes_LegacyFormsAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run("page event at "+version, func(t *testing.T) {
			yml := legacySpecWithPageEvent(version, "reviewer_reject")
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
		t.Run("reviewers.agent at "+version, func(t *testing.T) {
			if err := spec.ValidateBytes([]byte(specWithReviewersAgentCount(version))); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
	}
}

// TestValidateBytes_V2AcceptsReplacementSurfaces is the v2-accepts branch:
// the explicit reject tokens (now on the `actions` matrix's page_human_on
// reserved key, E52.10 / #2222) plus an agents[] list validate cleanly.
func TestValidateBytes_V2AcceptsReplacementSurfaces(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    actions:
      page_human_on:
        - advisory_reviewer_reject
        - gating_reviewer_reject
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
          human: 1
        produces:
          - artifact: plan
            schema: standard_v1
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("ValidateBytes(v2 replacement surfaces) = %v, want nil", err)
	}
}

// TestValidateBytes_V2SweepSkippedOnMalformedVersion covers the
// fall-through-to-v0 routing branches: a missing / non-string / unparseable
// version routes to v0 (major 0), so the sweep never runs and the
// pre-existing required-version error is preserved even for a document that
// also carries a legacy form.
func TestValidateBytes_V2SweepSkippedOnMalformedVersion(t *testing.T) {
	bodies := map[string]string{
		"missing version": `workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`,
		"non-string version": `version: 2
` + `workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`,
		"unparseable version": `version: "vNext"
workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`,
	}
	for name, yml := range bodies {
		t.Run(name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(yml))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			joined := strings.Join(messageStrings(ve), "\n")
			if strings.Contains(joined, "workflow-v2") {
				t.Errorf("error %q ran the sweep; the fall-through-to-v0 path must not", joined)
			}
		})
	}
}

// TestValidateBytes_V2SweepMatchesByKeyNameNotPosition is the CLI parity
// pin for the backend's TestCheckV2RemovedForms_MatchesByKeyNameNotPosition:
// the sweep matches by key name at any depth and deliberately over-triggers,
// so a legacy form in a position the v2 schema does not permit still reports
// the removed-form message rather than the structural error. Both copies
// must behave identically or a spec author gets different advice from
// `fishhawk validate` than from the backend.
func TestValidateBytes_V2SweepMatchesByKeyNameNotPosition(t *testing.T) {
	yml := `version: "2"
must_page_human:
  - reviewer_reject
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	if !strings.Contains(joined, "removed in workflow-v2") {
		t.Errorf("error %q is not the removed-form message; the sweep must over-trigger rather than defer to the structural error", joined)
	}
}

// TestValidateBytes_V2SweepSkipsNonMatchingShapes covers the CLI sweep's
// skip branches — a non-array `must_page_human` and a non-map `reviewers`
// are not legacy forms, so the sweep must stay silent and leave the report
// to schema validation. Mirrors the backend's
// TestCheckV2RemovedForms_SkipsNonMatchingShapes, which can call the
// unexported sweep directly; here the observable proof is that the error
// is the schema's type complaint rather than the removed-form message.
func TestValidateBytes_V2SweepSkipsNonMatchingShapes(t *testing.T) {
	bodies := map[string]string{
		"must_page_human is not an array": `version: "2"
workflows:
  feature_change:
    must_page_human: reviewer_reject
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`,
		"reviewers is not a map": `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          - agent
`,
	}
	for name, yml := range bodies {
		t.Run(name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(yml))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError from the schema layer", err)
			}
			joined := strings.Join(messageStrings(ve), "\n")
			if strings.Contains(joined, "removed in workflow-v2") {
				t.Errorf("error %q is the removed-form message; these shapes are not legacy forms", joined)
			}
		})
	}
}

// --- E52.6 / #2218: the three v2 reshapes ------------------------------------
//
// The CLI is schema-only by design: cli/internal/spec performs no typed decode
// and no graph-shape pass (that asymmetry is ratified as out of scope for
// #2218 — see docs/spec/workflow-v2.md). So these assert what the CLI CAN
// decide: the v2 forms validate, the legacy forms are rejected with the
// byte-identical actionable messages the backend emits, and both stay valid
// below major 2.

// v2ReshapedSpec is a v2 document using all three reshaped surfaces: the
// object form of constraints, auto_advance, and the needs shorthand.
const v2ReshapedSpec = `version: "2"
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
          forbidden_paths: ["infra/**"]
        produces:
          - artifact: pull_request
`

func TestValidateBytes_V2AcceptsReshapedSurfaces(t *testing.T) {
	if err := spec.ValidateBytes([]byte(v2ReshapedSpec)); err != nil {
		t.Errorf("ValidateBytes(v2 reshaped surfaces) = %v, want nil", err)
	}
}

// TestValidateBytes_V2RejectsListConstraints asserts the CLI's message is
// byte-identical to the backend's — the content assertion is what keeps the
// two deliberately-separate modules' strings in lockstep.
func TestValidateBytes_V2RejectsListConstraints(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
        produces:
          - artifact: pull_request
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	const wantMsg = `constraints is an OBJECT in workflow-v2, not a list: write the kinds as one object, e.g. constraints: {max_files_changed: 45, forbidden_paths: ["infra/**"]}; keys are unique, so the one-kind-per-entry list form is gone`
	if !strings.Contains(joined, wantMsg) {
		t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
	}
	if !strings.Contains(joined, "/stages/0/constraints") {
		t.Errorf("error %q does not name the offending path", joined)
	}
}

// TestValidateBytes_V2RejectsDriveKey is the same lockstep assertion for the
// auto_advance rename.
func TestValidateBytes_V2RejectsDriveKey(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    drive: true
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	const wantMsg = `the workflow flag "drive" is spelled "auto_advance" in workflow-v2: rename the key; the semantics are unchanged (fishhawkd auto-advances mechanical transitions, judgment points still park), and v0/v1 keep the "drive" spelling`
	if !strings.Contains(joined, wantMsg) {
		t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
	}
	if !strings.Contains(joined, "/workflows/feature_change/drive") {
		t.Errorf("error %q does not name the offending path", joined)
	}
}

// TestValidateBytes_ReshapedLegacyFormsAcceptedBelowMajor2 is the version
// gate's non-firing branch: `drive` and the list form of `constraints` are
// how v0 and v1 spell these, so neither sweep may fire there.
func TestValidateBytes_ReshapedLegacyFormsAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			yml := `version: "` + version + `"
workflows:
  feature_change:
    drive: true
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
        produces:
          - artifact: pull_request
`
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
	}
}

// TestValidateBytes_V2RejectsEmptyConstraintsObject: minProperties survives
// the dropped maxProperties in the CLI mirror too.
func TestValidateBytes_V2RejectsEmptyConstraintsObject(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints: {}
        produces:
          - artifact: pull_request
`
	if err := spec.ValidateBytes([]byte(yml)); err == nil {
		t.Error("ValidateBytes(constraints: {}) = nil, want the minProperties rejection")
	}
}

// TestValidateBytes_MultipleLegacyFormsReportDeterministically mirrors the
// backend's TestCheckV2RemovedForms_MultipleLegacyFormsReportDeterministically
// (approval condition 5), pinning the CLI walk to the SAME order. The CLI
// package is schema-only and its walk is unexported, so the cases are driven
// through ValidateBytes — the sweep runs before schema validation, so a
// document that is structurally invalid still reports the sweep's match.
//
// Both ordering claims are exercised: the fixed within-node check order, and
// the sorted-key walk that decides between sibling subtrees. Each document is
// swept repeatedly so Go's randomized map iteration cannot flaky-pass it.
func TestValidateBytes_MultipleLegacyFormsReportDeterministically(t *testing.T) {
	const repeats = 32

	cases := []struct {
		name     string
		yml      string
		wantMsg  string
		wantPath string
	}{
		{
			// SAME-NODE contention: the fixed check order decides.
			name: "page_event_beats_drive_and_constraints",
			yml: `version: "2"
workflows:
  feature_change:
    drive: true
    must_page_human: [reviewer_reject]
    constraints:
      - max_files_changed: 45
`,
			wantMsg:  `page event "reviewer_reject" was removed in workflow-v2`,
			wantPath: "/workflows/feature_change/must_page_human/0",
		},
		{
			name: "drive_beats_constraints_on_one_node",
			yml: `version: "2"
workflows:
  feature_change:
    drive: true
    constraints:
      - max_files_changed: 45
`,
			wantMsg:  `the workflow flag "drive" is spelled "auto_advance" in workflow-v2`,
			wantPath: "/workflows/feature_change/drive",
		},
		{
			// CROSS-NODE contention: `alpha` sorts before `zulu`, so its
			// form wins even though constraints is checked LAST per node.
			name: "sibling_subtrees_sorted_key_decides_alpha_constraints",
			yml: `version: "2"
workflows:
  alpha:
    constraints:
      - max_files_changed: 45
  zulu:
    drive: true
`,
			wantMsg:  `constraints is an OBJECT in workflow-v2, not a list`,
			wantPath: "/workflows/alpha/constraints",
		},
		{
			// Mirror image: swapping the forms swaps the winner. Without a
			// sorted walk one of this pair would fail.
			name: "sibling_subtrees_sorted_key_decides_alpha_drive",
			yml: `version: "2"
workflows:
  alpha:
    drive: true
  zulu:
    constraints:
      - max_files_changed: 45
`,
			wantMsg:  `the workflow flag "drive" is spelled "auto_advance" in workflow-v2`,
			wantPath: "/workflows/alpha/drive",
		},
		{
			// A realistic document carrying three legacy forms at their
			// natural positions: the workflow node is checked before the
			// walk descends, so `drive` beats both nested forms.
			name: "realistic_document_workflow_drive_wins",
			yml: `version: "2"
workflows:
  feature_change:
    drive: true
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
`,
			wantMsg:  `the workflow flag "drive" is spelled "auto_advance" in workflow-v2`,
			wantPath: "/workflows/feature_change/drive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < repeats; i++ {
				err := spec.ValidateBytes([]byte(tc.yml))
				var ve *spec.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("sweep %d: err = %v, want *ValidationError", i, err)
				}
				if len(ve.Errors) != 1 {
					t.Fatalf("sweep %d: %d entries, want exactly the one sweep match: %+v", i, len(ve.Errors), ve.Errors)
				}
				got := ve.Errors[0]
				if got.Path != tc.wantPath || !strings.Contains(got.Message, tc.wantMsg) {
					t.Fatalf("sweep %d reported {path=%q msg=%q}, want the deterministic {path=%q msg containing %q}",
						i, got.Path, got.Message, tc.wantPath, tc.wantMsg)
				}
			}
		})
	}
}

// --- E52.5 / #2217: stage-budget unit unification ----------------------------
//
// The CLI is schema-only, so it asserts what `fishhawk validate` CAN decide:
// the v2 duration/USD forms validate, the legacy minutes form is rejected with
// the byte-identical actionable message the backend emits, and the spellings
// stay partitioned by major (v0/v1 keep minutes and reject the v2 forms).

// TestValidateBytes_V2RejectsBudgetMaxRuntimeMinutes asserts the CLI's message
// is byte-identical to the backend's — the content assertion is what keeps the
// two deliberately-separate modules' strings in lockstep.
func TestValidateBytes_V2RejectsBudgetMaxRuntimeMinutes(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          max_runtime_minutes: 15
        produces:
          - artifact: pull_request
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	const wantMsg = `budget.max_runtime_minutes was renamed to budget.max_runtime in workflow-v2: write the same value as a Go duration string parsed by time.ParseDuration — max_runtime_minutes: 15 becomes max_runtime: 15m; v0/v1 keep the max_runtime_minutes spelling`
	if !strings.Contains(joined, wantMsg) {
		t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
	}
	if !strings.Contains(joined, "/stages/0/budget/max_runtime_minutes") {
		t.Errorf("error %q does not name the offending path", joined)
	}
}

// TestValidateBytes_V2AcceptsBudgetDurationAndUSD is the v2-accepts branch: a
// stage budget spelling the runtime cap as a Go duration and declaring the USD
// ceiling validates cleanly.
func TestValidateBytes_V2AcceptsBudgetDurationAndUSD(t *testing.T) {
	yml := `version: "2"
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
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("ValidateBytes(v2 budget duration + USD) = %v, want nil", err)
	}
}

// TestValidateBytes_BudgetMaxRuntimeMinutesAcceptedBelowMajor2 is the version
// gate's non-firing branch: minutes is how v0 and v1 spell the runtime cap, so
// the sweep must not fire there.
func TestValidateBytes_BudgetMaxRuntimeMinutesAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			yml := `version: "` + version + `"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        budget:
          max_runtime_minutes: 15
        produces:
          - artifact: pull_request
`
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
	}
}

// TestValidateBytes_V0V1RejectBudgetV2Spellings proves the v2 spellings do not
// leak below major 2: each major's $defs/budget is additionalProperties:false,
// so a v0/v1 stage budget declaring max_runtime or limit_usd is rejected.
func TestValidateBytes_V0V1RejectBudgetV2Spellings(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		for _, field := range []string{"max_runtime: 90s", "limit_usd: 8.5"} {
			t.Run(version+"/"+field, func(t *testing.T) {
				yml := `version: "` + version + `"
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
				if err := spec.ValidateBytes([]byte(yml)); err == nil {
					t.Errorf("ValidateBytes(version %s, %s) = nil, want rejection by additionalProperties:false", version, field)
				}
			})
		}
	}
}

// --- workflow-v2 removed approval surfaces (E52.2 / #2214) ------------------

// TestValidateBytes_V2RejectsApproversAllowList asserts the CLI rejects the
// removed gate `approvers` allow-list at v2 with a message BYTE-IDENTICAL to
// the backend's — the content assertion is the only lockstep mechanism, since
// the two Go modules share no constant.
func TestValidateBytes_V2RejectsApproversAllowList(t *testing.T) {
	yml := `version: "2"
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
              any_of: [tech_lead]
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	const wantMsg = `the gate "approvers" role allow-list was removed in workflow-v2: declare the forge-neutral "approvals" block instead (a role allow-list becomes approvals: {count, members | member_of | min_permission}); the mapping is NOT mechanical, so review the translation rather than applying it blind`
	if !strings.Contains(joined, wantMsg) {
		t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
	}
	if !strings.Contains(joined, "/gates/0/approvers") {
		t.Errorf("error %q does not name the offending path", joined)
	}
}

// TestValidateBytes_V2RejectsTopLevelRolesMap asserts the CLI rejects the
// removed top-level `roles` map at v2 with the backend's verbatim message.
func TestValidateBytes_V2RejectsTopLevelRolesMap(t *testing.T) {
	yml := `version: "2"
roles:
  tech_lead:
    members: ["@octocat"]
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
	err := spec.ValidateBytes([]byte(yml))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	const wantMsg = `the top-level "roles" map was removed in workflow-v2: forge-neutral membership moves onto a gate's "approvals" block (approvals.member_of / approvals.members); there is no top-level role map at major 2`
	if !strings.Contains(joined, wantMsg) {
		t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
	}
	if !strings.Contains(joined, "/roles") {
		t.Errorf("error %q does not name the offending path", joined)
	}
}

// --- workflow-v2 removed operator_agent block (E52.10 / #2222) --------------

// TestValidateBytes_V2RejectsOperatorAgentBlock is F7's CLI half: the removed
// delegation block is rejected at v2 with a message BYTE-IDENTICAL to the
// backend's, at both placement levels. The content assertion is the only
// lockstep mechanism — the two Go modules share no constant.
func TestValidateBytes_V2RejectsOperatorAgentBlock(t *testing.T) {
	const wantMsg = `the "operator_agent" block was removed in workflow-v2: declare the action matrix (actions: {approve: {mode: auto, when: clean_dual_approval}, …}) or the tier shorthand (autonomy: low | medium | high) instead; may_approve -> actions.approve, may_route_fixup -> actions.fixup, route_fixup_min_severity -> actions.fixup.min_severity, may_waive -> actions.waive, may_retry -> actions.retry, may_merge -> actions.merge, must_page_human -> actions.page_human_on, model_policy -> actions.model_policy, and knob-absence -> mode: gated`
	cases := map[string]struct{ yml, wantPath string }{
		"workflow level": {`version: "2"
workflows:
  feature_change:
    operator_agent:
      may_approve: clean_dual_approval
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`, "/workflows/feature_change/operator_agent"},
		"gate level": {`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvals:
              count: 1
            operator_agent:
              may_merge: gates_resolved_ci_green
`, "/gates/0/operator_agent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(tc.yml))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			joined := strings.Join(messageStrings(ve), "\n")
			if !strings.Contains(joined, wantMsg) {
				t.Errorf("error %q does not carry the backend's verbatim message %q", joined, wantMsg)
			}
			if !strings.Contains(joined, tc.wantPath) {
				t.Errorf("error %q does not name the offending path %q", joined, tc.wantPath)
			}
		})
	}
}

// TestValidateBytes_V2OperatorAgentRemovalPrecedesInnerForms documents the
// precedence the walk produces: a node is checked BEFORE its children are
// visited, so a removed operator_agent block that also carries a bare
// reviewer_reject reports the OUTER removal.
func TestValidateBytes_V2OperatorAgentRemovalPrecedesInnerForms(t *testing.T) {
	err := spec.ValidateBytes([]byte(`version: "2"
workflows:
  feature_change:
    operator_agent:
      must_page_human:
        - reviewer_reject
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	if !strings.Contains(joined, `the "operator_agent" block was removed in workflow-v2`) {
		t.Errorf("error %q is not the outer removed-operator_agent message", joined)
	}
}

// TestValidateBytes_OperatorAgentStillAcceptedBelowMajor2 is the non-firing
// branch of the version gate for this form: v0 and v1 still declare the block.
func TestValidateBytes_OperatorAgentStillAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			yml := `version: "` + version + `"
workflows:
  feature_change:
    operator_agent:
      may_approve: clean_dual_approval
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
	}
}

// TestValidateBytes_V2AcceptsAutonomyGrammar is the v2-accepts branch for the
// replacement surfaces: the tier shorthand, an explicit class override, an
// extension class and both reserved keys all validate cleanly.
func TestValidateBytes_V2AcceptsAutonomyGrammar(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    autonomy: medium
    actions:
      approve:
        mode: gated
      fixup:
        mode: auto
        when: convergent_concerns
        min_severity: high
      promote:
        mode: report
      page_human_on:
        - gating_reviewer_reject
      model_policy:
        strategy: follow_plan_recommendation
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvals:
              count: 1
            actions:
              merge:
                mode: auto
                when: gates_resolved_ci_green
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("ValidateBytes(v2 autonomy grammar) = %v, want nil", err)
	}
}

// TestValidateBytes_V2RejectsUnknownAutonomyTier pins F9 on the CLI side: the
// tier enum is a SCHEMA rejection naming the three legal tiers. The CLI is
// schema-only (no typed decode), so this and the removed-form sweep are the
// whole of its autonomy coverage — the `mode: auto` condition rules are
// enforced in the backend's typed layer by design (see the schema's
// action_entry description).
func TestValidateBytes_V2RejectsUnknownAutonomyTier(t *testing.T) {
	err := spec.ValidateBytes([]byte(`version: "2"
workflows:
  feature_change:
    autonomy: aggressive
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	joined := strings.Join(messageStrings(ve), "\n")
	for _, want := range []string{"low", "medium", "high"} {
		if !strings.Contains(joined, want) {
			t.Errorf("error %q does not name the legal tier %q", joined, want)
		}
	}
}

// TestValidateBytes_V2AcceptsApprovalsOnlyGate is the v2-accepts branch: a
// forge-neutral `approvals` gate — the sole approval predicate at major 2 —
// validates cleanly.
func TestValidateBytes_V2AcceptsApprovalsOnlyGate(t *testing.T) {
	yml := `version: "2"
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
              not: [author, agent]
`
	if err := spec.ValidateBytes([]byte(yml)); err != nil {
		t.Errorf("ValidateBytes(v2 approvals-only gate) = %v, want nil", err)
	}
}

// TestValidateBytes_ApproversAndRolesAcceptedBelowMajor2 is the version gate's
// non-firing branch: v0 and v1 documents legitimately carry both the gate
// `approvers` allow-list and the top-level `roles` map, so the sweep must not
// fire and the CLI (schema-only — it never resolves role refs) accepts them.
func TestValidateBytes_ApproversAndRolesAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			yml := `version: "` + version + `"
roles:
  tech_lead:
    members: ["@octocat"]
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
              any_of: [tech_lead]
`
			if err := spec.ValidateBytes([]byte(yml)); err != nil {
				t.Errorf("ValidateBytes(version %s) = %v, want nil", version, err)
			}
		})
	}
}

// --- workflow-v2 same-document reuse (E52.4 / #2216) -----------------------

// TestValidateBytes_V2Reuse_HappyPath is the CLI-side end-to-end case: a
// document exercising every rung of the ladder validates cleanly, with the
// schema seeing the RESOLVED document (so the stage that declares no executor
// of its own still satisfies $defs/stage's required list).
func TestValidateBytes_V2Reuse_HappyPath(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
  reviewers:
    human: 1
  budget:
    max_runtime: 45m
workflows:
  base:
    stages:
      - id: propose
        type: plan
      - id: apply
        type: implement
        produces:
          - artifact: pull_request
  derived:
    extends: base
    defaults:
      executor:
        agent: codex
    stages:
      - id: apply
        type: implement
        budget:
          max_runtime: 90m
      - id: gate
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: apply
`
	if err := spec.ValidateBytes([]byte(doc)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil", err)
	}
}

// TestValidateBytes_V2Reuse_RejectionMessagesMatchBackendVerbatim is the
// byte-parity assertion. The backend and CLI resolvers are duplicated across
// two Go modules with NO shared constant, so a divergent edit compiles fine
// on both sides; the identical literals asserted here and in
// backend/internal/spec/v2reuse_test.go are what hold them in lockstep.
func TestValidateBytes_V2Reuse_RejectionMessagesMatchBackendVerbatim(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		path    string
		message string
	}{
		{
			name: "unknown base",
			doc: `
version: "2"
workflows:
  zebra:
    extends: nope
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  alpha:
    stages:
      - id: b
        type: plan
        executor:
          agent: claude-code
`,
			path:    "/workflows/zebra/extends",
			message: `extends names workflow "nope", which this document does not define; defined workflows: alpha, zebra`,
		},
		{
			name: "two-workflow cycle",
			doc: `
version: "2"
workflows:
  alpha:
    extends: beta
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  beta:
    extends: alpha
    stages:
      - id: b
        type: plan
        executor:
          agent: claude-code
`,
			path:    "/workflows/alpha/extends",
			message: `extends forms a cycle: alpha -> beta -> alpha; a workflow cannot inherit from itself, directly or transitively`,
		},
		{
			name: "self extends",
			doc: `
version: "2"
workflows:
  solo:
    extends: solo
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
`,
			path:    "/workflows/solo/extends",
			message: `extends forms a cycle: solo -> solo; a workflow cannot inherit from itself, directly or transitively`,
		},
		{
			name: "duplicate stage id in a standalone workflow",
			doc: `
version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
      - id: apply
        type: implement
        executor:
          agent: codex
`,
			path:    "/workflows/wf/stages",
			message: `duplicate stage id "apply" declared at positions 0 and 1; two entries targeting one stage have no defined merge order under extends, so the document is rejected rather than resolved`,
		},
		{
			name: "duplicate stage id in a DERIVING workflow",
			doc: `
version: "2"
workflows:
  base:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
  derived:
    extends: base
    stages:
      - id: apply
        type: implement
        executor:
          agent: codex
      - id: apply
        type: implement
        executor:
          agent: claude-code
`,
			path:    "/workflows/derived/stages",
			message: `duplicate stage id "apply" declared at positions 0 and 1; two entries targeting one stage have no defined merge order under extends, so the document is rejected rather than resolved`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(tc.doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v (%T), want *spec.ValidationError", err, err)
			}
			if len(ve.Errors) != 1 {
				t.Fatalf("errors = %+v, want exactly one", ve.Errors)
			}
			if ve.Errors[0].Path != tc.path {
				t.Errorf("Path = %q, want %q", ve.Errors[0].Path, tc.path)
			}
			if ve.Errors[0].Message != tc.message {
				t.Errorf("Message = %q, want the backend-identical %q", ve.Errors[0].Message, tc.message)
			}
		})
	}
}

// --- ResolveReuse (#2340) --------------------------------------------------
//
// ResolveReuse shares decodeAndResolve with ValidateBytes, so a reader that
// decodes its output (e.g. `fishhawk doctor`'s execution-path rung) sees the
// same resolved document the validator validates and can never disagree with
// it about which stages carry an executor.

// stageExecutorAgents decodes a resolved document and returns, per workflow,
// the executor.agent of each stage by id (empty string when the stage's
// executor declares no agent branch).
func stageExecutorAgents(t *testing.T, resolved []byte) map[string]map[string]string {
	t.Helper()
	var doc struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Executor struct {
					Agent string `yaml:"agent"`
					Human bool   `yaml:"human"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(resolved, &doc); err != nil {
		t.Fatalf("unmarshal resolved doc: %v\n%s", err, resolved)
	}
	out := map[string]map[string]string{}
	for name, wf := range doc.Workflows {
		out[name] = map[string]string{}
		for _, st := range wf.Stages {
			out[name][st.ID] = st.Executor.Agent
		}
	}
	return out
}

// TestResolveReuse_FileDefaultsExecutorFoldedIntoEveryStage: a v2 document
// whose ONLY executor is a file-level defaults.executor.agent resolves to
// stages that each carry it.
func TestResolveReuse_FileDefaultsExecutorFoldedIntoEveryStage(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
      - id: implement
        type: implement
`
	resolved, err := spec.ResolveReuse([]byte(doc))
	if err != nil {
		t.Fatalf("ResolveReuse = %v, want nil", err)
	}
	agents := stageExecutorAgents(t, resolved)["feature_change"]
	for _, id := range []string{"plan", "implement"} {
		if agents[id] != "claude-code" {
			t.Errorf("stage %q executor.agent = %q, want claude-code (inherited from file defaults)", id, agents[id])
		}
	}
	// Faithfulness: the resolved output must still validate.
	if err := spec.ValidateBytes(resolved); err != nil {
		t.Errorf("ValidateBytes(ResolveReuse output) = %v, want nil", err)
	}
}

// TestResolveReuse_WorkflowDefaultsExecutorFoldedIntoEveryStage: the same for
// a workflow-level defaults block.
func TestResolveReuse_WorkflowDefaultsExecutorFoldedIntoEveryStage(t *testing.T) {
	const doc = `
version: "2"
workflows:
  feature_change:
    defaults:
      executor:
        agent: claude-code
    stages:
      - id: plan
        type: plan
      - id: implement
        type: implement
`
	resolved, err := spec.ResolveReuse([]byte(doc))
	if err != nil {
		t.Fatalf("ResolveReuse = %v, want nil", err)
	}
	agents := stageExecutorAgents(t, resolved)["feature_change"]
	for _, id := range []string{"plan", "implement"} {
		if agents[id] != "claude-code" {
			t.Errorf("stage %q executor.agent = %q, want claude-code (inherited from workflow defaults)", id, agents[id])
		}
	}
	if err := spec.ValidateBytes(resolved); err != nil {
		t.Errorf("ValidateBytes(ResolveReuse output) = %v, want nil", err)
	}
}

// TestResolveReuse_ExtendsInheritsStages: a workflow that omits `stages`
// entirely inherits them from its `extends` base.
func TestResolveReuse_ExtendsInheritsStages(t *testing.T) {
	const doc = `
version: "2"
workflows:
  base:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: implement
        type: implement
        executor:
          agent: claude-code
  derived:
    extends: base
`
	resolved, err := spec.ResolveReuse([]byte(doc))
	if err != nil {
		t.Fatalf("ResolveReuse = %v, want nil", err)
	}
	derived := stageExecutorAgents(t, resolved)["derived"]
	for _, id := range []string{"plan", "implement"} {
		if derived[id] != "claude-code" {
			t.Errorf("derived stage %q executor.agent = %q, want claude-code (inherited via extends)", id, derived[id])
		}
	}
	if err := spec.ValidateBytes(resolved); err != nil {
		t.Errorf("ValidateBytes(ResolveReuse output) = %v, want nil", err)
	}
}

// TestResolveReuse_BareTimeoutDefaultLeavesHumanStageUntouched: a bare
// {timeout: 30m} executor default is DROPPED for a `human: true` stage (the
// closed-branch half of the branch rule), so the human stage survives the new
// entry point without an agent grafted onto it.
func TestResolveReuse_BareTimeoutDefaultLeavesHumanStageUntouched(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    timeout: 30m
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: review
        type: review
        executor:
          human: true
`
	resolved, err := spec.ResolveReuse([]byte(doc))
	if err != nil {
		t.Fatalf("ResolveReuse = %v, want nil", err)
	}
	// The human stage must not have gained an agent (or any other key).
	var doc2 struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string         `yaml:"id"`
				Executor map[string]any `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(resolved, &doc2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, st := range doc2.Workflows["feature_change"].Stages {
		if st.ID != "review" {
			continue
		}
		if got := st.Executor["human"]; got != true {
			t.Errorf("review executor.human = %v, want true", got)
		}
		if len(st.Executor) != 1 {
			t.Errorf("review executor = %v, want the bare {human: true} untouched by the dropped timeout default", st.Executor)
		}
	}
	if err := spec.ValidateBytes(resolved); err != nil {
		t.Errorf("ValidateBytes(ResolveReuse output) = %v, want nil", err)
	}
}

// TestResolveReuse_LowerMajorsPassThrough: a v0 and a v1 document carry no
// reuse primitives, so they come back with their stages and executors intact
// and still validate.
func TestResolveReuse_LowerMajorsPassThrough(t *testing.T) {
	for _, version := range []string{"0.7", "1.0"} {
		t.Run(version, func(t *testing.T) {
			resolved, err := spec.ResolveReuse([]byte(minimalSpecAtVersion(version)))
			if err != nil {
				t.Fatalf("ResolveReuse = %v, want nil", err)
			}
			if agents := stageExecutorAgents(t, resolved)["trivial"]; agents["implement"] != "claude-code" {
				t.Errorf("implement executor.agent = %q, want claude-code (unchanged)", agents["implement"])
			}
			if err := spec.ValidateBytes(resolved); err != nil {
				t.Errorf("ValidateBytes(ResolveReuse output) = %v, want nil", err)
			}
		})
	}
}

// TestResolveReuse_ParseErrors: malformed YAML and an empty document each
// return *ParseError.
func TestResolveReuse_ParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"malformed YAML":  "key: [unclosed\n",
		"empty document":  "",
		"whitespace only": "   \n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := spec.ResolveReuse([]byte(in))
			var pe *spec.ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *ParseError", err, err)
			}
		})
	}
}

// TestResolveReuse_ValidationErrors: an unsupported version major and an
// `extends` naming an absent workflow each return *ValidationError, proving
// the whole shared prefix (routing + resolution) surfaces through this entry
// point too.
func TestResolveReuse_ValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unsupported major": minimalSpecAtVersion("3.0"),
		"unknown extends base": `
version: "2"
workflows:
  derived:
    extends: nope
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := spec.ResolveReuse([]byte(doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v (%T), want *ValidationError", err, err)
			}
		})
	}
}

// TestResolveReuse_SweepPrecedesResolution is the binding-condition assertion
// that the shared prefix runs the removed-forms sweep BEFORE reuse
// resolution. The document carries a removed v2 form (`drive`) AND a
// resolution failure (`extends` naming an absent base) in the SAME workflow.
// Under the correct order the sweep's removed-form message wins; under a
// swapped order the unknown-base resolution error would win instead. Both
// entry points to decodeAndResolve are exercised so neither can drift.
func TestResolveReuse_SweepPrecedesResolution(t *testing.T) {
	const doc = `
version: "2"
workflows:
  wf:
    drive: true
    extends: nope
    stages:
      - id: a
        type: implement
        executor:
          agent: claude-code
`
	const wantDriveMsg = `the workflow flag "drive" is spelled "auto_advance" in workflow-v2`
	const unknownBaseFrag = `does not define`

	assertRemovedFormWins := func(t *testing.T, err error) {
		t.Helper()
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v (%T), want *ValidationError", err, err)
		}
		joined := strings.Join(messageStrings(ve), "\n")
		if !strings.Contains(joined, wantDriveMsg) {
			t.Errorf("error %q is not the removed-form message; the sweep must run BEFORE resolution", joined)
		}
		if strings.Contains(joined, unknownBaseFrag) {
			t.Errorf("error %q is the resolution failure; a swapped order reported it instead of the removed form", joined)
		}
	}

	// Entry point 1: ResolveReuse.
	_, err := spec.ResolveReuse([]byte(doc))
	assertRemovedFormWins(t, err)

	// Entry point 2: ValidateBytes (the sweep still precedes resolution, and
	// both precede schema.Validate).
	assertRemovedFormWins(t, spec.ValidateBytes([]byte(doc)))
}

// --- applies_to workflow routing (E53.3 / #2226) ---
//
// `fishhawk validate` must not accept a spec the backend refuses at dispatch,
// so the backend's two semantic checks on a workflow's `applies_to` are
// mirrored here over the raw yaml tree. The implementations are independent
// (the two Go modules share no package); the tests below assert the SHIPPED
// message text on both sides, and TestAppliesToChangeKindMessageParity reads
// both source files and fails on any drift in the one hand-duplicated string.

// appliesToCLIDoc renders a minimal v2 document whose single workflow carries
// the given `applies_to` YAML block (already indented four spaces).
func appliesToCLIDoc(appliesTo string) []byte {
	return []byte(`
version: "2"
workflows:
  docs_change:
` + appliesTo + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)
}

// TestValidateBytes_AppliesTo_Valid is the positive control: a well-formed
// routing predicate — and a workflow declaring none at all — both validate
// clean, so the check cannot pass by rejecting everything.
func TestValidateBytes_AppliesTo_Valid(t *testing.T) {
	cases := []struct {
		name      string
		appliesTo string
	}{
		{
			name: "declared predicate across every accepted criterion",
			appliesTo: `    applies_to:
      paths:
        - "docs/**"
      labels:
        - documentation
      trigger:
        - diff
        - scheduled`,
		},
		{
			name:      "no applies_to at all",
			appliesTo: `    description: "accepts any change"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := spec.ValidateBytes(appliesToCLIDoc(tc.appliesTo)); err != nil {
				t.Errorf("ValidateBytes = %v, want nil", err)
			}
		})
	}
}

// TestValidateBytes_AppliesTo_ChangeKind_Rejected is the CLI parity check for
// the backend's no-producer rejection. The document is SCHEMA-valid — the
// shared predicate grammar keeps `change_kind` for its other consumers — so
// this proves the CLI's own semantic layer owns the refusal, and it asserts
// the full message plus the pointer.
func TestValidateBytes_AppliesTo_ChangeKind_Rejected(t *testing.T) {
	err := spec.ValidateBytes(appliesToCLIDoc(`    applies_to:
      change_kind:
        - refactor`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for a change_kind criterion in applies_to", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/applies_to/change_kind") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/applies_to/change_kind", msg)
	}
	if !strings.Contains(msg, spec.MsgAppliesToChangeKindUnsupported) {
		t.Errorf("error = %q, want it to carry MsgAppliesToChangeKindUnsupported", msg)
	}
}

// TestValidateBytes_AppliesTo_MalformedGlob_Rejected covers the one predicate
// grammar rule the schema cannot express (a glob is just a string to JSON
// Schema), so it reaches the SHARED Predicate.Validate through the CLI's raw
// projection and reports at the workflow's own pointer.
func TestValidateBytes_AppliesTo_MalformedGlob_Rejected(t *testing.T) {
	err := spec.ValidateBytes(appliesToCLIDoc(`    applies_to:
      paths:
        - "docs/[unclosed"`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for a malformed applies_to glob", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/applies_to") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/applies_to", msg)
	}
	if !strings.Contains(msg, "docs/[unclosed") {
		t.Errorf("error = %q, want it to name the offending glob", msg)
	}
}

// TestValidateBytes_AppliesTo_SchemaCaughtForms pins the grammar rules the
// SCHEMA owns, so `fishhawk validate` rejects them locally rather than
// deferring to the backend — including the empty predicate, which must never
// read as match-all.
func TestValidateBytes_AppliesTo_SchemaCaughtForms(t *testing.T) {
	cases := []struct {
		name      string
		appliesTo string
	}{
		{name: "empty predicate is not match-all", appliesTo: `    applies_to: {}`},
		{name: "unknown trigger form", appliesTo: `    applies_to:
      trigger:
        - webhook`},
		{name: "empty label", appliesTo: `    applies_to:
      labels:
        - ""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes(appliesToCLIDoc(tc.appliesTo))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(err.Error(), "applies_to") {
				t.Errorf("error = %q, want it to name applies_to", err.Error())
			}
		})
	}
}

// TestValidateBytes_AppliesTo_ExtendsDoesNotInherit pins the resolution
// interaction: `extends` folds only STAGES, so a deriving workflow does NOT
// inherit its base's routing declaration. The base's unsupported change_kind
// is therefore reported against the BASE and never attributed to the deriving
// workflow — every workflow in the document is validated in its own right.
func TestValidateBytes_AppliesTo_ExtendsDoesNotInherit(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    applies_to:
      change_kind:
        - refactor
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's applies_to", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/base/applies_to/change_kind") {
		t.Errorf("error = %q, want it reported against the declaring workflow (base)", msg)
	}
	if strings.Contains(msg, "/workflows/derived/applies_to") {
		t.Errorf("error = %q, want no applies_to error against derived; extends folds stages only", msg)
	}
}

// TestAppliesToChangeKindMessageParity is the MECHANICAL drift guard for the
// one hand-duplicated string in this change. The two spec packages live in
// separate Go modules and cannot share a constant, so parity is asserted by
// reading both source files over the repo root and requiring the identical
// declaration line in each — the same posture
// TestPredicateSourceParityAcrossModules takes for predicate.go, scaled to
// the single constant rather than the whole file (the two validate.go files
// are NOT byte-identical and are not meant to be).
func TestAppliesToChangeKindMessageParity(t *testing.T) {
	want := `const MsgAppliesToChangeKindUnsupported = ` + strconv.Quote(spec.MsgAppliesToChangeKindUnsupported)
	for _, path := range []string{
		"../../../backend/internal/spec/validate.go",
		"../../../cli/internal/spec/validate.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not declare the applies_to change_kind message verbatim;\nwant the line: %s\n"+
				"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or `fishhawk validate` and the backend will report different text for the same spec error.", path, want)
		}
	}
}

// --- applies_to paths on a plan-less workflow (E53.15 / #2377) ---
//
// `fishhawk validate` and `fishhawk doctor` both route through ValidateBytes,
// so the backend's plan-stage rule is mirrored here and each case below
// crosses the schema layer, the v2 reuse resolver and the semantic sweep end
// to end. The rule is UNCONDITIONAL: no other control admits a `paths`
// declaration on a workflow that ships no plan.

// planStageMsgFragment is the distinctive opening of the plan-stage rejection,
// used by the two ORDER tests to assert the message is ABSENT. A pointer
// substring will not do: the shared predicate's own malformed-glob message
// carries `/applies_to/paths/0`, so matching on the pointer would report a
// plan-stage error that was never raised.
const planStageMsgFragment = "but no plan stage"

// appliesToPlanlessCLIDoc renders a minimal v2 document whose single workflow
// carries the given `applies_to` YAML block (already indented four spaces)
// and declares implement + review stages but NO plan stage.
func appliesToPlanlessCLIDoc(appliesTo string) []byte {
	return []byte(`
version: "2"
workflows:
  docs_change:
` + appliesTo + `
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
`)
}

// TestValidateBytes_AppliesTo_PathsNoPlanStage_Rejected is case (a): the
// refusal at the exact pointer, asserted against the shipped constant.
func TestValidateBytes_AppliesTo_PathsNoPlanStage_Rejected(t *testing.T) {
	err := spec.ValidateBytes(appliesToPlanlessCLIDoc(`    applies_to:
      paths:
        - "docs/**"`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for applies_to.paths on a plan-less workflow", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/applies_to/paths") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/applies_to/paths", msg)
	}
	if want := fmt.Sprintf(spec.MsgFmtAppliesToPathsNoPlanStage, "docs_change"); !strings.Contains(msg, want) {
		t.Errorf("error = %q, want it to carry the shipped rendering %q", msg, want)
	}
}

// TestValidateBytes_AppliesTo_PathsNoPlanStage_MessageNamesFixes is case (b):
// the message names the workflow, the reason and both real alternatives.
func TestValidateBytes_AppliesTo_PathsNoPlanStage_MessageNamesFixes(t *testing.T) {
	msg := fmt.Sprintf(spec.MsgFmtAppliesToPathsNoPlanStage, "routine_change")
	for _, want := range []string{
		"routine_change",
		"plan stage",
		"labels",
		"trigger",
		"constraints.allowed_paths",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to name %q", msg, want)
		}
	}
}

// TestValidateBytes_AppliesTo_PathsWithPlanStage_Accepted is case (c), the
// POSITIVE CONTROL: the identical declaration on a plan-BEARING workflow
// validates clean.
func TestValidateBytes_AppliesTo_PathsWithPlanStage_Accepted(t *testing.T) {
	if err := spec.ValidateBytes(appliesToCLIDoc(`    applies_to:
      paths:
        - "docs/**"`)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil for paths on a plan-bearing workflow", err)
	}
}

// TestValidateBytes_AppliesTo_PlanlessPathIndependentCriteriaAccepted is
// cases (d), (e) and (i): `labels` alone, `trigger` alone and no declaration
// at all stay valid on a plan-less workflow — the routing controls this
// repository's three plan-less workflows actually rely on.
func TestValidateBytes_AppliesTo_PlanlessPathIndependentCriteriaAccepted(t *testing.T) {
	cases := []struct {
		name      string
		appliesTo string
	}{
		{name: "labels only", appliesTo: `    applies_to:
      labels:
        - "type:chore"`},
		{name: "trigger only", appliesTo: `    applies_to:
      trigger:
        - scheduled`},
		{name: "no applies_to at all", appliesTo: `    description: "accepts any change"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := spec.ValidateBytes(appliesToPlanlessCLIDoc(tc.appliesTo)); err != nil {
				t.Errorf("ValidateBytes = %v, want nil", err)
			}
		})
	}
}

// TestValidateBytes_AppliesTo_PlanlessLabelsPlusPaths_Rejected is case (f):
// the rule keys on the presence of `paths`, not on its exclusivity.
func TestValidateBytes_AppliesTo_PlanlessLabelsPlusPaths_Rejected(t *testing.T) {
	err := spec.ValidateBytes(appliesToPlanlessCLIDoc(`    applies_to:
      labels:
        - "type:chore"
      paths:
        - "docs/**"`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError; labels alongside paths must not admit the declaration", err)
	}
	if !strings.Contains(err.Error(), "/workflows/docs_change/applies_to/paths") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/applies_to/paths", err.Error())
	}
}

// TestValidateBytes_AppliesTo_ChangeKindWinsOverNoPlanStage is case (g), the
// rung-1 order contract mirrored: change_kind reports first.
func TestValidateBytes_AppliesTo_ChangeKindWinsOverNoPlanStage(t *testing.T) {
	err := spec.ValidateBytes(appliesToPlanlessCLIDoc(`    applies_to:
      change_kind:
        - refactor
      paths:
        - "docs/**"`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, spec.MsgAppliesToChangeKindUnsupported) {
		t.Errorf("error = %q, want the change_kind rejection to win over the plan-stage rule", msg)
	}
	if strings.Contains(msg, planStageMsgFragment) {
		t.Errorf("error = %q, want no plan-stage error alongside it; rung 1 returns", msg)
	}
}

// TestValidateBytes_AppliesTo_MalformedGlobWinsOverNoPlanStage is case (h),
// the rung-2 order contract mirrored: the shared predicate grammar error wins,
// proving rung 3 runs last.
func TestValidateBytes_AppliesTo_MalformedGlobWinsOverNoPlanStage(t *testing.T) {
	err := spec.ValidateBytes(appliesToPlanlessCLIDoc(`    applies_to:
      paths:
        - "docs/[unclosed"`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "docs/[unclosed") {
		t.Errorf("error = %q, want the malformed-glob diagnosis naming the offending glob", msg)
	}
	if strings.Contains(msg, planStageMsgFragment) {
		t.Errorf("error = %q, want no plan-stage error alongside it; rung 2 returns", msg)
	}
}

// TestValidateBytes_AppliesTo_PathsWithInheritedPlanStage_Accepted is case
// (j), which doubles as the reuse-resolution seam test: the sweep runs AFTER
// `extends` folds stages, so a workflow whose only plan stage is inherited
// does produce a scope.files set and its `paths` declaration is evaluable.
func TestValidateBytes_AppliesTo_PathsWithInheritedPlanStage_Accepted(t *testing.T) {
	if err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
    applies_to:
      paths:
        - "docs/**"
`)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil; the inherited plan stage makes paths evaluable", err)
	}
}

// TestValidateBytes_AppliesTo_PlanlessBaseAttribution is case (k): a
// plan-less BASE declaring `paths` is reported against the base and never
// against the deriving workflow.
func TestValidateBytes_AppliesTo_PlanlessBaseAttribution(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    applies_to:
      paths:
        - "docs/**"
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's applies_to", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/base/applies_to/paths") {
		t.Errorf("error = %q, want it reported against the declaring workflow (base)", msg)
	}
	if strings.Contains(msg, "/workflows/derived/applies_to") {
		t.Errorf("error = %q, want no applies_to error against derived; extends folds stages only", msg)
	}
}

// TestValidateBytes_AppliesTo_PlanlessPathsNotAdmittedByPostHocEnvelope
// mirrors the backend's LOAD-BEARING REGRESSION TEST for the rule's
// unconditionality (see that test's comment for why the rejected carve-out is
// wrong on the merits). Mirrored deliberately: `fishhawk validate` must not
// accept a spec the backend refuses, so a carve-out reintroduced in EITHER
// sweep has to fail a test — this package's rung 3 is hand-written against
// the raw document rather than shared with the backend's typed check, so the
// backend's test cannot cover it.
//
// Every other plan-less fixture in this block declares no constraints, so a
// carve-out keyed on a stage's post-hoc path envelope would leave them all
// green. This one declares the envelope and requires the refusal to stand.
func TestValidateBytes_AppliesTo_PlanlessPathsNotAdmittedByPostHocEnvelope(t *testing.T) {
	// The stage produces pull_request, so the post-hoc-constraint binding
	// rule is satisfied and the applies_to rule is the only candidate for
	// the error these cases must still report.
	planless := func(constraints string) []byte {
		return []byte(`
version: "2"
workflows:
  docs_change:
    applies_to:
      paths:
        - "docs/**"
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
` + constraints + `        produces:
          - artifact: pull_request
`)
	}
	cases := []struct {
		name        string
		constraints string
	}{
		{name: "allowed_paths", constraints: "          allowed_paths: [\"docs/**\"]\n"},
		{name: "forbidden_paths", constraints: "          forbidden_paths: [\"backend/**\"]\n"},
		{name: "both", constraints: "          allowed_paths: [\"docs/**\"]\n          forbidden_paths: [\"backend/**\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes(planless(tc.constraints))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want the applies_to.paths refusal to stand; a stage's post-hoc path envelope must NOT admit a routing criterion that is still never evaluated (#2377)", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "/workflows/docs_change/applies_to/paths") {
				t.Fatalf("error = %q, want it to name /workflows/docs_change/applies_to/paths", msg)
			}
			if want := fmt.Sprintf(spec.MsgFmtAppliesToPathsNoPlanStage, "docs_change"); !strings.Contains(msg, want) {
				t.Errorf("error = %q, want it to carry the unchanged shipped rendering %q", msg, want)
			}
		})
	}
}

// TestAppliesToPathsNoPlanStageMessageParity is the MECHANICAL drift guard for
// the plan-stage message, modelled on TestAppliesToChangeKindMessageParity.
// The two spec packages live in separate Go modules and the CLI cannot import
// backend/internal/spec, so a source read over the repo root is the only
// cross-module guard available.
func TestAppliesToPathsNoPlanStageMessageParity(t *testing.T) {
	want := `const MsgFmtAppliesToPathsNoPlanStage = ` + strconv.Quote(spec.MsgFmtAppliesToPathsNoPlanStage)
	for _, path := range []string{
		"../../../backend/internal/spec/validate.go",
		"../../../cli/internal/spec/validate.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not declare the applies_to plan-stage message verbatim;\nwant the line: %s\n"+
				"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or `fishhawk validate` and the backend will report different text for the same spec error.", path, want)
		}
	}
}

// --- escalations (E53.4 / #2227) ---
//
// `fishhawk validate` must not accept a spec the backend refuses at dispatch,
// so the backend's only-ever-raise family is mirrored here over the raw yaml
// tree. The implementations are independent (the two Go modules share no
// package); the tests below assert the SHIPPED message text on both sides, and
// TestEscalationMessageParity reads both source files and fails on any drift in
// the three hand-duplicated strings.
//
// ONE check is deliberately backend-only: the `max_autonomy` no-op check needs
// the v2 autonomy resolver this package does not carry. Its absence here is the
// documented boundary, asserted by TestValidateBytes_Escalations_MaxAutonomy_
// NotCheckedByTheCLI so the gap is a pinned decision rather than an oversight.

// escalationCLIDoc renders a v2 workflow with one approval gate declaring
// count 2 / min_permission write plus the caller's `escalations` block
// (already indented four spaces).
func escalationCLIDoc(escalations string) []byte {
	return []byte(`
version: "2"
workflows:
  feature_change:
    autonomy: high
` + escalations + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 2
              min_permission: write
`)
}

// escalationCLIBlock wraps one escalation's `require` body in the list syntax.
func escalationCLIBlock(require string) string {
	return "    escalations:\n      - match:\n          paths: [\"infra/**\"]\n        require:\n" + require
}

// TestValidateBytes_Escalations_Valid is the positive control: a genuinely
// raising escalation on every dimension — and a workflow declaring none —
// validate clean, so the mirrored check cannot pass by rejecting everything.
func TestValidateBytes_Escalations_Valid(t *testing.T) {
	cases := []struct {
		name    string
		require string
	}{
		{name: "count above the baseline", require: "          approvals:\n            count: 3"},
		{name: "min_permission above the baseline", require: "          approvals:\n            min_permission: admin"},
		{name: "member_of no gate declares", require: "          approvals:\n            member_of: platform/security"},
		{name: "max_autonomy ceiling", require: "          max_autonomy: low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := spec.ValidateBytes(escalationCLIDoc(escalationCLIBlock(tc.require))); err != nil {
				t.Fatalf("ValidateBytes: %v, want a raising escalation accepted", err)
			}
		})
	}
	t.Run("no escalations at all", func(t *testing.T) {
		if err := spec.ValidateBytes(escalationCLIDoc("")); err != nil {
			t.Fatalf("ValidateBytes: %v, want a workflow declaring no escalations accepted", err)
		}
	})
}

// TestValidateBytes_Escalations_ChangeKind_Rejected is the CLI parity check
// for the no-producer refusal.
func TestValidateBytes_Escalations_ChangeKind_Rejected(t *testing.T) {
	err := spec.ValidateBytes(escalationCLIDoc(`    escalations:
      - match:
          change_kind: [refactor]
        require:
          max_autonomy: low`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for change_kind in an escalation match", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/feature_change/escalations/0/match/change_kind") {
		t.Errorf("error = %q, want it to name the escalation's match/change_kind pointer", msg)
	}
	if !strings.Contains(msg, spec.MsgEscalationChangeKindUnsupported) {
		t.Errorf("error = %q, want it to carry MsgEscalationChangeKindUnsupported", msg)
	}
}

// TestValidateBytes_Escalations_MalformedGlob_Rejected covers the shared
// Predicate.Validate rung at the escalation's own pointer. (The empty
// predicate and unknown trigger forms are schema-caught — see
// TestValidateBytes_Escalations_SchemaCaughtForms.)
func TestValidateBytes_Escalations_MalformedGlob_Rejected(t *testing.T) {
	err := spec.ValidateBytes(escalationCLIDoc(`    escalations:
      - match:
          paths: ["infra/["]
        require:
          max_autonomy: low`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for a malformed escalation glob", err)
	}
	if !strings.Contains(err.Error(), "/workflows/feature_change/escalations/0/match") {
		t.Errorf("error = %q, want it reported at the escalation's match pointer", err.Error())
	}
}

// TestValidateBytes_Escalations_MustRaise mirrors the backend's per-dimension
// table: each weakened-or-no-op value is refused, naming the dimension's
// pointer and carrying the shared no-raise shape.
func TestValidateBytes_Escalations_MustRaise(t *testing.T) {
	cases := []struct {
		name     string
		require  string
		wantPath string
	}{
		{
			name:     "count equal to the baseline",
			require:  "          approvals:\n            count: 2",
			wantPath: "/workflows/feature_change/escalations/0/require/approvals/count",
		},
		{
			name:     "count below the baseline",
			require:  "          approvals:\n            count: 1",
			wantPath: "/workflows/feature_change/escalations/0/require/approvals/count",
		},
		{
			name:     "min_permission equal to the baseline",
			require:  "          approvals:\n            min_permission: write",
			wantPath: "/workflows/feature_change/escalations/0/require/approvals/min_permission",
		},
		{
			name:     "min_permission below the baseline",
			require:  "          approvals:\n            min_permission: read",
			wantPath: "/workflows/feature_change/escalations/0/require/approvals/min_permission",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes(escalationCLIDoc(escalationCLIBlock(tc.require)))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError for a non-raising escalation", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantPath) {
				t.Errorf("error = %q, want it to name %q", msg, tc.wantPath)
			}
			if !strings.Contains(msg, "does not raise") || !strings.Contains(msg, "may only ever RAISE") {
				t.Errorf("error = %q, want the shared no-raise shape", msg)
			}
		})
	}
}

// NOTE (#2227 fixup): the omitted-count baseline regression is pinned on the
// BACKEND (spec.TestValidate_Escalations_OmittedCountBaseline via the exported
// spec.Validate) rather than here, because it is reachable only on a struct
// that BYPASSED the schema — the v2 schema requires `count` on an approvals
// block, and ValidateBytes runs schema.Validate BEFORE checkEscalations, so a
// countless gate fails schema before the semantic baseline check ever sees it.
// baselineApprovalCountFromRaw carries the same effective-count-of-1 default as
// the backend for parity (a countless gate reaching checkEscalations off a
// schema-bypassed path is defended identically), but that path is not
// exercisable through ValidateBytes from this external test package.

// TestValidateBytes_Escalations_ApprovalsWithNoApprovalGate mirrors the
// workflow-wide inertness rejection.
func TestValidateBytes_Escalations_ApprovalsWithNoApprovalGate(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  gateless:
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 5
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for require.approvals with no approval gate", err)
	}
	want := fmt.Sprintf(spec.MsgFmtEscalationApprovalsNoApprovalGate, "gateless", 0, "gateless")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to carry %q", err.Error(), want)
	}
}

// escalationCLIMemberOfDoc renders two approval gates whose member_of
// declarations the caller chooses, plus one member_of escalation.
func escalationCLIMemberOfDoc(gateOne, gateTwo, escalated string) []byte {
	memberLine := func(group string) string {
		if group == "" {
			return ""
		}
		return "\n              member_of: " + group
	}
	return []byte(`
version: "2"
workflows:
  feature_change:
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            member_of: ` + escalated + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1` + memberLine(gateOne) + `
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 1` + memberLine(gateTwo) + `
`)
}

// TestValidateBytes_Escalations_MemberOfBoundaryPair is the CLI half of the
// no-op check and its boundary — the same three cases the backend pins.
func TestValidateBytes_Escalations_MemberOfBoundaryPair(t *testing.T) {
	t.Run("every approval gate already requires the group", func(t *testing.T) {
		err := spec.ValidateBytes(escalationCLIMemberOfDoc("platform/security", "platform/security", "platform/security"))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *ValidationError for a member_of every gate already requires", err)
		}
		if !strings.Contains(err.Error(), "/workflows/feature_change/escalations/0/require/approvals/member_of") {
			t.Errorf("error = %q, want it to name the member_of pointer", err.Error())
		}
	})
	t.Run("only one of two gates requires the group", func(t *testing.T) {
		if err := spec.ValidateBytes(escalationCLIMemberOfDoc("platform/security", "", "platform/security")); err != nil {
			t.Fatalf("ValidateBytes: %v, want acceptance — the escalation raises the gate that omits the group", err)
		}
	})
	t.Run("no gate requires the group", func(t *testing.T) {
		if err := spec.ValidateBytes(escalationCLIMemberOfDoc("", "", "platform/security")); err != nil {
			t.Fatalf("ValidateBytes: %v, want acceptance for the ordinary raising case", err)
		}
	})
}

// TestValidateBytes_Escalations_SchemaCaughtForms pins the structural rules
// the mirrored SCHEMA gives the CLI for free — including the two minProperties
// rules that make a no-op `require` unwritable.
func TestValidateBytes_Escalations_SchemaCaughtForms(t *testing.T) {
	cases := []struct {
		name        string
		escalations string
	}{
		{name: "empty list", escalations: "    escalations: []"},
		{name: "entry missing match", escalations: "    escalations:\n      - require:\n          max_autonomy: low"},
		{name: "entry missing require", escalations: "    escalations:\n      - match:\n          paths: [\"infra/**\"]"},
		{name: "hoisted paths", escalations: "    escalations:\n      - paths: [\"infra/**\"]\n        require:\n          max_autonomy: low"},
		{name: "empty require", escalations: "    escalations:\n      - match:\n          paths: [\"infra/**\"]\n        require: {}"},
		{name: "empty nested approvals", escalations: escalationCLIBlock("          approvals: {}")},
		{name: "empty match predicate", escalations: "    escalations:\n      - match: {}\n        require:\n          max_autonomy: low"},
		{name: "unknown trigger form", escalations: "    escalations:\n      - match:\n          trigger: [rebase]\n        require:\n          max_autonomy: low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes(escalationCLIDoc(tc.escalations))
			if err == nil {
				t.Fatal("err = nil, want a rejection from the mirrored schema")
			}
			if !strings.Contains(err.Error(), "escalations") {
				t.Errorf("error = %q, want it to name escalations", err.Error())
			}
		})
	}
}

// TestValidateBytes_Escalations_MaxAutonomy_NotCheckedByTheCLI pins the ONE
// documented gap: the ceiling's no-op check needs the v2 autonomy resolver,
// which this package deliberately does not carry, so the CLI accepts a
// no-op ceiling the backend refuses. Pinned so the boundary is a decision
// rather than an oversight — if the CLI ever grows the resolver, this test
// fails and the gap is closed deliberately.
func TestValidateBytes_Escalations_MaxAutonomy_NotCheckedByTheCLI(t *testing.T) {
	// `autonomy: high` with a `high` ceiling clamps nothing; the backend
	// refuses it as a no-op.
	if err := spec.ValidateBytes(escalationCLIDoc(escalationCLIBlock("          max_autonomy: high"))); err != nil {
		t.Fatalf("ValidateBytes: %v, want the CLI to accept it — the max_autonomy no-op check is backend-only", err)
	}
}

// TestValidateBytes_Escalations_ExtendsDoesNotInherit pins the resolution
// interaction: `extends` folds only STAGES, so a base's invalid escalation is
// reported against the BASE and never attributed to the deriving workflow.
func TestValidateBytes_Escalations_ExtendsDoesNotInherit(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    escalations:
      - match:
          change_kind: [refactor]
        require:
          max_autonomy: low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's escalation", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/base/escalations/0/match/change_kind") {
		t.Errorf("error = %q, want it reported against the declaring workflow (base)", msg)
	}
	if strings.Contains(msg, "/workflows/derived/escalations") {
		t.Errorf("error = %q, want no escalation error against derived; extends folds stages only", msg)
	}
}

// --- escalation match.paths on a plan-less workflow (E53.16 / #2382) ---
//
// The CLI twin of backend/internal/spec/escalation_test.go's block, driving the
// real spec.ValidateBytes end to end through the schema layer, the v2 reuse
// resolver and the semantic sweep, so `fishhawk validate` and `fishhawk doctor`
// cannot accept an escalation the backend refuses at dispatch.

// escalationPlanlessCLIDoc renders a v2 document whose single workflow declares
// implement + review stages and NO plan stage, with a workflow-level
// `autonomy: high` block so a `require: {max_autonomy: low}` escalation
// genuinely raises. The caller supplies the `escalations` block (four-space
// indented).
func escalationPlanlessCLIDoc(escalations string) []byte {
	return []byte(`
version: "2"
workflows:
  docs_change:
    autonomy: high
` + escalations + `
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
`)
}

// planlessRaisingEscalationCLI wraps one match body (six-space indented) in a
// paths-or-otherwise escalation with a genuinely-raising max_autonomy require.
func planlessRaisingEscalationCLI(match string) string {
	return "    escalations:\n      - match:\n" + match + "\n        require:\n          max_autonomy: low"
}

// TestValidateBytes_Escalations_PathsNoPlanStage_Rejected is the CLI twin of
// the refusal: same pointer, same shipped rendering.
func TestValidateBytes_Escalations_PathsNoPlanStage_Rejected(t *testing.T) {
	err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          paths: ["**/crypto/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for match.paths on a plan-less workflow", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/escalations/0/match/paths") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/escalations/0/match/paths", msg)
	}
	if want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "docs_change", 0); !strings.Contains(msg, want) {
		t.Errorf("error = %q, want it to carry the shipped rendering %q", msg, want)
	}
}

// TestValidateBytes_Escalations_PathsNoPlanStage_MessageNamesFixes is the CLI
// twin of the message-shape assertion.
func TestValidateBytes_Escalations_PathsNoPlanStage_MessageNamesFixes(t *testing.T) {
	msg := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "routine_change", 2)
	for _, want := range []string{
		"routine_change",
		"plan stage",
		"labels",
		"trigger",
		"approvals",
		"min_permission",
		"max_autonomy",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message = %q, want it to name %q", msg, want)
		}
	}
}

// TestValidateBytes_Escalations_PlanlessLabelsOnlyAccepted is the CLI POSITIVE
// CONTROL: a labels-only escalation match validates on a plan-less workflow.
func TestValidateBytes_Escalations_PlanlessLabelsOnlyAccepted(t *testing.T) {
	if err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          labels: ["area:auth"]`))); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil for a labels-only escalation on a plan-less workflow", err)
	}
}

// TestValidateBytes_Escalations_PlanlessTriggerOnlyAccepted is the second CLI
// POSITIVE CONTROL: a trigger-only escalation match validates on a plan-less
// workflow.
func TestValidateBytes_Escalations_PlanlessTriggerOnlyAccepted(t *testing.T) {
	if err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          trigger: [diff]`))); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil for a trigger-only escalation on a plan-less workflow", err)
	}
}

// TestValidateBytes_Escalations_PathsWithPlanStage_Accepted is the positive
// control on the plan-BEARING side.
func TestValidateBytes_Escalations_PathsWithPlanStage_Accepted(t *testing.T) {
	if err := spec.ValidateBytes(escalationCLIDoc(escalationCLIBlock("          approvals:\n            count: 3"))); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil for paths on a plan-bearing workflow", err)
	}
}

// TestValidateBytes_Escalations_PlanlessLabelsPlusPaths_Rejected pins that the
// rule keys on the presence of `paths`, not on exclusivity.
func TestValidateBytes_Escalations_PlanlessLabelsPlusPaths_Rejected(t *testing.T) {
	err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          labels: ["area:auth"]
          paths: ["**/crypto/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError; labels alongside paths must not admit the escalation", err)
	}
	if !strings.Contains(err.Error(), "/workflows/docs_change/escalations/0/match/paths") {
		t.Errorf("error = %q, want it to name /workflows/docs_change/escalations/0/match/paths", err.Error())
	}
}

// TestValidateBytes_Escalations_PathsWithInheritedPlanStage_Accepted doubles as
// the reuse-resolution seam test: a deriving workflow inheriting its only plan
// stage through `extends` has an evaluable paths escalation.
func TestValidateBytes_Escalations_PathsWithInheritedPlanStage_Accepted(t *testing.T) {
	if err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  derived:
    extends: base
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
`)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil; the inherited plan stage makes the escalation evaluable", err)
	}
}

// TestValidateBytes_Escalations_PlanlessBaseAttribution is the CLI attribution
// pin: a plan-less base's bad escalation is reported against the base.
func TestValidateBytes_Escalations_PlanlessBaseAttribution(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  base:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  derived:
    extends: base
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError naming the base's escalation", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/base/escalations/0/match/paths") {
		t.Errorf("error = %q, want it reported against the declaring workflow (base)", msg)
	}
	if strings.Contains(msg, "/workflows/derived/escalations") {
		t.Errorf("error = %q, want no escalation error against derived; extends folds stages only", msg)
	}
}

// TestValidateBytes_Escalations_PlanlessPathsNotAdmittedByPostHocEnvelope is
// the CLI twin of the unconditionality regression: a stage's post-hoc path
// envelope does NOT admit a plan-less paths escalation.
func TestValidateBytes_Escalations_PlanlessPathsNotAdmittedByPostHocEnvelope(t *testing.T) {
	planless := func(constraints string) []byte {
		return []byte(`
version: "2"
workflows:
  docs_change:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          max_autonomy: low
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
` + constraints + `        produces:
          - artifact: pull_request
`)
	}
	cases := []struct {
		name        string
		constraints string
	}{
		{name: "allowed_paths", constraints: "          allowed_paths: [\"infra/**\"]\n"},
		{name: "forbidden_paths", constraints: "          forbidden_paths: [\"backend/**\"]\n"},
		{name: "both", constraints: "          allowed_paths: [\"infra/**\"]\n          forbidden_paths: [\"backend/**\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes(planless(tc.constraints))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want the match.paths refusal to stand; a post-hoc path envelope must NOT admit an escalation that still never fires (#2382)", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "/workflows/docs_change/escalations/0/match/paths") {
				t.Fatalf("error = %q, want /workflows/docs_change/escalations/0/match/paths", msg)
			}
			if want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "docs_change", 0); !strings.Contains(msg, want) {
				t.Errorf("error = %q, want the unchanged shipped rendering %q", msg, want)
			}
		})
	}
}

// TestValidateBytes_Escalations_ChangeKindWinsOverNoPlanStage is the CLI rung-1
// order contract mirrored: change_kind reports first.
func TestValidateBytes_Escalations_ChangeKindWinsOverNoPlanStage(t *testing.T) {
	err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          change_kind: [refactor]
          paths: ["infra/**"]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/escalations/0/match/change_kind") {
		t.Errorf("error = %q, want the change_kind pointer", msg)
	}
	if !strings.Contains(msg, spec.MsgEscalationChangeKindUnsupported) {
		t.Errorf("error = %q, want the change_kind rejection to win over the plan-stage rule", msg)
	}
}

// TestValidateBytes_Escalations_MalformedGlobWinsOverNoPlanStage is the CLI
// rung-2 order contract mirrored: the shared predicate grammar error reports at
// the match pointer, proving the paths rung runs last.
func TestValidateBytes_Escalations_MalformedGlobWinsOverNoPlanStage(t *testing.T) {
	err := spec.ValidateBytes(escalationPlanlessCLIDoc(planlessRaisingEscalationCLI(`          paths: ["infra/["]`)))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/escalations/0/match:") {
		t.Errorf("error = %q, want the entry reported at the predicate's own match pointer", msg)
	}
	if !strings.Contains(msg, "malformed path glob") {
		t.Errorf("error = %q, want the shared predicate grammar error to win over the plan-stage rule", msg)
	}
	if strings.Contains(msg, spec.MsgFmtEscalationPathsNoPlanStage[:40]) {
		t.Errorf("error = %q, want no plan-stage refusal; the grammar error runs before the paths rung", msg)
	}
}

// TestValidateBytes_Escalations_NoRaiseWinsOverNoPlanStage is the CLI LAST-rung
// pin: a no-raise count alongside an inert paths match reports the
// only-ever-raise diagnosis (a require-side rung), not the paths refusal.
func TestValidateBytes_Escalations_NoRaiseWinsOverNoPlanStage(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  docs_change:
    autonomy: high
    escalations:
      - match:
          paths: ["infra/**"]
        require:
          approvals:
            count: 1
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        gates:
          - type: approval
            approvals:
              count: 2
      - id: review
        type: review
        executor:
          human: true
`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/escalations/0/require/approvals/count") {
		t.Errorf("error = %q, want the count raise-check to win over the plan-stage rule", msg)
	}
	if strings.Contains(msg, "/match/paths") {
		t.Errorf("error = %q, want no paths refusal; the require-side rung reports first", msg)
	}
}

// TestValidateBytes_Escalations_MultipleEntriesReportsOffendingIndex pins index
// correctness in both pointer and rendered message: a clean entry 0 followed by
// a paths-bearing entry 1 reports index 1.
func TestValidateBytes_Escalations_MultipleEntriesReportsOffendingIndex(t *testing.T) {
	err := spec.ValidateBytes(escalationPlanlessCLIDoc(`    escalations:
      - match:
          labels: ["area:auth"]
        require:
          max_autonomy: low
      - match:
          paths: ["**/crypto/**"]
        require:
          max_autonomy: low`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for the second entry", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/docs_change/escalations/1/match/paths") {
		t.Errorf("error = %q, want it to name escalation index 1", msg)
	}
	if want := fmt.Sprintf(spec.MsgFmtEscalationPathsNoPlanStage, "docs_change", 1); !strings.Contains(msg, want) {
		t.Errorf("error = %q, want the rendering for index 1 %q", msg, want)
	}
}

// TestValidateBytes_Escalations_StagesNotAList_NoStackedDiagnosis is the
// CLI-only case for the documented divergence. The internal branch it guards —
// hasPlanStageFromRaw returning readable=false, so the paths rung SKIPS rather
// than stacking a semantic no-plan-stage error on a structural one — is not
// reachable through ValidateBytes: the v2 schema requires a list-typed `stages`
// and rejects a non-list node before the semantic sweep runs. This test asserts
// the resulting USER-VISIBLE contract: a malformed `stages` node reports the
// structural error, and the CLI never emits the plan-stage semantic diagnosis
// on top of it. (The sibling `readable` guard in checkAppliesTo, #2377, is
// unreachable for the same reason and untested for it.)
func TestValidateBytes_Escalations_StagesNotAList_NoStackedDiagnosis(t *testing.T) {
	err := spec.ValidateBytes([]byte(`
version: "2"
workflows:
  docs_change:
    autonomy: high
    escalations:
      - match:
          paths: ["**/crypto/**"]
        require:
          max_autonomy: low
    stages: "not a list"
`))
	if err == nil {
		t.Fatal("want a structural error for a non-list stages node, got nil")
	}
	if strings.Contains(err.Error(), spec.MsgFmtEscalationPathsNoPlanStage[:40]) {
		t.Errorf("error = %q, want the structural stages error, not a stacked no-plan-stage diagnosis", err.Error())
	}
}

// TestEscalationPathsNoPlanStageMessageParity is the MECHANICAL drift guard for
// the plan-stage escalation message, modelled on
// TestAppliesToPathsNoPlanStageMessageParity. NOTE the backend copy lives in
// escalation.go (beside the other escalation constants), NOT validate.go, so
// the read paths differ from #2377's. The two spec packages are separate Go
// modules and the CLI cannot import backend/internal/spec, so a source read
// over the repo root is the only cross-module guard available.
func TestEscalationPathsNoPlanStageMessageParity(t *testing.T) {
	want := `const MsgFmtEscalationPathsNoPlanStage = ` + strconv.Quote(spec.MsgFmtEscalationPathsNoPlanStage)
	for _, path := range []string{
		"../../../backend/internal/spec/escalation.go",
		"../../../cli/internal/spec/validate.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not declare the escalation plan-stage message verbatim;\nwant the line: %s\n"+
				"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or `fishhawk validate` and the backend will report different text for the same spec error.", path, want)
		}
	}
}

// --- permissions (E53.5 / #2228) ---

// permissionsCLIDoc wraps a stage body (8-space indented) in a minimal v2
// workflow with one agent implement stage.
func permissionsCLIDoc(stageBody string) []byte {
	return []byte(`version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
` + stageBody + `
`)
}

// TestValidateBytes_Permissions_Valid is the positive control: a valid
// permissions block validates clean, so the mirrored checks cannot pass by
// rejecting everything.
func TestValidateBytes_Permissions_Valid(t *testing.T) {
	if err := spec.ValidateBytes(permissionsCLIDoc(`        permissions:
          network:
            target_hosts: ["staging.example.com"]
          write: ["src/**/*.go"]
          shell: restricted`)); err != nil {
		t.Fatalf("ValidateBytes: %v, want a valid permissions block accepted", err)
	}
}

// TestValidateBytes_Permissions_BothSpellings_Rejected is the CLI parity check
// for the egress + permissions.network conflict.
func TestValidateBytes_Permissions_BothSpellings_Rejected(t *testing.T) {
	err := spec.ValidateBytes(permissionsCLIDoc(`        egress:
          target_hosts: ["a.example.com"]
        permissions:
          network:
            target_hosts: ["b.example.com"]`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for both egress and permissions.network", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/wf/stages/0/permissions/network") {
		t.Errorf("error = %q, want it to name the permissions/network pointer", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf(spec.MsgFmtPermissionsEgressConflict, "wf", "apply")) {
		t.Errorf("error = %q, want the byte-identical conflict message", msg)
	}
}

// TestValidateBytes_Permissions_MalformedWriteGlob_Rejected covers the shared
// Predicate.Validate rung for a write glob at the permissions pointer.
func TestValidateBytes_Permissions_MalformedWriteGlob_Rejected(t *testing.T) {
	err := spec.ValidateBytes(permissionsCLIDoc(`        permissions:
          write: ["src/["]`))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError for a malformed write glob", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/workflows/wf/stages/0/permissions/write") || !strings.Contains(msg, "malformed path glob") {
		t.Errorf("error = %q, want it to name the write pointer and a malformed-glob message", msg)
	}
}

// TestValidateBytes_Permissions_UnknownShell_Rejected: an unknown shell posture
// is caught by the schema's enum before the semantic sweep, so `fishhawk
// validate` still refuses it.
func TestValidateBytes_Permissions_UnknownShell_Rejected(t *testing.T) {
	err := spec.ValidateBytes(permissionsCLIDoc(`        permissions:
          shell: dangerous`))
	if err == nil {
		t.Fatal("want an error for an unknown shell posture, got nil")
	}
}

// TestPermissionsMessageParity is the MECHANICAL drift guard for the one
// hand-duplicated permissions constant, the same posture
// TestAppliesToChangeKindMessageParity takes: the two modules cannot share a
// constant, so parity is asserted by reading both source files and requiring
// the identical declaration line in each.
func TestPermissionsMessageParity(t *testing.T) {
	want := `const MsgFmtPermissionsEgressConflict = ` + strconv.Quote(spec.MsgFmtPermissionsEgressConflict)
	for _, path := range []string{
		"../../../backend/internal/spec/permissions.go",
		"../../../cli/internal/spec/validate.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not declare the permissions conflict message verbatim;\nwant the line: %s\n"+
				"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or `fishhawk validate` and the backend will report different text for the same spec error.", path, want)
		}
	}
}

// TestEscalationMessageParity is the MECHANICAL drift guard for the three
// hand-duplicated escalation strings, the same posture
// TestAppliesToChangeKindMessageParity takes for its single constant: the two
// modules cannot share a constant, so parity is asserted by reading both
// source files and requiring the identical declaration line in each.
func TestEscalationMessageParity(t *testing.T) {
	wants := []string{
		`const MsgEscalationChangeKindUnsupported = ` + strconv.Quote(spec.MsgEscalationChangeKindUnsupported),
		`const MsgFmtEscalationNoRaise = ` + strconv.Quote(spec.MsgFmtEscalationNoRaise),
		`const MsgFmtEscalationApprovalsNoApprovalGate = ` + strconv.Quote(spec.MsgFmtEscalationApprovalsNoApprovalGate),
	}
	for _, path := range []string{
		"../../../backend/internal/spec/escalation.go",
		"../../../cli/internal/spec/validate.go",
	} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(src), want) {
				t.Errorf("%s does not declare an escalation message verbatim;\nwant the line: %s\n"+
					"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or `fishhawk validate` and the backend will report different text for the same spec error.", path, want)
			}
		}
	}
}
