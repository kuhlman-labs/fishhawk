package spec_test

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// TestCharterMessageParityAcrossModules is the MECHANICAL drift guard for the
// hand-duplicated charter strings (E54.11 / #2801, approval condition AC2). The
// backend (package server) and the CLI (package spec) live in separate Go
// modules and cannot share a constant, so parity is asserted by reading both
// source files over the repo root and requiring the identical VALUES declared in
// each — the same posture TestAppliesToChangeKindMessageParity and
// TestGraphShapeMessageParity take.
//
// It asserts the VALUES verbatim, NOT the const names: a test that matched only
// the names would pass while the values diverged (the binding non-vacuity
// condition). The two message templates share a name across modules (both
// exported MsgFmt*), so those are matched as whole `const NAME = "..."` lines;
// the three reason consts DIFFER in name (backend unexported reason*, CLI
// exported Reason*), so their VALUES are matched as quoted substrings.
//
// COUNTERFACTUAL (approval condition): change one character of the CLI's
// MsgFmtCharterRequired and this test goes RED — `want` is derived from the CLI
// const, so it no longer matches the unchanged backend source. Same for any of
// the three reason values.
func TestCharterMessageParityAcrossModules(t *testing.T) {
	const backendPath = "../../../backend/internal/server/charter_gate.go"
	const cliPath = "../../../cli/internal/spec/charter.go"

	// Whole-line message-const parity: declared verbatim, single-line, in both.
	messageLines := []string{
		"const MsgFmtCharterRequired = " + strconv.Quote(spec.MsgFmtCharterRequired),
		"const MsgCharterConventionsUnreadableSuffix = " + strconv.Quote(spec.MsgCharterConventionsUnreadableSuffix),
	}
	// Reason VALUE parity: the quoted value appears in both files. Names differ
	// across modules, so match the value, not the declaration line.
	reasonValues := []string{
		strconv.Quote(spec.ReasonCharterAbsent),
		strconv.Quote(spec.ReasonCharterPathEmpty),
		strconv.Quote(spec.ReasonConventionsUnavailable),
	}

	for _, path := range []string{backendPath, cliPath} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		s := string(src)
		for _, want := range messageLines {
			if !strings.Contains(s, want) {
				t.Errorf("%s does not declare the charter message verbatim;\nwant the line: %s\n"+
					"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or "+
					"`fishhawk validate` and the backend will report different text for the same charter refusal.", path, want)
			}
		}
		for _, want := range reasonValues {
			if !strings.Contains(s, want) {
				t.Errorf("%s does not declare the charter reason value %s verbatim;\n"+
					"the three reason values must stay byte-identical across modules or details.reason will drift.", path, want)
			}
		}
	}
}

// --- WorkflowsRequiringCharter ---

// groomingDirectCLIDoc: a v2 workflow whose grooming stage declares the
// grooming_report artifact DIRECTLY.
const groomingDirectCLIDoc = `version: "2"
workflows:
  tidy_the_backlog:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
`

// groomingInheritedCLIDoc: derived_groom carries the grooming_report ONLY through
// its `extends: base_groom` base — its own declared stage produces a
// pull_request. After reuse resolution derived_groom's resolved stages include
// the base's grooming stage, so it must be flagged; over the UNRESOLVED tree it
// would be missed. This is counterfactual (3)'s vehicle.
const groomingInheritedCLIDoc = `version: "2"
workflows:
  base_groom:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
  derived_groom:
    extends: base_groom
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// nonGroomingCLIDoc: an ordinary code-change workflow, no grooming_report.
const nonGroomingCLIDoc = `version: "2"
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

// multiWorkflowCLIDoc: two grooming workflows plus one non-grooming; the result
// must be the grooming ids only, sorted.
const multiWorkflowCLIDoc = `version: "2"
workflows:
  zzz_groom:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  aaa_groom:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
`

