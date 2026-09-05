package workmgmt

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestDefaultRoundTrip exercises the embed → YAML → schema → semantic →
// typed struct path end-to-end: package init has already validated the
// embedded default (a failure panics the test binary), so here we assert
// the typed result carries every section the conventions layer needs.
func TestDefaultRoundTrip(t *testing.T) {
	d := Default()

	if d.SpecVersion != "work-management-v0" {
		t.Errorf("SpecVersion = %q, want %q", d.SpecVersion, "work-management-v0")
	}
	if d.Provider != "github_projects" {
		t.Errorf("Provider = %q, want %q", d.Provider, "github_projects")
	}
	if d.Project == nil {
		t.Fatal("Project is nil; github_projects requires a connection block")
	}
	if d.Project.Owner == "" || d.Project.Number == 0 {
		t.Errorf("Project = %+v, want owner and number set", *d.Project)
	}

	for _, lvl := range []string{"low", "medium", "high"} {
		if d.ComplexityLevels[lvl] == "" {
			t.Errorf("complexity_levels[%s] is empty", lvl)
		}
	}

	for _, typ := range []string{"feature", "bug", "chore", "adr", "epic"} {
		it, ok := d.Types[typ]
		if !ok {
			t.Errorf("types is missing %q", typ)
			continue
		}
		if len(it.BodySkeleton) == 0 {
			t.Errorf("types.%s.body_skeleton is empty", typ)
		}
	}

	adr := d.Types["adr"]
	if adr.Numbering == nil {
		t.Fatal("types.adr.numbering is nil; ADR numbering is required")
	}
	if adr.Numbering.Scheme != "sequential" {
		t.Errorf("types.adr.numbering.scheme = %q, want sequential", adr.Numbering.Scheme)
	}
	if adr.Numbering.Prefix != "ADR-" {
		t.Errorf("types.adr.numbering.prefix = %q, want ADR-", adr.Numbering.Prefix)
	}
	// The shipped default sets numbering.pad: 3 so ADR titles render in the
	// established zero-padded [ADR-NNN] series ([ADR-036], [ADR-041]) (#1148).
	if adr.Numbering.Pad != 3 {
		t.Errorf("types.adr.numbering.pad = %d, want 3 (zero-padded default, #1148)", adr.Numbering.Pad)
	}
	if d.Types["feature"].EpicLink != "required" {
		t.Errorf("types.feature.epic_link = %q, want required", d.Types["feature"].EpicLink)
	}

	// The epic type is the second numbered type (#1508): sequential numbering
	// with the bare "E" prefix and pad 0 (unpadded [E29], not [E029]), no epic
	// link, and the `epic` default label. Asserting the SHIPPED default values
	// (the #1169 done-means gate) — a comment-only/no-op YAML touch fails here.
	epic := d.Types["epic"]
	if epic.Numbering == nil {
		t.Fatal("types.epic.numbering is nil; epic is a numbered type")
	}
	if epic.Numbering.Scheme != "sequential" {
		t.Errorf("types.epic.numbering.scheme = %q, want sequential", epic.Numbering.Scheme)
	}
	if epic.Numbering.Prefix != "E" {
		t.Errorf("types.epic.numbering.prefix = %q, want E", epic.Numbering.Prefix)
	}
	// pad 0 → no zero-padding, so titles render [E29] not [E029] (#1508).
	if epic.Numbering.Pad != 0 {
		t.Errorf("types.epic.numbering.pad = %d, want 0 (unpadded [E29], #1508)", epic.Numbering.Pad)
	}
	if epic.EpicLink != "none" {
		t.Errorf("types.epic.epic_link = %q, want none", epic.EpicLink)
	}
	if len(epic.DefaultLabels) != 1 || epic.DefaultLabels[0] != "epic" {
		t.Errorf("types.epic.default_labels = %v, want [epic]", epic.DefaultLabels)
	}
	// Status=Backlog is load-bearing: a null Status breaks the Project #7
	// kanban view, so the "conventions-complete" epic must ship it (opus LOW
	// binding condition). complexity=high is the shipped epic default.
	if epic.DefaultFields.Status != "Backlog" {
		t.Errorf("types.epic.default_fields.status = %q, want Backlog", epic.DefaultFields.Status)
	}
	if epic.DefaultFields.Complexity != "high" {
		t.Errorf("types.epic.default_fields.complexity = %q, want high", epic.DefaultFields.Complexity)
	}
}

// TestDefaultDoneMeansHintIsTestable locks the operator's required-field
// discipline (#1005, condition 6): the Done-means hint must say the
// condition is testable.
func TestDefaultDoneMeansHintIsTestable(t *testing.T) {
	hint := Default().FieldHints["Done-means"]
	if hint == "" {
		t.Fatal("field_hints[Done-means] is empty")
	}
	if !strings.Contains(strings.ToLower(hint), "testable") {
		t.Errorf("Done-means hint %q does not state the condition is testable", hint)
	}
}

