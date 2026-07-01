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
	// The shipped default sets numbering.pad: 3 (#1148), so the {number}
	// substitution zero-pads to width 3 ([ADR-036], not the bare [ADR-36]).
	if want := "[ADR-036] use postgres"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

// TestApply_ADRZeroPadsViaDefault is the #1148 done-means: filing an adr
// through the SHIPPED Default() conventions (numbering.pad: 3) with existing
// numbers up to 40 renders the zero-padded [ADR-041] form — exactly what
// fishhawk_file_issue produces. It pins the shipped default, not a hand-built
// Pad:3 copy, exercising the default yaml -> Numbering.Pad -> renderTitle
// %0*d chain end to end.
func TestApply_ADRZeroPadsViaDefault(t *testing.T) {
	conv := testConventions(t)
	existing := make([]int, 0, 40)
	for n := 1; n <= 40; n++ {
		existing = append(existing, n)
	}
	item, num, err := Apply(FilingRequest{
		Type:            "adr",
		Summary:         "zero-pad adr numbers",
		Body:            "## Context\n\n…\n",
		ExistingNumbers: existing,
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 41 {
		t.Errorf("next ADR number = %d, want 41", num)
	}
	if want := "[ADR-041] zero-pad adr numbers"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

// TestApply_ADREmptyExistingNumbersFailsClosed is the #1265 done-means: a
// numbered type (adr) filed with omitted/nil existing_numbers must fail loud
// with a *SemanticError carrying the numbered-type cause, rendering NO item,
// rather than silently allocating ADR-001.
func TestApply_ADREmptyExistingNumbersFailsClosed(t *testing.T) {
	conv := testConventions(t)
	item, num, err := Apply(FilingRequest{
		Type:    "adr",
		Summary: "first decision",
		Body:    "## Context\n\n…\n",
	}, conv)
	var se *SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("want *SemanticError, got err=%v num=%d", err, num)
	}
	if !strings.Contains(se.Error(), "existing_numbers is required") {
		t.Errorf("Msg = %q, want the existing_numbers-required message", se.Error())
	}
	if se.Details["existing_numbers_required"] != true {
		t.Errorf("Details.existing_numbers_required = %v, want true", se.Details["existing_numbers_required"])
	}
	if item.Title != "" {
		t.Errorf("rendered a title %q despite the fail-closed allocate", item.Title)
	}
}

// TestApply_ADRSeedZeroYieldsOne pins the documented explicit-first escape:
// a non-empty seed existing_numbers:[0] (max 0 -> 1) still files [ADR-001].
func TestApply_ADRSeedZeroYieldsOne(t *testing.T) {
	conv := testConventions(t)
	item, num, err := Apply(FilingRequest{
		Type:            "adr",
		Summary:         "first decision",
		Body:            "## Context\n\n…\n",
		ExistingNumbers: []int{0},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if num != 1 {
		t.Errorf("seeded-first ADR number = %d, want 1", num)
	}
	if want := "[ADR-001] first decision"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
}

// TestApply_EpicRendersUnpaddedNumberAndAllocatesNext is the #1508 done-means:
// filing an epic through the SHIPPED Default() conventions must be ACCEPTED —
// the exact previously-reported "unknown work-item type: epic" rejection is
// gone — and render the unpadded [E29] title (numbering.pad: 0, contrasting the
// adr pad:3 [ADR-041] case) with allocateNumber returning max+1 (28 → 29). The
// explicit no-error assertion is the direct regression for the reported bug, at
// the Apply layer where type acceptance is decided.
func TestApply_EpicRendersUnpaddedNumberAndAllocatesNext(t *testing.T) {
	conv := testConventions(t)
	existing := make([]int, 0, 28)
	for n := 1; n <= 28; n++ {
		existing = append(existing, n)
	}
	item, num, err := Apply(FilingRequest{
		Type:            "epic",
		Summary:         "onboarding epic",
		Body:            "## Summary\n\n…\n",
		ExistingNumbers: existing,
	}, conv)
	// The reported bug was Apply rejecting type "epic" with "unknown
	// work-item type: epic"; assert acceptance explicitly, not merely the
	// rendered title.
	if err != nil {
		t.Fatalf("Apply(epic) = %v, want no error (epic is now a known type, #1508)", err)
	}
	if num != 29 {
		t.Errorf("next epic number = %d, want 29 (allocateNumber max+1)", num)
	}
	// pad 0 → no zero-padding: the bare [E29], not [E029].
	if want := "[E29] onboarding epic"; item.Title != want {
		t.Errorf("title = %q, want %q", item.Title, want)
	}
	if got := strings.Join(item.Classification.Labels, ","); got != "epic" {
		t.Errorf("labels = %q, want epic", got)
	}
	if item.BoardPlacement.Status != "Backlog" {
		t.Errorf("status = %q, want Backlog", item.BoardPlacement.Status)
	}
	if item.Classification.Complexity != "high" {
		t.Errorf("complexity = %q, want high (epic type default)", item.Classification.Complexity)
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

func TestApply_DependsOnPreservedOnRelations(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "chore",
		Summary:   "depends on siblings",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		// Mixed `#N` and bare `N` forms both validate at file time.
		Relations: Relations{DependsOn: []string{"#41", "42"}},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := strings.Join(item.Relations.DependsOn, ","); got != "#41,42" {
		t.Errorf("depends_on not carried through Apply: %q", got)
	}
}

func TestApply_DependsOnMalformedRejected(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{
		Type:      "chore",
		Summary:   "bad dep",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Relations: Relations{DependsOn: []string{"#41", "not-a-ref"}},
	}, conv)
	var se *SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("want *SemanticError for malformed depends_on, got %v", err)
	}
	if !strings.Contains(se.Error(), "depends_on entry") || !strings.Contains(se.Error(), "not-a-ref") {
		t.Errorf("error should name the malformed value: %q", se.Error())
	}
}

func TestApply_UnknownTypeFailsClosed(t *testing.T) {
	conv := testConventions(t)
	// "spike" is not a key in the shipped conventions (epic now IS, #1508).
	_, _, err := Apply(FilingRequest{Type: "spike", Summary: "x"}, conv)
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
	// adr declares epic_link: none. Seed existing_numbers so allocation
	// succeeds and the apply reaches the relations check (#1265 makes
	// existing_numbers mandatory for the numbered adr type).
	_, _, err := Apply(FilingRequest{
		Type:            "adr",
		Summary:         "x",
		Relations:       Relations{ParentEpic: "#1"},
		ExistingNumbers: []int{1},
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

// TestApply_OffSkeletonSectionFailsLoud is the #1184 silent-drop fix: a
// Sections key that matches no body_skeleton section must fail loud (the
// caller's content would otherwise vanish), naming the unknown key and the
// expected skeleton names in both the message and structured Details.
func TestApply_OffSkeletonSectionFailsLoud(t *testing.T) {
	conv := testConventions(t)
	_, _, err := Apply(FilingRequest{
		Type:      "chore",
		Summary:   "bump deps",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary": "bump the pinned tools",
			"Impact":  "this content would be silently dropped", // off-skeleton
		},
	}, conv)
	var se *SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("want *SemanticError, got %v", err)
	}
	if !strings.Contains(se.Error(), "Impact") {
		t.Errorf("error should name the unknown section, got %q", se.Error())
	}
	// chore's skeleton is [Summary, Done-means]; the expected names appear.
	if !strings.Contains(se.Error(), "Done-means") {
		t.Errorf("error should name the expected skeleton sections, got %q", se.Error())
	}
	unknown, _ := se.Details["unknown_sections"].([]string)
	if len(unknown) != 1 || unknown[0] != "Impact" {
		t.Errorf("Details.unknown_sections = %v, want [Impact]", se.Details["unknown_sections"])
	}
	expected, _ := se.Details["expected_sections"].([]string)
	if len(expected) != 2 || expected[0] != "Summary" || expected[1] != "Done-means" {
		t.Errorf("Details.expected_sections = %v, want [Summary Done-means]", se.Details["expected_sections"])
	}
}

// TestApply_ValidSkeletonSectionsStillRender pins that the fail-loud guard
// does not regress the happy path: every Sections key on-skeleton assembles
// the body as before.
func TestApply_ValidSkeletonSectionsStillRender(t *testing.T) {
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
	for _, want := range []string{"## Summary", "bump the pinned tools", "## Done-means", "CI green on the bump PR"} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("assembled body missing %q:\n%s", want, item.Body)
		}
	}
}

// TestApply_MissingPlaceholderCarriesDetails asserts renderTitle's
// SemanticError carries the structured missing-placeholder list (#1184) so
// the handler can surface details.missing_placeholders, while the human Msg
// is unchanged.
func TestApply_MissingPlaceholderCarriesDetails(t *testing.T) {
	conv := testConventions(t)
	// feature title_format "[E{epic}.{n}] {summary}"; omit both vars.
	_, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "x",
		Relations: Relations{ParentEpic: "#1"},
	}, conv)
	var se *SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("want *SemanticError, got %v", err)
	}
	if !strings.Contains(se.Error(), "unresolved placeholder") {
		t.Errorf("Msg = %q, want the verbatim unresolved-placeholder message", se.Error())
	}
	missing, _ := se.Details["missing_placeholders"].([]string)
	if len(missing) != 2 || missing[0] != "epic" || missing[1] != "n" {
		t.Errorf("Details.missing_placeholders = %v, want [epic n]", se.Details["missing_placeholders"])
	}
}

func TestMergeLabels_DedupsPreservingOrder(t *testing.T) {
	got := mergeLabels([]string{"type:bug", "x"}, []string{"x", "y", ""})
	want := []string{"type:bug", "x", "y"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeLabels = %v, want %v", got, want)
	}
}
