package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
)

// writeConstraintsFile writes body as a constraints file in a TempDir.
func writeConstraintsFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "constraints.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// agentPolicyViolations decodes the policy_event violations enforceConstraints
// emitted for the agent_instruction_paths constraint.
func agentPolicyViolations(t *testing.T, evs []agent.Event) [][]string {
	t.Helper()
	var out [][]string
	for _, ev := range evs {
		if ev.Kind != "policy_event" {
			continue
		}
		var p struct {
			Outcome    string   `json:"outcome"`
			Constraint string   `json:"constraint"`
			Files      []string `json:"files"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode policy_event payload %s: %v", ev.Payload, err)
		}
		if p.Outcome == "violation" && p.Constraint == "agent_instruction_paths" {
			out = append(out, p.Files)
		}
	}
	return out
}

// TestEnforceConstraints_BuiltinAgentInstructionPaths pins the runner's
// in-line default (#3942) end to end, from a constraints file through
// enforceConstraints to the policy_event it emits. The constraints file
// declares NOTHING ({}) in every arm but the opt-out, so the built-in is the
// only thing that can produce a violation, and the default is keyed on the
// RESOLVED stageType argument, never cfg.stage.
func TestEnforceConstraints_BuiltinAgentInstructionPaths(t *testing.T) {
	const nested = "backend/.Claude/skills/x/SKILL.md"
	d := constraint.Diff{ChangedFiles: []constraint.ChangedFile{
		{Path: "main.go", Status: constraint.StatusModified},
		{Path: nested, Status: constraint.StatusAdded},
	}}
	empty := writeConstraintsFile(t, `{}`)
	optOut := writeConstraintsFile(t, `{"agent_instruction_paths":{"enforce":false}}`)

	for _, tc := range []struct {
		name      string
		file      string
		cfgStage  string
		stageType string
		wantHit   bool
	}{
		// (a) a resolved implement stage gets the default.
		{"resolved implement stage is enforced", empty, "implement", "implement", true},
		// (b) cfg.stage alone never applies it: an unresolved stageType (a
		// local replay without --fetch-prompt) gets no default.
		{"cfg.stage implement without a resolved type is not", empty, "implement", "", false},
		// (c) the resolved type wins over a mislabelled --stage flag.
		{"resolved implement wins over cfg.stage build", empty, "build", "implement", true},
		// (d) an explicit opt-out in the constraints file is honored.
		{"enforce false opts out", optOut, "implement", "implement", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{constraintsFile: tc.file, stage: tc.cfgStage}
			evs, err := enforceConstraints(cfg, tc.stageType, d)
			hits := agentPolicyViolations(t, evs)
			if !tc.wantHit {
				if err != nil || len(hits) != 0 {
					t.Fatalf("err = %v, agent_instruction_paths violations = %v; want neither", err, hits)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want a constraint-violation error (the caller demotes to category B)")
			}
			if len(hits) != 1 || !slices.Equal(hits[0], []string{nested}) {
				t.Fatalf("agent_instruction_paths violations = %v, want exactly one naming %s", hits, nested)
			}
		})
	}
}