// TestDefaultBodySkeletons locks the SHIPPED feature/bug/chore body_skeleton
// values (#1614, E34.7): a comment-only or mis-positioned YAML touch fails
// here even though a mere scope-presence check would pass (the #1169
// done-means discipline). "Acceptance criteria" sits after Done-means and
// before Notes on feature/bug; chore is deliberately unchanged (optional for
// chore per the issue).
func TestDefaultBodySkeletons(t *testing.T) {
	d := Default()

	wantFeature := []string{"Summary", "Proposal", "Where to look", "Done-means", "Acceptance criteria", "Notes", "Relations"}
	if got := d.Types["feature"].BodySkeleton; strings.Join(got, ",") != strings.Join(wantFeature, ",") {
		t.Errorf("feature body_skeleton = %v, want %v", got, wantFeature)
	}

	wantBug := []string{"Summary", "Observed", "Proposal", "Where to look", "Done-means", "Acceptance criteria", "Notes", "Relations"}
	if got := d.Types["bug"].BodySkeleton; strings.Join(got, ",") != strings.Join(wantBug, ",") {
		t.Errorf("bug body_skeleton = %v, want %v", got, wantBug)
	}

	wantChore := []string{"Summary", "Done-means"}
	if got := d.Types["chore"].BodySkeleton; strings.Join(got, ",") != strings.Join(wantChore, ",") {
		t.Errorf("chore body_skeleton = %v, want %v (unchanged; Acceptance criteria is optional for chore)", got, wantChore)
	}

	// 'Where to look' is listed in optional_sections on feature and bug (and
	// nowhere else): a comment-only YAML touch that dropped the list would
	// fail here (#1615, #1169 done-means discipline).
	if got := d.Types["feature"].OptionalSections; strings.Join(got, ",") != "Where to look" {
		t.Errorf("feature optional_sections = %v, want [Where to look]", got)
	}
	if got := d.Types["bug"].OptionalSections; strings.Join(got, ",") != "Where to look" {
		t.Errorf("bug optional_sections = %v, want [Where to look]", got)
	}
	if got := d.Types["chore"].OptionalSections; len(got) != 0 {
		t.Errorf("chore optional_sections = %v, want none", got)
	}
}

// TestDefaultWhereToLookHint is the #1615 (E34.8) binding coverage condition:
// field_hints["Where to look"] must be non-empty, mark the section NON-BINDING,
// and draw the explicit contrast with the plan's binding scope.files.
func TestDefaultWhereToLookHint(t *testing.T) {
	hint := Default().FieldHints["Where to look"]
	if hint == "" {
		t.Fatal("field_hints[Where to look] is empty")
	}
	if !strings.Contains(strings.ToLower(hint), "non-binding") {
		t.Errorf("Where to look hint %q does not mark the section non-binding", hint)
	}
	if !strings.Contains(hint, "scope.files") {
		t.Errorf("Where to look hint %q does not contrast with binding scope.files", hint)
	}
}

// TestDefaultAcceptanceCriteriaHint is the binding coverage condition on
// #1614: field_hints["Acceptance criteria"] must be non-empty and state the
// behavioral-contract distinction from Done-means (observable/falsifiable
// behaviors, not the change-complete checklist).
func TestDefaultAcceptanceCriteriaHint(t *testing.T) {
	hint := Default().FieldHints["Acceptance criteria"]
	if hint == "" {
		t.Fatal("field_hints[Acceptance criteria] is empty")
	}
	lower := strings.ToLower(hint)
	if !strings.Contains(lower, "observable") && !strings.Contains(lower, "falsifiable") {
		t.Errorf("Acceptance criteria hint %q does not state the behaviors must be observable/falsifiable", hint)
	}
	if !strings.Contains(hint, "Done-means") {
		t.Errorf("Acceptance criteria hint %q does not distinguish itself from Done-means", hint)
	}
}

// TestDefaultEpicScopeHint is the E34.10 (#1617) binding coverage condition:
// field_hints["Scope"] must be non-empty, state that child references carry
// no checkbox state, and carry the rationale that GitHub sub-issues are the
// authoritative live progress view.
func TestDefaultEpicScopeHint(t *testing.T) {
	hint := Default().FieldHints["Scope"]
	if hint == "" {
		t.Fatal("field_hints[Scope] is empty")
	}
	lower := strings.ToLower(hint)
	if !strings.Contains(lower, "no checkbox") {
		t.Errorf("Scope hint %q does not state child references carry no checkbox state", hint)
	}
	if !strings.Contains(lower, "sub-issue") {
		t.Errorf("Scope hint %q does not cite GitHub sub-issues as the live progress view", hint)
	}
}

// TestDefaultRequiresMandatoryTrio asserts the shipped default carries the
// mandatory required-field trio.
func TestDefaultRequiresMandatoryTrio(t *testing.T) {
	if missing := missingMandatoryFields(Default().RequiredFields); len(missing) != 0 {
		t.Errorf("default is missing mandatory required_fields: %v", missing)
	}
}

const minimalConfig = `
spec_version: work-management-v0
provider: github_projects
project:
  owner: acme
  number: 3
required_fields:
  - Summary
  - Done-means
  - complexity
types:
  feature:
    body_skeleton:
      - Summary
`

func TestParseValid(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("Parse(minimal) = %v, want nil", err)
	}
	if c.Provider != "github_projects" {
		t.Errorf("Provider = %q, want github_projects", c.Provider)
	}
	if c.Project == nil || c.Project.Owner != "acme" || c.Project.Number != 3 {
		t.Errorf("Project = %+v, want {acme 3}", c.Project)
	}
}

// TestParseValidJira proves a provider:jira config carrying the additive
// jira connection block (project_key + optional issue_types, and NO
// base_url — the instance URL and creds are server-side env) parses
// cleanly and round-trips into the typed *JiraConnection. No provider
// behavior is exercised here; provider:jira still fails closed at filing
// time until the concrete provider lands.
func TestParseValidJira(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: jira
jira:
  project_key: FISH
  issue_types:
    feature: Story
    bug: Bug
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(jira) = %v, want nil", err)
	}
	if c.Provider != "jira" {
		t.Errorf("Provider = %q, want jira", c.Provider)
	}
	if c.Jira == nil {
		t.Fatal("Jira is nil; the jira block did not round-trip into the struct")
	}
	if c.Jira.ProjectKey != "FISH" {
		t.Errorf("Jira.ProjectKey = %q, want FISH", c.Jira.ProjectKey)
	}
	if c.Jira.IssueTypes["feature"] != "Story" || c.Jira.IssueTypes["bug"] != "Bug" {
		t.Errorf("Jira.IssueTypes = %v, want {feature:Story bug:Bug}", c.Jira.IssueTypes)
	}
}

