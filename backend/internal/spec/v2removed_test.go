package spec

import (
	"errors"
	"strings"
	"testing"
)

// --- workflow-v2 removed back-compat forms (E52.3 / #2215) -------------------
//
// workflow-v2 deletes the bare `reviewer_reject` page-event token and the
// `reviewers.agent` integer count. The JSON Schema alone would reject both,
// but with a generic enum / additionalProperties message naming no
// replacement — so ParseBytes runs checkV2RemovedForms first for a routed
// major >= 2. These tests pin the SHIPPED rejection message, not just the
// fact of rejection: an edit that dropped the members from the schema but
// left the generic message in place fails here.

// v2SpecWorkflowLevelPageEvent is a v2 document carrying the legacy bare
// token in a WORKFLOW-level operator_agent block.
const v2SpecWorkflowLevelPageEvent = `version: "2"
workflows:
  feature_change:
    operator_agent:
      must_page_human:
        - budget_override
        - reviewer_reject
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// v2SpecGateLevelPageEvent is a v2 document carrying the legacy bare token
// in a PER-GATE operator_agent block — a different nesting site, which the
// generic walk must reach just as well.
const v2SpecGateLevelPageEvent = `version: "2"
roles:
  tech_lead:
    github: [octocat]
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
            operator_agent:
              must_page_human:
                - reviewer_reject
`

// v2SpecReviewersAgentCount is a v2 document carrying the removed bare
// `reviewers.agent` integer on a plan stage.
const v2SpecReviewersAgentCount = `version: "2"
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

// TestParseV2_RejectsBareReviewerRejectAtWorkflowLevel asserts a v2 document
// listing the removed token under a workflow-level operator_agent is
// rejected with a message naming BOTH replacement tokens, and a path naming
// the offending array member.
func TestParseV2_RejectsBareReviewerRejectAtWorkflowLevel(t *testing.T) {
	_, err := ParseBytes([]byte(v2SpecWorkflowLevelPageEvent))
	se := requireSchemaError(t, err)
	assertNamesRejectReplacements(t, se.Message)
	if !strings.Contains(se.Path, "must_page_human") {
		t.Errorf("Path = %q, want it to name the offending must_page_human member", se.Path)
	}
	// Index 1: the sweep must point at the offending member, not the array.
	if !strings.HasSuffix(se.Path, "/must_page_human/1") {
		t.Errorf("Path = %q, want it to end at /must_page_human/1", se.Path)
	}
}

// TestParseV2_RejectsBareReviewerRejectAtGateLevel proves the generic walk
// reaches the per-gate operator_agent block too — a hand-rolled
// workflows->stages traversal that only knew the workflow-level site would
// pass the test above and fail this one.
func TestParseV2_RejectsBareReviewerRejectAtGateLevel(t *testing.T) {
	_, err := ParseBytes([]byte(v2SpecGateLevelPageEvent))
	se := requireSchemaError(t, err)
	assertNamesRejectReplacements(t, se.Message)
	if !strings.Contains(se.Path, "gates") || !strings.Contains(se.Path, "must_page_human") {
		t.Errorf("Path = %q, want it to locate the gate-level must_page_human member", se.Path)
	}
}

// TestParseV2_RejectsReviewersAgentCount asserts the removed bare integer is
// rejected with a message naming reviewers.agents[] as the replacement.
// Deleting the property from an additionalProperties:false block is what
// makes this reachable; a permissive block would let the document validate.
func TestParseV2_RejectsReviewersAgentCount(t *testing.T) {
	_, err := ParseBytes([]byte(v2SpecReviewersAgentCount))
	se := requireSchemaError(t, err)
	if !strings.Contains(se.Message, "reviewers.agents") {
		t.Errorf("Message = %q, want it to name reviewers.agents[] as the replacement", se.Message)
	}
	if !strings.Contains(se.Message, "len(agents)") {
		t.Errorf("Message = %q, want it to state the effective count is len(agents)", se.Message)
	}
	if !strings.HasSuffix(se.Path, "/reviewers/agent") {
		t.Errorf("Path = %q, want it to end at /reviewers/agent", se.Path)
	}
}

