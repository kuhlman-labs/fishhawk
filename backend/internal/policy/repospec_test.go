package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		t.Run(tc.Workflow+"."+tc.Stage, func(t *testing.T) {
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

// agentPathLoaderBlock returns the marked loader block of a repospec test file.
func agentPathLoaderBlock(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(raw)
	const begin, end = "// BEGIN agent-path fixture loader", "// END agent-path fixture loader"
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("%s: agent-path loader markers missing or out of order", path)
	}
	return s[i : j+len(end)]
}

// TestAgentPathLoaderParity pins that this module's fixture loader is
// byte-identical to the runner module's twin, so the two tests cannot disagree
// about what makes the shared fixture valid.
func TestAgentPathLoaderParity(t *testing.T) {
	ours := agentPathLoaderBlock(t, "repospec_test.go")
	theirs := agentPathLoaderBlock(t, filepath.Join("..", "..", "..", "runner", "internal", "constraint", "repospec_test.go"))
	if ours != theirs {
		t.Fatalf("agent-path fixture loader drifted between backend/internal/policy and runner/internal/constraint repospec_test.go; make the BEGIN/END blocks identical")
	}
}
