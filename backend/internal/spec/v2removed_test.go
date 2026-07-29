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
// generic walk must reach just as well. The gate uses the forge-neutral
// `approvals` predicate (the legacy `approvers` allow-list and top-level
// `roles` map are themselves removed at v2, E52.2 / #2214), so `reviewer_reject`
// is the ONLY removed form here and the sweep must report it.
const v2SpecGateLevelPageEvent = `version: "2"
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
              not: [author, agent]
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

// --- E52.2 / #2214: the two REMOVED approval surfaces -----------------------
//
// workflow-v2 removes the gate `approvers` role allow-list and the top-level
// `roles` map — `approvals` becomes the sole approval predicate. The schema
// rejects both as undeclared properties of additionalProperties:false blocks,
// but with a generic message naming no replacement, so the sweep names it.
// These pin the SHIPPED actionable message (content, not merely that an error
// occurred) and its path.

// TestParseV2_RejectsApproversAllowList asserts a v2 gate carrying the removed
// `approvers` allow-list is rejected with a *SchemaError naming `approvals` as
// the replacement, at the offending gate path.
func TestParseV2_RejectsApproversAllowList(t *testing.T) {
	doc := `version: "2"
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
	_, err := ParseBytes([]byte(doc))
	se := requireSchemaError(t, err)
	if se.Message != msgV2RemovedApprovers {
		t.Errorf("Message = %q, want the actionable %q", se.Message, msgV2RemovedApprovers)
	}
	if !strings.Contains(se.Message, "approvals") {
		t.Errorf("Message = %q, want it to name approvals as the replacement", se.Message)
	}
	if !strings.HasSuffix(se.Path, "/gates/0/approvers") {
		t.Errorf("Path = %q, want it to end at /gates/0/approvers", se.Path)
	}
	// The generic additionalProperties message must NOT win.
	if strings.Contains(se.Message, "additional propert") {
		t.Errorf("Message = %q, want the sweep's actionable message, not the generic additionalProperties one", se.Message)
	}
}

// TestParseV2_RejectsTopLevelRolesMap asserts a v2 document carrying the removed
// top-level `roles` map is rejected with a *SchemaError naming
// approvals.member_of / approvals.members as the replacement.
func TestParseV2_RejectsTopLevelRolesMap(t *testing.T) {
	doc := `version: "2"
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
	_, err := ParseBytes([]byte(doc))
	se := requireSchemaError(t, err)
	if se.Message != msgV2RemovedRolesMap {
		t.Errorf("Message = %q, want the actionable %q", se.Message, msgV2RemovedRolesMap)
	}
	if !strings.Contains(se.Message, "approvals.member_of") || !strings.Contains(se.Message, "approvals.members") {
		t.Errorf("Message = %q, want it to name approvals.member_of / approvals.members", se.Message)
	}
	if se.Path != "/roles" {
		t.Errorf("Path = %q, want /roles", se.Path)
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
		{
			name: "budget.max_runtime_minutes under an unknown subtree",
			raw: map[string]any{
				"version": "2",
				"totally_unknown_block": map[string]any{
					"budget": map[string]any{"max_runtime_minutes": 15},
				},
			},
			wantMsg:  msgV2RenamedBudgetMaxRuntimeMinutes,
			wantPath: "/totally_unknown_block/budget/max_runtime_minutes",
		},
		{
			// An `approvers` map hung in a position v2 does not permit — the
			// sweep still reports the removed-form message rather than deferring
			// to the structural error (E52.2 / #2214).
			name: "approvers under an unknown subtree",
			raw: map[string]any{
				"version": "2",
				"totally_unknown_block": map[string]any{
					"approvers": map[string]any{"any_of": []any{"tech_lead"}},
				},
			},
			wantMsg:  msgV2RemovedApprovers,
			wantPath: "/totally_unknown_block/approvers",
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
		{"roles is not a map", map[string]any{"roles": []any{"tech_lead"}}},
		{"approvers is not a map", map[string]any{"approvers": []any{"any_of"}}},
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
		// E52.2 / #2214 acceptance criterion 5: a v0/v1 spec carrying the gate
		// `approvers` allow-list AND the top-level `roles` map it references
		// still parses clean AND resolves its role refs — validateApproverRefs
		// is retained and reached for v0/v1. (A dangling ref would fail Validate;
		// a clean parse proves it resolved.)
		t.Run("approvers+roles at "+version, func(t *testing.T) {
			s, err := ParseBytes([]byte(`version: "` + version + `"
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
`))
			if err != nil {
				t.Fatalf("ParseBytes(version %s): %v", version, err)
			}
			if _, ok := s.Roles["tech_lead"]; !ok {
				t.Errorf("Roles = %v, want the tech_lead role retained for v0/v1", s.Roles)
			}
			g := s.Workflows["feature_change"].Stages[0].Gates[0]
			if g.Approvers == nil || len(g.Approvers.AnyOf) != 1 || g.Approvers.AnyOf[0] != "tech_lead" {
				t.Errorf("Approvers = %+v, want any_of=[tech_lead] (the resolved v0/v1 role ref)", g.Approvers)
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

// --- E52.6 / #2218: the two RESHAPED forms -----------------------------------
//
// `drive` and the list form of `constraints` are both rejected by the v2
// schema (an undeclared property of an additionalProperties:false block, and
// an array where an object is required), but with generic messages naming no
// replacement. These pin the SHIPPED actionable message and its path.

func TestCheckV2RemovedForms_DriveRenamedToAutoAdvance(t *testing.T) {
	doc := `version: "2"
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
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error for a v2 `drive` key")
	}
	var serr *SchemaError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %T (%v), want *SchemaError", err, err)
	}
	if serr.Path != "/workflows/feature_change/drive" {
		t.Errorf("path = %q, want /workflows/feature_change/drive", serr.Path)
	}
	if serr.Message != msgV2RenamedDrive {
		t.Errorf("message = %q, want the actionable %q", serr.Message, msgV2RenamedDrive)
	}
	if !strings.Contains(serr.Message, "auto_advance") {
		t.Errorf("message %q must name auto_advance", serr.Message)
	}
}