// TestCheckV2RemovedForms_MatchesByKeyNameNotPosition pins the sweep's
// deliberate over-trigger as INTENDED behavior (approval condition 2). The
// sweep matches by key name at ANY depth, so a legacy form sitting in a
// position the v2 schema does not permit at all — here, a must_page_human
// array and a reviewers map hung directly off the ROOT document, neither of
// which v2 declares — still reports the removed-form message rather than
// deferring to the structural error. That is the price of never missing a
// legacy form; a position-aware sweep would have to re-encode the schema's
// structure in Go, which E52 is actively restructuring.
func TestCheckV2RemovedForms_MatchesByKeyNameNotPosition(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		wantMsg  string
		wantPath string
	}{
		{
			name: "must_page_human at the document root (undeclared position)",
			raw: map[string]any{
				"version":         "2",
				"must_page_human": []any{"reviewer_reject"},
			},
			wantMsg:  msgV2RemovedReviewerReject,
			wantPath: "/must_page_human/0",
		},
		{
			name: "reviewers under an unknown subtree",
			raw: map[string]any{
				"version": "2",
				"totally_unknown_block": map[string]any{
					"reviewers": map[string]any{"agent": 1},
				},
			},
			wantMsg:  msgV2RemovedReviewersAgent,
			wantPath: "/totally_unknown_block/reviewers/agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := checkV2RemovedForms(tc.raw)
			if se == nil {
				t.Fatal("checkV2RemovedForms = nil; want the removed-form error even in an undeclared position")
			}
			if se.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", se.Message, tc.wantMsg)
			}
			if se.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", se.Path, tc.wantPath)
			}
		})
	}
}

// TestCheckV2RemovedForms_SkipsNonMatchingShapes covers the sweep's skip
// branches: a scalar root, a non-array must_page_human, a non-map reviewers,
// and an array whose members are not the legacy token. None is a legacy
// form, so the sweep must stay silent and leave the report to the schema.
func TestCheckV2RemovedForms_SkipsNonMatchingShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"scalar root", "just a string"},
		{"nil root", nil},
		{"must_page_human is not an array", map[string]any{"must_page_human": "reviewer_reject"}},
		{"must_page_human members are not strings", map[string]any{"must_page_human": []any{1, true, nil}}},
		{"must_page_human without the legacy token", map[string]any{"must_page_human": []any{"gating_reviewer_reject"}}},
		{"reviewers is not a map", map[string]any{"reviewers": []any{"agent"}}},
		{"reviewers without the agent key", map[string]any{"reviewers": map[string]any{"human": 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if se := checkV2RemovedForms(tc.raw); se != nil {
				t.Errorf("checkV2RemovedForms = %+v, want nil", se)
			}
		})
	}
}

// TestParseV2_AcceptsReplacementSurfaces is the v2-accepts branch: the
// explicit reject tokens plus an agents[] list parse cleanly and populate
// the typed struct. This is also the DisallowUnknownFields pin — a v2
// document must still decode into the shared Spec type, which retains the
// removed fields for v0/v1.
func TestParseV2_AcceptsReplacementSurfaces(t *testing.T) {
	s, err := ParseBytes([]byte(`version: "2"
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
`))
	if err != nil {
		t.Fatalf("ParseBytes(v2 replacement surfaces): %v", err)
	}
	wf := s.Workflows["feature_change"]
	if got := wf.OperatorAgent.MustPageHuman; len(got) != 2 ||
		got[0] != PageEventAdvisoryReviewerReject || got[1] != PageEventGatingReviewerReject {
		t.Errorf("MustPageHuman = %v, want the two explicit tokens", got)
	}
	rv := wf.Stages[0].Reviewers
	if rv == nil {
		t.Fatal("Stage.Reviewers = nil, want the declared block")
	}
	if got := rv.AgentCount(); got != 2 {
		t.Errorf("AgentCount() = %d, want 2 (len(agents))", got)
	}
	if rv.Agent != 0 {
		t.Errorf("Reviewers.Agent = %d, want 0 — a v2 document can no longer populate the removed field", rv.Agent)
	}
}

