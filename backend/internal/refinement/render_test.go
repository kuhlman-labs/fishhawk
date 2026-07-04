package refinement

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// equivalentChildRequest builds the FilingRequest RenderChild routes through
// Apply, so the byte-compat test can call Apply directly with the SAME inputs
// and compare. It mirrors RenderChild's construction exactly (feature type,
// the same sections/labels/title-vars/relations).
func equivalentChildRequest(child ChildDraft, ordinal int, opts RenderOptions) workmgmt.FilingRequest {
	return workmgmt.FilingRequest{
		Type:    "feature",
		Summary: child.Summary,
		Sections: map[string]string{
			"Proposal":            child.Proposal,
			"Done-means":          child.DoneMeans,
			"Acceptance criteria": bulletize(child.AcceptanceCriteria),
		},
		Labels: child.Labels,
		TitleVars: map[string]string{
			"epic": opts.epicNumber(),
			"n":    strconv.Itoa(ordinal),
		},
		Relations: workmgmt.Relations{ParentEpic: opts.parentEpicRef()},
	}
}

func TestRenderChild_ConventionsComplete(t *testing.T) {
	conv := workmgmt.Default()
	child := ChildDraft{
		Summary:            "wire the widget",
		Proposal:           "connect A to B",
		DoneMeans:          "A talks to B",
		AcceptanceCriteria: []string{"A calls B", "B replies"},
		// Only area supplied — autonomy:medium must be DEFAULTED in.
		Labels: []string{"area:backend"},
	}
	item, err := RenderChild(child, 1, RenderOptions{}, conv)
	if err != nil {
		t.Fatalf("RenderChild: %v", err)
	}

	if item.Title != "[EX.1] wire the widget" {
		t.Errorf("title = %q, want '[EX.1] wire the widget'", item.Title)
	}
	if !containsLabel(item.Classification.Labels, "type:feature") {
		t.Errorf("labels %v missing type:feature", item.Classification.Labels)
	}
	if !containsLabel(item.Classification.Labels, "autonomy:medium") {
		t.Errorf("labels %v missing defaulted autonomy:medium", item.Classification.Labels)
	}
	if len(item.Classification.MissingLabelNamespaces) != 0 {
		t.Errorf("missing label namespaces = %v, want none (area + autonomy present)", item.Classification.MissingLabelNamespaces)
	}

	// Skeleton section order, with Acceptance criteria populated as bullets and
	// the optional 'Where to look' omitted.
	body := item.Body
	assertOrder(t, body, "## Summary", "## Proposal", "## Done-means", "## Acceptance criteria", "## Notes", "## Relations")
	if strings.Contains(body, "## Where to look") {
		t.Errorf("body renders optional 'Where to look' section that was not supplied:\n%s", body)
	}
	if !strings.Contains(body, "## Acceptance criteria\n\n- A calls B\n- B replies") {
		t.Errorf("Acceptance criteria section not bulleted as expected:\n%s", body)
	}
}

