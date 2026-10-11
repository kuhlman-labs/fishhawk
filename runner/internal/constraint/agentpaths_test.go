package constraint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// pkgSrcDir anchors fixture paths on this package's source dir (#4179).
var pkgSrcDir = mustGetwd()

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}

// agentClassesFixture is the shared fixture
// testdata/policy/agent-instruction-classes.json, which
// backend/internal/agentpaths is pinned to as well.
type agentClassesFixture struct {
	// Comment is never read: it exists so DisallowUnknownFields accepts the
	// fixture's documentary `_comment` key. Do not delete it.
	Comment          string   `json:"_comment"`
	ConfigDirs       []string `json:"config_dirs"`
	InstructionFiles []string `json:"instruction_files"`
	Match            []string `json:"match"`
	NoMatch          []string `json:"no_match"`
}

// loadAgentClassesFixture strictly decodes the shared fixture, failing closed
// on a duplicate key, trailing content (checkStrictJSON), an unknown key, or an
// empty list.
func loadAgentClassesFixture(t *testing.T) agentClassesFixture {
	t.Helper()
	p := filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "policy", "agent-instruction-classes.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := checkStrictJSON(raw); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	var f agentClassesFixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	for name, l := range map[string][]string{
		"config_dirs":       f.ConfigDirs,
		"instruction_files": f.InstructionFiles,
		"match":             f.Match,
		"no_match":          f.NoMatch,
	} {
		if len(l) == 0 {
			t.Fatalf("%s: %s is empty: the parity test would be vacuous", p, name)
		}
	}
	return f
}

// TestAgentPathsMatchSharedFixture pins this twin's lists to the shared
// fixture, so it cannot drift from backend/internal/agentpaths (pinned to the
// same fixture) without a red test in this module.
func TestAgentPathsMatchSharedFixture(t *testing.T) {
	f := loadAgentClassesFixture(t)
	sorted := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return out
	}
	if got, want := sorted(agentConfigDirs), sorted(f.ConfigDirs); !slices.Equal(got, want) {
		t.Errorf("agentConfigDirs = %v, fixture config_dirs = %v; change this twin, backend/internal/agentpaths and the fixture together", got, want)
	}
	if got, want := sorted(agentInstructionFiles), sorted(f.InstructionFiles); !slices.Equal(got, want) {
		t.Errorf("agentInstructionFiles = %v, fixture instruction_files = %v; change this twin, backend/internal/agentpaths and the fixture together", got, want)
	}
}

// enforcedDefault is the config ApplyImplementDefaults sets: present, no
// opt-out, nothing writable. No other constraint is configured, so any
// violation in a test using it comes from the built-in alone.
func enforcedDefault() Constraints {
	return Constraints{AgentInstructionPaths: &AgentInstructionPaths{}}
}

// agentViolations returns the agent_instruction_paths violations in vs.
func agentViolations(vs []Violation) []Violation {
	var out []Violation
	for _, v := range vs {
		if v.Constraint == "agent_instruction_paths" {
			out = append(out, v)
		}
	}
	return out
}