func TestWorkflowsRequiringCharter(t *testing.T) {
	t.Run("direct produces", func(t *testing.T) {
		got, err := spec.WorkflowsRequiringCharter([]byte(groomingDirectCLIDoc))
		if err != nil {
			t.Fatalf("WorkflowsRequiringCharter: %v", err)
		}
		if len(got) != 1 || got[0] != "tidy_the_backlog" {
			t.Errorf("got %v, want [tidy_the_backlog]", got)
		}
	})

	t.Run("inherited produces via extends", func(t *testing.T) {
		got, err := spec.WorkflowsRequiringCharter([]byte(groomingInheritedCLIDoc))
		if err != nil {
			t.Fatalf("WorkflowsRequiringCharter: %v", err)
		}
		// derived_groom is flagged ONLY because reuse resolution folds the base's
		// grooming stage into it — the discriminating assertion for counterfactual (3).
		if !contains(got, "derived_groom") {
			t.Errorf("got %v, want it to include derived_groom (grooming inherited through extends)", got)
		}
		if !contains(got, "base_groom") {
			t.Errorf("got %v, want it to include base_groom", got)
		}
	})

	t.Run("non-grooming is empty", func(t *testing.T) {
		got, err := spec.WorkflowsRequiringCharter([]byte(nonGroomingCLIDoc))
		if err != nil {
			t.Fatalf("WorkflowsRequiringCharter: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want empty for a non-grooming spec", got)
		}
	})

	t.Run("multi-workflow returns sorted grooming ids only", func(t *testing.T) {
		got, err := spec.WorkflowsRequiringCharter([]byte(multiWorkflowCLIDoc))
		if err != nil {
			t.Fatalf("WorkflowsRequiringCharter: %v", err)
		}
		want := []string{"aaa_groom", "zzz_groom"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got %v, want %v (sorted, grooming only)", got, want)
			}
		}
	})
}

