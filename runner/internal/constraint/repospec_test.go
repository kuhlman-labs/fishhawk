package constraint

import (
	"bytes"
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
	for _, tc := range loadAgentPathCases(t) {
		t.Run(tc.Workflow+"."+tc.Stage, func(t *testing.T) {
			c := Constraints{ForbiddenPaths: stageForbiddenPaths(t, tc.Workflow, tc.Stage)}
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

// BEGIN agent-path fixture loader — byte-identical in
// runner/internal/constraint/repospec_test.go and
// backend/internal/policy/repospec_test.go (the modules cannot import each
// other); backend/internal/policy TestAgentPathLoaderParity fails on drift.

// agentPathCase is one workflow stage's expectation, expanded from the shared
// fixture testdata/policy/agent-instruction-paths.json.
type agentPathCase struct {
	Workflow, Stage  string
	MustForbid       []string
	MustStayWritable []string
}

// requiredAgentPathWorkflows are the workflow stages the fixture MUST cover,
// so deleting a case cannot silently drop its assertions.
var requiredAgentPathWorkflows = []string{"feature_change.implement", "routine_change.implement"}

// loadAgentPathCases reads the shared fixture and expands each case's class
// names into paths. It fails closed on anything that would make the test
// vacuous: an unknown JSON key (a misspelled must_forbid), an unknown or empty
// class, a case with an empty must_forbid or must_stay_writable, or a missing
// required workflow stage.
func loadAgentPathCases(t *testing.T) []agentPathCase {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "policy", "agent-instruction-paths.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var f struct {
		Comment string              `json:"_comment"`
		Classes map[string][]string `json:"classes"`
		Cases   []struct {
			Workflow         string   `json:"workflow"`
			Stage            string   `json:"stage"`
			MustForbid       []string `json:"must_forbid"`
			MustStayWritable []string `json:"must_stay_writable"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	expand := func(where string, classes []string) []string {
		if len(classes) == 0 {
			t.Fatalf("%s: %s lists no classes", p, where)
		}
		var out []string
		for _, c := range classes {
			paths := f.Classes[c]
			if len(paths) == 0 {
				t.Fatalf("%s: %s: class %q is unknown or empty", p, where, c)
			}
			out = append(out, paths...)
		}
		return out
	}
	seen := map[string]bool{}
	cases := make([]agentPathCase, 0, len(f.Cases))
	for _, c := range f.Cases {
		id := c.Workflow + "." + c.Stage
		seen[id] = true
		cases = append(cases, agentPathCase{
			Workflow:         c.Workflow,
			Stage:            c.Stage,
			MustForbid:       expand(id+" must_forbid", c.MustForbid),
			MustStayWritable: expand(id+" must_stay_writable", c.MustStayWritable),
		})
	}
	for _, id := range requiredAgentPathWorkflows {
		if !seen[id] {
			t.Fatalf("%s: required case %s is missing", p, id)
		}
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

// END agent-path fixture loader
