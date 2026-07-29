package spec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// --- workflow-v2 shape normalization (E52.6 / #2218) ------------------------
//
// LAYER B. Every assertion here parses a workflow-v2 document, so BOTH sides
// of any v1/v2 comparison pass through the new normalization. That makes these
// tests evidence for "v2 means what v1 means" and NEVER evidence that the
// v0/v1 path was preserved — the preservation proof is the Layer A
// characterization tests in spec_test.go and
// backend/internal/server/deploy_legacy_constraints_test.go, which were run
// green against unmodified HEAD before any production file was edited.

const v2PlanImplementLonghand = `version: "2"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
`

const v2PlanImplementNeeds = `version: "2"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan]
        produces:
          - artifact: pull_request
`

func mustParseV2(t *testing.T, doc string) *Spec {
	t.Helper()
	s, err := ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return s
}

func stageByID(t *testing.T, s *Spec, wf, id string) Stage {
	t.Helper()
	for _, st := range s.Workflows[wf].Stages {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("stage %q not found in workflow %q", id, wf)
	return Stage{}
}

// TestNormalizeV2Shapes_NeedsMatchesLonghand is acceptance criterion 5: the
// shorthand resolves to the SAME Stage.Inputs the longhand form produces.
func TestNormalizeV2Shapes_NeedsMatchesLonghand(t *testing.T) {
	long := stageByID(t, mustParseV2(t, v2PlanImplementLonghand), "feature_change", "implement")
	short := stageByID(t, mustParseV2(t, v2PlanImplementNeeds), "feature_change", "implement")
	if !reflect.DeepEqual(short.Inputs, long.Inputs) {
		t.Errorf("needs-derived inputs = %#v, want the longhand form %#v", short.Inputs, long.Inputs)
	}
	if len(short.Inputs) != 1 || short.Inputs[0].Artifact != string(ArtifactPlan) {
		t.Errorf("needs: [plan] should derive the plan artifact, got %#v", short.Inputs)
	}
}

// TestNormalizeV2Shapes_NeedsImplementDerivesPullRequest pins the second
// (and only other) derivable referent type.
func TestNormalizeV2Shapes_NeedsImplementDerivesPullRequest(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
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
        needs: [implement]
`
	st := stageByID(t, mustParseV2(t, doc), "feature_change", "review")
	want := []Input{{Artifact: string(ArtifactPullRequest), FromStage: "implement"}}
	if !reflect.DeepEqual(st.Inputs, want) {
		t.Errorf("inputs = %#v, want %#v", st.Inputs, want)
	}
}

// TestNormalizeV2Shapes_NeedsCombinedWithLonghandInputs is acceptance
// criterion 6's ALLOW decision plus its ordering rule: declared inputs keep
// their positions and derived entries follow in `needs` order.
func TestNormalizeV2Shapes_NeedsCombinedWithLonghandInputs(t *testing.T) {
	doc := `version: "2"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        needs: [plan]
        produces:
          - artifact: pull_request
`
	st := stageByID(t, mustParseV2(t, doc), "feature_change", "implement")
	want := []Input{
		{Source: InputSourceGitHubIssue, Required: true},
		{Artifact: string(ArtifactPlan), FromStage: "plan"},
	}
	if !reflect.DeepEqual(st.Inputs, want) {
		t.Errorf("inputs = %#v, want declared-then-derived %#v", st.Inputs, want)
	}
}

// TestNormalizeV2Shapes_NeedsDedupesExactDuplicate: a derived entry whose
// (artifact, from_stage) pair is already declared longhand is dropped, so the
// resolved input set is identical however the author spelled it.
func TestNormalizeV2Shapes_NeedsDedupesExactDuplicate(t *testing.T) {
	doc := `version: "2"
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
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
        needs: [plan]
        produces:
          - artifact: pull_request
`
	st := stageByID(t, mustParseV2(t, doc), "feature_change", "implement")
	want := []Input{{Artifact: string(ArtifactPlan), FromStage: "plan"}}
	if !reflect.DeepEqual(st.Inputs, want) {
		t.Errorf("inputs = %#v, want the duplicate deduped to %#v", st.Inputs, want)
	}
}

// TestNormalizeV2Shapes_ConstraintsObjectYieldsOneMultiKindConstraint proves
// the dropped maxProperties BEHAVIOURALLY: a schema-only edit that forgot the
// normalization fails here.
func TestNormalizeV2Shapes_ConstraintsObjectYieldsOneMultiKindConstraint(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          max_files_changed: 45
          forbidden_paths: ["infra/**"]
          allowed_paths: ["backend/**"]
          required_outcomes: [tests_added_or_updated]
        produces:
          - artifact: pull_request
`
	cs := stageByID(t, mustParseV2(t, doc), "feature_change", "implement").Constraints
	if len(cs) != 1 {
		t.Fatalf("constraints = %d entries, want exactly 1 (an object denotes exactly one Constraint)", len(cs))
	}
	want := Constraint{
		MaxFilesChanged:  45,
		ForbiddenPaths:   []string{"infra/**"},
		AllowedPaths:     []string{"backend/**"},
		RequiredOutcomes: []string{"tests_added_or_updated"},
	}
	if !reflect.DeepEqual(cs[0], want) {
		t.Errorf("constraints[0] = %#v, want every declared kind on one entry %#v", cs[0], want)
	}
}

// TestNormalizeV2Shapes_AutoAdvance proves the rename BEHAVIOURALLY at the
// parse boundary: true reaches Workflow.Drive, and false / absent both leave
// it false.
func TestNormalizeV2Shapes_AutoAdvance(t *testing.T) {
	body := `
workflows:
  feature_change:
    %s
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"true", "auto_advance: true", true},
		{"false", "auto_advance: false", false},
		{"absent", "description: no flag", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := "version: \"2\"" + strings.Replace(body, "%s", tc.line, 1)
			s := mustParseV2(t, doc)
			if got := s.Workflows["feature_change"].Drive; got != tc.want {
				t.Errorf("Workflow.Drive = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- rejection modes -------------------------------------------------------

// TestParseV2_NeedsRejectionModes covers the three `needs` failure branches.
// Two of them are DELIBERATELY routed to the pre-existing from_stage
// graph-shape messages rather than to a second competing error from the
// normalizer (acceptance criterion 7).
func TestParseV2_NeedsRejectionModes(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			name: "unknown_referent_falls_to_existing_from_stage_rule",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [nope]
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/inputs/0/from_stage",
			wantMsg:  `from_stage "nope" does not match any stage id in workflow "feature_change"`,
		},
		{
			name: "self_reference_falls_to_existing_ordering_rule",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [implement]
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/inputs/0/from_stage",
			wantMsg:  "must be a stage earlier in the workflow",
		},
		{
			name: "later_reference_falls_to_existing_ordering_rule",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [late_plan]
        produces:
          - artifact: pull_request
      - id: late_plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`,
			wantPath: "/workflows/feature_change/stages/0/inputs/0/from_stage",
			wantMsg:  "must be a stage earlier in the workflow",
		},
		{
			name: "review_referent_has_no_default_artifact",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [review]
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/1/needs/0",
			wantMsg:  `needs "review" references a "review" stage, which has no default input artifact`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.doc))
			if err == nil {
				t.Fatalf("ParseBytes: want an error")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %T (%v), want *ValidationError", err, err)
			}
			if verr.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", verr.Path, tc.wantPath)
			}
			if !strings.Contains(verr.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", verr.Message, tc.wantMsg)
			}
		})
	}
}

