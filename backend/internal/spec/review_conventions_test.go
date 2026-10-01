package spec_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Review conventions (ADR-068 / E55.2 / #2243). Every rejection case here
// drives REAL bytes through ParseBytes (YAML -> version-routed schema -> typed
// decode with DisallowUnknownFields -> v2 reuse resolution -> Validate) and
// asserts the exact ValidationError path AND the shipped message constant, and
// each fixture is built so the rule under test is the ONLY one that fires: the
// other declared entries are selected, the selecting stage has an agent
// reviewer, and so on. That is what makes deleting any one control turn its
// case GREEN-to-RED rather than being masked by a sibling rule.

// rcPlanStage is a plan stage with one agent reviewer selecting `conventions`
// (a YAML flow list, e.g. "[backend]").
func rcPlanStage(conventions string) string {
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
          conventions: ` + conventions + `
`
}

// rcImplementStage is an implement stage with one agent reviewer selecting
// `conventions`.
func rcImplementStage(conventions string) string {
	return `      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        reviewers:
          agents:
            - provider: anthropic
          conventions: ` + conventions + `
`
}

// rcDoc renders a v2 document: the review_conventions block (already indented
// two spaces under the key; empty omits the key), then one workflow whose
// stages are the concatenated stage snippets.
func rcDoc(conventions string, stages ...string) []byte {
	doc := "version: \"2\"\n"
	if conventions != "" {
		doc += "review_conventions:\n" + conventions
	}
	doc += "workflows:\n  feature_change:\n    stages:\n" + strings.Join(stages, "")
	return []byte(doc)
}

// rcBackend declares one convention `backend` at a canonical path.
const rcBackend = "  backend:\n    path: docs/conventions/backend.md\n"

// rcEntry declares convention `name` with an arbitrary YAML body (indented
// four spaces).
func rcEntry(name, body string) string {
	return "  " + name + ":\n" + body
}

func pathInvalid(name, p, reason string) string {
	return fmt.Sprintf(spec.MsgFmtReviewConventionPathInvalid, name, p, reason)
}

func TestParse_ReviewConventions_Valid(t *testing.T) {
	t.Run("minimal entry selected by a plan stage", func(t *testing.T) {
		s, err := spec.ParseBytes(rcDoc(rcBackend, rcPlanStage("[backend]")))
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		conv, ok := s.ReviewConventions["backend"]
		if !ok || conv.Path != "docs/conventions/backend.md" {
			t.Fatalf("ReviewConventions = %#v, want backend -> docs/conventions/backend.md", s.ReviewConventions)
		}
		if conv.AppliesTo != nil || conv.SeverityCap != "" || conv.Required != nil {
			t.Fatalf("minimal entry decoded optional fields: %#v", conv)
		}
		if !conv.IsRequired() {
			t.Fatal("IsRequired() = false for an entry with no `required`, want the schema default true")
		}
		got := s.Workflows["feature_change"].Stages[0].Reviewers.Conventions
		if !reflect.DeepEqual(got, []string{"backend"}) {
			t.Fatalf("plan reviewers.conventions = %v, want [backend]", got)
		}
	})

	t.Run("applies_to + severity_cap + required:false, selected on plan and implement", func(t *testing.T) {
		conventions := rcEntry("crypto", `    path: docs/conventions/crypto.md
    applies_to:
      paths: ["backend/internal/crypto/**"]
      labels: [security]
    severity_cap: medium
    required: false
`) + rcBackend
		s, err := spec.ParseBytes(rcDoc(conventions, rcPlanStage("[crypto, backend]"), rcImplementStage("[backend]")))
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		c := s.ReviewConventions["crypto"]
		if c.AppliesTo == nil || !reflect.DeepEqual(c.AppliesTo.Paths, []string{"backend/internal/crypto/**"}) ||
			!reflect.DeepEqual(c.AppliesTo.Labels, []string{"security"}) {
			t.Fatalf("crypto.applies_to = %#v", c.AppliesTo)
		}
		if c.SeverityCap != spec.ReviewConventionSeverityCapMedium {
			t.Fatalf("crypto.severity_cap = %q, want medium", c.SeverityCap)
		}
		if c.Required == nil || *c.Required || c.IsRequired() {
			t.Fatalf("crypto.required = %v / IsRequired %v, want explicit false", c.Required, c.IsRequired())
		}
	})

	t.Run("conventions inherited from file-level defaults.reviewers onto a plan stage", func(t *testing.T) {
		doc := []byte(`version: "2"
defaults:
  reviewers:
    agents:
      - provider: anthropic
    conventions: [backend]
review_conventions:
` + rcBackend + `workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)
		s, err := spec.ParseBytes(doc)
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		r := s.Workflows["feature_change"].Stages[0].Reviewers
		if r == nil || !reflect.DeepEqual(r.Conventions, []string{"backend"}) {
			t.Fatalf("inherited reviewers = %#v, want conventions [backend] from defaults.reviewers", r)
		}
	})

	t.Run("a document declaring neither key is unaffected", func(t *testing.T) {
		s, err := spec.ParseBytes(rcDoc("", `      - id: plan
        type: plan
        executor:
          agent: claude-code
`))
		if err != nil {
			t.Fatalf("ParseBytes: %v", err)
		}
		if s.ReviewConventions != nil {
			t.Fatalf("ReviewConventions = %#v, want nil", s.ReviewConventions)
		}
	})
}

