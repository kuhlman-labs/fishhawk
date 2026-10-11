# backend/internal/agentpaths

The ONE canonical definition of an agent-instruction path (E80.12 / #3942): a
repository path a coding agent's CLI auto-discovers and loads as instructions.

## Contract

- `ConfigDirs()` — `.agents`, `.claude`, `.codex`. A path is an instruction
  path when ANY component equals one of these (so a file named `.claude` and
  everything under a `.claude/` directory at any depth both match).
- `InstructionFiles()` — `AGENTS.md`, `AGENTS.override.md`, `CLAUDE.md`,
  `CLAUDE.local.md`. A path is an instruction path when its FINAL component
  equals one of these, at any depth.
- `IsInstructionPath(rel)` — the predicate over a cleaned, forward-slash,
  repo-relative path. Every comparison is case-insensitive: on a
  case-insensitive filesystem (macOS APFS by default) a CLI opening
  `AGENTS.md` or `.agents/skills` resolves to a tracked `agents.md` or
  `.Agents/`.
- Both accessors return copies; the lists cannot be mutated by a caller.

The discovery rationale per entry (codex reads `AGENTS.md` and
`AGENTS.override.md` per directory and `.agents/skills` up to the repo root;
Claude Code reads `CLAUDE.md` and `CLAUDE.local.md` per directory; `.claude/`
and `.codex/` are the CLIs' config dirs) is the doc comment on
`IsInstructionPath`.

## Consumers

- `backend/internal/reviewsandbox` — the grounded-review tree export skips
  every instruction path (C1), so a reviewer CLI never auto-loads repository
  instructions from the tree under review.
- The built-in `agent_instruction_paths` constraint — the backend policy gate
  forbids an implement stage from writing an instruction path unless the
  stage opts out.

## The runner twin and the shared fixture

The runner module cannot import backend, so `runner/internal/constraint/agentpaths.go`
carries a twin of both lists and the predicate. Both modules' tests strictly
decode the shared fixture `testdata/policy/agent-instruction-classes.json` and
assert their Go lists equal its `config_dirs` / `instruction_files`, then drive
its `match` / `no_match` probes (root, nested and re-cased) through their
predicate. **Change `agentpaths.go`, the runner twin and the fixture together**;
any one changed alone fails a test. The fixture paths are data only and are
never created as files.

`TestIsInstructionPath_CoversHandWrittenGlobClasses` additionally reads (never
edits) `testdata/policy/agent-instruction-paths.json`, the repospec fixture for
the hand-written per-workflow globs, and asserts the predicate covers every one
of its agent-instruction classes, so the built-in is a superset of the globs it
is meant to replace. That fixture's `guard_files` (`scripts/is-run-agent`) is
not an instruction path and stays a spec-level `forbidden_paths` concern.