// TestParseV2_NeedsToReviewNamesLonghandEscape pins that the one error the
// normalizer itself raises tells the author what to do instead.
func TestParseV2_NeedsToReviewNamesLonghandEscape(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [review]
        produces:
          - artifact: pull_request
`
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error")
	}
	if !strings.Contains(err.Error(), "inputs:") {
		t.Errorf("message %q should direct the author to longhand inputs:", err.Error())
	}
}

// TestParseV2_ConstraintsEmptyObjectRejected: minProperties survives the
// dropped maxProperties, so `constraints: {}` is still a rejected no-op.
func TestParseV2_ConstraintsEmptyObjectRejected(t *testing.T) {
	doc := `version: "2"
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
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error for constraints: {}")
	}
	var serr *SchemaError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %T (%v), want *SchemaError", err, err)
	}
	if want := "/workflows/feature_change/stages/0/constraints"; serr.Path != want {
		t.Errorf("path = %q, want %q", serr.Path, want)
	}
	// The library renders minProperties as "got 0, want 1".
	if !strings.Contains(serr.Message, "want 1") {
		t.Errorf("message = %q, want the schema's minProperties rejection", serr.Message)
	}
}

// TestParseV2_ConstraintsScalarIsSchemaError proves the ORDERING contract:
// normalization runs AFTER schema validation, so a `constraints` value that
// is neither an object nor a list is the SCHEMA's error and never reaches
// (or panics inside) the normalizer.
func TestParseV2_ConstraintsScalarIsSchemaError(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints: "max_files_changed"
        produces:
          - artifact: pull_request
`
	_, err := ParseBytes([]byte(doc))
	if err == nil {
		t.Fatal("ParseBytes: want an error")
	}
	var serr *SchemaError
	if !errors.As(err, &serr) {
		t.Fatalf("error = %T (%v), want the SCHEMA's *SchemaError", err, err)
	}
	if serr.Message == msgV2ReshapedConstraints {
		t.Errorf("a scalar constraints value should get the schema's type error, not the list-form reshape message")
	}
}

// TestParseV2_LonghandInputsWithExternalSourceStillValidates is acceptance
// criterion 6's other half: adding `needs` did not disturb longhand inputs,
// including the external trigger branch.
func TestParseV2_LonghandInputsWithExternalSourceStillValidates(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: pull_request
`
	st := stageByID(t, mustParseV2(t, doc), "feature_change", "implement")
	want := []Input{{Source: InputSourceGitHubIssue, Required: true}}
	if !reflect.DeepEqual(st.Inputs, want) {
		t.Errorf("inputs = %#v, want %#v", st.Inputs, want)
	}
}