// TestRenderChild_ByteCompatNoDeps is binding condition (a): a child with ZERO
// depends_on edges renders a body BYTE-IDENTICAL to a direct workmgmt.Apply
// call for the equivalent FilingRequest.
func TestRenderChild_ByteCompatNoDeps(t *testing.T) {
	conv := workmgmt.Default()
	child := ChildDraft{
		Summary:            "no-deps child",
		Proposal:           "does a thing",
		DoneMeans:          "thing done",
		AcceptanceCriteria: []string{"ok"},
		Labels:             []string{"area:backend", "autonomy:low"},
		// DependsOn empty.
	}
	opts := RenderOptions{}

	rendered, err := RenderChild(child, 2, opts, conv)
	if err != nil {
		t.Fatalf("RenderChild: %v", err)
	}
	applyItem, _, err := workmgmt.Apply(equivalentChildRequest(child, 2, opts), conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rendered.Body != applyItem.Body {
		t.Errorf("no-deps child body diverged from a direct Apply call.\nrendered:\n%q\napply:\n%q", rendered.Body, applyItem.Body)
	}
}

// TestRenderChild_ByteCompatWithDeps is binding condition (b): a child WITH
// depends_on edges renders the Apply output PLUS exactly the appended
// 'Depends on: #N' marker line(s) and nothing else. The marker is never folded
// or dropped to force equality.
func TestRenderChild_ByteCompatWithDeps(t *testing.T) {
	conv := workmgmt.Default()
	child := ChildDraft{
		Summary:            "dependent child",
		Proposal:           "does a thing after others",
		DoneMeans:          "thing done",
		AcceptanceCriteria: []string{"ok"},
		Labels:             []string{"area:backend", "autonomy:low"},
		DependsOn:          []int{1, 3},
	}
	opts := RenderOptions{}

	rendered, err := RenderChild(child, 2, opts, conv)
	if err != nil {
		t.Fatalf("RenderChild: %v", err)
	}
	// The equivalent Apply request omits depends_on entirely (the marker is
	// appended by the renderer, not by Apply).
	applyItem, _, err := workmgmt.Apply(equivalentChildRequest(child, 2, opts), conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := strings.TrimRight(applyItem.Body, "\n") + "\n\n" + "Depends on: #1, #3"
	if rendered.Body != want {
		t.Errorf("dependent child body != Apply output + marker (and nothing else).\ngot:\n%q\nwant:\n%q", rendered.Body, want)
	}
	// And the Apply body prefix is preserved verbatim (marker is additive).
	if !strings.HasPrefix(rendered.Body, strings.TrimRight(applyItem.Body, "\n")) {
		t.Errorf("Apply body was mutated, not merely extended:\n%q", rendered.Body)
	}
}

func TestRenderChild_ApplyErrorPropagates(t *testing.T) {
	conv := workmgmt.Default()
	// An empty summary makes workmgmt.Apply fail closed (Summary is required);
	// RenderChild must surface that error, not swallow it.
	_, err := RenderChild(ChildDraft{Summary: "", Proposal: "p"}, 1, RenderOptions{}, conv)
	if err == nil {
		t.Fatal("RenderChild swallowed an Apply error for an empty summary")
	}
	if !strings.Contains(err.Error(), "Summary is required") {
		t.Errorf("error %q does not carry the Apply failure", err.Error())
	}
}

func TestRenderDraft_PropagatesChildError(t *testing.T) {
	conv := workmgmt.Default()
	draft := validDraft()
	draft.Children[0].Summary = "" // makes the child's Apply fail
	if _, err := RenderDraft(draft, RenderOptions{}, conv); err == nil {
		t.Fatal("RenderDraft swallowed a child render error")
	}
}

func TestRenderEpic_FoldsOutOfScope(t *testing.T) {
	conv := workmgmt.Default()
	epic := EpicSpec{Summary: "stand up X", Scope: "X and wiring", OutOfScope: "the Y subsystem"}

	item, err := RenderEpic(epic, RenderOptions{}, conv)
	if err != nil {
		t.Fatalf("RenderEpic: %v", err)
	}
	// Numbering seeded [0] -> epic number 1.
	if item.Title != "[E1] stand up X" {
		t.Errorf("title = %q, want '[E1] stand up X'", item.Title)
	}
	if !containsLabel(item.Classification.Labels, "epic") {
		t.Errorf("labels %v missing 'epic'", item.Classification.Labels)
	}
	// OutOfScope folded into the Scope section (epic skeleton has no
	// out-of-scope section).
	if !strings.Contains(item.Body, "## Scope\n\nX and wiring\n\n### Out of scope\n\nthe Y subsystem") {
		t.Errorf("out-of-scope not folded into Scope section:\n%s", item.Body)
	}
	// Folded as an h3 sub-heading (### Out of scope), NOT a top-level ## section
	// (which Apply's validateSections would reject).
	if strings.Contains(item.Body, "\n## Out of scope\n") {
		t.Errorf("out-of-scope rendered as its own top-level section (would fail Apply's validateSections):\n%s", item.Body)
	}
}

func TestRenderDraft_EpicThenChildren(t *testing.T) {
	conv := workmgmt.Default()
	draft := validDraft()
	draft.Children[1].DependsOn = []int{1}

	items, err := RenderDraft(draft, RenderOptions{}, conv)
	if err != nil {
		t.Fatalf("RenderDraft: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("RenderDraft returned %d items, want 3 (epic + 2 children)", len(items))
	}
	if items[0].Type != "epic" {
		t.Errorf("items[0].Type = %q, want epic", items[0].Type)
	}
	if items[1].Title != "[EX.1] child one" || items[2].Title != "[EX.2] child two" {
		t.Errorf("child titles = %q, %q; want '[EX.1] child one', '[EX.2] child two'", items[1].Title, items[2].Title)
	}
	// The dependent child carries the draft-ordinal marker.
	if !strings.Contains(items[2].Body, "Depends on: #1") {
		t.Errorf("child two body missing 'Depends on: #1' marker:\n%s", items[2].Body)
	}
}

// TestFilingRequestForChild_SentinelParity is the builder-parity pin: the
// exported FilingRequestForChild builder, called with the preview sentinel
// epic/parent refs and nil dependsOnRefs, reproduces EXACTLY the FilingRequest
// the pre-extraction RenderChild built (equivalentChildRequest) — so "what was
// previewed is what files" holds by construction and the preview stays
// byte-identical across the extraction.
func TestFilingRequestForChild_SentinelParity(t *testing.T) {
	child := ChildDraft{
		Summary:            "dependent child",
		Proposal:           "does a thing after others",
		DoneMeans:          "thing done",
		AcceptanceCriteria: []string{"ok"},
		Labels:             []string{"area:backend", "autonomy:low"},
		DependsOn:          []int{1, 3},
	}
	opts := RenderOptions{}
	got := FilingRequestForChild(child, 2, opts.epicNumber(), opts.parentEpicRef(), nil)
	want := equivalentChildRequest(child, 2, opts)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilingRequestForChild sentinel parity mismatch.\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestFilingRequestForEpic_SentinelParity pins the epic builder: with the
// preview sentinel existing-numbers seed it reproduces the epic FilingRequest
// RenderEpic built pre-extraction (epic type, summary, folded scope, seeded
// numbering).
func TestFilingRequestForEpic_SentinelParity(t *testing.T) {
	epic := EpicSpec{Summary: "stand up X", Scope: "X and wiring", OutOfScope: "the Y subsystem"}
	got := FilingRequestForEpic(epic, sentinelEpicExistingNumbers)
	want := workmgmt.FilingRequest{
		Type:    "epic",
		Summary: epic.Summary,
		Sections: map[string]string{
			"Summary": epic.Summary,
			"Scope":   "X and wiring\n\n### Out of scope\n\nthe Y subsystem",
		},
		ExistingNumbers: sentinelEpicExistingNumbers,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilingRequestForEpic sentinel parity mismatch.\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestFilingRequestForChild_RealValues shows the executor path: real epic
// number, real `#N` parent ref, and real `#N` depends_on refs land on the
// request so the github provider's ensureDependsOnMarker renders the marker.
func TestFilingRequestForChild_RealValues(t *testing.T) {
	child := ChildDraft{
		Summary: "c", Proposal: "p", DoneMeans: "d",
		AcceptanceCriteria: []string{"ok"}, Labels: []string{"area:backend"},
		DependsOn: []int{1},
	}
	got := FilingRequestForChild(child, 3, "34", "#1601", []string{"#1607"})
	if got.TitleVars["epic"] != "34" || got.TitleVars["n"] != "3" {
		t.Errorf("title vars = %v, want epic=34 n=3", got.TitleVars)
	}
	if got.Relations.ParentEpic != "#1601" {
		t.Errorf("parent epic = %q, want #1601", got.Relations.ParentEpic)
	}
	if len(got.Relations.DependsOn) != 1 || got.Relations.DependsOn[0] != "#1607" {
		t.Errorf("depends_on = %v, want [#1607]", got.Relations.DependsOn)
	}
}

// TestFilingRequestForChild_OmitsEpicOnEmpty guards the omit-on-empty contract
// the #1644 fix depends on: the executor passes epicNumber "" so the {epic}
// title var is OMITTED entirely, letting the server-side deriveEpicTitleVar fill
// in the epic's discovered ordinal from the parent-epic [E<n>] title. The `#N`
// parent-epic RELATION and depends_on refs are still set from the epic ISSUE
// number the executor supplies.
func TestFilingRequestForChild_OmitsEpicOnEmpty(t *testing.T) {
	child := ChildDraft{
		Summary: "c", Proposal: "p", DoneMeans: "d",
		AcceptanceCriteria: []string{"ok"}, Labels: []string{"area:backend"},
		DependsOn: []int{1},
	}
	got := FilingRequestForChild(child, 3, "", "#1601", []string{"#1607"})
	if _, ok := got.TitleVars["epic"]; ok {
		t.Errorf("title vars = %v, want NO epic key on empty epicNumber", got.TitleVars)
	}
	if got.TitleVars["n"] != "3" {
		t.Errorf("title vars = %v, want n=3", got.TitleVars)
	}
	if got.Relations.ParentEpic != "#1601" {
		t.Errorf("parent epic = %q, want #1601 (issue-number relation still set)", got.Relations.ParentEpic)
	}
	if len(got.Relations.DependsOn) != 1 || got.Relations.DependsOn[0] != "#1607" {
		t.Errorf("depends_on = %v, want [#1607]", got.Relations.DependsOn)
	}
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// assertOrder checks each marker appears in body in the given order.
func assertOrder(t *testing.T, body string, markers ...string) {
	t.Helper()
	prev := 0
	for _, m := range markers {
		idx := strings.Index(body[prev:], m)
		if idx < 0 {
			t.Errorf("marker %q not found after position %d in body:\n%s", m, prev, body)
			return
		}
		prev += idx + len(m)
	}
}
