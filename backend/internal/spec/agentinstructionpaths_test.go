package spec_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The built-in agent-instruction-path constraint (E80.12 / #3942): the ONE
// default rule (Stage.AgentInstructionPathsRule), the workflow-v2 opt-out
// key's schema half, and its two backend-only stage-binding rules
// (validateAgentInstructionPaths). Each rejection row isolates one control:
// every other rule on the path accepts the fixture, so deleting the named
// control turns the row green-for-the-wrong-reason into a visible failure.

// aipConstraint builds a one-kind Constraint carrying only an
// agent_instruction_paths declaration.
func aipConstraint(enforce *bool, writable ...string) spec.Constraint {
	return spec.Constraint{AgentInstructionPaths: &spec.AgentInstructionPathsConstraint{
		Enforce:  enforce,
		Writable: writable,
	}}
}

// TestAgentInstructionPathsRule pins the default: an implement stage is
// enforced unless some entry declares enforce:false, and writable is the
// sorted, de-duplicated union across entries. A non-implement stage is never
// enforced, whatever it declares.
func TestAgentInstructionPathsRule(t *testing.T) {
	cases := []struct {
		name         string
		stage        spec.Stage
		wantEnforced bool
		wantWritable []string
	}{
		{
			name:         "implement with nothing declared is enforced (the default)",
			stage:        spec.Stage{Type: spec.StageTypeImplement},
			wantEnforced: true,
		},
		{
			name: "implement with unrelated constraints only is still enforced",
			stage: spec.Stage{Type: spec.StageTypeImplement, Constraints: []spec.Constraint{
				{MaxFilesChanged: 45, ForbiddenPaths: []string{"infra/**"}},
			}},
			wantEnforced: true,
		},
		{
			name:         "implement with enforce:false is off",
			stage:        spec.Stage{Type: spec.StageTypeImplement, Constraints: []spec.Constraint{aipConstraint(boolPtr(false))}},
			wantEnforced: false,
		},
		{
			name: "enforce:false in ANY entry wins over a writable in another",
			stage: spec.Stage{Type: spec.StageTypeImplement, Constraints: []spec.Constraint{
				aipConstraint(nil, "AGENTS.md"),
				aipConstraint(boolPtr(false)),
			}},
			wantEnforced: false,
		},
		{
			name:         "implement with enforce:true restates the default",
			stage:        spec.Stage{Type: spec.StageTypeImplement, Constraints: []spec.Constraint{aipConstraint(boolPtr(true))}},
			wantEnforced: true,
		},
		{
			name: "writable is the sorted, de-duplicated union across entries",
			stage: spec.Stage{Type: spec.StageTypeImplement, Constraints: []spec.Constraint{
				aipConstraint(nil, "docs/CLAUDE.md", "AGENTS.md"),
				{MaxFilesChanged: 5},
				aipConstraint(boolPtr(true), "AGENTS.md", ".claude/settings.json"),
			}},
			wantEnforced: true,
			wantWritable: []string{".claude/settings.json", "AGENTS.md", "docs/CLAUDE.md"},
		},
		{
			name:         "a non-implement stage is never enforced",
			stage:        spec.Stage{Type: spec.StageTypeReview},
			wantEnforced: false,
		},
		{
			name:         "a non-implement stage declaring writable is still not enforced",
			stage:        spec.Stage{Type: spec.StageTypeAcceptance, Constraints: []spec.Constraint{aipConstraint(nil, "AGENTS.md")}},
			wantEnforced: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enforced, writable := tc.stage.AgentInstructionPathsRule()
			if enforced != tc.wantEnforced {
				t.Errorf("enforced = %v, want %v", enforced, tc.wantEnforced)
			}
			if !reflect.DeepEqual(writable, tc.wantWritable) {
				t.Errorf("writable = %#v, want %#v", writable, tc.wantWritable)
			}
		})
	}
}

// aipV2Doc renders a one-stage workflow-v2 document whose constraints object
// is exactly the given agent_instruction_paths body (indented under it).
func aipV2Doc(stageType, aipBody, produces string) []byte {
	return []byte(`version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: ` + stageType + `
        executor:
          agent: claude-code
        constraints:
          agent_instruction_paths:
` + aipBody + `        produces:
` + produces)
}

const aipProducesPR = "          - artifact: pull_request\n"