// --- constraint binding paths: kind form at v2, index form below -----------

// TestValidate_ConstraintBindingPathIsVersionAware pins acceptance criterion
// 3: the three ADR-038 / #1888 binding MESSAGES are unchanged verbatim, and
// only the PATH form differs — `/constraints/<kind>` at major >= 2, where an
// index into the one-element normalized slice names nothing the author wrote,
// and the unchanged `/constraints/0` below it.
func TestValidate_ConstraintBindingPathIsVersionAware(t *testing.T) {
	const (
		preflightMsg = `pre-flight deploy constraint is valid only on a deploy stage, not a "implement" stage (ADR-038)`
		postHocMsg   = "post-hoc diff constraint is not valid on a deploy stage; a delegating deploy produces no reviewable diff (ADR-038)"
	)
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			name: "v2_preflight_change_freeze_false_on_implement",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          change_freeze: false
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/constraints/change_freeze",
			wantMsg:  preflightMsg,
		},
		{
			name: "v2_preflight_allowed_environments_on_implement",
			doc: `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          allowed_environments: [prod]
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/constraints/allowed_environments",
			wantMsg:  preflightMsg,
		},
		{
			name: "v2_posthoc_on_deploy",
			doc: `version: "2"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          max_files_changed: 5
        produces:
          - artifact: deployment
`,
			wantPath: "/workflows/release/stages/0/constraints/max_files_changed",
			wantMsg:  postHocMsg,
		},
		{
			name: "v1_sibling_preflight_keeps_index_form",
			doc: `version: "1.6"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - change_freeze: false
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/constraints/0",
			wantMsg:  preflightMsg,
		},
		{
			name: "v1_sibling_posthoc_keeps_index_form",
			doc: `version: "1.6"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        constraints:
          - max_files_changed: 5
        produces:
          - artifact: deployment
`,
			wantPath: "/workflows/release/stages/0/constraints/0",
			wantMsg:  postHocMsg,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ParseBytes: want an error")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %T (%v), want *ValidationError", err, err)
			}
			if verr.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", verr.Path, tc.wantPath)
			}
			if verr.Message != tc.wantMsg {
				t.Errorf("message = %q, want the UNCHANGED %q", verr.Message, tc.wantMsg)
			}
		})
	}
}

