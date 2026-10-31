package spec_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The workflow-v2 `verdict_transfer` block (ADR-124 / #4657). The schema
// enforces only the member types and the closed member set: an empty,
// incomplete or malformed declaration must PARSE, because ADR-124 D2 makes
// each of those a classification refusal (basedelta), never an admission
// failure that would block a repository's unrelated runs. The cli twin is
// cli/internal/spec/verdict_transfer_test.go.

// vtBaseDoc is a minimal valid v2 document; every case appends only a
// top-level block to it, so the positive control proves any rejection is the
// block's.
const vtBaseDoc = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// vtShellMetachars is a completeness_check value full of shell syntax. The
// field is a declarative reference that fishhawkd never executes or
// interpolates, so it must come back as exactly these bytes.
const vtShellMetachars = "go test ./x -run 'A|B' && echo $HOME ${PATH} $(id) `whoami`; rm -rf / > /dev/null"

// vtV1BaseDoc is vtBaseDoc at major 1, whose closed root declares no
// verdict_transfer; TestVerdictTransfer_BaseDocumentAccepted proves it parses
// without the block.
var vtV1BaseDoc = strings.Replace(vtBaseDoc, `version: "2"`, `version: "1.0"`, 1)

func vtParse(t *testing.T, block string) (*spec.Spec, error) {
	t.Helper()
	return spec.ParseBytes([]byte(vtBaseDoc + block))
}

func TestVerdictTransfer_BaseDocumentAccepted(t *testing.T) {
	s, err := vtParse(t, "")
	if err != nil {
		t.Fatalf("base document must parse (positive control): %v", err)
	}
	if s.VerdictTransfer != nil {
		t.Errorf("VerdictTransfer = %+v, want nil when the block is absent (undeclared)", s.VerdictTransfer)
	}
	s, err = spec.ParseBytes([]byte(vtV1BaseDoc))
	if err != nil {
		t.Fatalf("v1 base document must parse (positive control for the v1 rejection): %v", err)
	}
	if s.VerdictTransfer != nil {
		t.Errorf("v1 VerdictTransfer = %+v, want nil: no v0/v1 schema declares the block", s.VerdictTransfer)
	}
}

func TestVerdictTransfer_EmptyBlockIsDeclaredEmpty(t *testing.T) {
	s, err := vtParse(t, "verdict_transfer: {}\n")
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if s.VerdictTransfer == nil {
		t.Fatal("VerdictTransfer = nil, want non-nil: an empty block is declared (refused later as surface_incomplete), not undeclared")
	}
	if want := (spec.VerdictTransfer{}); !reflect.DeepEqual(*s.VerdictTransfer, want) {
		t.Errorf("VerdictTransfer = %+v, want every member zero", *s.VerdictTransfer)
	}
}

func TestVerdictTransfer_FullBlockRoundTrips(t *testing.T) {
	block := `verdict_transfer:
  global_surface:
    - "go.work"
    - "**/go.mod"
    - "ci/**"
  shared_artifact_dirs:
    - "web/"
    - "assets/"
  doc_check_inputs:
    - "docs/api/**"
  inert_paths:
    - "**/README.md"
    - "docs/**"
  opaque_paths:
    - "vendor/**"
  completeness_check: "go test ./internal/coverage -run TestDeclarationComplete"
`
	s, err := vtParse(t, block)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	want := &spec.VerdictTransfer{
		GlobalSurface:      []string{"go.work", "**/go.mod", "ci/**"},
		SharedArtifactDirs: []string{"web/", "assets/"},
		DocCheckInputs:     []string{"docs/api/**"},
		InertPaths:         []string{"**/README.md", "docs/**"},
		OpaquePaths:        []string{"vendor/**"},
		CompletenessCheck:  "go test ./internal/coverage -run TestDeclarationComplete",
	}
	if !reflect.DeepEqual(s.VerdictTransfer, want) {
		t.Errorf("VerdictTransfer = %+v\nwant %+v", s.VerdictTransfer, want)
	}
}

// TestVerdictTransfer_MalformedPatternAcceptedAtParse pins ADR-124 D2's
// no-admission-failure rule: a doublestar syntax error, a non-canonical
// directory and an empty entry are the classifier's to refuse
// (invalid_surface:<entry>), so the schema carries no pattern, minLength or
// minItems facet that would reject them here.
func TestVerdictTransfer_MalformedPatternAcceptedAtParse(t *testing.T) {
	block := `verdict_transfer:
  global_surface: ["scripts/[", ""]
  shared_artifact_dirs: ["web", "/"]
  inert_paths: []
`
	s, err := vtParse(t, block)
	if err != nil {
		t.Fatalf("a malformed entry must parse (refused later as invalid_surface): %v", err)
	}
	if got, want := s.VerdictTransfer.GlobalSurface, []string{"scripts/[", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("GlobalSurface = %q, want %q", got, want)
	}
	if got, want := s.VerdictTransfer.SharedArtifactDirs, []string{"web", "/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SharedArtifactDirs = %q, want %q", got, want)
	}
}

// TestVerdictTransfer_CompletenessCheckIsPlainText pins that completeness_check
// is declarative text: shell metacharacters, command substitutions and
// variable references come back byte-identical, never expanded or evaluated.
func TestVerdictTransfer_CompletenessCheckIsPlainText(t *testing.T) {
	t.Setenv("HOME", "/expanded-home-would-show-here")
	block := "verdict_transfer:\n  global_surface: [\"go.mod\"]\n  completeness_check: '" +
		strings.ReplaceAll(vtShellMetachars, "'", "''") + "'\n"
	s, err := vtParse(t, block)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := s.VerdictTransfer.CompletenessCheck; got != vtShellMetachars {
		t.Errorf("CompletenessCheck = %q, want the declared text verbatim %q", got, vtShellMetachars)
	}
}

func TestVerdictTransfer_Rejections(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"unknown member", vtBaseDoc + "verdict_transfer:\n  global_surface: [\"go.mod\"]\n  extra_class: [\"x/**\"]\n"},
		{"non-string list item", vtBaseDoc + "verdict_transfer:\n  inert_paths: [\"docs/**\", 7]\n"},
		{"non-array global_surface", vtBaseDoc + "verdict_transfer:\n  global_surface: \"go.mod\"\n"},
		{"non-string completeness_check", vtBaseDoc + "verdict_transfer:\n  completeness_check: [\"a\"]\n"},
		{"null block", vtBaseDoc + "verdict_transfer: null\n"},
		{"v1 document carrying the block", vtV1BaseDoc + "verdict_transfer:\n  global_surface: [\"go.mod\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ParseBytes accepted the document, want a rejection naming verdict_transfer")
			}
			// A *SchemaError proves the SCHEMA refused it, not the typed
			// decode's DisallowUnknownFields backstop.
			var serr *spec.SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("error %q (%T) is not a *spec.SchemaError", err, err)
			}
			if !strings.Contains(err.Error(), "verdict_transfer") {
				t.Errorf("error %q does not name verdict_transfer", err)
			}
		})
	}
}