// TestParse_AgentInstructionPaths_Accepted is the over-breadth guard for the
// rejection tables below: each valid form parses, and the parsed stage
// resolves through the ONE default rule to the expected posture.
func TestParse_AgentInstructionPaths_Accepted(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantEnforced bool
		wantWritable []string
	}{
		{"exact root AGENTS.md writable", "            writable: [AGENTS.md]\n", true, []string{"AGENTS.md"}},
		{"nested instruction file and config-dir path", "            writable: [docs/CLAUDE.md, .claude/settings.json]\n", true, []string{".claude/settings.json", "docs/CLAUDE.md"}},
		{"enforce false", "            enforce: false\n", false, nil},
		{"enforce true", "            enforce: true\n", true, nil},
		{"enforce true with writable", "            enforce: true\n            writable: [AGENTS.md]\n", true, []string{"AGENTS.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := spec.ParseBytes(aipV2Doc("implement", tc.body, aipProducesPR))
			if err != nil {
				t.Fatalf("ParseBytes: %v, want accepted", err)
			}
			enforced, writable := s.Workflows["wf"].Stages[0].AgentInstructionPathsRule()
			if enforced != tc.wantEnforced || !reflect.DeepEqual(writable, tc.wantWritable) {
				t.Errorf("rule = (%v, %#v), want (%v, %#v)", enforced, writable, tc.wantEnforced, tc.wantWritable)
			}
		})
	}
}

