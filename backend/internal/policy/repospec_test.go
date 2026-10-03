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
	for _, tc := range loadAgentPathCases(t) {
		t.Run(tc.Workflow, func(t *testing.T) {
			c := Constraints{ForbiddenPaths: resolvedForbiddenPaths(t, tc.Workflow, tc.Stage)}
			for _, p := range tc.MustForbid {
				if !hasForbiddenViolation(Evaluate(diff(p), c)) {
					t.Errorf("%s.%s: %q is NOT forbidden (agent-instruction path admitted)", tc.Workflow, tc.Stage, p)
				}
			}
			for _, p := range tc.MustStayWritable {
				if v := Evaluate(diff(p), c); len(v) != 0 {
					t.Errorf("%s.%s: %q must stay writable but is forbidden: %v", tc.Workflow, tc.Stage, p, v)
				}
			}
		})
	}
}

// agentPathCase is one workflow stage's expectation, expanded from the shared
// fixture testdata/policy/agent-instruction-paths.json.
type agentPathCase struct {
	Workflow, Stage  string
	MustForbid       []string
	MustStayWritable []string
}

// loadAgentPathCases reads the shared fixture (ONE path table and case list
// for this test and its twin in the other module) and expands each case's
// class names into paths. An unknown or empty class fails the test, so a
// typo cannot make a case vacuous.
func loadAgentPathCases(t *testing.T) []agentPathCase {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "policy", "agent-instruction-paths.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var f struct {
		Classes map[string][]string `json:"classes"`
		Cases   []struct {
			Workflow         string   `json:"workflow"`
			Stage            string   `json:"stage"`
			MustForbid       []string `json:"must_forbid"`
			MustStayWritable []string `json:"must_stay_writable"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	expand := func(classes []string) []string {
		var out []string
		for _, c := range classes {
			paths := f.Classes[c]
			if len(paths) == 0 {
				t.Fatalf("%s: class %q is unknown or empty", p, c)
			}
			out = append(out, paths...)
		}
		return out
	}
	if len(f.Cases) == 0 {
		t.Fatalf("%s: no cases", p)
	}
	cases := make([]agentPathCase, 0, len(f.Cases))
	for _, c := range f.Cases {
		cases = append(cases, agentPathCase{
			Workflow:         c.Workflow,
			Stage:            c.Stage,
			MustForbid:       expand(c.MustForbid),
			MustStayWritable: expand(c.MustStayWritable),
		})
	}
	return cases
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
