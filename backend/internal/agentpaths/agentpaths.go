// Package agentpaths owns the ONE canonical definition of an
// agent-instruction path (E80.12 / #3942): a repository path a coding agent's
// CLI auto-discovers and loads as instructions. The review sandbox skips these
// paths when it materializes a grounded tree, and the built-in
// agent_instruction_paths constraint forbids an implement stage from writing
// them. Both read this package, so the two cannot disagree.
//
// The runner module cannot import backend, so runner/internal/constraint
// carries a twin (agentpaths.go). Both modules' tests pin their lists to the
// shared fixture testdata/policy/agent-instruction-classes.json. Change the
// three together.
package agentpaths

import "strings"

// configDirs are the directory components whose whole subtree is an
// agent-instruction path, and instructionFiles the file basenames that are
// one at any depth. Both are compared case-insensitively by IsInstructionPath.
// See IsInstructionPath for the discovery rationale behind each entry.
var (
	configDirs       = []string{".agents", ".claude", ".codex"}
	instructionFiles = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md"}
)

// ConfigDirs returns a copy of the agent config directory names.
func ConfigDirs() []string {
	return append([]string(nil), configDirs...)
}

// InstructionFiles returns a copy of the agent instruction-file basenames.
func InstructionFiles() []string {
	return append([]string(nil), instructionFiles...)
}

// IsInstructionPath reports whether a cleaned, forward-slash repo-relative
// path is an agent-instruction file or lives under an agent-config directory,
// enumerated against both CLIs' documented discovery:
//
//   - AGENTS.md, AGENTS.override.md: codex auto-discovers BOTH at every
//     directory level. AGENTS.override.md is codex's local-only override,
//     checked BEFORE AGENTS.md at each level, so it loads as an instruction
//     exactly like AGENTS.md (its omission was the C1 bypass in #2486's
//     fix-up).
//   - CLAUDE.md, CLAUDE.local.md: claude-code auto-discovers these at every
//     directory level.
//   - any component named .claude or .codex: the two CLIs' config/state
//     directories (settings, commands, agents, skills, config.toml), matched
//     wholesale so nothing under them is loaded as instructions.
//   - any component named .agents: the cross-tool Agent Skills directory
//     (agentskills.io). Codex auto-discovers skills from .agents/skills at
//     every directory level up to the repository root, so a tracked SKILL.md
//     there loads as an instruction exactly like AGENTS.md. Matched wholesale
//     like .claude/.codex rather than narrowed to .agents/skills, so a future
//     discovery path under the same directory is closed by default.
//
// Every comparison is CASE-INSENSITIVE. On a case-insensitive filesystem
// (macOS APFS by default) a CLI opening `.agents/skills` or `AGENTS.md`
// resolves to a tracked `.Agents/` or `agents.md`, so an exact-case match
// would let a re-cased path through.
func IsInstructionPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		for _, dir := range configDirs {
			if strings.EqualFold(p, dir) {
				return true
			}
		}
		if i == len(parts)-1 {
			for _, name := range instructionFiles {
				if strings.EqualFold(p, name) {
					return true
				}
			}
		}
	}
	return false
}