// TestParseJiraParentField proves the additive jira.parent_field round-trips
// through the schema-validated DisallowUnknownFields decode into the typed
// JiraConnection.ParentField — the typed-config boundary directly, not only
// via the end-to-end provider seam. A classic epic-link custom field id is
// decoded verbatim; an absent field leaves ParentField empty (the provider
// then defaults to the team-managed `parent` reference).
func TestParseJiraParentField(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: jira
jira:
  project_key: FISH
  parent_field: customfield_10014
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(jira parent_field) = %v, want nil", err)
	}
	if c.Jira == nil {
		t.Fatal("Jira is nil; the jira block did not round-trip into the struct")
	}
	if c.Jira.ParentField != "customfield_10014" {
		t.Errorf("Jira.ParentField = %q, want customfield_10014", c.Jira.ParentField)
	}

	// Absent parent_field leaves the field empty (provider defaults to parent).
	noField := `
spec_version: work-management-v0
provider: jira
jira:
  project_key: FISH
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`
	c2, err := Parse(strings.NewReader(noField))
	if err != nil {
		t.Fatalf("Parse(jira no parent_field) = %v, want nil", err)
	}
	if c2.Jira == nil || c2.Jira.ParentField != "" {
		t.Errorf("Jira.ParentField = %q, want empty when parent_field is absent", c2.Jira.ParentField)
	}
}

// TestParseValidGitLab proves a provider:gitlab config carrying the additive
// gitlab connection block parses cleanly and round-trips into the typed
// *GitLabConnection, both with the optional project override and with an
// empty block (project defaults to the filing repo path in the provider).
// The schema↔struct seam: the GitLab struct field must land in the same
// commit as the schema key, or the DisallowUnknownFields decode rejects the
// gitlab key. No provider behavior is exercised here; the concrete gitlab
// provider and its wiring land in sibling slices.
func TestParseValidGitLab(t *testing.T) {
	withProject := `
spec_version: work-management-v0
provider: gitlab
gitlab:
  project: group/subgroup/app
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`
	c, err := Parse(strings.NewReader(withProject))
	if err != nil {
		t.Fatalf("Parse(gitlab with project) = %v, want nil", err)
	}
	if c.Provider != "gitlab" {
		t.Errorf("Provider = %q, want gitlab", c.Provider)
	}
	if c.GitLab == nil {
		t.Fatal("GitLab is nil; the gitlab block did not round-trip into the struct")
	}
	if c.GitLab.Project != "group/subgroup/app" {
		t.Errorf("GitLab.Project = %q, want group/subgroup/app", c.GitLab.Project)
	}

	// An empty gitlab block is valid (project is optional): the semantic
	// check only requires the block to be present under provider gitlab, and
	// a present-but-empty block round-trips into a non-nil struct with an
	// empty Project.
	empty := `
spec_version: work-management-v0
provider: gitlab
gitlab: {}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`
	c2, err := Parse(strings.NewReader(empty))
	if err != nil {
		t.Fatalf("Parse(gitlab empty block) = %v, want nil", err)
	}
	if c2.GitLab == nil {
		t.Fatal("GitLab is nil; a present-but-empty gitlab block must round-trip into a non-nil struct")
	}
	if c2.GitLab.Project != "" {
		t.Errorf("GitLab.Project = %q, want empty when the project override is absent", c2.GitLab.Project)
	}
}

// TestParseDoneMeansSpaceVariant proves the mandatory-field check is
// robust to "Done means" vs "Done-means".
func TestParseDoneMeansSpaceVariant(t *testing.T) {
	cfg := strings.Replace(minimalConfig, "  - Done-means", "  - Done means", 1)
	if _, err := Parse(strings.NewReader(cfg)); err != nil {
		t.Fatalf("Parse(Done means variant) = %v, want nil", err)
	}
}

func TestParseSchemaErrors(t *testing.T) {
	// wantPath, when non-empty, additionally pins the *SchemaError's JSON
	// Pointer so a case proves WHICH construct rejected the config, not
	// merely that something did.
	cases := map[string]struct {
		cfg      string
		wantPath string
	}{
		"unknown top-level key": {cfg: minimalConfig + "playbook: nope\n"},
		"wrong spec_version": {cfg: `
spec_version: work-management-v9
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`},
		"unknown provider": {cfg: `
spec_version: work-management-v0
provider: trello
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`},
		"empty body_skeleton": {cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: []}}
`},
		"malformed label": {cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary], default_labels: ["NOT VALID"]}}
`},
		"non-object document": {cfg: `just a string`},
		// product_feedback:{} omits the required enabled flag, guarding
		// required:["enabled"] against a regression that would re-introduce
		// the implicit enabled=false kill-switch footgun (#1132).
		"product_feedback missing required enabled": {cfg: minimalConfig + "product_feedback: {}\n"},
		// An unknown nested key under product_feedback guards the nested
		// object's additionalProperties:false (#1132).
		"product_feedback unknown nested key": {cfg: minimalConfig + "product_feedback:\n  enabled: false\n  extra: nope\n"},
		// The charter block's whole declaration-time contract is structural
		// (E54.1 / #2233): required:["path"] + minLength:1 + the nested
		// additionalProperties:false. Each of the three is pinned by its own
		// case, and each asserts the JSON Pointer names /charter so a passing
		// case cannot be some unrelated construct rejecting the config.
		// Deleting required/minLength from the EMBEDDED mirror (the copy
		// //go:embed compiles) reddens the first two.
		"charter missing required path": {cfg: minimalConfig + "charter: {}\n", wantPath: "/charter"},
		"charter empty path":            {cfg: minimalConfig + "charter:\n  path: \"\"\n", wantPath: "/charter/path"},
		"charter unknown nested key":    {cfg: minimalConfig + "charter:\n  path: docs/charter.md\n  extra: nope\n", wantPath: "/charter"},
		// The selection block's whole declaration-time contract is structural
		// (E45.24 / #2231): required:["source_view"] + minLength:1 + the
		// nested additionalProperties:false + the closed order_by enum. Each
		// of the four is pinned by its own case asserting the JSON Pointer, so
		// a passing case cannot be some unrelated construct rejecting the
		// config. Deleting required/enum from the EMBEDDED mirror (the copy
		// //go:embed compiles) reddens the first and the last.
		"selection missing required source_view": {cfg: minimalConfig + "selection: {}\n", wantPath: "/selection"},
		"selection empty source_view":            {cfg: minimalConfig + "selection:\n  source_view: \"\"\n", wantPath: "/selection/source_view"},
		"selection unknown nested key":           {cfg: minimalConfig + "selection:\n  source_view: \"Up Next\"\n  bogus: 1\n", wantPath: "/selection"},
		"selection invalid order_by":             {cfg: minimalConfig + "selection:\n  source_view: \"Up Next\"\n  order_by: whatever\n", wantPath: "/selection/order_by"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.cfg))
			if err == nil {
				t.Fatal("Parse accepted an invalid config")
			}
			var serr *SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("error = %v (%T), want *SchemaError", err, err)
			}
			if tc.wantPath != "" && serr.Path != tc.wantPath {
				t.Errorf("SchemaError.Path = %q, want %q (full error: %v)", serr.Path, tc.wantPath, serr)
			}
		})
	}
}

