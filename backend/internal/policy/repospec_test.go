package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agentpaths"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// twinRepospecPath is the runner module's copy of this test file.
var twinRepospecPath = filepath.Join("..", "..", "..", "runner", "internal", "constraint", "repospec_test.go")

// repoSpecStage is one implement stage as the PRODUCT resolves it: its
// forbidden_paths globs plus the built-in agent-instruction-path rule from
// spec.Stage.AgentInstructionPathsRule (#3942).
type repoSpecStage struct {
	forbidden []string
	enforced  bool
	writable  []string
}

// loadRepoSpecImplementStages returns every `implement` stage in this
// repository's own .fishhawk/workflows.yaml, keyed "<workflow>.<stage>", as
// the PRODUCT resolves the spec (spec.ParseBytes, so workflow-v2
// defaults/extends reuse applies).
func loadRepoSpecImplementStages(t *testing.T) map[string]repoSpecStage {
	t.Helper()
	p := filepath.Join("..", "..", "..", ".fishhawk", "workflows.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	s, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	out := map[string]repoSpecStage{}
	for wf, w := range s.Workflows {
		for _, st := range w.Stages {
			if st.Type != spec.StageTypeImplement {
				continue
			}
			var rs repoSpecStage
			for _, c := range st.Constraints {
				rs.forbidden = append(rs.forbidden, c.ForbiddenPaths...)
			}
			rs.enforced, rs.writable = st.AgentInstructionPathsRule()
			out[wf+"."+st.ID] = rs
		}
	}
	return out
}

// repoSpecRefusals names what refuses a change to p on st: forbidden_paths
// through this package's Evaluate, and the built-in rule through
// agentpaths.IsInstructionPath minus the stage's exact writable exemptions.
// The built-in half mirrors the post-implement gate's decision directly
// because policy.Constraints does not carry the built-in yet (#3942 slice 2);
// once it does, route both halves through Evaluate.
func repoSpecRefusals(st repoSpecStage, p string) []string {
	out := refusingConstraints(Evaluate(diff(p), Constraints{ForbiddenPaths: st.forbidden}))
	if st.enforced && agentpaths.IsInstructionPath(p) && !slices.Contains(st.writable, p) {
		out = append(out, "agent_instruction_paths")
	}
	return out
}

// BEGIN agent-path fixture loader — byte-identical in
// runner/internal/constraint/repospec_test.go and
// backend/internal/policy/repospec_test.go (the modules cannot import each
// other). TestAgentPathLoaderParity, present in BOTH modules so a scoped
// verify of either one runs it, fails on any drift. Only how each module
// parses the spec (loadRepoSpecImplementStages, repoSpecStage) and evaluates
// one path against a stage (repoSpecRefusals) differs, and sits outside this
// block.

// TestRepoSpecForbidsAgentInstructionPaths pins that this repository's own
// implement stages refuse agent-instruction paths at ANY depth and in ANY
// letter case. Since #3942 that protection is the product's built-in
// agent_instruction_paths constraint (on for every implement stage) plus each
// stage's declared relaxation, not hand-written forbidden_paths globs, so each
// path is evaluated through this module's own resolution of both: the
// stage's forbidden_paths AND its built-in rule. Codex discovers
// .agents/skills at every directory level, Claude Code discovers nested
// .claude/, and on a case-insensitive filesystem a CLI opening `.agents`
// resolves to a tracked `.Agents`, so a root-only or exact-case match is a
// bypass. feature_change relaxes exactly ONE path (the exact ROOT AGENTS.md,
// which the conventions require updating), routine_change (no human
// approval) none. The path table and per-stage expectations are data, in the
// shared fixture testdata/policy/agent-instruction-paths.json; EVERY
// implement stage in the spec must have a case there.
func TestRepoSpecForbidsAgentInstructionPaths(t *testing.T) {
	stages := loadRepoSpecImplementStages(t)
	required := make([]string, 0, len(stages))
	for id := range stages {
		required = append(required, id)
	}
	for _, tc := range loadAgentPathCases(t, required) {
		id := tc.Workflow + "." + tc.Stage
		t.Run(id, func(t *testing.T) {
			st, ok := stages[id]
			if !ok {
				t.Fatalf("%s is not an implement stage in the spec", id)
			}
			for _, p := range tc.MustForbid {
				if len(repoSpecRefusals(st, p)) == 0 {
					t.Errorf("%s: %q is NOT refused by forbidden_paths or the built-in agent_instruction_paths rule", id, p)
				}
			}
			for _, p := range tc.MustStayWritable {
				if r := repoSpecRefusals(st, p); len(r) != 0 {
					t.Errorf("%s: %q must stay writable but is refused by %v", id, p, r)
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

// loadAgentPathCases reads the shared fixture and expands each case's class
// names into paths. It fails closed on anything that would make the test
// vacuous or ambiguous: a duplicate JSON key (encoding/json would silently
// keep the last one), trailing content, an unknown key (a misspelled
// must_forbid), an unknown or empty class, an empty must_forbid or
// must_stay_writable, a duplicate case, a case naming no implement stage, or
// a missing case for any id in required (every implement stage in the spec).
func loadAgentPathCases(t *testing.T, required []string) []agentPathCase {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "policy", "agent-instruction-paths.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := checkStrictJSON(raw); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	var f struct {
		// Comment is never read: it exists so DisallowUnknownFields accepts
		// the fixture's documentary `_comment` key. Do not delete it.
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
	if len(required) == 0 {
		t.Fatalf("no implement stages found in the spec: the test would be vacuous")
	}
	known := map[string]bool{}
	for _, id := range required {
		known[id] = true
	}
	seen := map[string]bool{}
	cases := make([]agentPathCase, 0, len(f.Cases))
	for _, c := range f.Cases {
		id := c.Workflow + "." + c.Stage
		if seen[id] {
			t.Fatalf("%s: duplicate case %s", p, id)
		}
		if !known[id] {
			t.Fatalf("%s: case %s names no implement stage in the spec", p, id)
		}
		seen[id] = true
		cases = append(cases, agentPathCase{
			Workflow:         c.Workflow,
			Stage:            c.Stage,
			MustForbid:       expand(id+" must_forbid", c.MustForbid),
			MustStayWritable: expand(id+" must_stay_writable", c.MustStayWritable),
		})
	}
	for _, id := range required {
		if !seen[id] {
			t.Fatalf("%s: implement stage %s has no case", p, id)
		}
	}
	return cases
}

// checkStrictJSON rejects a duplicate object key at any depth, and any
// content after the single top-level value.
func checkStrictJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if keys[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				keys[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // the closing delimiter
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing content after the top-level value")
	}
	return nil
}

// refusingConstraints names the forbidden_paths and agent_instruction_paths
// violations in vs that carry files, so a violation from any other
// constraint kind cannot stand in for a refusal.
func refusingConstraints(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		if (v.Constraint == "forbidden_paths" || v.Constraint == "agent_instruction_paths") && len(v.Files) > 0 {
			out = append(out, v.Constraint)
		}
	}
	return out
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

// TestAgentPathLoaderParity pins that this module's marked block is
// byte-identical to the other module's twin, so the two tests cannot disagree
// about what makes the shared fixture valid or what they assert. It exists in
// both modules, so a scoped verify of either one runs it.
func TestAgentPathLoaderParity(t *testing.T) {
	ours := agentPathLoaderBlock(t, "repospec_test.go")
	theirs := agentPathLoaderBlock(t, twinRepospecPath)
	if ours != theirs {
		t.Fatalf("agent-path fixture loader drifted between backend/internal/policy and runner/internal/constraint repospec_test.go; make the BEGIN/END blocks identical")
	}
}
