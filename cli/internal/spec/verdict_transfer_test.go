package spec_test

import (
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/docgen"
	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// The cli half of the workflow-v2 `verdict_transfer` proof (ADR-124 / #4657):
// `fishhawk validate` must accept and refuse exactly what the backend's
// ParseBytes does (backend/internal/spec/verdict_transfer_test.go), so a
// repository's operator can check the block before it lands. It also pins
// that the generated site reference documents every member (the block lives
// under $defs/verdict_transfer, the shape docgen renders member by member).

const vtCLIBaseDoc = `version: "2"
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

// vtMembers is the closed member set of $defs/verdict_transfer.
var vtMembers = []string{
	"global_surface",
	"shared_artifact_dirs",
	"doc_check_inputs",
	"inert_paths",
	"opaque_paths",
	"completeness_check",
}

func TestValidateBytes_VerdictTransferAccepted(t *testing.T) {
	v1Base := strings.Replace(vtCLIBaseDoc, `version: "2"`, `version: "1.0"`, 1)
	cases := []struct {
		name string
		doc  string
	}{
		{"base document (positive control)", vtCLIBaseDoc},
		{"v1 base document (positive control)", v1Base},
		{"empty block", vtCLIBaseDoc + "verdict_transfer: {}\n"},
		{"full block", vtCLIBaseDoc + `verdict_transfer:
  global_surface: ["go.work", "**/go.mod", "ci/**"]
  shared_artifact_dirs: ["web/"]
  doc_check_inputs: ["docs/api/**"]
  inert_paths: ["**/README.md", "docs/**"]
  opaque_paths: ["vendor/**"]
  completeness_check: "go test ./internal/coverage -run TestDeclarationComplete"
`},
		// ADR-124 D2: a malformed entry is refused by the classifier
		// (invalid_surface:<entry>), never at admission.
		{"malformed pattern", vtCLIBaseDoc + "verdict_transfer:\n  global_surface: [\"scripts/[\", \"\"]\n  shared_artifact_dirs: [\"web\"]\n"},
		{"shell metacharacters in completeness_check", vtCLIBaseDoc + "verdict_transfer:\n  completeness_check: 'go test -run ''A|B'' && echo $(id) `whoami` ${HOME}'\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := spec.ValidateBytes([]byte(tc.doc)); err != nil {
				t.Fatalf("ValidateBytes: %v", err)
			}
		})
	}
}

func TestValidateBytes_VerdictTransferRejected(t *testing.T) {
	v1Base := strings.Replace(vtCLIBaseDoc, `version: "2"`, `version: "1.0"`, 1)
	cases := []struct {
		name string
		doc  string
	}{
		{"unknown member", vtCLIBaseDoc + "verdict_transfer:\n  global_surface: [\"go.mod\"]\n  extra_class: [\"x/**\"]\n"},
		{"non-string list item", vtCLIBaseDoc + "verdict_transfer:\n  inert_paths: [\"docs/**\", 7]\n"},
		{"non-array global_surface", vtCLIBaseDoc + "verdict_transfer:\n  global_surface: \"go.mod\"\n"},
		{"null block", vtCLIBaseDoc + "verdict_transfer: null\n"},
		{"v1 document carrying the block", v1Base + "verdict_transfer:\n  global_surface: [\"go.mod\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateBytes([]byte(tc.doc))
			if err == nil {
				t.Fatal("ValidateBytes accepted the document, want a rejection naming verdict_transfer")
			}
			if !strings.Contains(err.Error(), "verdict_transfer") {
				t.Errorf("error %q does not name verdict_transfer", err)
			}
		})
	}
}

// TestSiteReference_DocumentsEveryVerdictTransferMember renders the cli's
// embedded workflow-v2 schema mirror through the same docgen renderer
// scripts/gen-site-reference uses and requires a row for every member, then
// requires the committed site page to carry the same rows (the drift test
// TestSiteReferenceMatchesCommittedTree separately pins the page byte-exact
// to a fresh render). A root-only object would render as one row with no
// member table.
func TestSiteReference_DocumentsEveryVerdictTransferMember(t *testing.T) {
	schema, err := os.ReadFile("schemas/workflow-v2.schema.json")
	if err != nil {
		t.Fatalf("read schema mirror: %v", err)
	}
	r, err := docgen.RenderSchemaDoc("workflow-v2", schema)
	if err != nil {
		t.Fatalf("RenderSchemaDoc: %v", err)
	}
	site, err := os.ReadFile("../../../site/src/content/docs/reference/workflow-spec.md")
	if err != nil {
		t.Fatalf("read site reference: %v", err)
	}
	rendered := map[string]bool{}
	for _, p := range r.Rendered {
		rendered[p] = true
	}
	const heading = "##### `verdict_transfer`"
	for name, md := range map[string]string{"fresh render": r.Markdown, "committed site page": string(site)} {
		if !strings.Contains(md, heading) {
			t.Errorf("%s has no %q section", name, heading)
		}
		if !strings.Contains(md, "| `verdict_transfer` | `verdict_transfer` |") {
			t.Errorf("%s has no top-level verdict_transfer row referencing $defs/verdict_transfer", name)
		}
		for _, m := range vtMembers {
			if !strings.Contains(md, "| `"+m+"` |") {
				t.Errorf("%s has no row for verdict_transfer member %q", name, m)
			}
		}
	}
	for _, m := range vtMembers {
		if p := "/$defs/verdict_transfer/properties/" + m; !rendered[p] {
			t.Errorf("render did not emit %s", p)
		}
	}
}