func TestCheckV2RemovedForms_ConstraintsListReshapedToObject(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 45
          - forbidden_paths: ["infra/**"]
        produces:
          - artifact: pull_request
`
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error for a v2 list-form `constraints`")
	}
	var serr *SchemaError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %T (%v), want *SchemaError", err, err)
	}
	if serr.Path != "/workflows/feature_change/stages/0/constraints" {
		t.Errorf("path = %q, want the stage's constraints", serr.Path)
	}
	if serr.Message != msgV2ReshapedConstraints {
		t.Errorf("message = %q, want the actionable %q", serr.Message, msgV2ReshapedConstraints)
	}
	// The message must SHOW the object form, not merely assert a type.
	if !strings.Contains(serr.Message, "max_files_changed: 45") {
		t.Errorf("message %q should show the object form", serr.Message)
	}
}

// TestCheckV2RemovedForms_ReshapedFormsNeverFireBelowMajor2 is the version
// gate: v0 and v1 documents spell both forms legally, so the sweep must not
// fire on them.
func TestCheckV2RemovedForms_ReshapedFormsNeverFireBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		doc := `version: "` + version + `"
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
		s, err := ParseBytes([]byte(doc))
		if err != nil {
			t.Fatalf("version %s: ParseBytes: %v", version, err)
		}
		if !s.Workflows["feature_change"].Drive {
			t.Errorf("version %s: drive: true did not reach Workflow.Drive", version)
		}
		if n := len(s.Workflows["feature_change"].Stages[0].Constraints); n != 1 {
			t.Errorf("version %s: constraints = %d entries, want the list form preserved", version, n)
		}
	}
}

// TestCheckV2RemovedAtNode_FixedOrder pins the report order across all seven
// forms, so a document carrying several always reports the same one. The two
// E52.2 removals (approvers, then roles) are appended LAST, after the E52.5
// budget rename — so a node also carrying the earlier forms still reports them
// first, proving the existing five-form order was byte-preserved.
func TestCheckV2RemovedAtNode_FixedOrder(t *testing.T) {
	node := map[string]any{
		"must_page_human": []any{PageEventReviewerReject},
		"reviewers":       map[string]any{"agent": 1},
		"drive":           true,
		"constraints":     []any{map[string]any{"max_files_changed": 1}},
		"budget":          map[string]any{"max_runtime_minutes": 15},
		"approvers":       map[string]any{"any_of": []any{"tech_lead"}},
		"roles":           map[string]any{"tech_lead": map[string]any{"members": []any{"@octocat"}}},
	}
	order := []string{
		msgV2RemovedReviewerReject,
		msgV2RemovedReviewersAgent,
		msgV2RenamedDrive,
		msgV2ReshapedConstraints,
		msgV2RenamedBudgetMaxRuntimeMinutes,
		msgV2RemovedApprovers,
		msgV2RemovedRolesMap,
	}
	keys := []string{"must_page_human", "reviewers", "drive", "constraints", "budget", "approvers", "roles"}
	for i, want := range order {
		got := checkV2RemovedAtNode(node, "")
		if got == nil {
			t.Fatalf("step %d: checkV2RemovedAtNode = nil, want %q", i, want)
		}
		if got.Message != want {
			t.Fatalf("step %d: message = %q, want %q", i, got.Message, want)
		}
		delete(node, keys[i])
	}
	if got := checkV2RemovedAtNode(node, ""); got != nil {
		t.Errorf("checkV2RemovedAtNode on a clean node = %+v, want nil", got)
	}
}