// TestParse_ReviewConventions_SemanticRejections pins one case per semantic
// rule, each asserting the exact path and the shipped message.
func TestParse_ReviewConventions_SemanticRejections(t *testing.T) {
	plan := rcPlanStage("[backend]")
	cases := []struct {
		name     string
		doc      []byte
		wantPath string
		wantMsg  string
	}{
		// --- declaration: canonical repo-relative path ---
		{
			name:     "path absolute",
			doc:      rcDoc(rcEntry("backend", "    path: /docs/conventions/backend.md\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "/docs/conventions/backend.md", spec.MsgReviewConventionPathAbsolute),
		},
		{
			name:     "path backslash",
			doc:      rcDoc(rcEntry("backend", "    path: 'docs\\conventions.md'\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", `docs\conventions.md`, spec.MsgReviewConventionPathBackslash),
		},
		{
			name:     "path dot-dot segment",
			doc:      rcDoc(rcEntry("backend", "    path: ../outside.md\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "../outside.md", fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, "..")),
		},
		{
			name:     "path dot segment",
			doc:      rcDoc(rcEntry("backend", "    path: docs/./backend.md\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "docs/./backend.md", fmt.Sprintf(spec.MsgFmtReviewConventionPathDotSegment, ".")),
		},
		{
			name:     "path empty segment",
			doc:      rcDoc(rcEntry("backend", "    path: docs//backend.md\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "docs//backend.md", spec.MsgReviewConventionPathEmptySegment),
		},
		{
			name:     "path non-canonical trailing slash",
			doc:      rcDoc(rcEntry("backend", "    path: docs/conventions/\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "docs/conventions/", spec.MsgReviewConventionPathEmptySegment),
		},
		{
			name:     "path control char",
			doc:      rcDoc(rcEntry("backend", "    path: \"docs/back\\tend.md\"\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "docs/back\tend.md", fmt.Sprintf(spec.MsgFmtReviewConventionPathControlChar, '\t')),
		},
		{
			name:     "path line separator",
			doc:      rcDoc(rcEntry("backend", "    path: \"docs/back\\u2028end.md\"\n"), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "docs/back\u2028end.md", fmt.Sprintf(spec.MsgFmtReviewConventionPathControlChar, '\u2028')),
		},
		// --- declaration: applies_to ---
		{
			name: "applies_to change_kind",
			doc: rcDoc(rcEntry("backend", `    path: docs/conventions/backend.md
    applies_to:
      change_kind: [refactor]
`), plan),
			wantPath: "/review_conventions/backend/applies_to/change_kind",
			wantMsg:  spec.MsgReviewConventionChangeKindUnsupported,
		},
		{
			name: "applies_to malformed glob (shared Predicate.Validate)",
			doc: rcDoc(rcEntry("backend", `    path: docs/conventions/backend.md
    applies_to:
      paths: ["a/[b"]
`), plan),
			wantPath: "/review_conventions/backend/applies_to",
			wantMsg:  `/review_conventions/backend/applies_to/paths/0: malformed path glob "a/[b"`,
		},
		// --- stage checks ---
		{
			name: "review-typed stage selects a convention",
			doc: rcDoc(rcBackend, plan, `      - id: review
        type: review
        executor:
          human: true
        reviewers:
          agents:
            - provider: anthropic
          conventions: [backend]
`),
			wantPath: "/workflows/feature_change/stages/1/reviewers/conventions",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionStageType, "review", "review"),
		},
		{
			name: "acceptance-typed stage selects a convention",
			doc: rcDoc(rcBackend, plan, `      - id: accept
        type: acceptance
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
          conventions: [backend]
`),
			wantPath: "/workflows/feature_change/stages/1/reviewers/conventions",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionStageType, "accept", "acceptance"),
		},
		{
			// `backend` is selected on implement so the unreferenced rule
			// cannot fire; only the undeclared `security` is wrong.
			name:     "undeclared name",
			doc:      rcDoc(rcBackend, rcPlanStage("[security]"), rcImplementStage("[backend]")),
			wantPath: "/workflows/feature_change/stages/0/reviewers/conventions/0",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionUnknown, "plan", "security", "security"),
		},
		{
			name:     "undeclared name at a later selector index",
			doc:      rcDoc(rcBackend, rcPlanStage("[backend, security]")),
			wantPath: "/workflows/feature_change/stages/0/reviewers/conventions/1",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionUnknown, "plan", "security", "security"),
		},
		{
			name: "no agent reviewers",
			doc: rcDoc(rcBackend, `      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          human: 1
          conventions: [backend]
`),
			wantPath: "/workflows/feature_change/stages/0/reviewers/conventions",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionNoAgents, "plan"),
		},
		// --- reference check ---
		{
			name:     "declared entry no stage selects",
			doc:      rcDoc(rcBackend+rcEntry("security", "    path: docs/conventions/security.md\n"), plan),
			wantPath: "/review_conventions/security",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionUnreferenced, "security"),
		},
		{
			// The reuse asymmetry: a selection made ONLY inside a
			// defaults.reviewers block that every stage overrides selects
			// nothing on the resolved document.
			name: "selected only in a defaults.reviewers block no stage inherits",
			doc: []byte(`version: "2"
defaults:
  reviewers:
    agents:
      - provider: anthropic
    conventions: [backend]
review_conventions:
` + rcBackend + `workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
`),
			wantPath: "/review_conventions/backend",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionUnreferenced, "backend"),
		},
		{
			// defaults.reviewers is taken WHOLE onto every stage without its
			// own reviewers, so it lands on the acceptance stage too and is
			// rejected there, at the stage's resolved path.
			name: "defaults.reviewers conventions inherited by an acceptance stage",
			doc: []byte(`version: "2"
defaults:
  reviewers:
    agents:
      - provider: anthropic
    conventions: [backend]
review_conventions:
` + rcBackend + `workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
      - id: accept
        type: acceptance
        executor:
          agent: claude-code
`),
			wantPath: "/workflows/feature_change/stages/1/reviewers/conventions",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionStageType, "accept", "acceptance"),
		},
		// --- rule order ---
		{
			name: "order: path wins over applies_to change_kind",
			doc: rcDoc(rcEntry("backend", `    path: /abs.md
    applies_to:
      change_kind: [refactor]
`), plan),
			wantPath: "/review_conventions/backend/path",
			wantMsg:  pathInvalid("backend", "/abs.md", spec.MsgReviewConventionPathAbsolute),
		},
		{
			name: "order: change_kind wins over a malformed glob",
			doc: rcDoc(rcEntry("backend", `    path: docs/conventions/backend.md
    applies_to:
      change_kind: [refactor]
      paths: ["a/[b"]
`), plan),
			wantPath: "/review_conventions/backend/applies_to/change_kind",
			wantMsg:  spec.MsgReviewConventionChangeKindUnsupported,
		},
		{
			name: "order: declarations are checked in sorted name order",
			doc: rcDoc(rcEntry("zeta", "    path: /z.md\n")+rcEntry("alpha", "    path: /a.md\n"),
				rcPlanStage("[alpha, zeta]")),
			wantPath: "/review_conventions/alpha/path",
			wantMsg:  pathInvalid("alpha", "/a.md", spec.MsgReviewConventionPathAbsolute),
		},
		{
			name: "order: stage type wins over an undeclared name",
			doc: rcDoc(rcBackend, plan, `      - id: review
        type: review
        executor:
          human: true
        reviewers:
          agents:
            - provider: anthropic
          conventions: [security]
`),
			wantPath: "/workflows/feature_change/stages/1/reviewers/conventions",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionStageType, "review", "review"),
		},
		{
			name: "order: undeclared name wins over no agent reviewers",
			doc: rcDoc(rcBackend, plan+`      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          human: 1
          conventions: [security]
`),
			wantPath: "/workflows/feature_change/stages/1/reviewers/conventions/0",
			wantMsg:  fmt.Sprintf(spec.MsgFmtReviewConventionUnknown, "implement", "security", "security"),
		},
		{
			name:     "order: a declaration error wins over the reference check",
			doc:      rcDoc(rcBackend+rcEntry("security", "    path: /security.md\n"), plan),
			wantPath: "/review_conventions/security/path",
			wantMsg:  pathInvalid("security", "/security.md", spec.MsgReviewConventionPathAbsolute),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(tc.doc)
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v (%T), want *ValidationError\ndoc:\n%s", err, err, tc.doc)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", ve.Path, tc.wantPath)
			}
			if ve.Message != tc.wantMsg {
				t.Errorf("message =\n  %s\nwant\n  %s", ve.Message, tc.wantMsg)
			}
		})
	}
}

// TestParse_ReviewConventions_SchemaRejections pins the schema-only controls
// (additionalProperties:false, the enum, uniqueItems, minItems, minProperties,
// propertyNames, required) — configuration with no Go control behind it.
func TestParse_ReviewConventions_SchemaRejections(t *testing.T) {
	plan := rcPlanStage("[backend]")
	cases := []struct {
		name     string
		doc      []byte
		wantPath string
	}{
		{"inline text alternative", rcDoc(rcEntry("backend", "    path: docs/b.md\n    text: be careful\n"), plan), "/review_conventions/backend"},
		{"severity_cap high", rcDoc(rcEntry("backend", "    path: docs/b.md\n    severity_cap: high\n"), plan), "/review_conventions/backend/severity_cap"},
		{"missing path", rcDoc(rcEntry("backend", "    severity_cap: low\n"), plan), "/review_conventions/backend"},
		{"empty path", rcDoc(rcEntry("backend", "    path: \"\"\n"), plan), "/review_conventions/backend/path"},
		{"applies_to as a bare glob list", rcDoc(rcEntry("backend", "    path: docs/b.md\n    applies_to: [\"backend/**\"]\n"), plan), "/review_conventions/backend/applies_to"},
		{"empty review_conventions map", []byte("version: \"2\"\nreview_conventions: {}\nworkflows:\n  feature_change:\n    stages:\n" + plan), "/review_conventions"},
		{"convention name not snake_case", rcDoc(rcEntry("Backend", "    path: docs/b.md\n"), rcPlanStage("[backend]")), "/review_conventions"},
		{"duplicate selector entry", rcDoc(rcBackend, rcPlanStage("[backend, backend]")), "/workflows/feature_change/stages/0/reviewers/conventions"},
		{"empty selector list", rcDoc(rcBackend, rcPlanStage("[]")), "/workflows/feature_change/stages/0/reviewers/conventions"},
		{"selector item not snake_case", rcDoc(rcBackend, rcPlanStage("[Backend]")), "/workflows/feature_change/stages/0/reviewers/conventions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes(tc.doc)
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want *SchemaError\ndoc:\n%s", err, err, tc.doc)
			}
			if se.Path != tc.wantPath && !strings.HasPrefix(se.Path, tc.wantPath+"/") {
				t.Errorf("schema error path = %q, want at or under %q (message: %s)", se.Path, tc.wantPath, se.Message)
			}
		})
	}
}