// TestWorkflowsRequiringCharter_ErrorAndMalformedShapes exercises the error
// return and the skip-on-shape-mismatch branches. The malformed shapes are
// version-less (major 0) so no reuse resolution runs on them; each is simply not
// something to inspect, and the walk must skip it without panicking rather than
// flag it.
func TestWorkflowsRequiringCharter_ErrorAndMalformedShapes(t *testing.T) {
	t.Run("decodeAndResolve error is surfaced", func(t *testing.T) {
		// An unsupported version major fails decodeAndResolve's version routing.
		_, err := spec.WorkflowsRequiringCharter([]byte("version: \"9\"\nworkflows: {}\n"))
		if err == nil {
			t.Errorf("err = nil, want the version-routing error surfaced")
		}
	})

	malformed := []struct {
		name string
		doc  string
	}{
		{name: "top-level not a map", doc: "[]"},
		{name: "workflows not a map", doc: "workflows: hi"},
		{name: "workflow value not a map", doc: "workflows:\n  a: hi"},
		{name: "stages not a list", doc: "workflows:\n  a:\n    stages: hi"},
		{name: "stage not a map", doc: "workflows:\n  a:\n    stages:\n      - notamap"},
		{name: "produces entry not a map", doc: "workflows:\n  a:\n    stages:\n      - produces:\n          - notamap"},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spec.WorkflowsRequiringCharter([]byte(tc.doc))
			if err != nil {
				t.Fatalf("err = %v, want nil (malformed shapes are skipped, not errored)", err)
			}
			if len(got) != 0 {
				t.Errorf("got %v, want empty — a malformed shape must not be flagged as grooming", got)
			}
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// --- CharterAdmissionReason truth table ---

func TestCharterAdmissionReason(t *testing.T) {
	cases := []struct {
		name       string
		declared   bool
		path       string
		loadErr    error
		wantReason string
	}{
		{name: "admit", declared: true, path: ".fishhawk/charter.md", wantReason: ""},
		{name: "charter_absent", declared: false, path: "", wantReason: spec.ReasonCharterAbsent},
		{name: "charter_path_empty (empty)", declared: true, path: "", wantReason: spec.ReasonCharterPathEmpty},
		{name: "charter_path_empty (whitespace)", declared: true, path: "   ", wantReason: spec.ReasonCharterPathEmpty},
		{name: "conventions_unavailable (err wins over absent)", declared: false, path: "", loadErr: errors.New("boom"), wantReason: spec.ReasonConventionsUnavailable},
		{name: "conventions_unavailable (err wins over declared)", declared: true, path: ".fishhawk/charter.md", loadErr: errors.New("boom"), wantReason: spec.ReasonConventionsUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spec.CharterAdmissionReason(tc.declared, tc.path, tc.loadErr); got != tc.wantReason {
				t.Errorf("CharterAdmissionReason(%v, %q, %v) = %q, want %q", tc.declared, tc.path, tc.loadErr, got, tc.wantReason)
			}
		})
	}
}

// --- ValidateConventionsDocument ---

// validConventionsCLIDoc satisfies the work-management-v0 SCHEMA (required
// spec_version/provider/types/required_fields; provider enum; types
// patternProperties). It deliberately omits the `project` block that provider
// github_projects requires — that is a backend SEMANTIC check, not a schema
// rule, so it is admitted here (see TestValidateConventionsDocument_SemanticGapAdmitted).
const validConventionsCLIDoc = `spec_version: work-management-v0
provider: github_projects
required_fields: [Summary, Done-means, complexity]
types:
  chore:
    body_skeleton: [Summary, Done-means]
`

func TestValidateConventionsDocument(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		if err := spec.ValidateConventionsDocument([]byte(validConventionsCLIDoc)); err != nil {
			t.Errorf("ValidateConventionsDocument = %v, want nil for a schema-valid single document", err)
		}
	})

	t.Run("schema violation -> ValidationError", func(t *testing.T) {
		// Drop the required spec_version.
		bad := strings.Replace(validConventionsCLIDoc, "spec_version: work-management-v0\n", "", 1)
		err := spec.ValidateConventionsDocument([]byte(bad))
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("err = %v, want *ValidationError for a schema violation", err)
		}
	})

	t.Run("multi-document -> ParseError", func(t *testing.T) {
		multi := validConventionsCLIDoc + "---\n" + validConventionsCLIDoc
		err := spec.ValidateConventionsDocument([]byte(multi))
		var pe *spec.ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want *ParseError for a multi-document stream", err)
		}
		if !strings.Contains(pe.Msg, "multiple YAML documents") {
			t.Errorf("ParseError = %q, want it to name the multi-document rejection", pe.Msg)
		}
	})

	t.Run("empty -> ParseError", func(t *testing.T) {
		err := spec.ValidateConventionsDocument([]byte("   \n"))
		var pe *spec.ParseError
		if !errors.As(err, &pe) {
			t.Errorf("err = %v, want *ParseError for an empty document", err)
		}
	})

	t.Run("malformed non-empty YAML -> ParseError", func(t *testing.T) {
		err := spec.ValidateConventionsDocument([]byte("{ unterminated\n"))
		var pe *spec.ParseError
		if !errors.As(err, &pe) {
			t.Errorf("err = %v, want *ParseError for unparseable YAML", err)
		}
	})
}

// TestValidateConventionsDocument_SemanticGapAdmitted DOCUMENTS the deliberate
// residual (approval condition AC4 / escape hatch (b)): the CLI validates the
// conventions file against the canonical SCHEMA but does NOT reproduce the
// backend's cross-field SEMANTIC checks (which live in Go, not the schema). A
// conventions file that violates ONLY a semantic rule — here, provider
// github_projects with no `project` connection block, which
// workmgmt.validateSemantics rejects — is ADMITTED here though run admission
// refuses it. This is the intended direction: the CLI can be LESS strict than
// run admission (a missed early warning), never MORE strict. Asserted as
// admitted, with this comment naming it as a known divergence rather than a bug.
func TestValidateConventionsDocument_SemanticGapAdmitted(t *testing.T) {
	// validConventionsCLIDoc is github_projects with no project block: schema-OK,
	// semantically-invalid at the backend.
	if err := spec.ValidateConventionsDocument([]byte(validConventionsCLIDoc)); err != nil {
		t.Errorf("ValidateConventionsDocument = %v, want nil — the CLI intentionally does not run the backend's semantic checks", err)
	}
}