func TestParseSemanticErrors(t *testing.T) {
	cases := map[string]struct {
		cfg  string
		want string
	}{
		"missing mandatory field": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
			want: "Done-means",
		},
		"github_projects without project": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
			want: "requires a project connection",
		},
		"jira without jira block": {
			cfg: `
spec_version: work-management-v0
provider: jira
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
			want: "requires a jira connection",
		},
		// ADR-058: provider gitlab requires the gitlab block (fail-closed mode 1).
		"gitlab without gitlab block": {
			cfg: `
spec_version: work-management-v0
provider: gitlab
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
			want: "requires a gitlab connection",
		},
		// ADR-058: a gitlab block under a non-gitlab provider is rejected
		// (fail-closed mode 2 — the off-provider-block rejection).
		"gitlab block under github_projects": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
gitlab: {project: group/app}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
			want: `gitlab connection block is set but provider is "github_projects", not gitlab`,
		},
		"adr without numbering": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {adr: {body_skeleton: [Context]}}
`,
			want: "numbering rule",
		},
		"transition targets undeclared state": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
states: {backlog: Backlog}
transitions: {run_started: in_progress}
`,
			want: "not declared in states",
		},
		// #1615: an optional_sections entry that names a heading absent from
		// the type's body_skeleton is rejected fail-closed.
		"optional_section absent from body_skeleton": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary, Proposal], optional_sections: [Where to look]}}
`,
			want: `optional_section "Where to look", which is absent`,
		},
		// #1616: a label_defaults value whose namespace prefix does not match
		// its key is rejected fail-closed (config fail-closed, verification 5).
		"label_defaults value missing key namespace prefix": {
			cfg: `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary], label_defaults: {autonomy: high}}}
`,
			want: `must begin with the namespace prefix "autonomy:"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.cfg))
			if err == nil {
				t.Fatal("Parse accepted a semantically invalid config")
			}
			var serr *SemanticError
			if !errors.As(err, &serr) {
				t.Fatalf("error = %v (%T), want *SemanticError", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseWithComplexityLevels proves a config carrying the optional
// complexity_levels block and a typed default complexity parses cleanly.
func TestParseWithComplexityLevels(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
complexity_levels: {low: x, medium: y, high: z}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary], default_fields: {complexity: medium}}}
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(complexity_levels) = %v, want nil", err)
	}
	if c.Types["feature"].DefaultFields.Complexity != "medium" {
		t.Errorf("feature complexity = %q, want medium", c.Types["feature"].DefaultFields.Complexity)
	}
}

// TestParseOptionalSections proves the additive optional_sections key
// satisfies the schema AND round-trips through the DisallowUnknownFields JSON
// decode into the typed ItemType.OptionalSections — the schema↔struct seam
// (schema and struct must land together, or the DisallowUnknownFields decode
// rejects the new key). A valid entry (on-skeleton) parses; the off-skeleton
// rejection is covered by TestParseSemanticErrors.
func TestParseOptionalSections(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types:
  feature:
    body_skeleton: [Summary, Where to look, Done-means]
    optional_sections: [Where to look]
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(optional_sections) = %v, want nil", err)
	}
	got := c.Types["feature"].OptionalSections
	if len(got) != 1 || got[0] != "Where to look" {
		t.Errorf("OptionalSections = %v, want [Where to look]", got)
	}
}

// TestParseLabelCompletenessFields proves the additive label_defaults +
// required_label_namespaces keys satisfy the schema AND round-trip through the
// DisallowUnknownFields JSON decode into the typed ItemType — the schema↔struct
// seam (#1616): the struct fields must land in the same commit as the schema
// keys, or the DisallowUnknownFields decode rejects them. The prefix-mismatch
// rejection is covered by TestParseSemanticErrors.
func TestParseLabelCompletenessFields(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types:
  feature:
    body_skeleton: [Summary]
    label_defaults: {autonomy: autonomy:medium}
    required_label_namespaces: [area, autonomy]
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(label completeness fields) = %v, want nil", err)
	}
	ft := c.Types["feature"]
	if got := ft.LabelDefaults["autonomy"]; got != "autonomy:medium" {
		t.Errorf("LabelDefaults[autonomy] = %q, want autonomy:medium", got)
	}
	if got := strings.Join(ft.RequiredLabelNamespaces, ","); got != "area,autonomy" {
		t.Errorf("RequiredLabelNamespaces = %q, want area,autonomy", got)
	}
}