// TestValidate_DiffCoveragePathsAreVersionAware pins the #1888 messages
// verbatim at both path forms.
func TestValidate_DiffCoveragePathsAreVersionAware(t *testing.T) {
	// Every fixture declares the pull_request artifact — including the
	// acceptance-stage one, which also keeps its own acceptance artifact.
	// E52.7 / #2219 binds post-hoc diff constraints to the PRODUCED artifact
	// at major >= 2 and reports that violation BEFORE the #1888 stage-type
	// check, so a v2 stage declaring no pull_request never reaches the
	// implement-only message these cases pin. Declaring it is what routes each
	// case to the #1888 branch it was written to exercise; the asserted paths
	// and messages are unchanged.
	v2Stage := func(stageType, reportPath string) string {
		produces := "          - artifact: pull_request\n"
		if stageType == "acceptance" {
			produces += "          - artifact: acceptance\n"
		}
		return `version: "2"
workflows:
  feature_change:
    stages:
      - id: s
        type: ` + stageType + `
        executor:
          agent: claude-code
        constraints:
          diff_coverage:
            command: "make cov"
            report_path: "` + reportPath + `"
            min_new_line_coverage: 80
        produces:
` + produces
	}
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			name:     "v2_wrong_stage_type",
			doc:      v2Stage("acceptance", "cov.info"),
			wantPath: "/workflows/feature_change/stages/0/constraints/diff_coverage",
			wantMsg:  `diff_coverage is valid only on an implement stage, not a "acceptance" stage (#1888): the measurement is emitted by the implement runner, and a declared constraint with no measurement is a violation`,
		},
		{
			name:     "v2_absolute_report_path",
			doc:      v2Stage("implement", "/etc/passwd"),
			wantPath: "/workflows/feature_change/stages/0/constraints/diff_coverage/report_path",
			wantMsg:  `report_path "/etc/passwd" must be repo-relative, not absolute`,
		},
		{
			name:     "v2_backslash_report_path",
			doc:      v2Stage("implement", `cov\\report.info`),
			wantPath: "/workflows/feature_change/stages/0/constraints/diff_coverage/report_path",
			wantMsg:  `must be repo-relative, not absolute`,
		},
		{
			name:     "v2_dotdot_escape_report_path",
			doc:      v2Stage("implement", "../outside/cov.info"),
			wantPath: "/workflows/feature_change/stages/0/constraints/diff_coverage/report_path",
			wantMsg:  "must stay inside the repository (no `..` escape)",
		},
		{
			name: "v1_sibling_keeps_index_form",
			doc: `version: "1.6"
workflows:
  feature_change:
    stages:
      - id: s
        type: implement
        executor:
          agent: claude-code
        constraints:
          - diff_coverage:
              command: "make cov"
              report_path: "../outside/cov.info"
              min_new_line_coverage: 80
        produces:
          - artifact: pull_request
`,
			wantPath: "/workflows/feature_change/stages/0/constraints/0/diff_coverage/report_path",
			wantMsg:  "must stay inside the repository (no `..` escape)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ParseBytes: want an error")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %T (%v), want *ValidationError", err, err)
			}
			if verr.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", verr.Path, tc.wantPath)
			}
			if !strings.Contains(verr.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain the verbatim %q", verr.Message, tc.wantMsg)
			}
		})
	}
}

// --- defensive branches ----------------------------------------------------