// TestParse_AgentInstructionPaths_DefaultOnAtV1 pins "default-on at every
// major": a v1 implement stage declares nothing (it cannot — see the v1
// schema-rejection row) and still resolves enforced.
func TestParse_AgentInstructionPaths_DefaultOnAtV1(t *testing.T) {
	s, err := spec.ParseBytes([]byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: s
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 10
        produces:
          - artifact: pull_request
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if enforced, writable := s.Workflows["feature_change"].Stages[0].AgentInstructionPathsRule(); !enforced || writable != nil {
		t.Errorf("v1 implement rule = (%v, %#v), want (true, nil)", enforced, writable)
	}
}

// TestValidate_AgentInstructionPaths_Rejected covers the two backend-only
// stage-binding rules, one row per failure mode. Each fixture passes the
// schema and every earlier validate rule, so the named control is the only
// thing that rejects it.
func TestValidate_AgentInstructionPaths_Rejected(t *testing.T) {
	const base = "/workflows/wf/stages/0/constraints/agent_instruction_paths"
	cases := []struct {
		name      string
		doc       []byte
		wantPath  string
		wantInMsg string
	}{
		{
			// Typed acceptance AND producing pull_request, so the
			// produced-artifact rule passes and the implement-only rule is
			// the only candidate.
			name:      "non-implement stage that produces pull_request",
			doc:       aipV2Doc("acceptance", "            enforce: false\n", aipProducesPR+"          - artifact: acceptance\n"),
			wantPath:  base,
			wantInMsg: `agent_instruction_paths is valid only on an implement stage, not a "acceptance" stage (#3942)`,
		},
		{
			// docs/README.md is clean and passes the schema pattern; only the
			// instruction-path membership check rejects it.
			name:      "writable entry that is not an instruction path",
			doc:       aipV2Doc("implement", "            writable: [docs/README.md]\n", aipProducesPR),
			wantPath:  base + "/writable/0",
			wantInMsg: `"docs/README.md" is not an agent-instruction path`,
		},
		{
			// ./AGENTS.md IS an instruction path and the schema pattern admits
			// it; only the canonical-form check rejects it.
			name:      "writable entry not in canonical form",
			doc:       aipV2Doc("implement", "            writable: [./AGENTS.md]\n", aipProducesPR),
			wantPath:  base + "/writable/0",
			wantInMsg: `"./AGENTS.md" must be written in canonical form "AGENTS.md"`,
		},
		{
			// ../AGENTS.md equals its own path.Clean and is an instruction
			// path; only the `..` check rejects it.
			name:      "writable entry with a dotdot component",
			doc:       aipV2Doc("implement", "            writable: [../AGENTS.md]\n", aipProducesPR),
			wantPath:  base + "/writable/0",
			wantInMsg: "must stay inside the repository (no `..` component)",
		},
		{
			name:      "the reported index names the offending entry",
			doc:       aipV2Doc("implement", "            writable: [AGENTS.md, docs/README.md]\n", aipProducesPR),
			wantPath:  base + "/writable/1",
			wantInMsg: `"docs/README.md"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(tc.doc)
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T (%v), want *ValidationError", err, err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ve.Path, tc.wantPath)
			}
			if !strings.Contains(ve.Message, tc.wantInMsg) {
				t.Errorf("Message = %q, want it to contain %q", ve.Message, tc.wantInMsg)
			}
		})
	}
}

// TestValidate_AgentInstructionPaths_DirectBranches drives Validate on a
// typed Spec for the branches the schema masks on the ParseBytes path: an
// absolute writable entry (the pattern rejects a leading `/` first) and the
// below-major-2 path form (no v0/v1 schema admits the key at all).
func TestValidate_AgentInstructionPaths_DirectBranches(t *testing.T) {
	stage := func(c spec.Constraint, typ spec.StageType) spec.Stage {
		return spec.Stage{
			ID:          "apply",
			Type:        typ,
			Executor:    spec.Executor{Agent: "claude-code"},
			Constraints: []spec.Constraint{c},
			Produces:    []spec.Produces{{Artifact: spec.ArtifactPullRequest}},
		}
	}
	cases := []struct {
		name      string
		version   string
		stage     spec.Stage
		wantPath  string
		wantInMsg string
	}{
		{
			name:      "absolute writable entry",
			version:   "2",
			stage:     stage(aipConstraint(nil, "/AGENTS.md"), spec.StageTypeImplement),
			wantPath:  "/workflows/wf/stages/0/constraints/agent_instruction_paths/writable/0",
			wantInMsg: `"/AGENTS.md" must be repo-relative, not absolute`,
		},
		{
			name:      "below major 2 reports the index-plus-kind path",
			version:   "1.6",
			stage:     stage(aipConstraint(boolPtr(false)), spec.StageTypeReview),
			wantPath:  "/workflows/wf/stages/0/constraints/0/agent_instruction_paths",
			wantInMsg: `not a "review" stage`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.Validate(&spec.Spec{
				Version:   tc.version,
				Workflows: map[string]spec.Workflow{"wf": {Stages: []spec.Stage{tc.stage}}},
			})
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T (%v), want *ValidationError", err, err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", ve.Path, tc.wantPath)
			}
			if !strings.Contains(ve.Message, tc.wantInMsg) {
				t.Errorf("Message = %q, want it to contain %q", ve.Message, tc.wantInMsg)
			}
		})
	}
	// The valid sibling of the absolute row parses clean through the same
	// direct path, so the rejection above is the control and not a fixture
	// defect.
	if err := spec.Validate(&spec.Spec{
		Version:   "2",
		Workflows: map[string]spec.Workflow{"wf": {Stages: []spec.Stage{stage(aipConstraint(nil, "AGENTS.md"), spec.StageTypeImplement)}}},
	}); err != nil {
		t.Errorf("Validate(valid sibling) = %v, want nil", err)
	}
}

// TestSchema_AgentInstructionPaths_Rejected covers the schema half: the
// contradiction `not` clause, the no-glob/no-leading-slash pattern,
// minProperties, and v0/v1's inability to declare the key at all. Each is a
// SchemaError, never a ValidationError, because the shipped canonical schema
// (and therefore both mirrors) answers first.
func TestSchema_AgentInstructionPaths_Rejected(t *testing.T) {
	const aip = "/workflows/wf/stages/0/constraints/agent_instruction_paths"
	// wantKind is a zero value of the jsonschema/v6 keyword kind the deepest
	// leaf must carry, compared by dynamic type: it pins WHICH rule rejected
	// the row (f8ae15a1), so a row rejected by some other keyword at the same
	// location fails instead of passing for the wrong reason.
	cases := []struct {
		name     string
		doc      []byte
		wantPath string
		wantKind jsonschema.ErrorKind
	}{
		// enforce:false and writable are each valid alone; only the `not`
		// clause rejects the pair.
		{"enforce false together with writable", aipV2Doc("implement", "            enforce: false\n            writable: [AGENTS.md]\n", aipProducesPR), aip, &kind.Not{}},
		{"glob metacharacter in writable", aipV2Doc("implement", "            writable: [\"**/AGENTS.md\"]\n", aipProducesPR), aip + "/writable/0", &kind.Pattern{}},
		{"brace glob in writable", aipV2Doc("implement", "            writable: [\"{AGENTS,CLAUDE}.md\"]\n", aipProducesPR), aip + "/writable/0", &kind.Pattern{}},
		{"leading slash in writable", aipV2Doc("implement", "            writable: [/AGENTS.md]\n", aipProducesPR), aip + "/writable/0", &kind.Pattern{}},
		{"empty object", []byte(`version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        constraints:
          agent_instruction_paths: {}
        produces:
          - artifact: pull_request
`), aip, &kind.MinProperties{}},
		{"empty writable list", aipV2Doc("implement", "            writable: []\n", aipProducesPR), aip + "/writable", &kind.MinItems{}},
		{"unknown member", aipV2Doc("implement", "            readable: [AGENTS.md]\n", aipProducesPR), aip, &kind.AdditionalProperties{}},
		{"v1 document declaring the key", []byte(`version: "1.6"
workflows:
  feature_change:
    stages:
      - id: s
        type: implement
        executor:
          agent: claude-code
        constraints:
          - agent_instruction_paths:
              enforce: false
        produces:
          - artifact: pull_request
`), "/workflows/feature_change/stages/0/constraints/0", &kind.AdditionalProperties{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(tc.doc)
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %T (%v), want *SchemaError", err, err)
			}
			if se.Path != tc.wantPath {
				t.Errorf("SchemaError.Path = %q, want %q", se.Path, tc.wantPath)
			}
			gotKind := spec.SchemaErrorLeafKind(se)
			if reflect.TypeOf(gotKind) != reflect.TypeOf(tc.wantKind) {
				t.Errorf("SchemaError leaf kind = %T (%v), want %T", gotKind, se, tc.wantKind)
			}
		})
	}
}
