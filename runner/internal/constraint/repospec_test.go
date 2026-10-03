package constraint

import (
	"encoding/json"
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
// bypass. Both stages also forbid the auto-loaded instruction files at any
// depth; feature_change deliberately leaves exactly ONE writable — the ROOT
// AGENTS.md, which the repo's conventions require updating — while
// routine_change (no human approval) forbids that too. The backend's own
// evaluator is pinned against the product-parsed spec by the twin test in
// backend/internal/policy (repospec_test.go); both read the shared path table in
// testdata/policy/agent-instruction-paths.json.
func TestRepoSpecForbidsAgentInstructionPaths(t *testing.T) {
	f := loadAgentPathFixture(t)
	cases := agentPathCases(f)
	for _, tc := range cases {
		t.Run(tc.wf, func(t *testing.T) {
			c := Constraints{ForbiddenPaths: stageForbiddenPaths(t, tc.wf, tc.stage)}
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

// agentPathFixture is testdata/policy/agent-instruction-paths.json, the ONE
// path table shared with this test's twin in the other module.
type agentPathFixture struct {
	ConfigDirPaths         []string `json:"config_dir_paths"`
	InstructionFiles       []string `json:"instruction_files"`
	RootAgents             string   `json:"root_agents"`
	RootAgentsCaseVariants []string `json:"root_agents_case_variants"`
	GuardFiles             []string `json:"guard_files"`
	Ordinary               []string `json:"ordinary"`
}

func loadAgentPathFixture(t *testing.T) agentPathFixture {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "policy", "agent-instruction-paths.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var f agentPathFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	if len(f.ConfigDirPaths) == 0 || len(f.InstructionFiles) == 0 || f.RootAgents == "" ||
		len(f.RootAgentsCaseVariants) == 0 || len(f.GuardFiles) == 0 || len(f.Ordinary) == 0 {
		t.Fatalf("%s: a path class is empty — the assertions would be vacuous", p)
	}
	return f
}

// agentPathCases composes the fixture's classes into each workflow's
// must-forbid / must-stay-writable sets. routine_change's guard files are
// excluded from must-forbid because its allowed_paths list (not
// forbidden_paths) already keeps scripts/ out of reach.
func agentPathCases(f agentPathFixture) []struct {
	wf, stage    string
	mustForbid   []string
	mustNotMatch []string
} {
	cat := func(lists ...[]string) []string {
		var out []string
		for _, l := range lists {
			out = append(out, l...)
		}
		return out
	}
	return []struct {
		wf, stage    string
		mustForbid   []string
		mustNotMatch []string
	}{
		{"feature_change", "implement",
			cat(f.ConfigDirPaths, f.InstructionFiles, f.RootAgentsCaseVariants, f.GuardFiles),
			cat(f.Ordinary, []string{f.RootAgents})},
		{"routine_change", "implement",
			cat(f.ConfigDirPaths, f.InstructionFiles, f.RootAgentsCaseVariants, []string{f.RootAgents}),
			f.Ordinary},
	}
}
