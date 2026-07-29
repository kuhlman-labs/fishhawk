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

// v2SpecWithPageEvent renders a v2 document listing the given page event
// under a workflow-level operator_agent block.
func v2SpecWithPageEvent(event string) string {
	return `version: "2"
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
			yml := strings.Replace(v2SpecWithPageEvent("reviewer_reject"), `version: "2"`, `version: "`+version+`"`, 1)
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
// the explicit reject tokens plus an agents[] list validate cleanly.
func TestValidateBytes_V2AcceptsReplacementSurfaces(t *testing.T) {
	yml := `version: "2"
workflows:
  feature_change:
    operator_agent:
      must_page_human:
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
    operator_agent:
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