// rcHandBuiltSpec is a Validate-ready Spec declaring one convention at path p,
// selected by a plan stage with an agent reviewer — the fixture for inputs the
// YAML/schema layer cannot express (an empty path, invalid UTF-8).
func rcHandBuiltSpec(p string) *spec.Spec {
	return &spec.Spec{
		Version:           "2",
		ReviewConventions: map[string]spec.ReviewConvention{"backend": {Path: p}},
		Workflows: map[string]spec.Workflow{
			"feature_change": {Stages: []spec.Stage{{
				ID:       "plan",
				Type:     spec.StageTypePlan,
				Executor: spec.Executor{Agent: "claude-code"},
				Reviewers: &spec.ReviewersConfig{
					Agents:      []spec.AgentReviewer{{Provider: "anthropic"}},
					Conventions: []string{"backend"},
				},
			}}},
		},
	}
}

func TestValidate_ReviewConventionPath_HandBuiltOnly(t *testing.T) {
	for _, tc := range []struct {
		name, path, reason string
	}{
		{"empty", "", spec.MsgReviewConventionPathEmpty},
		{"invalid UTF-8", "docs/\xffbackend.md", spec.MsgReviewConventionPathInvalidUTF8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.Validate(rcHandBuiltSpec(tc.path))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if want := pathInvalid("backend", tc.path, tc.reason); ve.Path != "/review_conventions/backend/path" || ve.Message != want {
				t.Fatalf("got %s: %s\nwant /review_conventions/backend/path: %s", ve.Path, ve.Message, want)
			}
		})
	}
	if err := spec.Validate(rcHandBuiltSpec("docs/conventions/backend.md")); err != nil {
		t.Fatalf("control: canonical path rejected: %v", err)
	}
}

