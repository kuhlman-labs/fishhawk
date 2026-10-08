package artifact_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestArtifactKindEnumMatchesOpenAPI pins the REST surface's Artifact.kind
// enum (docs/api/v0.openapi.yaml) to the artifact.Kind consts declared in
// artifact.go, both ways (#4015, carried from #4011): a kind the store admits
// but the API does not advertise — upkeep_report and comms_report were — or an
// advertised kind no const declares fails here, naming both sites.
func TestArtifactKindEnumMatchesOpenAPI(t *testing.T) {
	declared := declaredArtifactKinds(t)
	if len(declared) == 0 {
		t.Fatal("found no `Kind = \"...\"` const in artifact.go; the parser or the type name drifted")
	}

	raw, err := os.ReadFile("../../../docs/api/v0.openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI document: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	enum := doc.Components.Schemas["Artifact"].Properties["kind"].Enum
	if len(enum) == 0 {
		t.Fatal("components.schemas.Artifact.properties.kind.enum is absent or empty in docs/api/v0.openapi.yaml")
	}
	advertised := map[string]bool{}
	for _, k := range enum {
		if advertised[k] {
			t.Errorf("Artifact.kind enum lists %q twice", k)
		}
		advertised[k] = true
	}

	for _, k := range sortedKeys(declared) {
		if !advertised[k] {
			t.Errorf("artifact.Kind %q (backend/internal/artifact/artifact.go) is missing from components.schemas.Artifact.properties.kind.enum in docs/api/v0.openapi.yaml", k)
		}
	}
	for _, k := range sortedKeys(advertised) {
		if !declared[k] {
			t.Errorf("docs/api/v0.openapi.yaml Artifact.kind enum lists %q but no artifact.Kind const declares it in backend/internal/artifact/artifact.go", k)
		}
	}
}

// declaredArtifactKinds returns the string value of every `Ident Kind =
// "value"` const in artifact.go.
func declaredArtifactKinds(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "artifact.go", nil, 0)
	if err != nil {
		t.Fatalf("parse artifact.go: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) == 0 {
				continue
			}
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "Kind" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("Kind const %v has a non-string-literal value; the guard cannot resolve it", vs.Names)
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					t.Fatalf("unquote %s: %v", lit.Value, uerr)
				}
				out[s] = true
			}
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