// TestCheckV2RemovedAtNode_ConstraintsObjectNotReported: only the LIST form is
// a legacy form; the v2 object must pass the sweep untouched.
func TestCheckV2RemovedAtNode_ConstraintsObjectNotReported(t *testing.T) {
	node := map[string]any{"constraints": map[string]any{"max_files_changed": 1}}
	if got := checkV2RemovedAtNode(node, ""); got != nil {
		t.Errorf("checkV2RemovedAtNode on the v2 object form = %+v, want nil", got)
	}
}

// --- E52.5 / #2217: the RENAMED budget.max_runtime_minutes form --------------
//
// v2 renames budget.max_runtime_minutes to budget.max_runtime (a Go duration).
// The schema alone rejects the legacy key as an undeclared property of an
// additionalProperties:false block, but with a generic message naming no
// replacement — so the sweep names it. These pin the SHIPPED actionable
// message and its path, and the version gate that keeps v0/v1 silent.

// TestCheckV2RemovedForms_BudgetMaxRuntimeMinutesRenamed asserts a v2 document
// declaring the legacy minutes key on a stage budget is rejected with EXACTLY
// msgV2RenamedBudgetMaxRuntimeMinutes at the offending path — not the generic
// additionalProperties message.
func TestCheckV2RemovedForms_BudgetMaxRuntimeMinutesRenamed(t *testing.T) {
	doc := `version: "2"
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
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error for a v2 `budget.max_runtime_minutes` key")
	}
	var serr *SchemaError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %T (%v), want *SchemaError", err, err)
	}
	if serr.Path != "/workflows/feature_change/stages/0/budget/max_runtime_minutes" {
		t.Errorf("path = %q, want the stage's budget/max_runtime_minutes", serr.Path)
	}
	if serr.Message != msgV2RenamedBudgetMaxRuntimeMinutes {
		t.Errorf("message = %q, want the actionable %q", serr.Message, msgV2RenamedBudgetMaxRuntimeMinutes)
	}
	// The generic additionalProperties message must NOT win.
	if strings.Contains(serr.Message, "additional propert") {
		t.Errorf("message = %q, want the sweep's actionable message, not the generic additionalProperties one", serr.Message)
	}
	// The message must SHOW the minutes-to-duration equivalence.
	if !strings.Contains(serr.Message, "max_runtime: 15m") || !strings.Contains(serr.Message, "max_runtime_minutes: 15") {
		t.Errorf("message %q should show the minutes-to-duration equivalence", serr.Message)
	}
}

// TestCheckV2RemovedForms_BudgetRenameNeverFiresBelowMajor2 is the version
// gate: v0 and v1 spell the runtime cap as max_runtime_minutes, so the sweep
// must not fire on them and the value must still decode.
func TestCheckV2RemovedForms_BudgetRenameNeverFiresBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		doc := `version: "` + version + `"
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
		s, err := ParseBytes([]byte(doc))
		if err != nil {
			t.Fatalf("version %s: ParseBytes: %v", version, err)
		}
		b := s.Workflows["feature_change"].Stages[0].Budget
		if b == nil || b.MaxRuntimeMinutes != 15 {
			t.Errorf("version %s: budget.max_runtime_minutes did not decode to 15 (%+v)", version, b)
		}
	}
}