// TestParseWithStatesAndTransitions proves a config carrying the optional
// board states + transitions blocks parses cleanly and the typed maps are
// populated when every transition target is a declared canonical state.
func TestParseWithStatesAndTransitions(t *testing.T) {
	cfg := `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
states:
  backlog: Backlog
  in_progress: In Progress
  in_review: In Review
  blocked: Blocked
  done: Done
transitions:
  run_started: in_progress
  pr_opened: in_review
  run_failed: blocked
  run_merged: done
  issue_closed: done
  issue_reopened: backlog
`
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(states+transitions) = %v, want nil", err)
	}
	if c.States["in_progress"] != "In Progress" {
		t.Errorf("states[in_progress] = %q, want %q", c.States["in_progress"], "In Progress")
	}
	if c.Transitions["run_started"] != "in_progress" {
		t.Errorf("transitions[run_started] = %q, want %q", c.Transitions["run_started"], "in_progress")
	}
	// The issue-lifecycle edges (#1817) are accepted as new optional
	// transitions keys and resolve to declared canonical states.
	if c.Transitions["issue_closed"] != "done" {
		t.Errorf("transitions[issue_closed] = %q, want %q", c.Transitions["issue_closed"], "done")
	}
	if c.Transitions["issue_reopened"] != "backlog" {
		t.Errorf("transitions[issue_reopened] = %q, want %q", c.Transitions["issue_reopened"], "backlog")
	}
}

// TestDefaultStatesAndTransitions locks the shipped default's board-state
// map and lifecycle transitions (#1012, #1816): the canonical edges resolve
// through the states map to the Project #7 Status options, including the
// up_next state and the campaign_started edge. These are behavioral
// assertions on the SHIPPED default, so a comment-only no-op touch of the
// YAML that dropped the real key fails here.
func TestDefaultStatesAndTransitions(t *testing.T) {
	d := Default()
	wantStates := map[string]string{
		"backlog":     "Backlog",
		"up_next":     "Up Next",
		"in_progress": "In Progress",
		"in_review":   "In Review",
		"blocked":     "Blocked",
		"done":        "Done",
	}
	for k, want := range wantStates {
		if d.States[k] != want {
			t.Errorf("default states[%s] = %q, want %q", k, d.States[k], want)
		}
	}
	wantTransitions := map[string]string{
		"run_started":      "in_progress",
		"pr_opened":        "in_review",
		"run_failed":       "blocked",
		"run_merged":       "done",
		"campaign_started": "up_next",
		"issue_closed":     "done",
		"issue_reopened":   "backlog",
	}
	for event, target := range wantTransitions {
		if d.Transitions[event] != target {
			t.Errorf("default transitions[%s] = %q, want %q", event, d.Transitions[event], target)
		}
		// Cross-reference invariant: every default transition target must
		// resolve to a declared state.
		if _, ok := d.States[target]; !ok {
			t.Errorf("default transition %s targets %q, absent from states", event, target)
		}
	}
}

func TestParseMultiDocument(t *testing.T) {
	cfg := minimalConfig + "\n---\nspec_version: work-management-v0\n"
	_, err := Parse(strings.NewReader(cfg))
	if err == nil {
		t.Fatal("Parse accepted a multi-document stream")
	}
	var yerr *YAMLError
	if !errors.As(err, &yerr) {
		t.Fatalf("error = %v (%T), want *YAMLError", err, err)
	}
	if !strings.Contains(err.Error(), "single document") {
		t.Errorf("error %q does not name the single-document requirement", err.Error())
	}
}

func TestParseYAMLErrors(t *testing.T) {
	cases := map[string]string{
		"empty document":  "",
		"whitespace only": "   \n\t\n",
		"malformed yaml":  "provider: [unclosed",
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(cfg))
			if err == nil {
				t.Fatal("Parse accepted unparseable input")
			}
			var yerr *YAMLError
			if !errors.As(err, &yerr) {
				t.Fatalf("error = %v (%T), want *YAMLError", err, err)
			}
		})
	}
}

func TestParseReadError(t *testing.T) {
	readErr := errors.New("disk on fire")
	_, err := Parse(errReader{err: readErr})
	if !errors.Is(err, readErr) {
		t.Fatalf("Parse(failing reader) = %v, want wrapped %v", err, readErr)
	}
}

func TestEmbeddedSchemaHash(t *testing.T) {
	h := EmbeddedSchemaHash()
	if len(h) != 64 {
		t.Fatalf("EmbeddedSchemaHash() = %q, want 64 hex chars", h)
	}
	if strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("EmbeddedSchemaHash() = %q, want lowercase hex", h)
	}
	if again := EmbeddedSchemaHash(); again != h {
		t.Fatalf("EmbeddedSchemaHash() not stable: %q then %q", h, again)
	}
}

func TestErrorMessages(t *testing.T) {
	cause := errors.New("line 3: bad indent")
	yWithMsg := &YAMLError{Msg: "empty document"}
	if got := yWithMsg.Error(); !strings.Contains(got, "empty document") {
		t.Errorf("YAMLError.Error() = %q, want it to contain the message", got)
	}
	yWithCause := &YAMLError{Cause: cause}
	if got := yWithCause.Error(); !strings.Contains(got, cause.Error()) {
		t.Errorf("YAMLError.Error() = %q, want it to contain the cause", got)
	}
	if !errors.Is(yWithCause, cause) {
		t.Error("YAMLError does not unwrap to its cause")
	}
	yEmpty := &YAMLError{}
	if got := yEmpty.Error(); !strings.Contains(got, "unknown error") {
		t.Errorf("YAMLError.Error() = %q, want the unknown-error fallback", got)
	}

	serr := &SchemaError{Path: "/provider", Message: "value must be one of"}
	for _, want := range []string{serr.Path, serr.Message} {
		if !strings.Contains(serr.Error(), want) {
			t.Errorf("SchemaError.Error() = %q, want it to contain %q", serr.Error(), want)
		}
	}

	sem := &SemanticError{Msg: "type \"adr\" must declare a numbering rule"}
	if !strings.Contains(sem.Error(), "numbering rule") {
		t.Errorf("SemanticError.Error() = %q, want it to contain the message", sem.Error())
	}
}