// TestReviewConventionPath_RejectsEveryNonCanonicalSpelling pins the property
// the omitted path.Clean rung would have asserted: every spelling path.Clean
// would rewrite is refused, and a canonical spelling (including dotted file
// and directory NAMES, which are not "." / ".." segments) is accepted.
func TestReviewConventionPath_RejectsEveryNonCanonicalSpelling(t *testing.T) {
	// Spellings path.Clean rewrites (non-canonical), then the out-of-tree
	// spellings it leaves untouched (".", "..", "../a", "/a"), which repodoc
	// refuses too.
	cleanRewrites := []string{"a//b", "a///b", "a/./b", "./a", "a/", "a/b/", "a/../b", "a/..", "a/b/.", "//a"}
	for _, p := range cleanRewrites {
		if path.Clean(p) == p {
			t.Fatalf("fixture %q is canonical; cleanRewrites must hold only spellings path.Clean rewrites", p)
		}
	}
	for _, p := range append(cleanRewrites, ".", "..", "../a", "/a") {
		var ve *spec.ValidationError
		if err := spec.Validate(rcHandBuiltSpec(p)); !errors.As(err, &ve) || ve.Path != "/review_conventions/backend/path" {
			t.Errorf("path %q: err = %v, want a ValidationError at /review_conventions/backend/path", p, err)
		}
	}
	for _, p := range []string{"a", "a/b.md", "docs/conventions/backend.md", "a..b/c", ".hidden/x.md", "x/..y", "CONVENTIONS.md"} {
		if path.Clean(p) != p {
			t.Fatalf("fixture %q is not canonical", p)
		}
		if err := spec.Validate(rcHandBuiltSpec(p)); err != nil {
			t.Errorf("canonical path %q rejected: %v", p, err)
		}
	}
}

