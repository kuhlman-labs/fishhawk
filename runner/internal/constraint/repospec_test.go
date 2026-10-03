package constraint

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoSpecPath is this repository's own workflow spec, relative to this
// package directory.
var repoSpecPath = filepath.Join("..", "..", "..", ".fishhawk", "workflows.yaml")

// stageForbiddenPaths returns the forbidden_paths of stage stageID in
// workflow wf of the repository's real .fishhawk/workflows.yaml, failing the
// test if the workflow, stage or list is absent (so a renamed stage cannot
// turn this test vacuous).
func stageForbiddenPaths(t *testing.T, wf, stageID string) []string {
	t.Helper()
	raw, err := os.ReadFile(repoSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", repoSpecPath, err)
	}
	var spec struct {
		Workflows map[string]struct {
			Stages []struct {
				ID          string `yaml:"id"`
				Constraints struct {
					ForbiddenPaths []string `yaml:"forbidden_paths"`
				} `yaml:"constraints"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", repoSpecPath, err)
	}
	w, ok := spec.Workflows[wf]
	if !ok {
		t.Fatalf("workflow %q not found in %s", wf, repoSpecPath)
	}
	for _, st := range w.Stages {
		if st.ID == stageID {
			if len(st.Constraints.ForbiddenPaths) == 0 {
				t.Fatalf("%s.%s has no forbidden_paths", wf, stageID)
			}
			return st.Constraints.ForbiddenPaths
		}
	}
	t.Fatalf("stage %q not found in workflow %q", stageID, wf)
	return nil
}

// TestRepoSpecForbidsAgentInstructionPaths pins that this repository's own
// implement stages forbid agent-instruction paths at ANY depth and in ANY
// letter case, through the real forbidden-paths evaluator. Codex discovers
// .agents/skills at every directory level, Claude Code discovers nested
// .claude/, and on a case-insensitive filesystem a CLI opening `.agents`
// resolves to a tracked `.Agents` — so a root-only or exact-case glob is a
// bypass. routine_change (which merges without human approval) additionally
// forbids the auto-loaded instruction files; feature_change deliberately does
// not, because the repo's conventions require AGENTS.md updates there.
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
	instructionFiles := []string{
		"AGENTS.md",
		"backend/AGENTS.md",
		"docs/agents.md",
		"AGENTS.override.md",
		"backend/agents.OVERRIDE.md",
		"CLAUDE.md",
		"docs/claude.md",
		"CLAUDE.local.md",
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
		{"feature_change", "implement", configDirPaths, append(append([]string{}, ordinary...), "AGENTS.md")},
		{"routine_change", "implement", append(append([]string{}, configDirPaths...), instructionFiles...), ordinary},
	}
	for _, tc := range cases {
		t.Run(tc.wf, func(t *testing.T) {
			c := Constraints{ForbiddenPaths: stageForbiddenPaths(t, tc.wf, tc.stage)}
			for _, p := range tc.mustForbid {
				if v := Evaluate(diff(p), c); len(v) == 0 {
					t.Errorf("%s.%s: %q is NOT forbidden (agent-instruction path admitted)", tc.wf, tc.stage, p)
				}
			}
			for _, p := range tc.mustNotMatch {
				if v := Evaluate(diff(p), c); len(v) != 0 {
					t.Errorf("%s.%s: ordinary path %q is forbidden: %v", tc.wf, tc.stage, p, v)
				}
			}
		})
	}
}
