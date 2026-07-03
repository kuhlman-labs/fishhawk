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

// TestApply_FeatureAcceptanceCriteriaRendersInPosition is the #1614 (E34.7)
// new-key done-means: filing a feature with an "Acceptance criteria"
// Sections entry through the SHIPPED Default() conventions succeeds and the
// content lands under "## Acceptance criteria", positioned after
// "## Done-means" and before "## Notes".
func TestApply_FeatureAcceptanceCriteriaRendersInPosition(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "add the thing",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary":             "add the thing",
			"Done-means":          "the thing exists",
			"Acceptance criteria": "- a user can see the thing",
		},
		Relations: Relations{ParentEpic: "#1005"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(item.Body, "## Acceptance criteria") {
		t.Fatalf("assembled body missing ## Acceptance criteria heading:\n%s", item.Body)
	}
	if !strings.Contains(item.Body, "- a user can see the thing") {
		t.Errorf("assembled body missing acceptance criteria content:\n%s", item.Body)
	}
	doneIdx := strings.Index(item.Body, "## Done-means")
	acIdx := strings.Index(item.Body, "## Acceptance criteria")
	notesIdx := strings.Index(item.Body, "## Notes")
	if doneIdx == -1 || acIdx == -1 || notesIdx == -1 {
		t.Fatalf("missing expected headings; body:\n%s", item.Body)
	}
	if doneIdx >= acIdx || acIdx >= notesIdx {
		t.Errorf("Acceptance criteria not positioned between Done-means and Notes: doneIdx=%d acIdx=%d notesIdx=%d\nbody:\n%s", doneIdx, acIdx, notesIdx, item.Body)
	}
}

// TestApply_BugAcceptanceCriteriaRendersInPosition mirrors the feature case
// for the bug skeleton.
func TestApply_BugAcceptanceCriteriaRendersInPosition(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "bug",
		Summary:   "fix the thing",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary":             "fix the thing",
			"Done-means":          "the bug is fixed",
			"Acceptance criteria": "- the error no longer occurs",
		},
	}, conv)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doneIdx := strings.Index(item.Body, "## Done-means")
	acIdx := strings.Index(item.Body, "## Acceptance criteria")
	notesIdx := strings.Index(item.Body, "## Notes")
	if doneIdx == -1 || acIdx == -1 || notesIdx == -1 {
		t.Fatalf("missing expected headings; body:\n%s", item.Body)
	}
	if doneIdx >= acIdx || acIdx >= notesIdx {
		t.Errorf("Acceptance criteria not positioned between Done-means and Notes: doneIdx=%d acIdx=%d notesIdx=%d\nbody:\n%s", doneIdx, acIdx, notesIdx, item.Body)
	}
	if !strings.Contains(item.Body, "- the error no longer occurs") {
		t.Errorf("assembled body missing acceptance criteria content:\n%s", item.Body)
	}
}

// TestApply_FeatureOldKeySetStillValidates is the #1614 (E34.7) additive
// done-means: a feature filed with only the pre-change section keys (no
// "Acceptance criteria" entry) still succeeds — validateSections rejects
// only off-skeleton keys, not missing ones — and the assembled body carries
// an empty "## Acceptance criteria" heading in position (assembleBody
// renders every skeleton heading unconditionally; see apply.go).
func TestApply_FeatureOldKeySetStillValidates(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "add the thing",
		TitleVars: map[string]string{"epic": "22", "n": "7"},
		Sections: map[string]string{
			"Summary":    "add the thing",
			"Proposal":   "do it this way",
			"Done-means": "the thing exists",
			"Notes":      "n/a",
		},
		Relations: Relations{ParentEpic: "#1005"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply(old key set) = %v, want nil (additive: missing Acceptance criteria key still validates)", err)
	}
	doneIdx := strings.Index(item.Body, "## Done-means")
	acIdx := strings.Index(item.Body, "## Acceptance criteria")
	notesIdx := strings.Index(item.Body, "## Notes")
	if doneIdx == -1 || acIdx == -1 || notesIdx == -1 {
		t.Fatalf("missing expected headings; body:\n%s", item.Body)
	}
	if doneIdx >= acIdx || acIdx >= notesIdx {
		t.Errorf("empty Acceptance criteria heading not positioned between Done-means and Notes: doneIdx=%d acIdx=%d notesIdx=%d\nbody:\n%s", doneIdx, acIdx, notesIdx, item.Body)
	}
	// Nothing was supplied for Acceptance criteria, so the heading renders
	// with no content before the next heading.
	between := item.Body[acIdx:notesIdx]
	if strings.TrimSpace(strings.TrimPrefix(between, "## Acceptance criteria")) != "" {
		t.Errorf("expected an empty Acceptance criteria section, got %q", between)
	}
}

