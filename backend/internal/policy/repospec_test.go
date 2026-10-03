package policy

import (
	"encoding/json"
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
// (no human approval) none. Both read the shared path table in
// testdata/policy/agent-instruction-paths.json.
func TestRepoSpecForbidsAgentInstructionPaths(t *testing.T) {
	f := loadAgentPathFixture(t)
	cases := agentPathCases(f)
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