// TestParse_LegacyFormsStillAcceptedBelowMajor2 is the non-firing branch of
// the version gate: the sweep must NOT fire for a v0 or v1 document, and
// both legacy forms must still populate the typed struct exactly as before.
func TestParse_LegacyFormsStillAcceptedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run("page event at "+version, func(t *testing.T) {
			s, err := ParseBytes([]byte(`version: "` + version + `"
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
			if err != nil {
				t.Fatalf("ParseBytes(version %s): %v", version, err)
			}
			got := s.Workflows["feature_change"].OperatorAgent.MustPageHuman
			if len(got) != 1 || got[0] != PageEventReviewerReject {
				t.Errorf("MustPageHuman = %v, want [%s]", got, PageEventReviewerReject)
			}
		})
		t.Run("reviewers.agent at "+version, func(t *testing.T) {
			s, err := ParseBytes([]byte(`version: "` + version + `"
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
`))
			if err != nil {
				t.Fatalf("ParseBytes(version %s): %v", version, err)
			}
			rv := s.Workflows["feature_change"].Stages[0].Reviewers
			if rv == nil {
				t.Fatal("Stage.Reviewers = nil, want the declared block")
			}
			if rv.Agent != 2 {
				t.Errorf("Reviewers.Agent = %d, want 2", rv.Agent)
			}
			if got := rv.AgentCount(); got != 2 {
				t.Errorf("AgentCount() = %d, want 2 (the bare-count fallback is retained for v0/v1)", got)
			}
		})
	}
}

// TestParse_MalformedVersionUnchangedByTheSweep covers the fall-through-to-v0
// routing branches (missing / non-string / unparseable version). Each returns
// major 0, so the sweep never runs and the pre-existing required-version /
// enum error is preserved verbatim — including for a document that ALSO
// carries a legacy form, which must not be reported ahead of the version
// error.
func TestParse_MalformedVersionUnchangedByTheSweep(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"missing version", `workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`},
		{"non-string version", `version: 2
workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`},
		{"unparseable version", `version: "vNext"
workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.yaml))
			se := requireSchemaError(t, err)
			if strings.Contains(se.Message, "workflow-v2") {
				t.Fatalf("Message = %q; the sweep must not run on the fall-through-to-v0 path", se.Message)
			}
			if !strings.Contains(se.Path, "version") && !strings.Contains(se.Message, "version") {
				t.Errorf("error (path %q, message %q) is not the pre-existing version error", se.Path, se.Message)
			}
		})
	}
}

// TestParse_UnsupportedMajorSkipsTheSweep pins the other non-firing branch:
// an unsupported major fails closed at routing, BEFORE the sweep, so the
// error names the supported majors rather than a legacy form.
func TestParse_UnsupportedMajorSkipsTheSweep(t *testing.T) {
	_, err := ParseBytes([]byte(`version: "9"
workflows:
  feature_change:
    operator_agent:
      must_page_human: [reviewer_reject]
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`))
	se := requireSchemaError(t, err)
	if !strings.Contains(se.Message, "not recognized") {
		t.Errorf("Message = %q, want the unsupported-major fail-closed error", se.Message)
	}
}

// requireSchemaError unwraps err to a *SchemaError or fails the test.
func requireSchemaError(t *testing.T, err error) *SchemaError {
	t.Helper()
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	return se
}

// assertNamesRejectReplacements asserts a removed-page-event message names
// both replacement tokens — the whole reason the sweep exists.
func assertNamesRejectReplacements(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{PageEventAdvisoryReviewerReject, PageEventGatingReviewerReject} {
		if !strings.Contains(msg, want) {
			t.Errorf("Message = %q, want it to name the replacement token %q", msg, want)
		}
	}
}