// TestApply_WhereToLookEndToEndFromDefault is the #1615 (E34.8) binding
// coverage condition: starting from the PARSED shipped default (the embedded
// YAML → schema → ItemType struct, via Default()), a supplied "Where to look"
// renders in position AND an omitted one is byte-identical to the pre-section
// output — proving the whole YAML→struct→render boundary in one test rather
// than a hand-built Go struct.
func TestApply_WhereToLookEndToEndFromDefault(t *testing.T) {
	conv := testConventions(t) // Default(): parsed from the embedded default YAML.

	// Supplied → the heading renders between Proposal and Done-means.
	supplied, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "add the thing",
		TitleVars: map[string]string{"epic": "34", "n": "8"},
		Sections: map[string]string{
			"Where to look": "- backend/internal/workmgmt/apply.go: assembleBody",
		},
		Relations: Relations{ParentEpic: "#1615"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply(where-to-look supplied) = %v", err)
	}
	propIdx := strings.Index(supplied.Body, "## Proposal")
	wtlIdx := strings.Index(supplied.Body, "## Where to look")
	doneIdx := strings.Index(supplied.Body, "## Done-means")
	if propIdx == -1 || wtlIdx == -1 || doneIdx == -1 {
		t.Fatalf("missing expected headings; body:\n%s", supplied.Body)
	}
	if propIdx >= wtlIdx || wtlIdx >= doneIdx {
		t.Errorf("Where to look not positioned between Proposal and Done-means: prop=%d wtl=%d done=%d\nbody:\n%s", propIdx, wtlIdx, doneIdx, supplied.Body)
	}
	if !strings.Contains(supplied.Body, "- backend/internal/workmgmt/apply.go: assembleBody") {
		t.Errorf("supplied Where to look content missing:\n%s", supplied.Body)
	}

	// Omitted → byte-identical to the same filing over a skeleton that never
	// listed the optional section (the additive guarantee). The golden is
	// derived from the SHIPPED feature skeleton with 'Where to look' removed,
	// rendered over the same Sections, so any accidental heading or spacing
	// drift on the omit path fails loudly.
	sections := map[string]string{
		"Summary":    "add the thing",
		"Proposal":   "do it this way",
		"Done-means": "the thing exists",
	}
	omitted, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "add the thing",
		TitleVars: map[string]string{"epic": "34", "n": "8"},
		Sections:  sections,
		Relations: Relations{ParentEpic: "#1615"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply(where-to-look omitted) = %v", err)
	}
	if strings.Contains(omitted.Body, "## Where to look") {
		t.Errorf("omitted optional section leaked its heading:\n%s", omitted.Body)
	}
	golden := ItemType{BodySkeleton: withoutSection(conv.Types["feature"].BodySkeleton, "Where to look")}
	if want := assembleBody(golden, sections); omitted.Body != want {
		t.Errorf("omitted body not byte-identical to the pre-section skeleton:\n got: %q\nwant: %q", omitted.Body, want)
	}
}

// TestApply_WhereToLookBugRendersInPosition mirrors the supplied-render case
// on the bug skeleton (the binding condition requires BOTH feature and bug).
func TestApply_WhereToLookBugRendersInPosition(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "bug",
		Summary:   "fix the thing",
		TitleVars: map[string]string{"epic": "34", "n": "8"},
		Sections: map[string]string{
			"Where to look": "- backend/internal/workmgmt/conventions.go: validateOptionalSections",
		},
	}, conv)
	if err != nil {
		t.Fatalf("Apply(bug where-to-look) = %v", err)
	}
	propIdx := strings.Index(item.Body, "## Proposal")
	wtlIdx := strings.Index(item.Body, "## Where to look")
	doneIdx := strings.Index(item.Body, "## Done-means")
	if propIdx == -1 || wtlIdx == -1 || doneIdx == -1 {
		t.Fatalf("missing expected headings; body:\n%s", item.Body)
	}
	if propIdx >= wtlIdx || wtlIdx >= doneIdx {
		t.Errorf("Where to look not between Proposal and Done-means on bug: prop=%d wtl=%d done=%d\nbody:\n%s", propIdx, wtlIdx, doneIdx, item.Body)
	}
}