// TestEvaluate_AgentInstructionPaths_FixtureProbes drives every shared probe
// through Evaluate with ONLY the built-in configured: each match probe (root,
// nested, re-cased) is one violation naming it, each no_match probe passes.
func TestEvaluate_AgentInstructionPaths_FixtureProbes(t *testing.T) {
	f := loadAgentClassesFixture(t)
	for _, p := range f.Match {
		vs := Evaluate(diff(p), enforcedDefault())
		av := agentViolations(vs)
		if len(vs) != 1 || len(av) != 1 || !slices.Equal(av[0].Files, []string{p}) {
			t.Errorf("match probe %q: violations = %v, want one agent_instruction_paths violation naming it", p, vs)
		}
	}
	for _, p := range f.NoMatch {
		if vs := Evaluate(diff(p), enforcedDefault()); len(vs) != 0 {
			t.Errorf("no_match probe %q: violations = %v, want none", p, vs)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// TestEvaluate_AgentInstructionPaths_Config pins each config arm: nil is not
// applied, enforce:false opts out, enforce:true enforces, and writable exempts
// only EXACT, case-sensitive paths.
func TestEvaluate_AgentInstructionPaths_Config(t *testing.T) {
	const nested = "backend/.Claude/skills/x/SKILL.md"
	for _, tc := range []struct {
		name  string
		files []string
		cfg   *AgentInstructionPaths
		want  []string // nil = no violation
	}{
		{"nil config is not applied", []string{nested}, nil, nil},
		{"enforce false opts out", []string{nested}, &AgentInstructionPaths{Enforce: boolPtr(false)}, nil},
		{"enforce true enforces", []string{nested}, &AgentInstructionPaths{Enforce: boolPtr(true)}, []string{nested}},
		{"writable exempts the exact root path", []string{"AGENTS.md"}, &AgentInstructionPaths{Writable: []string{"AGENTS.md"}}, nil},
		{"writable is case-sensitive", []string{"agents.md"}, &AgentInstructionPaths{Writable: []string{"AGENTS.md"}}, []string{"agents.md"}},
		{"writable is not a basename glob", []string{"docs/AGENTS.md"}, &AgentInstructionPaths{Writable: []string{"AGENTS.md"}}, []string{"docs/AGENTS.md"}},
		{"one violation lists every hit in diff order", []string{"main.go", "CLAUDE.md", ".codex/config.toml", "AGENTS.md"}, &AgentInstructionPaths{Writable: []string{"AGENTS.md"}}, []string{"CLAUDE.md", ".codex/config.toml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := Evaluate(diff(tc.files...), Constraints{AgentInstructionPaths: tc.cfg})
			if tc.want == nil {
				if len(vs) != 0 {
					t.Fatalf("violations = %v, want none", vs)
				}
				return
			}
			if len(vs) != 1 || vs[0].Constraint != "agent_instruction_paths" || !slices.Equal(vs[0].Files, tc.want) {
				t.Fatalf("violations = %v, want one agent_instruction_paths violation with files %v", vs, tc.want)
			}
		})
	}
}

// TestEvaluate_AgentInstructionPaths_RenameVsCopy pins that an R row's
// OldPath is a candidate (the rename moves an instruction file away) and a C
// row's is not (a copy leaves its source untouched). Path is an ordinary file
// in both rows, so only OldPath can produce the violation.
func TestEvaluate_AgentInstructionPaths_RenameVsCopy(t *testing.T) {
	const src = ".claude/settings.json"
	row := func(s Status) Diff {
		return Diff{ChangedFiles: []ChangedFile{{Path: "docs/notes.md", OldPath: src, Status: s}}}
	}
	vs := Evaluate(row(StatusRenamed), enforcedDefault())
	if len(vs) != 1 || !slices.Equal(vs[0].Files, []string{src}) {
		t.Errorf("R row from %s: violations = %v, want one naming the rename source", src, vs)
	}
	if vs := Evaluate(row(StatusCopied), enforcedDefault()); len(vs) != 0 {
		t.Errorf("C row from %s: violations = %v, want none (a copy leaves the source untouched)", src, vs)
	}
	// A writable source is exempt on the rename row too.
	c := Constraints{AgentInstructionPaths: &AgentInstructionPaths{Writable: []string{src}}}
	if vs := Evaluate(row(StatusRenamed), c); len(vs) != 0 {
		t.Errorf("R row from writable %s: violations = %v, want none", src, vs)
	}
}

// TestEvaluate_AgentInstructionPaths_RunsLast pins the built-in's position
// after every other check, so existing violation order is unchanged.
func TestEvaluate_AgentInstructionPaths_RunsLast(t *testing.T) {
	c := enforcedDefault()
	c.ForbiddenPaths = []string{"CLAUDE.md"}
	vs := Evaluate(diff("CLAUDE.md"), c)
	if len(vs) != 2 || vs[0].Constraint != "forbidden_paths" || vs[1].Constraint != "agent_instruction_paths" {
		t.Fatalf("violations = %v, want forbidden_paths then agent_instruction_paths", vs)
	}
}

// TestApplyImplementDefaults pins the default: only a resolved implement stage
// with no entry gets the empty, enforced config; an explicit entry is kept.
func TestApplyImplementDefaults(t *testing.T) {
	for _, st := range []string{"", "plan", "review", "acceptance", "Implement"} {
		var c Constraints
		ApplyImplementDefaults(st, &c)
		if c.AgentInstructionPaths != nil {
			t.Errorf("stageType %q: AgentInstructionPaths = %+v, want nil (no default)", st, c.AgentInstructionPaths)
		}
	}

	var c Constraints
	ApplyImplementDefaults("implement", &c)
	if c.AgentInstructionPaths == nil || !c.AgentInstructionPaths.enforced() || len(c.AgentInstructionPaths.Writable) != 0 {
		t.Fatalf("implement: AgentInstructionPaths = %+v, want an empty enforced config", c.AgentInstructionPaths)
	}

	optOut := &AgentInstructionPaths{Enforce: boolPtr(false)}
	c = Constraints{AgentInstructionPaths: optOut}
	ApplyImplementDefaults("implement", &c)
	if c.AgentInstructionPaths != optOut {
		t.Errorf("implement with an explicit opt-out: entry replaced with %+v", c.AgentInstructionPaths)
	}

	ApplyImplementDefaults("implement", nil) // must not panic
}

// TestConstraints_DecodesAgentInstructionPaths pins the constraints-file key:
// the field is tagged with the workflow-spec name.
func TestConstraints_DecodesAgentInstructionPaths(t *testing.T) {
	var c Constraints
	if err := json.Unmarshal([]byte(`{"agent_instruction_paths":{"enforce":false,"writable":["AGENTS.md"]}}`), &c); err != nil {
		t.Fatal(err)
	}
	a := c.AgentInstructionPaths
	if a == nil || a.Enforce == nil || *a.Enforce || !slices.Equal(a.Writable, []string{"AGENTS.md"}) {
		t.Fatalf("decoded AgentInstructionPaths = %+v", a)
	}
}
