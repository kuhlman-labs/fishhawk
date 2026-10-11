package constraint

import (
	"fmt"
	"strings"
)

// agentConfigDirs and agentInstructionFiles mirror
// backend/internal/agentpaths's configDirs and instructionFiles (E80.12 /
// #3942): the ONE canonical agent-instruction-path list, twinned here because
// the runner module cannot import backend. Change them together, along with the
// shared fixture testdata/policy/agent-instruction-classes.json, which both
// modules' tests pin their lists to. The discovery rationale per entry is on
// backend/internal/agentpaths.IsInstructionPath.
var (
	agentConfigDirs       = []string{".agents", ".claude", ".codex"}
	agentInstructionFiles = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md"}
)

// isAgentInstructionPath mirrors backend/internal/agentpaths.IsInstructionPath
// — change them together. A cleaned, forward-slash repo-relative path is an
// agent-instruction path when ANY component equals a config dir, or its FINAL
// component equals an instruction file, compared case-insensitively (a CLI on
// a case-insensitive filesystem resolves `AGENTS.md` to a tracked `agents.md`).
func isAgentInstructionPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		for _, dir := range agentConfigDirs {
			if strings.EqualFold(p, dir) {
				return true
			}
		}
		if i == len(parts)-1 {
			for _, name := range agentInstructionFiles {
				if strings.EqualFold(p, name) {
					return true
				}
			}
		}
	}
	return false
}

// AgentInstructionPaths is the built-in agent_instruction_paths constraint
// (#3942). A nil *AgentInstructionPaths on Constraints means the check is not
// applied; ApplyImplementDefaults sets an empty (enforced) value for an
// implement stage, so every implement stage gets it unless its constraints
// file opts out.
type AgentInstructionPaths struct {
	// Enforce false turns the built-in off for the stage. nil or true
	// enforces it.
	Enforce *bool `json:"enforce,omitempty"`
	// Writable lists EXACT, case-sensitive repo-relative paths the stage may
	// still write. Not globs: `AGENTS.md` exempts only the root file, never
	// `agents.md` or `docs/AGENTS.md`.
	Writable []string `json:"writable,omitempty"`
}

// enforced reports whether the check runs: the config is present and has not
// opted out with enforce:false.
func (a *AgentInstructionPaths) enforced() bool {
	return a != nil && (a.Enforce == nil || *a.Enforce)
}

// ApplyImplementDefaults applies the built-in default to a decoded constraints
// file: an implement stage with no agent_instruction_paths entry gets an empty,
// enforced one. stageType MUST be the stage type the backend resolved (the
// --fetch-prompt response), never the --stage flag, so a stage cannot dodge
// the default by how it was invoked; an empty stageType (a local replay
// without --fetch-prompt) leaves c unchanged, and the backend gate stays
// authoritative there. An explicit entry, including an opt-out, is kept.
func ApplyImplementDefaults(stageType string, c *Constraints) {
	if c == nil || stageType != "implement" || c.AgentInstructionPaths != nil {
		return
	}
	c.AgentInstructionPaths = &AgentInstructionPaths{}
}

// checkAgentInstructionPaths flags every changed file that writes an
// agent-instruction path not EXACTLY listed in writable, as ONE violation
// naming the offending paths in diff order. A row's candidates are its Path
// and, on an R (rename) row only, its OldPath: a rename moves the source away,
// which changes what a CLI discovers there. A C (copy) row leaves its source
// untouched, so its OldPath is never a candidate.
func checkAgentInstructionPaths(diff Diff, writable []string) []Violation {
	exempt := make(map[string]bool, len(writable))
	for _, w := range writable {
		exempt[w] = true
	}
	seen := map[string]bool{}
	var hit []string
	for _, f := range diff.ChangedFiles {
		candidates := []string{f.Path}
		if f.Status == StatusRenamed && f.OldPath != "" {
			candidates = append(candidates, f.OldPath)
		}
		for _, p := range candidates {
			if seen[p] || exempt[p] || !isAgentInstructionPath(p) {
				continue
			}
			seen[p] = true
			hit = append(hit, p)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return []Violation{{
		Constraint: "agent_instruction_paths",
		Detail:     fmt.Sprintf("%d agent-instruction path(s) changed; the built-in constraint forbids them unless the stage lists the exact path in agent_instruction_paths.writable", len(hit)),
		Files:      hit,
	}}
}