// TestCheckV2RemovedForms_MultipleLegacyFormsReportDeterministically closes
// the gap waived on #2215 on the explicit condition that the next sibling
// extending this sweep would close it (approval condition 5). This is that
// sibling: E52.6 takes the sweep from two legacy forms to four, which is
// precisely when ordering starts to matter.
//
// Two claims were previously asserted only in comments, and BOTH are load
// bearing for a document carrying several legacy forms at once:
//
//   - checkV2RemovedAtNode checks the four forms in a FIXED order, so which
//     form wins on ONE node is decided by that order.
//   - walkV2RemovedForms visits map keys in SORTED order, so which NODE wins
//     is decided by the key names — Go's map iteration order is randomized,
//     and without the sort the reported match would vary run to run.
//
// Every case is therefore built by a FACTORY that returns a freshly
// constructed equivalent map, and swept REPEATEDLY: a randomized iteration
// order that happened to agree once cannot flaky-pass this.
func TestCheckV2RemovedForms_MultipleLegacyFormsReportDeterministically(t *testing.T) {
	const repeats = 32

	cases := []struct {
		name    string
		build   func() any
		wantMsg string
		// wantPath is asserted exactly; it is what tells a reader WHICH of
		// the competing forms won, not merely that one did.
		wantPath string
	}{
		{
			// SAME-NODE contention: all four forms on one node, so
			// checkV2RemovedAtNode's fixed order alone decides.
			name: "four_forms_on_one_node_page_event_wins",
			build: func() any {
				return map[string]any{
					"must_page_human": []any{"budget_override", PageEventReviewerReject},
					"reviewers":       map[string]any{"agent": 2},
					"drive":           true,
					"constraints":     []any{map[string]any{"max_files_changed": 45}},
				}
			},
			wantMsg:  msgV2RemovedReviewerReject,
			wantPath: "/must_page_human/1",
		},
		{
			name: "three_forms_on_one_node_reviewers_agent_wins",
			build: func() any {
				return map[string]any{
					"reviewers":   map[string]any{"agent": 2},
					"drive":       true,
					"constraints": []any{map[string]any{"max_files_changed": 45}},
				}
			},
			wantMsg:  msgV2RemovedReviewersAgent,
			wantPath: "/reviewers/agent",
		},
		{
			name: "two_forms_on_one_node_drive_wins",
			build: func() any {
				return map[string]any{
					"drive":       true,
					"constraints": []any{map[string]any{"max_files_changed": 45}},
				}
			},
			wantMsg:  msgV2RenamedDrive,
			wantPath: "/drive",
		},
		{
			// CROSS-NODE contention: the two forms sit on SIBLING subtrees,
			// so the sorted-key walk — not the node check order — decides.
			// `alpha` sorts before `zulu`, so alpha's form wins even though
			// its form (constraints) is checked LAST within a node.
			name: "sibling_subtrees_sorted_key_decides_alpha_constraints",
			build: func() any {
				return map[string]any{
					"alpha": map[string]any{"constraints": []any{map[string]any{"max_files_changed": 45}}},
					"zulu":  map[string]any{"drive": true},
				}
			},
			wantMsg:  msgV2ReshapedConstraints,
			wantPath: "/alpha/constraints",
		},
		{
			// The mirror image: swapping which sibling carries which form
			// swaps the winner. If the walk were not sorted, one of this
			// pair would fail.
			name: "sibling_subtrees_sorted_key_decides_alpha_drive",
			build: func() any {
				return map[string]any{
					"alpha": map[string]any{"drive": true},
					"zulu":  map[string]any{"constraints": []any{map[string]any{"max_files_changed": 45}}},
				}
			},
			wantMsg:  msgV2RenamedDrive,
			wantPath: "/alpha/drive",
		},
		{
			// A REALISTIC document carrying three legacy forms at their
			// natural positions: a workflow-level `drive`, a
			// must_page_human token nested under operator_agent, and a
			// list-form `constraints` inside a stage. The workflow node is
			// checked BEFORE the walk descends into operator_agent or
			// stages, so `drive` beats both nested forms — pinning the
			// whole-document outcome, not just a single node's.
			name:     "realistic_document_workflow_drive_wins",
			build:    buildMultiLegacyFormDocument,
			wantMsg:  msgV2RenamedDrive,
			wantPath: "/workflows/feature_change/drive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < repeats; i++ {
				got := checkV2RemovedForms(tc.build())
				if got == nil {
					t.Fatalf("sweep %d: checkV2RemovedForms = nil, want a match", i)
				}
				if got.Message != tc.wantMsg || got.Path != tc.wantPath {
					t.Fatalf("sweep %d reported {path=%q msg=%q}, want the deterministic {path=%q msg=%q}",
						i, got.Path, got.Message, tc.wantPath, tc.wantMsg)
				}
			}
		})
	}
}

// buildMultiLegacyFormDocument returns a FRESH raw document carrying three
// legacy forms simultaneously, at the positions a real v2 spec would put
// them: a workflow-level `drive`, `must_page_human: [reviewer_reject]` under
// operator_agent, and a list-form `constraints` inside a stage.
func buildMultiLegacyFormDocument() any {
	return map[string]any{
		"version": "2",
		"workflows": map[string]any{
			"feature_change": map[string]any{
				"drive": true,
				"operator_agent": map[string]any{
					"must_page_human": []any{PageEventReviewerReject},
				},
				"stages": []any{
					map[string]any{
						"id":          "implement",
						"type":        "implement",
						"constraints": []any{map[string]any{"max_files_changed": 45}},
					},
				},
			},
		},
	}
}