// TestNormalizeV2Shapes_ShapeMismatchesAreSkipped exercises every skip branch
// in the walk. None is reachable through ParseBytes (schema validation runs
// first), so they are asserted directly: the contract is that a shape the
// normalizer does not recognize is left alone rather than panicking.
func TestNormalizeV2Shapes_ShapeMismatchesAreSkipped(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"root_not_a_map", []any{"nope"}},
		{"workflows_not_a_map", map[string]any{"workflows": "nope"}},
		{"workflow_not_a_map", map[string]any{"workflows": map[string]any{"w": "nope"}}},
		{"stages_not_a_list", map[string]any{"workflows": map[string]any{"w": map[string]any{"stages": "nope"}}}},
		{"stage_not_a_map", map[string]any{"workflows": map[string]any{"w": map[string]any{"stages": []any{"nope"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := normalizeV2Shapes(tc.raw); err != nil {
				t.Errorf("normalizeV2Shapes = %v, want nil (unrecognized shapes are skipped)", err)
			}
		})
	}
}

// TestNormalizeConstraintsObject_ListLeftAlone: the already-a-list branch is
// unreachable for a v2 document (the sweep and then the schema reject the
// list form earlier), but the rewrite is a total function, so a list passes
// through untouched rather than being wrapped a second time.
func TestNormalizeConstraintsObject_ListLeftAlone(t *testing.T) {
	list := []any{map[string]any{"max_files_changed": 5}}
	stage := map[string]any{"constraints": list}
	normalizeConstraintsObject(stage)
	if !reflect.DeepEqual(stage["constraints"], list) {
		t.Errorf("constraints = %#v, want the list untouched %#v", stage["constraints"], list)
	}
}

// TestExpandNeeds_DefensiveBranches covers the shapes expandNeeds tolerates:
// a non-list `needs` (key still deleted, nothing expanded), a non-string
// entry (skipped), and a referent whose stage carries a non-string id or
// type — which makes it read as NOT FOUND and so routes to validate.go's
// existing from_stage message rather than to a second error.
func TestExpandNeeds_DefensiveBranches(t *testing.T) {
	t.Run("needs_not_a_list_is_deleted_not_expanded", func(t *testing.T) {
		stage := map[string]any{"needs": "plan"}
		if err := expandNeeds(stage, map[string]string{"plan": "plan"}, "w", 0); err != nil {
			t.Fatalf("expandNeeds: %v", err)
		}
		if _, ok := stage["needs"]; ok {
			t.Error("needs key survived; the typed decode runs under DisallowUnknownFields")
		}
		if _, ok := stage["inputs"]; ok {
			t.Error("inputs were synthesized from a non-list needs")
		}
	})

	t.Run("non_string_entry_skipped", func(t *testing.T) {
		stage := map[string]any{"needs": []any{42, "plan"}}
		if err := expandNeeds(stage, map[string]string{"plan": "plan"}, "w", 0); err != nil {
			t.Fatalf("expandNeeds: %v", err)
		}
		got, _ := stage["inputs"].([]any)
		if len(got) != 1 {
			t.Fatalf("inputs = %#v, want only the one string entry expanded", got)
		}
	})

	t.Run("unknown_referent_emits_empty_artifact", func(t *testing.T) {
		stage := map[string]any{"needs": []any{"ghost"}}
		if err := expandNeeds(stage, map[string]string{}, "w", 0); err != nil {
			t.Fatalf("expandNeeds: %v", err)
		}
		want := []any{map[string]any{"artifact": "", "from_stage": "ghost"}}
		if !reflect.DeepEqual(stage["inputs"], want) {
			t.Errorf("inputs = %#v, want %#v so validate.go's from_stage rule reports it", stage["inputs"], want)
		}
	})

	t.Run("stage_with_non_string_id_or_type_is_not_indexed", func(t *testing.T) {
		got := stageTypesByID([]any{
			map[string]any{"id": 7, "type": "plan"},
			map[string]any{"id": "s", "type": 7},
			map[string]any{"id": "ok", "type": "plan"},
			"not-a-map",
		})
		if !reflect.DeepEqual(got, map[string]string{"ok": "plan"}) {
			t.Errorf("stageTypesByID = %#v, want only the well-formed entry", got)
		}
	})

	t.Run("non_map_declared_input_skipped_by_dedupe", func(t *testing.T) {
		if inputsContain([]any{"not-a-map"}, "plan", "plan") {
			t.Error("inputsContain matched a non-map declared input")
		}
	})
}

// TestSpecVersionMajor covers the path-form selector's parse, including the
// empty-Version case a programmatically built *Spec produces — which must
// resolve to 0 so the legacy index form is preserved for Validate-only
// callers.
func TestSpecVersionMajor(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"0.7":   0,
		"1.6":   1,
		"2":     2,
		"2.0":   2,
		"3":     3,
		"abc":   0,
		"x.1":   0,
		"1.2.3": 1,
	}
	for in, want := range cases {
		if got := specVersionMajor(in); got != want {
			t.Errorf("specVersionMajor(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestConstraintPath_EmptyKindFallsBackToIndex: a Constraint with no field
// set has no kind name, so the reported path falls back to the index form
// rather than emitting a dangling `/constraints/`.
func TestConstraintPath_EmptyKindFallsBackToIndex(t *testing.T) {
	if got := constraintPath(2, 3, ""); got != "/constraints/3" {
		t.Errorf("constraintPath(2, 3, \"\") = %q, want the index fallback", got)
	}
	if got := constraintPath(2, 3, "change_freeze"); got != "/constraints/change_freeze" {
		t.Errorf("constraintPath = %q, want the kind form", got)
	}
	if got := constraintPath(1, 3, "change_freeze"); got != "/constraints/3" {
		t.Errorf("constraintPath below major 2 = %q, want the index form", got)
	}
}

// TestConstraintKindNames pins the FIXED declaration order both name helpers
// report, so a v2 object mixing several kinds always names the same one.
func TestConstraintKindNames(t *testing.T) {
	tru := true
	if got := (Constraint{}).preflightKindName(); got != "" {
		t.Errorf("empty preflightKindName = %q, want \"\"", got)
	}
	if got := (Constraint{}).postHocKindName(); got != "" {
		t.Errorf("empty postHocKindName = %q, want \"\"", got)
	}
	all := Constraint{
		MaxFilesChanged:     1,
		ForbiddenPaths:      []string{"a"},
		AllowedPaths:        []string{"b"},
		RequiredOutcomes:    []string{"ci_green"},
		DiffCoverage:        &DiffCoverageConstraint{},
		AllowedEnvironments: []string{"prod"},
		ChangeFreeze:        &tru,
		RequiredUpstream:    []string{"ci_green"},
	}
	if got := all.postHocKindName(); got != "max_files_changed" {
		t.Errorf("postHocKindName = %q, want the first in declaration order", got)
	}
	if got := all.preflightKindName(); got != "allowed_environments" {
		t.Errorf("preflightKindName = %q, want the first in declaration order", got)
	}
	// change_freeze presence is pointer-detected, so `false` still names it.
	fls := false
	if got := (Constraint{ChangeFreeze: &fls}).preflightKindName(); got != "change_freeze" {
		t.Errorf("preflightKindName for {change_freeze: false} = %q, want change_freeze", got)
	}
	if got := (Constraint{DiffCoverage: &DiffCoverageConstraint{}}).postHocKindName(); got != "diff_coverage" {
		t.Errorf("postHocKindName = %q, want diff_coverage", got)
	}
	if got := (Constraint{RequiredUpstream: []string{"ci_green"}}).preflightKindName(); got != "required_upstream" {
		t.Errorf("preflightKindName = %q, want required_upstream", got)
	}
	if got := (Constraint{RequiredOutcomes: []string{"ci_green"}}).postHocKindName(); got != "required_outcomes" {
		t.Errorf("postHocKindName = %q, want required_outcomes", got)
	}
	if got := (Constraint{AllowedPaths: []string{"a"}}).postHocKindName(); got != "allowed_paths" {
		t.Errorf("postHocKindName = %q, want allowed_paths", got)
	}
	if got := (Constraint{ForbiddenPaths: []string{"a"}}).postHocKindName(); got != "forbidden_paths" {
		t.Errorf("postHocKindName = %q, want forbidden_paths", got)
	}
}
