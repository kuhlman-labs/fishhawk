package workmgmt

import (
	"errors"
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

	for _, typ := range []string{"feature", "bug", "chore", "adr"} {
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

// TestParseDoneMeansSpaceVariant proves the mandatory-field check is
// robust to "Done means" vs "Done-means".
func TestParseDoneMeansSpaceVariant(t *testing.T) {
	cfg := strings.Replace(minimalConfig, "  - Done-means", "  - Done means", 1)
	if _, err := Parse(strings.NewReader(cfg)); err != nil {
		t.Fatalf("Parse(Done means variant) = %v, want nil", err)
	}
}

func TestParseSchemaErrors(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key": minimalConfig + "playbook: nope\n",
		"wrong spec_version": `
spec_version: work-management-v9
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
		"unknown provider": `
spec_version: work-management-v0
provider: trello
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`,
		"empty body_skeleton": `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: []}}
`,
		"malformed label": `
spec_version: work-management-v0
provider: github_projects
project: {owner: a, number: 1}
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary], default_labels: ["NOT VALID"]}}
`,
		"non-object document": `just a string`,
		// product_feedback:{} omits the required enabled flag, guarding
		// required:["enabled"] against a regression that would re-introduce
		// the implicit enabled=false kill-switch footgun (#1132).
		"product_feedback missing required enabled": minimalConfig + "product_feedback: {}\n",
		// An unknown nested key under product_feedback guards the nested
		// object's additionalProperties:false (#1132).
		"product_feedback unknown nested key": minimalConfig + "product_feedback:\n  enabled: false\n  extra: nope\n",
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(cfg))
			if err == nil {
				t.Fatal("Parse accepted an invalid config")
			}
			var serr *SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("error = %v (%T), want *SchemaError", err, err)
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
}

// TestDefaultStatesAndTransitions locks the shipped default's board-state
// map and lifecycle transitions (#1012): the four canonical edges resolve
// through the states map to the Project #7 Status options.
func TestDefaultStatesAndTransitions(t *testing.T) {
	d := Default()
	wantStates := map[string]string{
		"backlog":     "Backlog",
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
		"run_started": "in_progress",
		"pr_opened":   "in_review",
		"run_failed":  "blocked",
		"run_merged":  "done",
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