// TestReviewConventionSeverityCapsMatchSchemaEnum holds the Go constants (and
// the planreview severity-ladder rationale beside them) against the schema
// enum: exactly [low, medium], `high` refused.
func TestReviewConventionSeverityCapsMatchSchemaEnum(t *testing.T) {
	raw, err := os.ReadFile("schemas/workflow-v2.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema mirror: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	got := doc.Defs["review_convention"].Properties["severity_cap"].Enum
	want := []string{spec.ReviewConventionSeverityCapLow, spec.ReviewConventionSeverityCapMedium}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema severity_cap enum = %v, want %v (the review_conventions.go constants)", got, want)
	}
}

func TestReviewConvention_IsRequired(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		req  *bool
		want bool
	}{
		{"absent defaults to true", nil, true},
		{"explicit false", &no, false},
		{"explicit true", &yes, true},
	} {
		if got := (spec.ReviewConvention{Path: "a.md", Required: tc.req}).IsRequired(); got != tc.want {
			t.Errorf("%s: IsRequired() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// rcSelectSpec is a hand-built Spec (NOT run through Validate) declaring the
// given conventions, for SelectReviewConventions.
func rcSelectSpec(convs map[string]spec.ReviewConvention) *spec.Spec {
	return &spec.Spec{Version: "2", ReviewConventions: convs}
}

func rcStage(t spec.StageType, names ...string) *spec.Stage {
	return &spec.Stage{
		ID:   string(t),
		Type: t,
		Reviewers: &spec.ReviewersConfig{
			Agents:      []spec.AgentReviewer{{Provider: "anthropic"}},
			Conventions: names,
		},
	}
}

func selectedNames(sel []spec.SelectedReviewConvention) []string {
	out := make([]string, 0, len(sel))
	for _, c := range sel {
		out = append(out, c.Name)
	}
	return out
}

func TestSelectReviewConventions(t *testing.T) {
	no := false
	s := rcSelectSpec(map[string]spec.ReviewConvention{
		"always":   {Path: "docs/always.md"},
		"crypto":   {Path: "docs/crypto.md", AppliesTo: &spec.Predicate{Paths: []string{"backend/internal/crypto/**"}}, SeverityCap: "low", Required: &no},
		"security": {Path: "docs/security.md", AppliesTo: &spec.Predicate{Labels: []string{"security"}}},
	})

	t.Run("nil stage, nil reviewers, empty selection return nothing", func(t *testing.T) {
		for _, st := range []*spec.Stage{nil, {ID: "p", Type: spec.StageTypePlan}, rcStage(spec.StageTypePlan)} {
			got, err := s.SelectReviewConventions(st, spec.Change{})
			if err != nil || got != nil {
				t.Fatalf("stage %#v: got %v, %v; want nil, nil", st, got, err)
			}
		}
	})

	t.Run("no applies_to attaches unconditionally, fields carried through", func(t *testing.T) {
		got, err := s.SelectReviewConventions(rcStage(spec.StageTypeImplement, "always"), spec.Change{})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := []spec.SelectedReviewConvention{{
			Name: "always", Path: "docs/always.md", SeverityCap: "", Required: true,
			DeclarationSite: "review_conventions.always in .fishhawk/workflows.yaml",
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v\nwant %#v", got, want)
		}
	})

	t.Run("paths match attaches with cap and required:false carried", func(t *testing.T) {
		got, err := s.SelectReviewConventions(rcStage(spec.StageTypePlan, "crypto"),
			spec.Change{Paths: []string{"backend/internal/crypto/aead.go"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 1 || got[0].Name != "crypto" || got[0].SeverityCap != "low" || got[0].Required {
			t.Fatalf("got %#v, want crypto with severity_cap low and Required false", got)
		}
	})

	t.Run("paths non-match is skipped", func(t *testing.T) {
		got, err := s.SelectReviewConventions(rcStage(spec.StageTypePlan, "crypto", "always"),
			spec.Change{Paths: []string{"frontend/app.ts"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !reflect.DeepEqual(selectedNames(got), []string{"always"}) {
			t.Fatalf("selected %v, want [always] (crypto's applies_to does not match)", selectedNames(got))
		}
	})

	t.Run("labels criterion", func(t *testing.T) {
		st := rcStage(spec.StageTypePlan, "security")
		got, err := s.SelectReviewConventions(st, spec.Change{Labels: []string{"bug", "security"}})
		if err != nil || !reflect.DeepEqual(selectedNames(got), []string{"security"}) {
			t.Fatalf("with label: got %v, %v; want [security]", selectedNames(got), err)
		}
		got, err = s.SelectReviewConventions(st, spec.Change{Labels: []string{"bug"}})
		if err != nil || len(got) != 0 {
			t.Fatalf("without label: got %v, %v; want none", selectedNames(got), err)
		}
	})

	t.Run("order follows the stage's list", func(t *testing.T) {
		got, err := s.SelectReviewConventions(rcStage(spec.StageTypeImplement, "security", "always", "crypto"),
			spec.Change{Paths: []string{"backend/internal/crypto/x.go"}, Labels: []string{"security"}})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if want := []string{"security", "always", "crypto"}; !reflect.DeepEqual(selectedNames(got), want) {
			t.Fatalf("order = %v, want %v", selectedNames(got), want)
		}
	})

	t.Run("a Match error is returned, never swallowed as a non-match", func(t *testing.T) {
		bad := rcSelectSpec(map[string]spec.ReviewConvention{
			"bad": {Path: "docs/bad.md", AppliesTo: &spec.Predicate{Paths: []string{"a/[b"}}},
		})
		got, err := bad.SelectReviewConventions(rcStage(spec.StageTypePlan, "bad"), spec.Change{Paths: []string{"a/x"}})
		if err == nil || !strings.Contains(err.Error(), `malformed glob "a/[b"`) || !strings.Contains(err.Error(), "review_conventions.bad") {
			t.Fatalf("got %v, %v; want an error naming review_conventions.bad and the malformed glob", got, err)
		}
		if got != nil {
			t.Fatalf("got %v alongside the error, want nil", got)
		}
	})

	t.Run("undeclared name errors", func(t *testing.T) {
		got, err := s.SelectReviewConventions(rcStage(spec.StageTypePlan, "always", "missing"), spec.Change{})
		if !errors.Is(err, spec.ErrReviewConventionUndeclared) || !strings.Contains(err.Error(), `"missing"`) || got != nil {
			t.Fatalf("got %v, %v; want ErrReviewConventionUndeclared naming \"missing\"", got, err)
		}
		var nilSpec *spec.Spec
		if _, err := nilSpec.SelectReviewConventions(rcStage(spec.StageTypePlan, "always"), spec.Change{}); !errors.Is(err, spec.ErrReviewConventionUndeclared) {
			t.Fatalf("nil *Spec: err = %v, want ErrReviewConventionUndeclared", err)
		}
	})

	t.Run("non-plan/implement stage errors", func(t *testing.T) {
		for _, typ := range []spec.StageType{spec.StageTypeReview, spec.StageTypeAcceptance, spec.StageTypeDeploy} {
			got, err := s.SelectReviewConventions(rcStage(typ, "always"), spec.Change{})
			if !errors.Is(err, spec.ErrReviewConventionStageType) || got != nil {
				t.Fatalf("%s stage: got %v, %v; want ErrReviewConventionStageType", typ, got, err)
			}
		}
	})
}
