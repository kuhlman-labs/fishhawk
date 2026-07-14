# runner/internal/agent/codex

`agent.Invoker` for OpenAI's Codex CLI (#840), the sibling of `runner/internal/agent/claudecode`. Operator-facing wiring (action inputs, secrets, migration) is in `runner/README.md` ("Choosing the coding agent"); this file covers adapter + selection internals.

## Invocation

`codex.go` spawns `codex exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check <prompt>` (flags pinned against codex-cli 0.137.0; `--dangerously-bypass-approvals-and-sandbox` is the codex analogue of claudecode's `--dangerously-skip-permissions` — non-interactive, no approval gate, OS-sandbox off so build/test/lint and the `/tmp` plan artifact work).

It reads one JSON event per stdout line and emits the same canonical envelope (`invocation_start` … per-line events … `invocation_end`) + `stage_progress` heartbeats + category-A `failureResult` shape as claudecode.

## Usage accounting

Codex reports usage PER TURN on each `turn.completed` line (`{input_tokens,cached_input_tokens,output_tokens,reasoning_output_tokens}`), so the adapter SUMS across turns (not last-wins like claudecode); `cached_input_tokens` is a subset of `input_tokens` (not re-added), `reasoning_output_tokens` is added to the output side.

Codex surfaces no `model` field today, so `Result.Model` is left empty and the bundle prices from the token split alone (`known_usage=false` only when no usage line appears, per #682).

## Kill robustness

Unlike claudecode's direct-child `cmd.Process.Kill()`, the codex adapter sets `SysProcAttr{Setpgid:true}` and kills the whole process GROUP (`syscall.Kill(-pid, SIGKILL)`) on budget/timeout — Codex spawns grandchildren (shell exec, MCP servers) that inherit the stdout pipe, so a direct-child kill could hang the `io.Discard` drain + `Wait`; `cmd.Cancel` is overridden to the group kill for the timeout path.

## Provider selection

`runner/cmd/fishhawk-runner/agentselect.go::selectInvoker` routes `claude-code → newInvoker`, `codex → newCodexInvoker` (both `var` seams for test fakes), else `errUnknownAgent` (category-A before invocation).

`apiKeyForAgent` sources the host key per provider — `codex → OPENAI_API_KEY`, else `ANTHROPIC_API_KEY` — and `main.go` forwards it to the adapter, which appends it to the child env alongside the `FISHHAWK_*` MCP vars.

The composite action installs `@openai/codex` only when `agent=codex` (and `@anthropic-ai/claude-code` only for `claude-code`).

Not ported from claudecode (out of #840 scope): thinking-block retry (#579), loop detection (#653), out-of-tree-write surfacing (#601).

## Binary override + version provenance (#1741)

`runner/cmd/fishhawk-runner/agentbin.go` resolves the agent CLI executable — the operator override `FISHHAWK_AGENT_BIN` (claude-code) / `FISHHAWK_CODEX_BIN` (codex), else the adapter `DefaultBinary` — threads it onto the concrete invoker's `.Binary` (empty preserves historical PATH resolution), and probes `<binary> --version` (run in a private temp dir so a misbehaving PATH-resolved binary's relative writes can't escape into the checkout; `unknown` on any error).

The resolved binary + version + provider are recorded on the `runner_started` line as `agent_binary`/`agent_version`/`agent_kind`, so an operator can pin a known-good CLI and confirm from the log which build ran.

The backend codex REVIEWER adapter (`backend/internal/codex`) is a distinct audit surface; its version recording is tracked separately in #1768.
