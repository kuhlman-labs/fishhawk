# runner/internal/agent/claudecode

`agent.Invoker` adapter for Anthropic's Claude Code CLI. Operator-facing behavior (provider selection, binary pinning, out-of-tree-write semantics) is in `runner/README.md`; this file covers adapter internals.

## Bounded in-driver agent retry (#579)

`claudecode.go` wraps a bounded retry around a single-attempt `invokeOnce` for the transient interleaved-thinking API 400 (`thinking/redacted_thinking blocks in the latest assistant message cannot be modified`) that kills long agent runs at high turn counts.

`isThinkingBlock400` (pure, unit-tested) detects the fault by the durable fragments `thinking` + `cannot be modified` in the terminal `result` event or stderr, corroborated by `api_error_status==400` when present.

On detection with retries remaining, the driver emits an `agent_retry` trace event and re-spawns `claude` fresh from the same prompt — no `--continue`/`--resume`, so the corrupted history can't carry over, and the working tree is deliberately **not** reset (unsafe in local `--no-pr` mode).

Retry budget is `Invoker.MaxThinkingBlockRetries` (counts retries, not attempts; defaulted to 1 in `New()` so an explicit 0 disables it).

The peer sentinel `agent.ErrAgentThinkingBlock` does NOT wrap `ErrAgentFailed`; `classifyErr` in `runner/cmd/fishhawk-runner/main.go` maps it to the `agent_api_thinking_block` err_class, but `Result.FailureCategory` stays `"A"` on the retry-exhausted path so stage-level retry and the category-A bundle signal are unaffected.

Aggregate `Result.TokensUsed` is cumulative across attempts (honest about doubled cost).

## Out-of-tree write detection (#611)

`claudecode.go` surfaces (does not block) agent writes outside the working tree.

`outOfTreeWrites(line, allowedRoots)` inspects each `assistant` stream-json line for file-writing tool calls (`fileWritingTools`: `Write`/`Edit`/`MultiEdit`/`NotebookEdit`) and returns any target not contained in `allowedRoots` = `inv.WorkingDir` + `allowedExtraDirs` (the latter is the single source of truth shared with the `--add-dir` flag so they can't drift).

Containment (`containedInAny`) resolves relative paths against the working dir, canonicalises symlinks via `resolveSymlinks` (which walks up to the deepest **existing** ancestor before `EvalSymlinks`, so a not-yet-created target and macOS's `/tmp`→`/private/tmp` symlink both resolve correctly), then judges inside-ness with `filepath.Rel`.

The scan loop appends an `out_of_tree_write` event per hit; it is **additive and fail-open** — never flips `Result.OK`, never fails the stage, never panics on an unparseable/unknown-shape line.

**Why the invocation flags are unchanged:** empirically (claude 2.1.156) no `--permission-mode` confines writes while keeping the non-interactive Bash (`go test`, `golangci-lint`, `scripts/test`) the implement stage needs — `acceptEdits`/`dontAsk` deny that Bash; `auto`/`allowedTools Bash` reopen out-of-tree writes via shell `>` redirect.

So the detector covers the Write/Edit-**tool** class (#601) only; **Bash-mediated writes are NOT caught**, and full confinement (an OS-level sandbox) is deferred to a dedicated ADR.
