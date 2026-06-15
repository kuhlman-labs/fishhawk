package workmgmt

import (
	"errors"
	"strings"
	"testing"
)

// testConventions returns the shipped default — the apply tests exercise
// each seeded type (feature/bug/chore/adr) against real conventions.
func testConventions(t *testing.T) Conventions {
	t.Helper()
	return Default()
}

func TestApply_FeatureRendersTitleLabelsAndStatus(t *testing.T) {
	conv := testConventions(t)
	item, num, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "do the thing",
		Body:      "## Summary\n\ndo the thing\n",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Labels:    []string{"area:server"},
		Relations: Relations{ParentEpic: "#1005"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 0 {
		t.Errorf("feature is not numbered, got number %d", num)
	}
	if want := "[E22.7] do the thing"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
	// default_labels first, then caller labels.
	if got := strings.Join(item.Classification.Labels, ","); got != "type:feature,area:server" {
		t.Errorf("labels = %q", got)
	}
	if item.Classification.Complexity != "medium" {
		t.Errorf("complexity = %q, want medium (type default)", item.Classification.Complexity)
	}
	if item.BoardPlacement.Status != "Backlog" {
		t.Errorf("status = %q, want Backlog", item.BoardPlacement.Status)
	}
	if item.Relations.ParentEpic != "#1005" {
		t.Errorf("parent epic not carried: %q", item.Relations.ParentEpic)
	}
}

func TestApply_ADRAllocatesNextNumberAndRendersPrefix(t *testing.T) {
	conv := testConventions(t)
	item, num, err := Apply(FilingRequest{
		Type:            "adr",
		Summary:         "use postgres",
		Body:            "## Context\n\n…\n",
		ExistingNumbers: []int{34, 12, 35},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 36 {
		t.Errorf("next ADR number = %d, want 36", num)
	}
	if want := "[ADR-36] use postgres"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

func TestApply_ADRZeroPadsNumberToWidth(t *testing.T) {
	// pad is an opt-in numbering knob (the shipped default does not pad).
	// Build an independent conventions copy with pad: 3 to exercise the
	// padding branch — Default() returns shared state, so we must not mutate
	// its aliased *Numbering. Existing numbers up to 40 -> next is 41,
	// rendered [ADR-041], not [ADR-41].
	conv := testConventions(t)
	adr := conv.Types["adr"]
	padded := *adr.Numbering
	padded.Pad = 3
	adr.Numbering = &padded
	types := make(map[string]ItemType, len(conv.Types))
	for k, v := range conv.Types {
		types[k] = v
	}
	types["adr"] = adr
	conv.Types = types
	existing := make([]int, 40)
	for i := range existing {
		existing[i] = i + 1
	}
	item, num, err := Apply(FilingRequest{
		Type:            "adr",
		Summary:         "pad the number",
		Body:            "## Context\n\n…\n",
		ExistingNumbers: existing,
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 41 {
		t.Errorf("next ADR number = %d, want 41", num)
	}
	if want := "[ADR-041] pad the number"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

func TestApply_ADRFirstNumberIsOne(t *testing.T) {
	conv := testConventions(t)
	item, num, err := Apply(FilingRequest{
		Type:    "adr",
		Summary: "first decision",
		Body:    "## Context\n\n…\n",
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 1 {
		t.Errorf("first ADR number = %d, want 1", num)
	}
	if want := "[ADR-1] first decision"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

func TestApply_ChoreAssemblesBodyFromSkeleton(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "chore",
		Summary:   "bump deps",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary":    "bump the pinned tools",
			"Done-means": "CI green on the bump PR",
		},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if item.Title != "[E22.7] bump deps" {
		t.Errorf("title = %q, want [E22.7] bump deps", item.Title)
	}
	for _, want := range []string{"## Summary", "bump the pinned tools", "## Done-means", "CI green on the bump PR"} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("assembled body missing %q:\n%s", want, item.Body)
		}
	}
	if item.Classification.Complexity != "low" {
		t.Errorf("chore complexity = %q, want low", item.Classification.Complexity)
	}
}

func TestApply_UnknownTypeFailsClosed(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{Type: "epic", Summary: "x"}, conv)
	var se *SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("want *SemanticError, got %v", err)
	}
	if !strings.Contains(se.Error(), "unknown work-item type") {
		t.Errorf("error = %q", se.Error())
	}
}

func TestApply_MissingSummaryRejected(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{Type: "chore", Summary: "  "}, conv)
	if err == nil || !strings.Contains(err.Error(), "Summary is required") {
		t.Fatalf("want Summary-required error, got %v", err)
	}
}

func TestApply_UnknownComplexityRejected(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{
		Type: "bug", Summary: "boom", Complexity: "epic",
	}, conv)
	if err == nil || !strings.Contains(err.Error(), "unknown complexity") {
		t.Fatalf("want unknown-complexity error, got %v", err)
	}
}

func TestApply_FeatureMissingEpicVarFailsClosed(t *testing.T) {
	conv := testConventions(t)
	// feature title_format is "[E{epic}.{n}] {summary}"; omit epic/n.
	_, _, err := Apply(FilingRequest{
		Type: "feature", Summary: "x", Relations: Relations{ParentEpic: "#1"},
	}, conv)
	if err == nil || !strings.Contains(err.Error(), "unresolved placeholder") {
		t.Fatalf("want unresolved-placeholder error, got %v", err)
	}
}

func TestApply_EpicLinkRequiredEnforced(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "x",
		TitleVars: map[string]string{"epic": "1", "n": "2"},
	}, conv)
	if err == nil || !strings.Contains(err.Error(), "requires a parent epic") {
		t.Fatalf("want epic-required error, got %v", err)
	}
}

func TestApply_EpicLinkNoneRejectsEpic(t *testing.T) {
	conv := testConventions(t)
	// adr declares epic_link: none.
	_, _, err := Apply(FilingRequest{
		Type:      "adr",
		Summary:   "x",
		Relations: Relations{ParentEpic: "#1"},
	}, conv)
	if err == nil || !strings.Contains(err.Error(), "does not take a parent epic") {
		t.Fatalf("want epic-none error, got %v", err)
	}
}

func TestApply_ComplexityOverrideWins(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type: "bug", Summary: "boom", Complexity: "high",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if want := "[E22.7] boom"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
	if item.Classification.Complexity != "high" {
		t.Errorf("complexity = %q, want high", item.Classification.Complexity)
	}
}

func TestMergeLabels_DedupsPreservingOrder(t *testing.T) {
	got := mergeLabels([]string{"type:bug", "x"}, []string{"x", "y", ""})
	want := []string{"type:bug", "x", "y"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeLabels = %v, want %v", got, want)
	}
}