// TestApply_WhereToLookPresentButEmptyRendersHeading pins the empty-vs-absent
// distinction: a present-but-empty "Where to look" key is content, so its
// heading still renders in position (contrast the omitted case above, which
// skips it entirely).
func TestApply_WhereToLookPresentButEmptyRendersHeading(t *testing.T) {
	conv := testConventions(t)
	item, _, err := Apply(FilingRequest{
		Type:      "feature",
		Summary:   "add the thing",
		TitleVars: map[string]string{"epic": "34", "n": "8"},
		Sections: map[string]string{
			"Where to look": "", // present, empty — NOT absent.
		},
		Relations: Relations{ParentEpic: "#1615"},
	}, conv)
	if err != nil {
		t.Fatalf("Apply(where-to-look present-but-empty) = %v", err)
	}
	propIdx := strings.Index(item.Body, "## Proposal")
	wtlIdx := strings.Index(item.Body, "## Where to look")
	doneIdx := strings.Index(item.Body, "## Done-means")
	if propIdx == -1 || wtlIdx == -1 || doneIdx == -1 {
		t.Fatalf("present-but-empty Where to look heading missing; body:\n%s", item.Body)
	}
	if propIdx >= wtlIdx || wtlIdx >= doneIdx {
		t.Errorf("present-but-empty Where to look not between Proposal and Done-means: prop=%d wtl=%d done=%d\nbody:\n%s", propIdx, wtlIdx, doneIdx, item.Body)
	}
}

// TestAssembleBody_OmittedOptionalIsByteIdenticalToAbsence is the direct
// unit-level additive guarantee: for the same Sections, assembleBody over a
// skeleton that lists an omitted optional section produces bytes identical to
// a skeleton that never listed it — including when the optional section is
// first (the separator keys on "already wrote", not the loop index).
func TestAssembleBody_OmittedOptionalIsByteIdenticalToAbsence(t *testing.T) {
	sections := map[string]string{"Summary": "s", "Proposal": "p", "Done-means": "d"}

	// Optional section in the middle.
	withMid := assembleBody(ItemType{
		BodySkeleton:     []string{"Summary", "Proposal", "Where to look", "Done-means"},
		OptionalSections: []string{"Where to look"},
	}, sections)
	wantMid := assembleBody(ItemType{BodySkeleton: []string{"Summary", "Proposal", "Done-means"}}, sections)
	if withMid != wantMid {
		t.Errorf("omitted middle optional section not byte-identical:\n got: %q\nwant: %q", withMid, wantMid)
	}
	if want := "## Summary\n\ns\n\n## Proposal\n\np\n\n## Done-means\n\nd\n"; withMid != want {
		t.Errorf("assembled body drifted from literal golden:\n got: %q\nwant: %q", withMid, want)
	}

	// Optional section FIRST — the index-based separator would have leaked a
	// leading "\n"; the wrote-based separator must not.
	withFirst := assembleBody(ItemType{
		BodySkeleton:     []string{"Where to look", "Summary"},
		OptionalSections: []string{"Where to look"},
	}, sections)
	wantFirst := assembleBody(ItemType{BodySkeleton: []string{"Summary"}}, sections)
	if withFirst != wantFirst {
		t.Errorf("omitted leading optional section not byte-identical:\n got: %q\nwant: %q", withFirst, wantFirst)
	}
}

// withoutSection returns skeleton with the first exact match of name removed.
func withoutSection(skeleton []string, name string) []string {
	out := make([]string, 0, len(skeleton))
	for _, s := range skeleton {
		if s == name {
			continue
		}
		out = append(out, s)
	}
	return out
}

func TestMergeLabels_DedupsPreservingOrder(t *testing.T) {
	got := mergeLabels([]string{"type:bug", "x"}, []string{"x", "y", ""})
	want := []string{"type:bug", "x", "y"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeLabels = %v, want %v", got, want)
	}
}
