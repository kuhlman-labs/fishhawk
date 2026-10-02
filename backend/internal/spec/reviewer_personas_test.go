package spec_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Reviewer personas (ADR-084 / E55.8 / #3753). The per-rule rejection rows
// live in the shared parity corpus (docs/spec/review-conventions-fixtures.json,
// the persona-* rows), run here by TestReviewConventionsCorpus_BackendMatches
// and in cli/internal/spec by TestReviewConventionsCorpus_CLIMatches. This file
// pins what a single-rule corpus row cannot: the RULE ORDER against the
// review_conventions family, the typed round-trip, the pure selection
// function's order and its two fail-closed branches, and the schema facts the
// grammar rests on.

// rpDoc assembles a workflow-v2 document from a top-level block and stages.
func rpDoc(top, stages string) string {
	return "version: \"2\"\n" + top + "workflows:\n  feature_change:\n    stages:\n" + stages
}

// rpPlanStage is a plan stage with one agent reviewer and the given reviewer
// tail lines (already indented under `reviewers:`).
func rpPlanStage(reviewerTail string) string {
	return `      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: anthropic
` + reviewerTail
}

const rpPersonaSecurity = `reviewer_personas:
  security:
    agent:
      provider: codex
      model: gpt-5.2-codex
    remit:
      path: docs/review/security-remit.md
      severity_cap: medium
`

func TestParseBytes_ReviewerPersonasRoundTrip(t *testing.T) {
	doc := rpDoc(rpPersonaSecurity, rpPlanStage("          personas: [security]\n"))
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	want := spec.ReviewerPersona{
		Agent: spec.AgentReviewer{Provider: "codex", Model: "gpt-5.2-codex"},
		Remit: spec.PersonaRemit{Path: "docs/review/security-remit.md", SeverityCap: "medium"},
	}
	if got := s.ReviewerPersonas["security"]; !reflect.DeepEqual(got, want) {
		t.Errorf("ReviewerPersonas[security] = %+v, want %+v", got, want)
	}
	st := s.Workflows["feature_change"].Stages[0]
	if !reflect.DeepEqual(st.Reviewers.Personas, []string{"security"}) {
		t.Errorf("stage Personas = %v, want [security]", st.Reviewers.Personas)
	}
	// The standard reviewer set is untouched by the attachment: a persona is
	// an EXTRA invocation, never an entry folded into Agents.
	if n := st.Reviewers.AgentCount(); n != 1 {
		t.Errorf("AgentCount = %d, want 1 (personas must not be counted into reviewers.agents)", n)
	}
}

// TestParseBytes_ReviewerPersonasInheritedThroughDefaults proves the resolved
// stage carries an attachment made only in defaults.reviewers — the typed half
// of the corpus's persona-defaults-inherited row.
func TestParseBytes_ReviewerPersonasInheritedThroughDefaults(t *testing.T) {
	doc := `version: "2"
defaults:
  reviewers:
    agents:
      - provider: anthropic
    personas: [security]
` + rpPersonaSecurity + `workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	st := s.Workflows["feature_change"].Stages[0]
	got, err := s.SelectReviewerPersonas(&st)
	if err != nil {
		t.Fatalf("SelectReviewerPersonas: %v", err)
	}
	if len(got) != 1 || got[0].Name != "security" {
		t.Fatalf("selected = %+v, want the inherited security persona", got)
	}
}

// TestReviewerPersonas_RuleOrderAfterReviewConventions pins the rule-order
// contract: each persona rung runs immediately AFTER its review_conventions
// sibling, and declarations run before any stage check. Every case violates
// exactly TWO rules and asserts which one Validate reports.
func TestReviewerPersonas_RuleOrderAfterReviewConventions(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
	}{
		{
			// rung 1 vs rung 1: a bad convention path AND a bad remit path.
			name: "conventions declaration before persona declaration",
			doc: rpDoc(`review_conventions:
  backend:
    path: /docs/conventions/backend.md
reviewer_personas:
  security:
    agent:
      provider: anthropic
    remit:
      path: /docs/review/security-remit.md
`, rpPlanStage("          conventions: [backend]\n          personas: [security]\n")),
			wantPath: "/review_conventions/backend/path",
		},
		{
			// rung 1 vs rung 2: a bad remit path AND an undeclared attachment.
			name: "persona declaration before any stage check",
			doc: rpDoc(`reviewer_personas:
  security:
    agent:
      provider: anthropic
    remit:
      path: /docs/review/security-remit.md
`, rpPlanStage("          personas: [security, ghost]\n")),
			wantPath: "/reviewer_personas/security/remit/path",
		},
		{
			// rung 2 vs rung 2 on ONE stage: an undeclared convention AND an
			// undeclared persona.
			name: "conventions stage check before persona stage check",
			doc: rpDoc(`review_conventions:
  backend:
    path: docs/conventions/backend.md
`+rpPersonaSecurity, rpPlanStage("          conventions: [backend, missing]\n          personas: [security, ghost]\n")),
			wantPath: "/workflows/feature_change/stages/0/reviewers/conventions/1",
		},
		{
			// rung 3 vs rung 3: an unselected convention AND an unattached
			// persona.
			name: "conventions reference check before persona reference check",
			doc: rpDoc(`review_conventions:
  backend:
    path: docs/conventions/backend.md
`+rpPersonaSecurity, rpPlanStage("")),
			wantPath: "/review_conventions/backend",
		},
		{
			// rung 2 vs rung 3: an undeclared attachment AND an unattached
			// persona — the stage check runs inside the workflow loop, the
			// reference check after it.
			name: "persona stage check before persona reference check",
			doc: rpDoc(rpPersonaSecurity+`  audit:
    agent:
      provider: anthropic
    remit:
      path: docs/review/audit-remit.md
`, rpPlanStage("          personas: [security, ghost]\n")),
			wantPath: "/workflows/feature_change/stages/0/reviewers/personas/1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %T %v, want *spec.ValidationError", err, err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("first error at %q (%s), want %q", ve.Path, ve.Message, tc.wantPath)
			}
		})
	}
}

// rpSelectSpec is a hand-built Spec (NOT run through Validate) declaring the
// given personas, for SelectReviewerPersonas.
func rpSelectSpec(personas map[string]spec.ReviewerPersona) *spec.Spec {
	return &spec.Spec{Version: "2", ReviewerPersonas: personas}
}

func rpStage(t spec.StageType, names ...string) *spec.Stage {
	return &spec.Stage{
		ID:   string(t),
		Type: t,
		Reviewers: &spec.ReviewersConfig{
			Agents:   []spec.AgentReviewer{{Provider: "anthropic"}},
			Personas: names,
		},
	}
}

func TestSelectReviewerPersonas_ListOrderAndFields(t *testing.T) {
	s := rpSelectSpec(map[string]spec.ReviewerPersona{
		"alpha": {Agent: spec.AgentReviewer{Provider: "anthropic"}, Remit: spec.PersonaRemit{Path: "a.md"}},
		"zeta": {
			Agent: spec.AgentReviewer{Provider: "codex", Model: "m", ReasoningEffort: "high"},
			Remit: spec.PersonaRemit{Path: "docs/z.md", SeverityCap: spec.ReviewConventionSeverityCapLow},
		},
	})
	for _, st := range []spec.StageType{spec.StageTypePlan, spec.StageTypeImplement} {
		got, err := s.SelectReviewerPersonas(rpStage(st, "zeta", "alpha"))
		if err != nil {
			t.Fatalf("%s: SelectReviewerPersonas: %v", st, err)
		}
		want := []spec.SelectedReviewerPersona{
			{
				Name:            "zeta",
				Agent:           spec.AgentReviewer{Provider: "codex", Model: "m", ReasoningEffort: "high"},
				RemitPath:       "docs/z.md",
				SeverityCap:     "low",
				DeclarationSite: "reviewer_personas.zeta.remit in .fishhawk/workflows.yaml",
			},
			{
				Name:            "alpha",
				Agent:           spec.AgentReviewer{Provider: "anthropic"},
				RemitPath:       "a.md",
				DeclarationSite: "reviewer_personas.alpha.remit in .fishhawk/workflows.yaml",
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: selected =\n%+v\nwant (attachment list order, NOT sorted)\n%+v", st, got, want)
		}
	}
}

func TestSelectReviewerPersonas_EmptyCases(t *testing.T) {
	s := rpSelectSpec(map[string]spec.ReviewerPersona{"a": {Remit: spec.PersonaRemit{Path: "a.md"}}})
	for name, st := range map[string]*spec.Stage{
		"nil stage":        nil,
		"nil reviewers":    {ID: "plan", Type: spec.StageTypePlan},
		"empty attachment": rpStage(spec.StageTypePlan),
	} {
		got, err := s.SelectReviewerPersonas(st)
		if err != nil || got != nil {
			t.Errorf("%s: SelectReviewerPersonas = (%v, %v), want (nil, nil)", name, got, err)
		}
	}
}

// TestSelectReviewerPersonas_FailsClosed covers both fail-closed branches for
// a hand-built spec that skipped Validate, asserting error IDENTITY and that
// NO persona is returned alongside the error.
func TestSelectReviewerPersonas_FailsClosed(t *testing.T) {
	declared := map[string]spec.ReviewerPersona{"security": {Agent: spec.AgentReviewer{Provider: "anthropic"}, Remit: spec.PersonaRemit{Path: "s.md"}}}
	cases := []struct {
		name string
		s    *spec.Spec
		st   *spec.Stage
		want error
	}{
		{"review stage", rpSelectSpec(declared), rpStage(spec.StageTypeReview, "security"), spec.ErrReviewerPersonaStageType},
		{"acceptance stage", rpSelectSpec(declared), rpStage(spec.StageTypeAcceptance, "security"), spec.ErrReviewerPersonaStageType},
		{"undeclared name after a declared one", rpSelectSpec(declared), rpStage(spec.StageTypeImplement, "security", "ghost"), spec.ErrReviewerPersonaUndeclared},
		{"nil spec", nil, rpStage(spec.StageTypePlan, "security"), spec.ErrReviewerPersonaUndeclared},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.s.SelectReviewerPersonas(tc.st)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("returned %+v alongside the error; a fail-closed selection returns no persona", got)
			}
		})
	}
}

// TestValidate_ReviewerPersonaAgentVersionEmptyIsAbsent: an empty
// agent_version on a hand-built persona is "no constraint", exactly as on a
// reviewers.agents[] entry — the syntax check runs only on a non-empty range.
func TestValidate_ReviewerPersonaAgentVersionEmptyIsAbsent(t *testing.T) {
	s := &spec.Spec{
		Version: "2",
		ReviewerPersonas: map[string]spec.ReviewerPersona{
			"security": {Agent: spec.AgentReviewer{Provider: "codex"}, Remit: spec.PersonaRemit{Path: "s.md"}},
		},
		Workflows: map[string]spec.Workflow{"wf": {Stages: []spec.Stage{
			{ID: "plan", Type: spec.StageTypePlan, Executor: spec.Executor{Agent: "claude-code"},
				Produces:  []spec.Produces{{Artifact: "plan", Schema: "standard_v1"}},
				Reviewers: &spec.ReviewersConfig{Agents: []spec.AgentReviewer{{Provider: "anthropic"}}, Personas: []string{"security"}}},
		}}},
	}
	if err := spec.Validate(s); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	s.ReviewerPersonas["security"] = spec.ReviewerPersona{Agent: spec.AgentReviewer{Provider: "codex", AgentVersion: ">=abc"}, Remit: spec.PersonaRemit{Path: "s.md"}}
	var ve *spec.ValidationError
	if err := spec.Validate(s); !errors.As(err, &ve) || ve.Path != "/reviewer_personas/security/agent/agent_version" {
		t.Fatalf("Validate(bad range) = %v, want a ValidationError at /reviewer_personas/security/agent/agent_version", err)
	}
}

// v2Defs returns the embedded workflow-v2 mirror's $defs.
func v2Defs(t *testing.T) map[string]any {
	t.Helper()
	return schemaDefs(t, decodeSchemaMirror(t, "schemas/workflow-v2.schema.json"))
}

func rpObj(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("schema node %v missing or not an object at %q", keys, k)
		}
		cur = next
	}
	return cur
}

// TestAgentReviewerDefIsSharedByAgentsAndPersonas pins the $defs/agent_reviewer
// extraction: reviewers.agents[] items and a persona's agent are the SAME
// definition (so a persona's provider, model and effort mean what they mean on
// a standard reviewer and cannot drift), and its provider enum is the closed
// reviewer-adapter set the frozen v1 major's inline agents items declare — the
// extraction dropped no provider.
func TestAgentReviewerDefIsSharedByAgentsAndPersonas(t *testing.T) {
	defs := v2Defs(t)
	const ref = "#/$defs/agent_reviewer"
	if got := rpObj(t, defs, "reviewers_config", "properties", "agents", "items")["$ref"]; got != ref {
		t.Errorf("reviewers_config.agents.items.$ref = %v, want %s", got, ref)
	}
	if got := rpObj(t, defs, "reviewer_persona", "properties", "agent")["$ref"]; got != ref {
		t.Errorf("reviewer_persona.agent.$ref = %v, want %s", got, ref)
	}
	if got := rpObj(t, defs, "reviewer_persona", "properties", "remit")["$ref"]; got != "#/$defs/persona_remit" {
		t.Errorf("reviewer_persona.remit.$ref = %v, want #/$defs/persona_remit", got)
	}
	v2Enum := rpObj(t, defs, "agent_reviewer", "properties", "provider")["enum"]
	want := []any{"anthropic", "claudecode", "codex"}
	if !reflect.DeepEqual(v2Enum, want) {
		t.Errorf("agent_reviewer provider enum = %v, want %v", v2Enum, want)
	}
	v1 := schemaDefs(t, decodeSchemaMirror(t, "schemas/workflow-v1.schema.json"))
	v1Enum := rpObj(t, v1, "reviewers_config", "properties", "agents", "items", "properties", "provider")["enum"]
	if !reflect.DeepEqual(v2Enum, v1Enum) {
		t.Errorf("v2 agent_reviewer provider enum %v != v1 inline agents provider enum %v", v2Enum, v1Enum)
	}
}

// TestPersonaRemitSeverityCapsMatchConventionConstants holds the remit's
// severity_cap enum to the review-convention constants: the remit reuses the
// conventions' closed set, so the two cannot be allowed to diverge.
func TestPersonaRemitSeverityCapsMatchConventionConstants(t *testing.T) {
	defs := v2Defs(t)
	got := rpObj(t, defs, "persona_remit", "properties", "severity_cap")["enum"]
	want := []any{spec.ReviewConventionSeverityCapLow, spec.ReviewConventionSeverityCapMedium}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("persona_remit.severity_cap enum = %v, want %v", got, want)
	}
	conv := rpObj(t, defs, "review_convention", "properties", "severity_cap")["enum"]
	if !reflect.DeepEqual(got, conv) {
		t.Errorf("persona_remit.severity_cap enum %v != review_convention.severity_cap enum %v", got, conv)
	}
}

// TestReviewerPersonaRemitPathReasons drives every review-convention path
// reason through the persona remit rule, so the remit is held to the SAME rule
// set (not a weaker copy) with the same reason text.
func TestReviewerPersonaRemitPathReasons(t *testing.T) {
	cases := map[string]string{
		"/abs.md":  spec.MsgReviewConventionPathAbsolute,
		`a\b.md`:   spec.MsgReviewConventionPathBackslash,
		"a//b.md":  spec.MsgReviewConventionPathEmptySegment,
		"a/./b.md": fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, "."),
		"../b.md":  fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, ".."),
		"a\tb.md":  fmt.Sprintf(spec.MsgFmtReviewConventionPathControlChar, '\t'),
		"a/ .md":   fmt.Sprintf(spec.MsgFmtReviewConventionPathControlChar, ' '),
	}
	for p, reason := range cases {
		s := &spec.Spec{Version: "2", ReviewerPersonas: map[string]spec.ReviewerPersona{
			"security": {Agent: spec.AgentReviewer{Provider: "anthropic"}, Remit: spec.PersonaRemit{Path: p}},
		}}
		var ve *spec.ValidationError
		err := spec.Validate(s)
		if !errors.As(err, &ve) || ve.Path != "/reviewer_personas/security/remit/path" {
			t.Errorf("path %q: err = %v, want a ValidationError at /reviewer_personas/security/remit/path", p, err)
			continue
		}
		if want := fmt.Sprintf(spec.MsgFmtReviewerPersonaRemitPathInvalid, "security", p, reason); ve.Message != want {
			t.Errorf("path %q: message\n got: %s\nwant: %s", p, ve.Message, want)
		}
	}
}

// TestValidate_ReviewerPersonaReferencedByEscalationOnly pins the widened
// REFERENCE rung (E55.9 / #3754): a persona no stage attaches statically but a
// workflow escalation requires through require.reviewers is referenced, and
// is NOT refused as unreferenced. The fixture is otherwise valid (one
// agent-reviewing plan stage for the escalated persona to join), so the
// escalation loop in validateReviewerPersonasReferenced is the only thing
// that can accept it; the second case is the negative control — the same
// persona with no escalation naming it IS refused, in the reworded message.
func TestValidate_ReviewerPersonaReferencedByEscalationOnly(t *testing.T) {
	escalation := `    escalations:
      - match:
          paths: ["backend/internal/spec/**"]
        require:
          reviewers: [security]
`
	doc := "version: \"2\"\n" + rpPersonaSecurity + "workflows:\n  feature_change:\n" + escalation + "    stages:\n" + rpPlanStage("")
	if _, err := spec.ParseBytes([]byte(doc)); err != nil {
		t.Fatalf("a persona referenced only by an escalation must be accepted: %v", err)
	}

	unreferenced := rpDoc(rpPersonaSecurity, rpPlanStage(""))
	_, err := spec.ParseBytes([]byte(unreferenced))
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want the unreferenced-persona *ValidationError", err)
	}
	if ve.Path != "/reviewer_personas/security" || ve.Message != fmt.Sprintf(spec.MsgFmtReviewerPersonaUnreferenced, "security") {
		t.Errorf("got %s: %s, want the unreferenced rung at /reviewer_personas/security", ve.Path, ve.Message)
	}
}

// TestSelectNamedReviewerPersonas_OrderDedupAndFailsClosed pins the
// escalation-attachment selector: input order (NOT sorted), duplicates
// dropped first-wins, the SAME SelectedReviewerPersona value a static
// attachment of that persona resolves to, an empty / nil input selecting
// nothing, and the fail-closed branch for a hand-built spec — error identity
// via errors.Is and NO partial result.
func TestSelectNamedReviewerPersonas_OrderDedupAndFailsClosed(t *testing.T) {
	s := rpSelectSpec(map[string]spec.ReviewerPersona{
		"alpha": {Agent: spec.AgentReviewer{Provider: "anthropic"}, Remit: spec.PersonaRemit{Path: "a.md"}},
		"zeta": {
			Agent: spec.AgentReviewer{Provider: "codex", Model: "m"},
			Remit: spec.PersonaRemit{Path: "docs/z.md", SeverityCap: spec.ReviewConventionSeverityCapMedium},
		},
	})
	got, err := s.SelectNamedReviewerPersonas([]string{"zeta", "alpha", "zeta"})
	if err != nil {
		t.Fatalf("SelectNamedReviewerPersonas: %v", err)
	}
	static, err := s.SelectReviewerPersonas(rpStage(spec.StageTypeImplement, "zeta", "alpha"))
	if err != nil {
		t.Fatalf("SelectReviewerPersonas: %v", err)
	}
	if !reflect.DeepEqual(got, static) {
		t.Errorf("named selection =\n%+v\nwant the static selection of the same personas (input order, de-duplicated)\n%+v", got, static)
	}

	for name, in := range map[string][]string{"nil": nil, "empty": {}} {
		if got, err := s.SelectNamedReviewerPersonas(in); err != nil || got != nil {
			t.Errorf("%s input: got (%v, %v), want (nil, nil)", name, got, err)
		}
	}

	for name, tc := range map[string]struct {
		s     *spec.Spec
		names []string
	}{
		"undeclared after a declared one": {s, []string{"alpha", "ghost"}},
		"nil spec":                        {nil, []string{"alpha"}},
	} {
		got, err := tc.s.SelectNamedReviewerPersonas(tc.names)
		if !errors.Is(err, spec.ErrReviewerPersonaUndeclared) {
			t.Errorf("%s: err = %v, want errors.Is ErrReviewerPersonaUndeclared", name, err)
		}
		if got != nil {
			t.Errorf("%s: returned %+v alongside the error; a fail-closed selection returns no persona", name, got)
		}
	}
}

// Decision record (ADR-084 D4(b) / binding rule 4 / E78.5 / #3756). The
// corpus carries the persona-decision-record-* rows (valid; index-absent and
// unknown-field schema rejections; the absolute-index semantic rejection with
// its exact text in BOTH modules). This block pins what a single-rule row
// cannot: the typed round-trip, the selected value on BOTH attachment routes,
// every path reason through the new rung, the rung's position between the
// remit rule and the agent_version rule, and the schema facts.

const rpPersonaArchitect = `reviewer_personas:
  architect:
    agent:
      provider: anthropic
    remit:
      path: docs/review/architect-remit.md
    decision_record:
      index: docs/adr/index.json
`

func TestParseBytes_ReviewerPersonaDecisionRecordRoundTrip(t *testing.T) {
	doc := rpDoc(rpPersonaArchitect, rpPlanStage("          personas: [architect]\n"))
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	want := spec.ReviewerPersona{
		Agent:          spec.AgentReviewer{Provider: "anthropic"},
		Remit:          spec.PersonaRemit{Path: "docs/review/architect-remit.md"},
		DecisionRecord: &spec.PersonaDecisionRecord{Index: "docs/adr/index.json"},
	}
	if got := s.ReviewerPersonas["architect"]; !reflect.DeepEqual(got, want) {
		t.Errorf("ReviewerPersonas[architect] = %+v (decision_record %+v), want %+v", got, got.DecisionRecord, want)
	}

	// A persona WITHOUT decision_record decodes to a nil pointer — the
	// additive-optional field changes nothing for an existing declaration.
	s, err = spec.ParseBytes([]byte(rpDoc(rpPersonaSecurity, rpPlanStage("          personas: [security]\n"))))
	if err != nil {
		t.Fatalf("ParseBytes(no decision_record): %v", err)
	}
	if dr := s.ReviewerPersonas["security"].DecisionRecord; dr != nil {
		t.Errorf("security.DecisionRecord = %+v, want nil when undeclared", dr)
	}
}

// TestSelectReviewerPersonas_DecisionRecordFieldsOnBothRoutes asserts the
// static attachment (SelectReviewerPersonas) and the escalation attachment
// (SelectNamedReviewerPersonas) carry the SAME DecisionRecordIndex and
// DecisionRecordDeclarationSite, and that a persona declaring no
// decision_record carries neither on either route (the review loop keys its
// extra read on DecisionRecordIndex != "").
func TestSelectReviewerPersonas_DecisionRecordFieldsOnBothRoutes(t *testing.T) {
	s := rpSelectSpec(map[string]spec.ReviewerPersona{
		"architect": {
			Agent:          spec.AgentReviewer{Provider: "anthropic"},
			Remit:          spec.PersonaRemit{Path: "docs/review/architect-remit.md"},
			DecisionRecord: &spec.PersonaDecisionRecord{Index: "docs/adr/index.json"},
		},
		"security": {Agent: spec.AgentReviewer{Provider: "codex"}, Remit: spec.PersonaRemit{Path: "s.md"}},
	})
	static, err := s.SelectReviewerPersonas(rpStage(spec.StageTypePlan, "architect", "security"))
	if err != nil {
		t.Fatalf("SelectReviewerPersonas: %v", err)
	}
	named, err := s.SelectNamedReviewerPersonas([]string{"architect", "security"})
	if err != nil {
		t.Fatalf("SelectNamedReviewerPersonas: %v", err)
	}
	for route, got := range map[string][]spec.SelectedReviewerPersona{"static": static, "escalation": named} {
		if len(got) != 2 {
			t.Fatalf("%s: selected %d personas, want 2", route, len(got))
		}
		arch, sec := got[0], got[1]
		if arch.DecisionRecordIndex != "docs/adr/index.json" {
			t.Errorf("%s: architect.DecisionRecordIndex = %q, want docs/adr/index.json", route, arch.DecisionRecordIndex)
		}
		if want := "reviewer_personas.architect.decision_record.index in .fishhawk/workflows.yaml"; arch.DecisionRecordDeclarationSite != want {
			t.Errorf("%s: architect.DecisionRecordDeclarationSite = %q, want %q", route, arch.DecisionRecordDeclarationSite, want)
		}
		if want := fmt.Sprintf(spec.DeclarationSiteFmtReviewerPersonaDecisionRecord, "architect"); arch.DecisionRecordDeclarationSite != want {
			t.Errorf("%s: DecisionRecordDeclarationSite %q is not formatted by DeclarationSiteFmtReviewerPersonaDecisionRecord (%q)", route, arch.DecisionRecordDeclarationSite, want)
		}
		if sec.DecisionRecordIndex != "" || sec.DecisionRecordDeclarationSite != "" {
			t.Errorf("%s: security (no decision_record) carries index %q / site %q, want both empty", route, sec.DecisionRecordIndex, sec.DecisionRecordDeclarationSite)
		}
	}
	if !reflect.DeepEqual(static, named) {
		t.Errorf("static selection\n%+v\n!= escalation selection\n%+v", static, named)
	}
}

// TestReviewerPersonaDecisionRecordIndexPathReasons drives every
// review-convention path reason through the decision_record.index rung, so
// the index is held to the SAME rule set as a remit with the same reason
// text. The remit path is valid in every case, so ONLY the new rung can
// refuse the document.
func TestReviewerPersonaDecisionRecordIndexPathReasons(t *testing.T) {
	cases := map[string]string{
		"/abs.json":  spec.MsgReviewConventionPathAbsolute,
		`a\b.json`:   spec.MsgReviewConventionPathBackslash,
		"a//b.json":  spec.MsgReviewConventionPathEmptySegment,
		"a/./b.json": fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, "."),
		"../b.json":  fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, ".."),
		"a\tb.json":  fmt.Sprintf(spec.MsgFmtReviewConventionPathControlChar, '\t'),
	}
	for p, reason := range cases {
		s := &spec.Spec{Version: "2", ReviewerPersonas: map[string]spec.ReviewerPersona{
			"architect": {
				Agent:          spec.AgentReviewer{Provider: "anthropic"},
				Remit:          spec.PersonaRemit{Path: "docs/review/architect-remit.md"},
				DecisionRecord: &spec.PersonaDecisionRecord{Index: p},
			},
		}}
		var ve *spec.ValidationError
		err := spec.Validate(s)
		if !errors.As(err, &ve) || ve.Path != "/reviewer_personas/architect/decision_record/index" {
			t.Errorf("index %q: err = %v, want a ValidationError at /reviewer_personas/architect/decision_record/index", p, err)
			continue
		}
		if want := fmt.Sprintf(spec.MsgFmtReviewerPersonaDecisionRecordIndexPathInvalid, "architect", p, reason); ve.Message != want {
			t.Errorf("index %q: message\n got: %s\nwant: %s", p, ve.Message, want)
		}
	}
}

// TestReviewerPersonaDecisionRecordRungOrder pins the new rung's position in
// rule-order rung 1: AFTER the remit path rule and BEFORE the agent_version
// range rule. Each case violates exactly two rules on one persona.
func TestReviewerPersonaDecisionRecordRungOrder(t *testing.T) {
	cases := []struct {
		name     string
		persona  spec.ReviewerPersona
		wantPath string
	}{
		{
			name: "remit path before decision_record.index",
			persona: spec.ReviewerPersona{
				Agent:          spec.AgentReviewer{Provider: "anthropic"},
				Remit:          spec.PersonaRemit{Path: "/abs-remit.md"},
				DecisionRecord: &spec.PersonaDecisionRecord{Index: "/abs-index.json"},
			},
			wantPath: "/reviewer_personas/architect/remit/path",
		},
		{
			name: "decision_record.index before agent_version",
			persona: spec.ReviewerPersona{
				Agent:          spec.AgentReviewer{Provider: "codex", AgentVersion: "bogus"},
				Remit:          spec.PersonaRemit{Path: "docs/review/architect-remit.md"},
				DecisionRecord: &spec.PersonaDecisionRecord{Index: "/abs-index.json"},
			},
			wantPath: "/reviewer_personas/architect/decision_record/index",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &spec.Spec{Version: "2", ReviewerPersonas: map[string]spec.ReviewerPersona{"architect": tc.persona}}
			var ve *spec.ValidationError
			if err := spec.Validate(s); !errors.As(err, &ve) || ve.Path != tc.wantPath {
				t.Fatalf("Validate = %v, want the first error at %s", err, tc.wantPath)
			}
		})
	}
}

// TestPersonaDecisionRecordSchemaShape pins the schema facts the typed decode
// rests on: reviewer_persona.decision_record $refs $defs/persona_decision_record,
// which is additionalProperties:false, requires index, and is NOT required on
// the persona (additive-optional — no major bump).
func TestPersonaDecisionRecordSchemaShape(t *testing.T) {
	defs := v2Defs(t)
	if got := rpObj(t, defs, "reviewer_persona", "properties", "decision_record")["$ref"]; got != "#/$defs/persona_decision_record" {
		t.Errorf("reviewer_persona.decision_record.$ref = %v, want #/$defs/persona_decision_record", got)
	}
	persona := rpObj(t, defs, "reviewer_persona")
	if got, want := persona["required"], []any{"agent", "remit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reviewer_persona.required = %v, want %v (decision_record must stay optional)", got, want)
	}
	dr := rpObj(t, defs, "persona_decision_record")
	if dr["additionalProperties"] != false {
		t.Errorf("persona_decision_record.additionalProperties = %v, want false", dr["additionalProperties"])
	}
	if got, want := dr["required"], []any{"index"}; !reflect.DeepEqual(got, want) {
		t.Errorf("persona_decision_record.required = %v, want %v", got, want)
	}
	props := rpObj(t, dr, "properties")
	if len(props) != 1 {
		t.Errorf("persona_decision_record declares %d properties, want exactly index (PersonaDecisionRecord is in lockstep)", len(props))
	}
}
