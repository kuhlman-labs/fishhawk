package spec_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The workflow-v2 top-level `merge_method` declaration (ADR-110; E93.2 /
// #4658): one forge merge method per repository, read from the run's cached
// admission spec by ResolveMergeMethod, absent = squash. The cli twin is
// cli/internal/spec/merge_method_test.go. Every fixture below is a
// hypothetical, non-Fishhawk repository shape; the one exception reads this
// repository's own spec through shippedSpecRelPath.

// mmBaseDoc is a minimal valid v2 document; every case appends only a
// top-level merge_method line to it, so the positive control proves any
// rejection is the declaration's.
const mmBaseDoc = `version: "2"
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

// mmV1BaseDoc and mmV0BaseDoc are mmBaseDoc at a frozen major whose closed
// root declares no merge_method.
var (
	mmV1BaseDoc = strings.Replace(mmBaseDoc, `version: "2"`, `version: "1.1"`, 1)
	mmV0BaseDoc = strings.Replace(mmBaseDoc, `version: "2"`, `version: "0.5"`, 1)
)

func TestMergeMethod_ParseAcceptsEachDeclaredValue(t *testing.T) {
	s, err := spec.ParseBytes([]byte(mmBaseDoc))
	if err != nil {
		t.Fatalf("base document must parse (positive control): %v", err)
	}
	if s.MergeMethod != "" {
		t.Errorf("MergeMethod = %q, want empty when undeclared", s.MergeMethod)
	}
	for _, v := range []string{spec.MergeMethodSquash, spec.MergeMethodMerge, spec.MergeMethodRebase} {
		t.Run(v, func(t *testing.T) {
			s, err := spec.ParseBytes([]byte(mmBaseDoc + "merge_method: " + v + "\n"))
			if err != nil {
				t.Fatalf("ParseBytes(merge_method: %s): %v", v, err)
			}
			if s.MergeMethod != v {
				t.Errorf("MergeMethod = %q, want %q", s.MergeMethod, v)
			}
		})
	}
}

func TestMergeMethod_ParseRefusesOutsideTheClosedSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"unknown method", mmBaseDoc + "merge_method: fast_forward\n"},
		{"wrong letter case", mmBaseDoc + "merge_method: Squash\n"},
		{"empty string", mmBaseDoc + "merge_method: \"\"\n"},
		{"null", mmBaseDoc + "merge_method: null\n"},
		{"list", mmBaseDoc + "merge_method: [squash]\n"},
		{"v1 document declaring it", mmV1BaseDoc + "merge_method: merge\n"},
		{"v0 document declaring it", mmV0BaseDoc + "merge_method: merge\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SchemaError refusing merge_method", err)
			}
			if !strings.Contains(se.Error(), "merge_method") {
				t.Errorf("error %q does not name merge_method", se.Error())
			}
		})
	}
}

func TestResolveMergeMethod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes []byte
		want  spec.MergeMethodDecision
	}{
		{"nil bytes (no cached spec)", nil, spec.MergeMethodDecision{Method: spec.MergeMethodSquash}},
		{"empty bytes (no cached spec)", []byte{}, spec.MergeMethodDecision{Method: spec.MergeMethodSquash}},
		{"v2 spec, merge_method absent", []byte(mmBaseDoc), spec.MergeMethodDecision{Method: spec.MergeMethodSquash}},
		{"v2 spec declaring squash", []byte(mmBaseDoc + "merge_method: squash\n"), spec.MergeMethodDecision{Method: spec.MergeMethodSquash, Declared: true}},
		{"v2 spec declaring merge", []byte(mmBaseDoc + "merge_method: merge\n"), spec.MergeMethodDecision{Method: spec.MergeMethodMerge, Declared: true}},
		{"v2 spec declaring rebase", []byte(mmBaseDoc + "merge_method: rebase\n"), spec.MergeMethodDecision{Method: spec.MergeMethodRebase, Declared: true}},
		{"v1.1 spec (frozen major, always squash)", []byte(mmV1BaseDoc), spec.MergeMethodDecision{Method: spec.MergeMethodSquash}},
		{"v0.5 spec (frozen major, always squash)", []byte(mmV0BaseDoc), spec.MergeMethodDecision{Method: spec.MergeMethodSquash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spec.ResolveMergeMethod(tc.bytes)
			if err != nil {
				t.Fatalf("ResolveMergeMethod: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveMergeMethod = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveMergeMethod_UnparseableSpecFailsClosed pins that bytes ParseBytes
// refuses surface as an error, never as the squash default: a caller must
// refuse to merge rather than guess the method.
func TestResolveMergeMethod_UnparseableSpecFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes []byte
	}{
		{"malformed YAML", []byte("version: [\n")},
		{"whitespace-only document", []byte("  \n\t\n")},
		{"schema-invalid declaration", []byte(mmBaseDoc + "merge_method: fast_forward\n")},
		{"unsupported major", []byte(strings.Replace(mmBaseDoc, `version: "2"`, `version: "9"`, 1) + "merge_method: merge\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spec.ResolveMergeMethod(tc.bytes)
			if err == nil {
				t.Fatalf("ResolveMergeMethod = %+v with nil error, want the ParseBytes error (fail closed)", got)
			}
			if got != (spec.MergeMethodDecision{}) {
				t.Errorf("ResolveMergeMethod returned %+v alongside its error, want the zero decision", got)
			}
		})
	}
}

// TestResolveMergeMethod_ShippedSpecKeepsSquashDefault is the done-means pin
// for this repository: its own workflow spec declares no merge_method, so
// every merge path keeps today's squash. If an operator ever declares one,
// this test names the deliberate change to make here.
func TestResolveMergeMethod_ShippedSpecKeepsSquashDefault(t *testing.T) {
	raw, err := os.ReadFile(shippedSpecRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", shippedSpecRelPath, err)
	}
	got, err := spec.ResolveMergeMethod(raw)
	if err != nil {
		t.Fatalf("ResolveMergeMethod(%s): %v", shippedSpecRelPath, err)
	}
	want := spec.MergeMethodDecision{Method: spec.MergeMethodSquash, Declared: false}
	if got != want {
		t.Errorf("ResolveMergeMethod(%s) = %+v, want %+v: this repository merges squash by default; a declaration here is a deliberate change that must update this test", shippedSpecRelPath, got, want)
	}
}
