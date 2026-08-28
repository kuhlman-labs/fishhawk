package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// validConventions is a schema-valid work-management conventions body used as
// the base for the loadCharterDeclaration fixtures. Callers append (or omit) a
// charter block.
const validConventions = `spec_version: work-management-v0
provider: github_projects
required_fields: [Summary, Done-means, complexity]
types:
  chore:
    body_skeleton: [Summary, Done-means]
`

// writeSpecWithConventions writes a spec file plus (when conventions != "") a
// sibling work-management.yaml, and returns the spec path.
func writeSpecWithConventions(t *testing.T, conventions string) string {
	t.Helper()
	dir := t.TempDir()
	specPath := filepath.Join(dir, "workflows.yaml")
	if err := os.WriteFile(specPath, []byte("version: \"2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if conventions != "" {
		if err := os.WriteFile(filepath.Join(dir, conventionsFileName), []byte(conventions), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return specPath
}

func TestLoadCharterDeclaration(t *testing.T) {
	t.Run("absent file -> shipped default", func(t *testing.T) {
		specPath := writeSpecWithConventions(t, "")
		declared, path, err := loadCharterDeclaration(specPath)
		if err != nil {
			t.Fatalf("err = %v, want nil (absent file admits via the shipped default)", err)
		}
		if !declared || path != defaultCharterPath {
			t.Errorf("(declared, path) = (%v, %q), want (true, %q)", declared, path, defaultCharterPath)
		}
	})

	t.Run("present with charter", func(t *testing.T) {
		specPath := writeSpecWithConventions(t, validConventions+"charter:\n  path: docs/charter.md\n")
		declared, path, err := loadCharterDeclaration(specPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !declared || path != "docs/charter.md" {
			t.Errorf("(declared, path) = (%v, %q), want (true, %q)", declared, path, "docs/charter.md")
		}
	})

	t.Run("present without charter", func(t *testing.T) {
		specPath := writeSpecWithConventions(t, validConventions)
		declared, path, err := loadCharterDeclaration(specPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if declared || path != "" {
			t.Errorf("(declared, path) = (%v, %q), want (false, \"\")", declared, path)
		}
	})

	t.Run("present with whitespace path", func(t *testing.T) {
		// A whitespace-only path passes the schema's minLength 1 but is trimmed
		// to empty by CharterAdmissionReason -> charter_path_empty. It is
		// returned VERBATIM here (untrimmed) so that branch stays distinguishable.
		specPath := writeSpecWithConventions(t, validConventions+"charter:\n  path: \"   \"\n")
		declared, path, err := loadCharterDeclaration(specPath)
		if err != nil {
			t.Fatalf("err = %v, want nil (whitespace path is schema-valid)", err)
		}
		if !declared || path != "   " {
			t.Errorf("(declared, path) = (%v, %q), want (true, %q)", declared, path, "   ")
		}
	})

	t.Run("unparseable YAML -> fail closed", func(t *testing.T) {
		specPath := writeSpecWithConventions(t, "::: not yaml :::\n")
		declared, _, err := loadCharterDeclaration(specPath)
		if err == nil {
			t.Fatalf("err = nil, want a fail-closed error for unparseable conventions")
		}
		if declared {
			t.Errorf("declared = true on a fail-closed read, want false")
		}
	})

	t.Run("schema-invalid conventions -> fail closed", func(t *testing.T) {
		// Drop the required spec_version: schema-invalid, so run admission would
		// refuse with conventions_unavailable; the CLI must fail closed too.
		specPath := writeSpecWithConventions(t, "provider: github_projects\nrequired_fields: [Summary]\ntypes:\n  chore:\n    body_skeleton: [Summary]\n")
		declared, _, err := loadCharterDeclaration(specPath)
		if err == nil {
			t.Fatalf("err = nil, want a fail-closed error for schema-invalid conventions")
		}
		if declared {
			t.Errorf("declared = true on a fail-closed read, want false")
		}
	})

	t.Run("unreadable file -> fail closed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: the 0000 mode bit does not deny read to the superuser, so this branch is vacuous; the unparseable-YAML case is the always-running proof of the same fail-closed return")
		}
		specPath := writeSpecWithConventions(t, validConventions)
		conventionsPath := filepath.Join(filepath.Dir(specPath), conventionsFileName)
		if err := os.Chmod(conventionsPath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(conventionsPath, 0o600) })
		declared, _, err := loadCharterDeclaration(specPath)
		if err == nil {
			t.Fatalf("err = nil, want a fail-closed error for an unreadable conventions file")
		}
		if declared {
			t.Errorf("declared = true on a fail-closed read, want false")
		}
	})
}

// TestDefaultCharterPathMatchesShippedDefault pins the step-5(i) coupling: the
// absent-file admit returns defaultCharterPath because the SHIPPED default
// conventions declare a charter at exactly that path. If the shipped default
// ever drops its charter block or changes the path, the absent-file admit would
// silently become a fail-open — this test turns that into a RED instead.
func TestDefaultCharterPathMatchesShippedDefault(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	// cli/cmd/fishhawk -> repo root.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	defaultPath := filepath.Join(root, "docs", "spec", "work-management-default.yaml")
	data, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatalf("read %s: %v", defaultPath, err)
	}
	var doc struct {
		Charter *struct {
			Path string `yaml:"path"`
		} `yaml:"charter"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal shipped default: %v", err)
	}
	if doc.Charter == nil {
		t.Fatalf("shipped default %s declares no charter block; the absent-file admit would be a fail-open", defaultPath)
	}
	if doc.Charter.Path != defaultCharterPath {
		t.Errorf("shipped default charter.path = %q, want %q (defaultCharterPath); the absent-file admit path is stale", doc.Charter.Path, defaultCharterPath)
	}
}