func TestProductFeedbackEnabled(t *testing.T) {
	// Default-on: nil ProductFeedback means egress is allowed, so an
	// existing config keeps working without the field.
	if !(Conventions{}).ProductFeedbackEnabled() {
		t.Error("nil ProductFeedback should be enabled by default")
	}
	if !(Conventions{ProductFeedback: &ProductFeedback{Enabled: true}}).ProductFeedbackEnabled() {
		t.Error("enabled=true should be enabled")
	}
	// Kill-switch: explicit enabled=false disables egress.
	if (Conventions{ProductFeedback: &ProductFeedback{Enabled: false}}).ProductFeedbackEnabled() {
		t.Error("enabled=false should be the kill-switch (disabled)")
	}
}

func TestDefault_ProductFeedbackEnabled(t *testing.T) {
	// The shipped default advertises the kill-switch in its enabled
	// (egress-on) position.
	if !Default().ProductFeedbackEnabled() {
		t.Error("shipped default should have product feedback enabled")
	}
}

// TestParseProductFeedbackDisabled exercises the schema↔Go-struct seam: a
// config declaring the optional product_feedback.enabled:false kill-switch
// must satisfy the schema AND round-trip through the DisallowUnknownFields
// JSON decode into the typed *ProductFeedback. A per-layer unit would pass
// while the schema (object) and struct (object) shapes silently diverge.
func TestParseProductFeedbackDisabled(t *testing.T) {
	cfg := minimalConfig + "product_feedback:\n  enabled: false\n"
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(product_feedback disabled) = %v, want nil", err)
	}
	if c.ProductFeedback == nil {
		t.Fatal("ProductFeedback is nil; the field did not round-trip into the struct")
	}
	if c.ProductFeedbackEnabled() {
		t.Error("product_feedback.enabled:false should disable egress")
	}
}

// TestParseProductFeedbackEnabled is the egress-on companion: the explicit
// enabled:true object round-trips and leaves egress allowed.
func TestParseProductFeedbackEnabled(t *testing.T) {
	cfg := minimalConfig + "product_feedback:\n  enabled: true\n"
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(product_feedback enabled) = %v, want nil", err)
	}
	if c.ProductFeedback == nil {
		t.Fatal("ProductFeedback is nil; the field did not round-trip into the struct")
	}
	if !c.ProductFeedbackEnabled() {
		t.Error("product_feedback.enabled:true should keep egress enabled")
	}
}

// TestDefaultCharterPath is the DONE-MEANS test for the shipped default's
// charter declaration (E54.1 / #2233). A config/default-value change is not
// structurally enforced by compilation, so presence of a touched YAML file
// proves nothing: this asserts the shipped OBSERVABLE value, and then stats
// the declared path from the package directory so the conventional location
// is proven to name a file that actually exists in this repository rather
// than merely being a well-formed string. A comment-only or no-op touch of
// the default YAML fails here.
func TestDefaultCharterPath(t *testing.T) {
	d := Default()
	if d.Charter == nil {
		t.Fatal("Default().Charter is nil; the shipped default declares no charter block")
	}
	const want = ".fishhawk/charter.md"
	if d.Charter.Path != want {
		t.Fatalf("Default().Charter.Path = %q, want %q", d.Charter.Path, want)
	}

	// backend/internal/workmgmt -> repo root is three levels up.
	onDisk := filepath.Join("..", "..", "..", filepath.FromSlash(want))
	info, err := os.Stat(onDisk)
	if err != nil {
		t.Fatalf("stat %s: %v — the shipped default declares a conventional charter path that does not resolve to a file in this repo", onDisk, err)
	}
	if info.IsDir() {
		t.Fatalf("%s is a directory, want a charter document", onDisk)
	}
	if info.Size() == 0 {
		t.Fatalf("%s is empty, want a charter document", onDisk)
	}
}

// TestParseCharter proves a repo config can override the conventional
// location: the declared path round-trips verbatim into the typed struct.
func TestParseCharter(t *testing.T) {
	cfg := minimalConfig + "charter:\n  path: docs/charter.md\n"
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(charter) = %v, want nil", err)
	}
	if c.Charter == nil {
		t.Fatal("Charter is nil; the block did not round-trip into the struct")
	}
	if c.Charter.Path != "docs/charter.md" {
		t.Errorf("Charter.Path = %q, want %q", c.Charter.Path, "docs/charter.md")
	}
}

// TestParseWithoutCharter is the no-behaviour-change pin (AC2): the charter
// block is additive-optional, so a config omitting it parses exactly as it
// did before — and the POINTER field is what keeps absent (nil)
// distinguishable from present-but-empty. A value-typed field could not
// satisfy the nil assertion below.
func TestParseWithoutCharter(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("Parse(no charter) = %v, want nil", err)
	}
	if c.Charter != nil {
		t.Errorf("Charter = %+v, want nil for a config that declares no charter block", c.Charter)
	}
}

// TestParseCharterNoSemanticPathRule records the deliberate decision (E54.1
// / #2233) that workmgmt adds NO path-shape semantic rule: a traversal- or
// absolute-path declaration parses here and is rejected fail-closed by
// backend/internal/repodoc's validatePath at the single point where the
// declared path is used to fetch the document. A second copy of the rule
// here would be a second owner of the same invariant, free to drift from the
// one that actually gates the read. If a future change adds declaration-time
// path validation, this test is the one to delete deliberately.
func TestParseCharterNoSemanticPathRule(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "../escape.md", `a\b.md`} {
		t.Run(p, func(t *testing.T) {
			c, err := Parse(strings.NewReader(minimalConfig + "charter:\n  path: " + strconv.Quote(p) + "\n"))
			if err != nil {
				t.Fatalf("Parse(charter path %q) = %v, want nil: workmgmt validates the path structurally only", p, err)
			}
			if c.Charter == nil || c.Charter.Path != p {
				t.Fatalf("Charter = %+v, want path %q", c.Charter, p)
			}
		})
	}
}

// --- E54.8 / #2240: the grooming churn-guard threshold block ----------------

const groomingBlockConfig = `
grooming:
  thresholds:
    min_rank_movement: 4
    min_score_delta: 0.2
    significant_hygiene_defects:
      - absent_done_means
      - missing_estimate
`

