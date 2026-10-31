package basedelta

import (
	"bytes"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// adr098GlobalSnapshot and adr098SharedSnapshot are the ADR-098 built-in
// set, copied verbatim and in order from surface.go at the base commit
// (c156d109) before ADR-124 moved it into this repository's declaration.
var adr098GlobalSnapshot = []string{
	"go.work", "**/go.mod", "**/go.sum", "scripts/**", ".golangci.yml", "deploy/gate-image/**", ".fishhawk/**",
	"docs/spec/**", "docs/api/**", "site/**", "docs/adr/**", "docs/ARCHITECTURE.md",
	"backend/internal/postgres/migrations/**", "backend/internal/audit/categories.go", "testdata/**",
	"deploy/helm/**", "backend/cmd/fishhawkd/**", "backend/Dockerfile", "Makefile", ".github/workflows/**",
	"cli/internal/docgen/**", "cli/internal/cmdinfo/**", "cli/cmd/fishhawk-docgen/**",
	"backend/internal/version/**", "cli/internal/version/**", "runner/internal/version/**",
	"backend/internal/plan/schemas/**", "backend/internal/spec/schemas/**", "backend/internal/crewmessage/schemas/**",
	"backend/internal/specialistjob/schemas/**", "backend/internal/operatorrole/schemas/**",
	"backend/internal/workmgmt/schemas/**", "backend/internal/operatorrole/defaults/**",
	"backend/internal/workmgmt/defaults/**", "backend/internal/spec/presets/**", "cli/internal/spec/schemas/**",
	"cli/internal/spec/presets/**", "runner/internal/plan/schemas/**",
	"backend/internal/spec/testdata/predicate-fixtures.json",
	"backend/internal/spec/testdata/review-conventions-fixtures.json",
	"backend/internal/spec/testdata/tier-expansion-fixtures.json",
	"cli/internal/spec/testdata/predicate-fixtures.json",
	"cli/internal/spec/testdata/review-conventions-fixtures.json",
	"cli/internal/spec/testdata/tier-expansion-fixtures.json",
	"backend/internal/server/testdata/acceptance-verdict-fixtures.json",
	"runner/cmd/fishhawk-runner/testdata/acceptance-verdict-fixtures.json",
}

var adr098SharedSnapshot = []string{
	"docs/", "frontend/", "infra/", "acceptance/", "auth/", ".github/", ".agents/", ".claude/", ".githooks/", "deploy/",
}

// Keep the original ADR-098 snapshot intact; workflow verification adds
// exactly frontend/** to G. All original patterns retain their order.
func TestThisRepoDeclaration_ADR098PlusFrontend(t *testing.T) {
	t.Parallel()
	s := thisRepoSurface(t)
	var original []string
	frontend := 0
	for _, pattern := range s.Global {
		if pattern == "frontend/**" {
			frontend++
			continue
		}
		original = append(original, pattern)
	}
	if frontend != 1 || !reflect.DeepEqual(original, adr098GlobalSnapshot) {
		t.Errorf("global_surface = %v; want ADR-098 plus exactly frontend/**", s.Global)
	}
	if !reflect.DeepEqual(s.SharedDirs, adr098SharedSnapshot) {
		t.Errorf("shared_artifact_dirs = %v, want %v", s.SharedDirs, adr098SharedSnapshot)
	}
	if len(s.DocCheck) != 0 || len(s.Opaque) != 0 {
		t.Errorf("doc_check_inputs = %v opaque_paths = %v, want both empty", s.DocCheck, s.Opaque)
	}
}

// knownUnclassified names tracked paths a change deliberately leaves
// unclassified (safe: such a path only refuses transfer). Empty today.
var knownUnclassified = map[string]bool{}

// Every tracked path must force a pass, be opaque, or classify through the
// real graph and the declaration (adapter graph, else inert): a tracked path
// no class covers refuses every transfer touching it as unclassified_path.
func TestThisRepository_EveryTrackedPathClassified(t *testing.T) {
	t.Parallel()
	requireGo(t)
	root := repoRoot()
	inside, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		t.Skipf("%s is not a git work tree (%v); the tracked-path check needs one", root, err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	g, err := realRepoGraph()
	if err != nil {
		t.Fatalf("Load(real repo): %v", err)
	}
	s := thisRepoSurface(t)
	n := 0
	for _, raw := range bytes.Split(out, []byte{0}) {
		p := string(raw)
		if p == "" {
			continue
		}
		n++
		classified := s.matchVerdictWide(p) || s.matchDocCheck(p) || s.matchOpaque(p)
		if !classified {
			_, _, classified = classifyPaths(g, s, []string{p})
		}
		switch {
		case !classified && !knownUnclassified[p]:
			t.Errorf("tracked path %s is unclassified: add it to the declaration (%s) or knownUnclassified", p, thisRepoFixture)
		case classified && knownUnclassified[p]:
			t.Errorf("knownUnclassified lists %s, which now classifies: drop the stale entry", p)
		}
	}
	if n < 1000 {
		t.Errorf("git ls-files listed %d paths: parser collapse?", n)
	}
}
