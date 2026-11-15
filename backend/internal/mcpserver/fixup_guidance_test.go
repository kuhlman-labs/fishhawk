package mcpserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- E106.27 / #5212: fix-up routing guidance on the served surfaces ---
//
// Every assertion reads the text an operator actually receives: the tool
// descriptions and output schemas SERVED over an in-memory MCP ListTools, or
// the reflected jsonschema tag the SDK serves beside a field. Anchors are
// lowercased and whitespace-flattened tokens, so a copy-edit does not redden a
// test while removing the rule does.

// fixupGuidanceTools registers the full tool set and returns the served tools
// by name.
func fixupGuidanceTools(t *testing.T) map[string]*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

// fixupGuidanceFlatten lowercases s and folds every whitespace run to one
// space, so a hard wrap in a description cannot hide an anchor.
func fixupGuidanceFlatten(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// fixupGuidanceDescription returns the flattened served description of name.
func fixupGuidanceDescription(t *testing.T, tools map[string]*mcp.Tool, name string) string {
	t.Helper()
	tool, ok := tools[name]
	if !ok || tool.Description == "" {
		t.Fatalf("%s not registered over ListTools", name)
	}
	return fixupGuidanceFlatten(tool.Description)
}

// fixupGuidanceOutputPropertyDescription returns the flattened description of
// one top-level property of name's SERVED output schema.
func fixupGuidanceOutputPropertyDescription(t *testing.T, tools map[string]*mcp.Tool, name, property string) string {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("%s not registered over ListTools", name)
	}
	raw, err := json.Marshal(tool.OutputSchema)
	if err != nil {
		t.Fatalf("marshal %s output schema: %v", name, err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode %s output schema: %v", name, err)
	}
	prop, ok := schema.Properties[property]
	if !ok {
		t.Fatalf("%s output schema has no %q property: %s", name, property, raw)
	}
	return fixupGuidanceFlatten(prop.Description)
}

// fixupGuidanceSlice returns desc from the first occurrence of start up to the
// first occurrence of end after it, failing when start is absent so nothing
// passes vacuously.
func fixupGuidanceSlice(t *testing.T, desc, start, end string) string {
	t.Helper()
	i := strings.Index(desc, start)
	if i < 0 {
		t.Fatalf("description has no %q section:\n%s", start, desc)
	}
	rest := desc[i:]
	if j := strings.Index(rest[len(start):], end); j >= 0 {
		return rest[:len(start)+j]
	}
	return rest
}

// TestFixupStage_DescriptionStatesRoutingGuidance pins one anchor per folded
// rule on the SERVED fishhawk_fixup_stage description, the ADR-110 check over
// the new paragraphs, and the next_steps / warnings output-schema descriptions.
func TestFixupStage_DescriptionStatesRoutingGuidance(t *testing.T) {
	tools := fixupGuidanceTools(t)
	desc := fixupGuidanceDescription(t, tools, "fishhawk_fixup_stage")

	for _, tc := range []struct{ rule, anchor string }{
		{"R2", "fishhawk_defer_concern"},
		{"R2", "re-opens a settled acceptance stage"},
		{"R5", "one substantive concern per pass"},
		{"R7", "counterfactual"},
		{"R11", "acceptance_triage_decided"},
		{"R13", "alone"},
		{"R14", "approval"},
		{"R3", "dated constraints block"},
		{"R4", "do not repeat the same route"},
		{"J2", "once the cause is fixed"},
		{"R8/R9", "fixup_trigger_sequence"},
		{"R8/R9", "expected_reviewers"},
		{"R12", "not an executed reproduction"},
	} {
		if !strings.Contains(desc, tc.anchor) {
			t.Errorf("%s: fishhawk_fixup_stage description lacks %q", tc.rule, tc.anchor)
		}
	}

	// R4: the "do not repeat" advice is tied to the NO-COMMIT refund in the
	// same sentence, so it is never read as applying to a crash refund.
	sameSentence := false
	for _, sentence := range strings.Split(desc, ". ") {
		if strings.Contains(sentence, "do not repeat the same route") && strings.Contains(sentence, "no commit") {
			sameSentence = true
		}
	}
	if !sameSentence {
		t.Errorf("R4: no sentence ties 'do not repeat the same route' to a 'no commit' refund:\n%s", desc)
	}

	// ADR-110: the new paragraphs name product mechanisms only.
	for _, section := range []string{
		fixupGuidanceSlice(t, desc, "sizing, cost and follow-through", "returns the re-opened stage row"),
		fixupGuidanceSlice(t, desc, "what a refund means depends on its shape", "every routed concern reaches the agent"),
	} {
		for _, banned := range []string{"kuhlman-labs", "scripts/", ".fishhawk/"} {
			if strings.Contains(section, banned) {
				t.Errorf("ADR-110: new fishhawk_fixup_stage guidance names a repository-specific value %q:\n%s", banned, section)
			}
		}
	}

	// J7 + A2: next_steps says it is ordered, why it is plural, and names
	// every terminal review kind the wait counts.
	nextSteps := fixupGuidanceOutputPropertyDescription(t, tools, "fishhawk_fixup_stage", "next_steps")
	for _, want := range []string{"ordered", "next_step", "implement_reviewed", "implement_review_failed", "implement_review_skipped", "fixup_trigger_sequence"} {
		if !strings.Contains(nextSteps, want) {
			t.Errorf("next_steps output-schema description lacks %q:\n%s", want, nextSteps)
		}
	}

	// A3: the warnings description and the tool description name the
	// mixed-severity advisory and no longer claim "only pr_body".
	warnings := fixupGuidanceOutputPropertyDescription(t, tools, "fishhawk_fixup_stage", "warnings")
	if !strings.Contains(warnings, "mixed-severity") {
		t.Errorf("warnings output-schema description does not name the mixed-severity advisory:\n%s", warnings)
	}
	if strings.Contains(warnings, "present only when such an instruction was routed") {
		t.Errorf("warnings output-schema description still claims it is present only for a pr_body instruction:\n%s", warnings)
	}
	if !strings.Contains(desc, "mixed-severity advisory") || strings.Contains(desc, `"warnings" is absent otherwise`) {
		t.Errorf("fishhawk_fixup_stage description does not name the mixed-severity advisory beside the pr_body warning:\n%s", desc)
	}
}

// TestFixupStage_OperatorEvidenceSchema_NamesCloseOutAndDroppedItem pins R1
// (the operator closes an operator_evidence concern) and R6 (re-routing a
// dropped item) on the reflected operator_evidence jsonschema tag.
func TestFixupStage_OperatorEvidenceSchema_NamesCloseOutAndDroppedItem(t *testing.T) {
	field, ok := reflect.TypeOf(FixupStageInput{}).FieldByName("OperatorEvidence")
	if !ok {
		t.Fatal("FixupStageInput has no OperatorEvidence field")
	}
	tag := fixupGuidanceFlatten(field.Tag.Get("jsonschema"))
	for _, want := range []string{"fishhawk_resolve_concerns", "fishhawk_waive_concern", "fishhawk_defer_concern", "dropped", "self-report"} {
		if !strings.Contains(tag, want) {
			t.Errorf("operator_evidence jsonschema lacks %q:\n%s", want, tag)
		}
	}
}

// TestFixupGuidance_GateViewAndAwaitReviewStateConfirmedIsNotAReproduction
// pins R12 and R1 on the served gate-view and await-review descriptions.
func TestFixupGuidance_GateViewAndAwaitReviewStateConfirmedIsNotAReproduction(t *testing.T) {
	tools := fixupGuidanceTools(t)

	gv := fixupGuidanceDescription(t, tools, "fishhawk_get_gate_view")
	if !strings.Contains(gv, "not an executed reproduction") {
		t.Errorf("fishhawk_get_gate_view does not say a confirmed resolution is not an executed reproduction")
	}
	// R1: the operator_evidence_routed dispute names the close-out verb in
	// the SAME bullet, not merely somewhere in the description.
	i := strings.Index(gv, "operator_evidence_routed")
	if i < 0 {
		t.Fatal("fishhawk_get_gate_view does not document the operator_evidence_routed veto")
	}
	window := gv[i:]
	if len(window) > 400 {
		window = window[:400]
	}
	for _, want := range []string{"fishhawk_resolve_concerns", "fishhawk_waive_concern", "fishhawk_defer_concern"} {
		if !strings.Contains(window, want) {
			t.Errorf("fishhawk_get_gate_view operator_evidence_routed bullet does not name %q:\n%s", want, window)
		}
	}

	ar := fixupGuidanceDescription(t, tools, "fishhawk_await_review")
	for _, want := range []string{"not an executed reproduction", "operator_evidence"} {
		if !strings.Contains(ar, want) {
			t.Errorf("fishhawk_await_review description lacks %q", want)
		}
	}
}

// TestFixupGuidance_FixupNoChangesSchemaSaysDoNotRepeat pins R4 on the
// reflected RunStageOutput.FixupNoChanges jsonschema tag.
func TestFixupGuidance_FixupNoChangesSchemaSaysDoNotRepeat(t *testing.T) {
	field, ok := reflect.TypeOf(RunStageOutput{}).FieldByName("FixupNoChanges")
	if !ok {
		t.Fatal("RunStageOutput has no FixupNoChanges field")
	}
	tag := fixupGuidanceFlatten(field.Tag.Get("jsonschema"))
	for _, want := range []string{"do not repeat the same route", "fishhawk_vouch_commit"} {
		if !strings.Contains(tag, want) {
			t.Errorf("fixup_no_changes jsonschema lacks %q:\n%s", want, tag)
		}
	}
}
