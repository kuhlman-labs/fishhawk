package spec_test

import (
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// The cli half of the workflow-v2 `merge_method` proof (ADR-110; E93.2 /
// #4658): `fishhawk validate` must accept and refuse exactly what the
// backend's ParseBytes does (backend/internal/spec/merge_method_test.go), so a
// repository's operator can check the declaration before it lands. Fixtures
// are hand-written, hypothetical repository shapes; this test reads no
// repository file. The generated site reference's coverage of the property is
// already byte-pinned by cli/internal/docgen/drift_test.go.

const mmCLIBaseDoc = `version: "2"
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

func TestValidateBytes_MergeMethodAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"absent (positive control)", mmCLIBaseDoc},
		{"squash", mmCLIBaseDoc + "merge_method: squash\n"},
		{"merge", mmCLIBaseDoc + "merge_method: merge\n"},
		{"rebase", mmCLIBaseDoc + "merge_method: rebase\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := spec.ValidateBytes([]byte(tc.doc)); err != nil {
				t.Fatalf("ValidateBytes: %v", err)
			}
		})
	}
}

func TestValidateBytes_MergeMethodRejected(t *testing.T) {
	v1Base := strings.Replace(mmCLIBaseDoc, `version: "2"`, `version: "1.1"`, 1)
	v0Base := strings.Replace(mmCLIBaseDoc, `version: "2"`, `version: "0.5"`, 1)
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"unknown method", mmCLIBaseDoc + "merge_method: fast_forward\n"},
		{"wrong letter case", mmCLIBaseDoc + "merge_method: Squash\n"},
		{"empty string", mmCLIBaseDoc + "merge_method: \"\"\n"},
		{"null", mmCLIBaseDoc + "merge_method: null\n"},
		{"list", mmCLIBaseDoc + "merge_method: [squash]\n"},
		{"v1 document declaring it", v1Base + "merge_method: merge\n"},
		{"v0 document declaring it", v0Base + "merge_method: merge\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ValidateBytes accepted the document, want a rejection naming merge_method")
			}
			if !strings.Contains(err.Error(), "merge_method") {
				t.Errorf("error %q does not name merge_method", err)
			}
		})
	}
}
