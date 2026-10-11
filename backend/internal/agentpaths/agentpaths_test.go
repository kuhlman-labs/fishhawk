package agentpaths

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// classesFixture is the shared fixture
// testdata/policy/agent-instruction-classes.json.
type classesFixture struct {
	// Comment is never read: it exists so DisallowUnknownFields accepts the
	// fixture's documentary `_comment` key. Do not delete it.
	Comment          string   `json:"_comment"`
	ConfigDirs       []string `json:"config_dirs"`
	InstructionFiles []string `json:"instruction_files"`
	Match            []string `json:"match"`
	NoMatch          []string `json:"no_match"`
}

// loadClassesFixture strictly decodes the shared fixture. It fails closed on
// anything that would make a test vacuous or ambiguous: a duplicate key at any
// depth, trailing content, an unknown key, or an empty list.
func loadClassesFixture(t *testing.T) classesFixture {
	t.Helper()
	p := filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "policy", "agent-instruction-classes.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := checkStrictJSON(raw); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	var f classesFixture
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

// TestAgentPathsMatchSharedFixture pins the canonical lists to the shared
// fixture, which the runner twin (runner/internal/constraint/agentpaths.go)
// is pinned to as well. Order-insensitive: the lists are sets.
func TestAgentPathsMatchSharedFixture(t *testing.T) {
	f := loadClassesFixture(t)
	sorted := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return out
	}
	if got, want := sorted(ConfigDirs()), sorted(f.ConfigDirs); !slices.Equal(got, want) {
		t.Errorf("ConfigDirs() = %v, fixture config_dirs = %v; change agentpaths.go, the runner twin and the fixture together", got, want)
	}
	if got, want := sorted(InstructionFiles()), sorted(f.InstructionFiles); !slices.Equal(got, want) {
		t.Errorf("InstructionFiles() = %v, fixture instruction_files = %v; change agentpaths.go, the runner twin and the fixture together", got, want)
	}
}

// TestIsInstructionPath_FixtureProbes drives every shared probe through the
// predicate: each match probe (root, nested and re-cased, for every dir and
// basename) is an instruction path and each no_match probe is not.
func TestIsInstructionPath_FixtureProbes(t *testing.T) {
	f := loadClassesFixture(t)
	for _, p := range f.Match {
		if !IsInstructionPath(p) {
			t.Errorf("IsInstructionPath(%q) = false, want true (fixture match probe)", p)
		}
	}
	for _, p := range f.NoMatch {
		if IsInstructionPath(p) {
			t.Errorf("IsInstructionPath(%q) = true, want false (fixture no_match probe)", p)
		}
	}
}

// TestIsInstructionPath_CoversHandWrittenGlobClasses proves the built-in
// covers every agent-instruction path the hand-written per-workflow globs
// cover today, by reading (never editing) the repospec fixture
// testdata/policy/agent-instruction-paths.json. Its guard_files class is NOT
// an instruction path (scripts/is-run-agent is a runner guard, not something
// a CLI loads), so it stays the spec's own forbidden_paths concern; its
// ordinary class must not match.
func TestIsInstructionPath_CoversHandWrittenGlobClasses(t *testing.T) {
	p := filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "policy", "agent-instruction-paths.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var f struct {
		Classes map[string][]string `json:"classes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	for _, class := range []string{"config_dir_paths", "instruction_files", "root_agents", "root_agents_case_variants"} {
		paths := f.Classes[class]
		if len(paths) == 0 {
			t.Fatalf("%s: class %q is missing or empty: the coverage check would be vacuous", p, class)
		}
		for _, path := range paths {
			if !IsInstructionPath(path) {
				t.Errorf("IsInstructionPath(%q) = false, want true (hand-written glob class %s)", path, class)
			}
		}
	}
	ordinary := f.Classes["ordinary"]
	if len(ordinary) == 0 {
		t.Fatalf("%s: class \"ordinary\" is missing or empty", p)
	}
	for _, path := range ordinary {
		if IsInstructionPath(path) {
			t.Errorf("IsInstructionPath(%q) = true, want false (ordinary class)", path)
		}
	}
}

// TestIsInstructionPath_Shapes pins the component semantics the fixture
// probes do not spell out: a config dir matches as ANY component (including
// the final one), an instruction file only as the FINAL component.
func TestIsInstructionPath_Shapes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".claude", true},
		{"a/b/.codex", true},
		{"AGENTS.md/notes.txt", false},
		{"CLAUDE.md/x", false},
		{"", false},
		{"docs", false},
	} {
		if got := IsInstructionPath(tc.path); got != tc.want {
			t.Errorf("IsInstructionPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestAccessorsReturnCopies pins that a caller mutating a returned slice
// cannot change the canonical list.
func TestAccessorsReturnCopies(t *testing.T) {
	d := ConfigDirs()
	d[0] = "mutated"
	if ConfigDirs()[0] == "mutated" {
		t.Error("ConfigDirs returned the backing slice, not a copy")
	}
	f := InstructionFiles()
	f[0] = "mutated"
	if InstructionFiles()[0] == "mutated" {
		t.Error("InstructionFiles returned the backing slice, not a copy")
	}
}

// TestCheckStrictJSON pins each fail-closed branch of the strict decoder.
func TestCheckStrictJSON(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		ok       bool
	}{
		{"valid", `{"a":[1,{"b":2}]}`, true},
		{"duplicate top-level key", `{"a":1,"a":2}`, false},
		{"duplicate nested key", `{"a":{"b":1,"b":2}}`, false},
		{"trailing content", `{"a":1} {}`, false},
		{"truncated", `{"a":[1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStrictJSON([]byte(tc.in))
			if (err == nil) != tc.ok {
				t.Fatalf("checkStrictJSON(%s) err = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}