// TestParseGroomingThresholds round-trips the full block into the typed
// struct and through the resolver.
func TestParseGroomingThresholds(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig + groomingBlockConfig))
	if err != nil {
		t.Fatalf("Parse(grooming block) = %v, want nil", err)
	}
	if c.Grooming == nil || c.Grooming.Thresholds == nil {
		t.Fatalf("Grooming = %+v, want a populated thresholds block", c.Grooming)
	}
	if got := c.Grooming.Thresholds.MinRankMovement; got == nil || *got != 4 {
		t.Errorf("min_rank_movement = %v, want 4", got)
	}
	if got := c.Grooming.Thresholds.MinScoreDelta; got == nil || *got != 0.2 {
		t.Errorf("min_score_delta = %v, want 0.2", got)
	}
	th := c.ResolveGroomingThresholds()
	if th.MinRankMovement != 4 || th.MinScoreDelta != 0.2 {
		t.Errorf("resolved = %+v, want the declared values", th)
	}
	if !th.IsSignificantHygieneDefect("absent_done_means") || th.IsSignificantHygieneDefect("unboarded") {
		t.Errorf("significance set = %v, want exactly the two declared", th.SignificantHygieneDefects)
	}
	if len(th.Clamped) != 0 {
		t.Errorf("clamped = %v, want none for in-range declared values", th.Clamped)
	}
}

// TestParseWithoutGrooming is the no-behaviour-change pin: the block is
// additive-optional, so a config omitting it parses as before and resolves the
// conservative package defaults. The POINTER field is what keeps absent (nil)
// distinguishable from present-but-empty.
func TestParseWithoutGrooming(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("Parse(no grooming) = %v, want nil", err)
	}
	if c.Grooming != nil {
		t.Errorf("Grooming = %+v, want nil for a config that declares no grooming block", c.Grooming)
	}
	th := c.ResolveGroomingThresholds()
	if th.MinRankMovement != DefaultGroomingMinRankMovement || th.MinScoreDelta != DefaultGroomingMinScoreDelta {
		t.Errorf("resolved = %+v, want the package defaults", th)
	}
}

// TestSchemaRejectsSubFloorThresholds asserts the JSON Schema itself refuses a
// value that would disable the guard, so the Go-side clamp in
// ResolveGroomingThresholds is a SECOND line rather than the only one. It also
// pins additionalProperties:false at both levels and the hygiene defect enum.
func TestSchemaRejectsSubFloorThresholds(t *testing.T) {
	cases := map[string]struct {
		cfg      string
		wantPath string
	}{
		"min_rank_movement 0 disables the bar": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    min_rank_movement: 0\n",
			wantPath: "/grooming/thresholds/min_rank_movement",
		},
		"min_rank_movement negative": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    min_rank_movement: -3\n",
			wantPath: "/grooming/thresholds/min_rank_movement",
		},
		"min_score_delta 0 disables the bar": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    min_score_delta: 0\n",
			wantPath: "/grooming/thresholds/min_score_delta",
		},
		"min_score_delta negative": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    min_score_delta: -0.1\n",
			wantPath: "/grooming/thresholds/min_score_delta",
		},
		"hygiene defect outside the grooming-report-v1 enum": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    significant_hygiene_defects: [not_a_defect]\n",
			wantPath: "/grooming/thresholds/significant_hygiene_defects/0",
		},
		"empty significance set": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    significant_hygiene_defects: []\n",
			wantPath: "/grooming/thresholds/significant_hygiene_defects",
		},
		"duplicate significance entries": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    significant_hygiene_defects: [unboarded, unboarded]\n",
			wantPath: "/grooming/thresholds/significant_hygiene_defects",
		},
		"unknown key under grooming": {
			cfg:      minimalConfig + "grooming:\n  extra: nope\n",
			wantPath: "/grooming",
		},
		"unknown key under grooming.thresholds": {
			cfg:      minimalConfig + "grooming:\n  thresholds:\n    extra: nope\n",
			wantPath: "/grooming/thresholds",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.cfg))
			var serr *SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("Parse = %v, want a *SchemaError", err)
			}
			if serr.Path != tc.wantPath {
				t.Errorf("SchemaError.Path = %q, want %q", serr.Path, tc.wantPath)
			}
		})
	}
}

// --- E45.24 / #2231: the board-selection source declaration ----------------

// TestParseSelection is the AC2-positive half: a config declaring the full
// block round-trips into the typed struct. It FAILS if either half of the
// declaration is missing — a schema without the property rejects the config
// as an unknown key under additionalProperties:false, and a Go struct without
// the field silently drops the value.
func TestParseSelection(t *testing.T) {
	cfg := minimalConfig + "selection:\n  source_view: \"Up Next\"\n  order_by: priority\n"
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(selection) = %v, want nil", err)
	}
	if c.Selection == nil {
		t.Fatal("Selection is nil; the block did not round-trip into the struct")
	}
	if c.Selection.SourceView != "Up Next" {
		t.Errorf("Selection.SourceView = %q, want %q", c.Selection.SourceView, "Up Next")
	}
	if c.Selection.OrderBy != "priority" {
		t.Errorf("Selection.OrderBy = %q, want %q", c.Selection.OrderBy, "priority")
	}
}

// TestParseSelectionOrderByOmittedIsUnset pins the omission contract settled
// by #2231's approval condition 1: order_by carries NO schema `default`
// annotation, so an omitted order_by parses to the EMPTY value and nothing
// populates it. Omission means "unset — no ordering policy declared", not
// "resolved to rank". A JSON Schema default is an annotation that populates
// nothing, so declaring one would have advertised behaviour no code
// implements while this assertion still held — which is exactly why the
// annotation was removed rather than the test weakened. The future consumer
// of this block is what decides what unset resolves to; this test goes red if
// anything here starts deciding it instead.
func TestParseSelectionOrderByOmittedIsUnset(t *testing.T) {
	cfg := minimalConfig + "selection:\n  source_view: \"Up Next\"\n"
	c, err := Parse(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("Parse(selection without order_by) = %v, want nil", err)
	}
	if c.Selection == nil {
		t.Fatal("Selection is nil; the block did not round-trip into the struct")
	}
	if c.Selection.SourceView != "Up Next" {
		t.Errorf("Selection.SourceView = %q, want %q", c.Selection.SourceView, "Up Next")
	}
	if c.Selection.OrderBy != "" {
		t.Errorf("Selection.OrderBy = %q, want the empty value: an omitted order_by is UNSET, and nothing may populate it", c.Selection.OrderBy)
	}
}

