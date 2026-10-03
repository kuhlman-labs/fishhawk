package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// repoSpecPath is this repository's own governing workflow spec, relative to
// this package directory.
var repoSpecPath = filepath.Join("..", "..", "..", ".fishhawk", "workflows.yaml")

// resolvedForbiddenPaths returns every forbidden_paths glob on stage stageID
// of workflow wf, as the PRODUCT resolves the spec (spec.ParseBytes, so
// workflow-v2 defaults/extends reuse applies). It fails the test when the
// workflow, stage or list is absent, so a renamed stage cannot turn the
// assertions vacuous.
func resolvedForbiddenPaths(t *testing.T, wf, stageID string) []string {
	t.Helper()
	raw, err := os.ReadFile(repoSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", repoSpecPath, err)
	}
	s, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", repoSpecPath, err)
	}
	w, ok := s.Workflows[wf]
	if !ok {
		t.Fatalf("workflow %q not found in %s", wf, repoSpecPath)
	}
	for _, st := range w.Stages {
		if st.ID != stageID {
			continue
		}
		var globs []string
		for _, c := range st.Constraints {
			globs = append(globs, c.ForbiddenPaths...)
		}
		if len(globs) == 0 {
			t.Fatalf("%s.%s has no forbidden_paths", wf, stageID)
		}
		return globs
	}
	t.Fatalf("stage %q not found in workflow %q", stageID, wf)
	return nil
}

// TestRepoSpecForbidsAgentInstructionPaths is the server-side twin of
// runner/internal/constraint's test of the same name: it runs the same path
// tables through THIS module's evaluator (policy.Evaluate) against the
// product-parsed spec. Agent-instruction paths are forbidden at ANY depth and
// in ANY letter case; feature_change leaves exactly one writable (the ROOT
// AGENTS.md, which the repo's conventions require updating), routine_change
// (no human approval) none. Keep the path tables equal to the runner's.
func TestRepoSpecForbidsAgentInstructionPaths(t *testing.T) {
	configDirPaths := []string{
		".agents/skills/x/SKILL.md",
		"backend/.agents/skills/x/SKILL.md",
		".Agents/skills/x/SKILL.md",
		"docs/.AGENTS/skills/x/SKILL.md",
		".claude/settings.json",
		"backend/.claude/skills/x/SKILL.md",
		".CLAUDE/settings.json",
		".codex/config.toml",
		"cli/.Codex/config.toml",
	}
	const rootAgents = "AGENTS.md"
	instructionFiles := []string{
		"backend/AGENTS.md",
		"docs/agents.md",
		"AGENTS.override.md",
		"backend/agents.OVERRIDE.md",
		"CLAUDE.md",
		"docs/claude.md",
		"CLAUDE.local.md",
		"web/claude.LOCAL.md",
	}
	ordinary := []string{
		"docs/README.md",
		"backend/internal/agents/agents.go",
		"docs/agents-guide.md",
	}

	cases := []struct {
		wf, stage    string
		mustForbid   []string
		mustNotMatch []string
	}{
		{"feature_change", "implement", append(append([]string{}, configDirPaths...), instructionFiles...), append(append([]string{}, ordinary...), rootAgents)},
		{"routine_change", "implement", append(append(append([]string{}, configDirPaths...), instructionFiles...), rootAgents), ordinary},
	}
	for _, tc := range cases {
		t.Run(tc.wf, func(t *testing.T) {
			c := Constraints{ForbiddenPaths: resolvedForbiddenPaths(t, tc.wf, tc.stage)}
			for _, p := range tc.mustForbid {
				if !hasForbiddenViolation(Evaluate(diff(p), c)) {
					t.Errorf("%s.%s: %q is NOT forbidden (agent-instruction path admitted)", tc.wf, tc.stage, p)
				}
			}
			for _, p := range tc.mustNotMatch {
				if v := Evaluate(diff(p), c); len(v) != 0 {
					t.Errorf("%s.%s: %q must stay writable but is forbidden: %v", tc.wf, tc.stage, p, v)
				}
			}
		})
	}
}

// hasForbiddenViolation reports whether vs carries a forbidden_paths hit, so
// a violation from any other constraint kind cannot stand in for one.
func hasForbiddenViolation(vs []Violation) bool {
	for _, v := range vs {
		if v.Constraint == "forbidden_paths" && len(v.Files) > 0 {
			return true
		}
	}
	return false
}
