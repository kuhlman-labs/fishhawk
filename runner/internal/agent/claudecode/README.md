# runner/internal/agent/claudecode

`agent.Invoker` adapter for Anthropic's Claude Code CLI. Operator-facing behavior (provider selection, binary pinning, out-of-tree-write semantics) is in `runner/README.md`; this file covers adapter internals.

## Bounded in-driver agent retry (#579)

`claudecode.go` wraps a bounded retry around a single-attempt `invokeOnce` for the transient interleaved-thinking API 400 (`thinking/redacted_thinking blocks in the latest assistant message cannot be modified`) that kills long agent runs at high turn counts.

`isThinkingBlock400` (pure, unit-tested) detects the fault by the durable fragments `thinking` + `cannot be modified` in the terminal `result` event or stderr, corroborated by `api_error_status==400` when present.

On detection with retries remaining, the driver emits an `agent_retry` trace event and re-spawns `claude` fresh from the same prompt — no `--continue`/`--resume`, so the corrupted history can't carry over, and the working tree is deliberately **not** reset (unsafe in local `--no-pr` mode).

Retry budget is `Invoker.MaxThinkingBlockRetries` (counts retries, not attempts; defaulted to 1 in `New()` so an explicit 0 disables it).

The peer sentinel `agent.ErrAgentThinkingBlock` does NOT wrap `ErrAgentFailed`; `classifyErr` in `runner/cmd/fishhawk-runner/main.go` maps it to the `agent_api_thinking_block` err_class, but `Result.FailureCategory` stays `"A"` on the retry-exhausted path so stage-level retry and the category-A bundle signal are unaffected.

Aggregate `Result.TokensUsed` is cumulative across attempts (honest about doubled cost).

## Model-quota-exhaustion classification (#2085)

`invokeOnce`'s terminal switch classifies a model-quota exhaustion (a usage / rate cap) as a distinct failure so the operator can tell it apart from a transient agent crash. Both otherwise collapse into a generic category-A `agent_error` / `agent exited with error: exit status 1`, so a capped account thrashes doomed auto-retries (run a75b0765: the same dispatch failed once as a ~11min/~52000-token real crash and once as a ~2s/0-token post-init exit — the cap fingerprint).

`isQuotaUnavailable(waitErr, tokensUsed, model, elapsed)` (pure, unit-tested) returns true iff the agent exited non-zero (`waitErr != nil`) having reached **no model call** — zero reported tokens (`tokensUsed == 0`) AND no model id seen (`model == ""`) — within `quotaUnavailableMaxWall` (30s) of spawn. `model == ""` is load-bearing: only `assistant`/`result` stream-json events carry a model id (`system.init` does not), so an empty model means no model turn ever started; `tokensUsed == 0` discriminates a cap-exhausted session from a genuine crash (which reports non-zero usage). The wall-clock bound is a conservative guard so a slow zero-token hang is not mislabeled a cap; `elapsed` is derived from the same fake-clock (`now`/`start`) seam the heartbeat uses, so the bound is deterministic in tests.

The arm sits **after** the thinking-block and external-API 5xx arms and **before** the generic `agent_error` arm, so a fast zero-token exit that also carried a thinking-block marker or a `>= 500` `api_error_status` still classifies as those, while a failure with `tokensUsed > 0`, a model id, or `elapsed` over the bound falls through unchanged.

The peer sentinel `agent.ErrAgentQuotaUnavailable` does **not** wrap `ErrAgentFailed`; `classifyErr` in `runner/cmd/fishhawk-runner/main.go` maps it to the `agent_quota_unavailable` err_class, but `Result.FailureCategory` stays `"A"`. It is **not retried in-driver** (a retry against an unreset cap fails identically), and the stable failure-reason phrase `could not obtain model quota` is what the backend's `implementFailedNextActions` (`backend/cmd/fishhawk-mcp/next_actions.go`) reads to steer the operator to wait for the cap to reset rather than burn retry budget — a best-effort string contract across the two go.work modules (same #1548 limitation as external-API).

## Out-of-tree write detection (#611)

`claudecode.go` surfaces (does not block) agent writes outside the working tree.

`outOfTreeWrites(line, allowedRoots)` inspects each `assistant` stream-json line for file-writing tool calls (`fileWritingTools`: `Write`/`Edit`/`MultiEdit`/`NotebookEdit`) and returns any target not contained in `allowedRoots` = `inv.WorkingDir` + `allowedExtraDirs` (the latter is the single source of truth shared with the `--add-dir` flag so they can't drift).

Containment (`containedInAny`) resolves relative paths against the working dir, canonicalises symlinks via `resolveSymlinks` (which walks up to the deepest **existing** ancestor before `EvalSymlinks`, so a not-yet-created target and macOS's `/tmp`→`/private/tmp` symlink both resolve correctly), then judges inside-ness with `filepath.Rel`.

The scan loop appends an `out_of_tree_write` event per hit; it is **additive and fail-open** — never flips `Result.OK`, never fails the stage, never panics on an unparseable/unknown-shape line.

**Why the invocation flags are unchanged:** empirically (claude 2.1.156) no `--permission-mode` confines writes while keeping the non-interactive Bash (`go test`, `golangci-lint`, `scripts/test`) the implement stage needs — `acceptEdits`/`dontAsk` deny that Bash; `auto`/`allowedTools Bash` reopen out-of-tree writes via shell `>` redirect.

So the detector covers the Write/Edit-**tool** class (#601) only; **Bash-mediated writes are NOT caught**, and full confinement (an OS-level sandbox) is deferred to a dedicated ADR.