// TestParseWithoutSelection is the AC2 no-behaviour-change pin: the block is
// additive-optional, so a config omitting it parses exactly as it did before
// — and the POINTER field is what keeps absent (nil) distinguishable from
// present-but-empty. A value-typed field could not satisfy the nil assertion.
func TestParseWithoutSelection(t *testing.T) {
	c, err := Parse(strings.NewReader(minimalConfig))
	if err != nil {
		t.Fatalf("Parse(no selection) = %v, want nil", err)
	}
	if c.Selection != nil {
		t.Errorf("Selection = %+v, want nil for a config that declares no selection block", c.Selection)
	}
}

// TestDefaultDeclaresNoSelection is the DONE-MEANS test for the comment-only
// edit to docs/spec/work-management-default.yaml and its mirror. A YAML
// comment is invisible to compilation and a presence-of-file check cannot
// tell a commented example from a live key, so this asserts the SHIPPED
// OBSERVABLE value: the product default declares NO live selection block.
// Nothing consumes Conventions.Selection, so a live declaration in the
// product default would assert a behaviour that does not exist. This goes RED
// the moment someone uncomments the block, making a product-default
// declaration a deliberate decision rather than a side effect.
func TestDefaultDeclaresNoSelection(t *testing.T) {
	if s := Default().Selection; s != nil {
		t.Errorf("Default().Selection = %+v, want nil: the shipped default declares the selection block as a COMMENT only, because nothing consumes it yet", s)
	}
}

// boardCapabilityMatrixPath is the repo-root-relative capability matrix, read
// from the package directory the same way TestDefaultCharterPath reaches
// .fishhawk/charter.md: backend/internal/workmgmt -> repo root is three
// levels up.
var boardCapabilityMatrixPath = filepath.Join("..", "..", "..", "docs", "board-capability-matrix.md")

// collectDeclaredUnavailableReasons AST-walks the workmgmt package tree and
// returns every string value declared as an UnavailableReason constant, keyed
// by value with the declaring file recorded for the failure message.
//
// It is a SOURCE-DERIVED sweep, which is the whole point: a hand-maintained
// list of today's reasons verifies today's coverage and stays green on the day
// a seventh reason is added — precisely the drift the caller is named for.
//
// Const blocks carry the preceding spec's type forward when a later spec omits
// it, so lastType tracks that carry-over. Both the unqualified Ident
// (in-package, where reader.go declares them) and a qualified SelectorExpr
// (a sibling package declaring one as workmgmt.UnavailableReason) are matched,
// so moving the declarations does not silently empty the sweep.
func collectDeclaredUnavailableReasons(t *testing.T) map[string]string {
	t.Helper()

	const typeName = "UnavailableReason"
	found := map[string]string{}

	isReasonType := func(e ast.Expr) bool {
		switch v := e.(type) {
		case *ast.Ident:
			return v.Name == typeName
		case *ast.SelectorExpr:
			return v.Sel != nil && v.Sel.Name == typeName
		}
		return false
	}

	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			var lastType ast.Expr
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					lastType = vs.Type
				}
				if lastType == nil || !isReasonType(lastType) {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						return uerr
					}
					found[val] = path
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("AST walk of the workmgmt package tree failed: %v", walkErr)
	}
	return found
}

// TestBoardCapabilityMatrixDocumentsEveryDeclaredUnavailableReason is the
// coverage guard for docs/board-capability-matrix.md (E45.24 / #2231 AC1). It
// AST-walks the workmgmt package source for every DECLARED UnavailableReason
// constant and asserts each declared string value appears in the matrix, so
// AC1's "covers every row, including the degradation posture for each" is
// machine-enforced rather than asserted in prose.
//
// It enforces DRIFT, not a snapshot: the reason set comes from the source, so
// a seventh reason added to reader.go reddens this test on the day it is
// added, with nobody having to remember this document exists. A hand-written
// list of the six current values would verify today's coverage and stay green
// in exactly the situation the test is named for.
func TestBoardCapabilityMatrixDocumentsEveryDeclaredUnavailableReason(t *testing.T) {
	raw, err := os.ReadFile(boardCapabilityMatrixPath)
	if err != nil {
		t.Fatalf("read %s: %v — the capability matrix ADR-064 requires is missing", boardCapabilityMatrixPath, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatalf("%s is empty, want the per-forge board capability matrix", boardCapabilityMatrixPath)
	}
	doc := string(raw)

	declared := collectDeclaredUnavailableReasons(t)

	// Vacuity floor: a walk or parse regression that collected nothing would
	// make the loop below iterate zero times and pass having proven nothing.
	// The closed set has six members today; a floor below that still catches a
	// sweep that found ~nothing, without freezing the count.
	const minDeclared = 5
	if len(declared) < minDeclared {
		t.Fatalf("sweep collected only %d UnavailableReason constants from workmgmt source (< %d); "+
			"the AST walk is likely broken and this test would false-pass", len(declared), minDeclared)
	}

	// sortedKeys is apply.go's existing helper, reused so the failure
	// output is deterministic across map iterations.
	for _, reason := range sortedKeys(declared) {
		if !strings.Contains(doc, reason) {
			t.Errorf("UnavailableReason %q (declared in %s) is absent from %s — every degradation reason must carry a row in the capability matrix",
				reason, declared[reason], boardCapabilityMatrixPath)
		}
	}
}
