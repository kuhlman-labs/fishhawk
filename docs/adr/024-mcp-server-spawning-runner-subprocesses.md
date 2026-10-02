---
id: ADR-024
title: "MCP server spawning runner subprocesses"
status: accepted
date: 2026-05-19
issue: https://github.com/kuhlman-labs/fishhawk/issues/433
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-024: MCP server spawning runner subprocesses

## Decision (2026-05-19)

Approved as drafted. Summary:

- **Q1 (progress):** stream events as MCP `notifications/progress`; final tool result is the structured outcome; audit log is the durable record.
- **Q2 (cancellation):** SIGTERM + 30s grace + SIGKILL. Runner SIGTERM handler is a prerequisite for graceful cleanup — filed as #435. Impl may ship as v0.5 (kill-only) before #435 lands.
- **Q3 (binary):** `runner_binary` input > `FISHHAWK_RUNNER_BIN` env > `exec.LookPath("fishhawk-runner")` — same as the CLI.
- **Q4 (auth):** nothing new. Forward operator's `FISHHAWK_API_TOKEN` to runner subprocess via env. Runner-side identity stays read-only per ADR-022 addendum.
- **Q5 (binary topology):** one binary. Tool always registered. Fails at call time when `fishhawk-runner` can't resolve. Revisit when hosted MCP becomes real.

Impl tracked at #434; prerequisite at #435.

---

## Context

Three E22 issues now share a common gap. After #426 lands, an agent inside Claude Code can:

1. Mint a real, stage-bearing local-runner run via `fishhawk_start_run`.
2. Approve a plan via `fishhawk_approve_plan`, cancel via `fishhawk_cancel_run`, retry via `fishhawk_retry_stage`.
3. Read run / stage / audit state via the existing read tools.

What the agent **can't** do is execute a stage. To advance from `pending → succeeded` on the local-runner backend, the operator has to drop to a terminal and type:

```
fishhawk runner start --run-id <U> --stage-id <V> --workflow <W> --stage plan
```

…and the same again for `--stage implement`. So the agent-driven dialogue ADR-019 frames as the goal is structurally:

> agent: I started the run. Please run `fishhawk runner start --run-id … --stage plan`.
> [operator switches terminal, runs the command, switches back]
> agent: Plan looks good. Approving. Now please run `fishhawk runner start … --stage implement`.

That handoff is the friction this ADR addresses. Closing it requires a new MCP tool — call it `fishhawk_run_stage` — that spawns the `fishhawk-runner` subprocess and streams the result back to the agent. But that's a non-trivial capability shift in what the MCP server is allowed to do, and the design has five real open questions (Q1-Q5 from the #427 issue body). This ADR settles them so the impl issue can be a straight build.

## Why this is a capability shift, not "just another tool"

Today's MCP tools are thin HTTP wrappers: validate input, call the backend, return. They complete in seconds, hold no resources, and are safe to cancel. A `run_stage` tool would:

- Spawn a child process that runs for **minutes** (typical plan stage: 3-5 min; implement: longer).
- Stream stdout/stderr across the MCP transport in real time.
- Hold a process handle that needs cleanup if Claude Code disconnects mid-stage.
- Touch the operator's filesystem (agent writes `/tmp/fishhawk-plan.json`, edits files in the working directory for implement).
- Need outbound network access (calls the backend, fetches the prompt, uploads the trace bundle).
- Carry credentials (the operator's API token; claude-code's own auth for the spawned agent).

This is a meaningful expansion of the MCP server's role. The decisions below are about how to make that shift cleanly.

## Q1: How does the agent see progress?

### Options

a. **Buffer + return on completion.** Tool blocks until the runner exits; returns the full event stream + final outcome as one tool result. Simplest. Slow feedback loop — the agent sees nothing for the duration of the stage.

b. **MCP `notifications/progress` stream.** Parse each runner JSONL event as it arrives; emit it as a progress notification; the final tool result is the structured outcome. The MCP SDK supports this — `mcp.ServerSession.NotifyProgress` is the surface — and the protocol's `notifications/progress` channel is designed for exactly this case.

c. **Fire-and-forget + polling.** Tool returns immediately with a "runner started" ack; agent polls a new `fishhawk_get_runner_status` tool. Decouples MCP transport duration from runner duration but introduces transient state that needs to live somewhere (in-memory map keyed by run id? backed by what on restart?).

### Recommendation

**Option (b) — stream events as `notifications/progress` updates; final tool result is the canonical structured outcome (exit code + terminal stage state + summary).**

- The MCP SDK supports it (`mcp/server.go:872 NotifyProgress`).
- The runner already emits JSONL events on stdout in the right shape; one line ≈ one progress notification.
- **Durable record lives in the audit log.** Every event the runner emits eventually lands as audit entries via the existing `trace_uploaded` ingest path. So if the agent's transport hiccups mid-stream, `fishhawk_list_audit` can recover the timeline. Progress notifications are a UX layer; the audit log is the source of truth.
- Option (c) introduces server-side state we don't need yet. If the streaming approach turns out to render poorly in Claude Code's UI, we can fall back to (a) by trivially dropping the notification calls. Option (c) is a much harder retreat.

## Q2: What does cancellation look like?

### The desired flow

1. Operator dismisses the Claude Code session (or hits cancel on the tool call). MCP server's tool-call context is cancelled.
2. MCP server sends `SIGTERM` to the runner subprocess.
3. Runner handles `SIGTERM`: uploads partial trace, transitions cleanup, exits with a distinct exit code.
4. Backend records the partial trace + leaves the stage in `running` for the SLA ticker to time out (existing category-D path).
5. MCP tool returns an error result naming the cancellation.

### The gap

**The runner today does not handle `SIGTERM`.** `runner/cmd/fishhawk-runner/main.go` uses `context.Background()` throughout — no signal handler, no graceful shutdown, no partial-trace upload on early exit. A SIGTERM would abruptly terminate the process with no audit footprint.

### Decision

- **MCP side sends SIGTERM with a 30-second grace period, then SIGKILL.** Standard subprocess-cleanup pattern. Both signals via `cmd.Process.Signal` / `cmd.Process.Kill`.
- **Runner-side SIGTERM handler is a prerequisite for clean cancellation** but is **out of scope for the MCP impl issue.** Filed as a separate dependency (see "Out of scope" below). Without it, cancellation is best-effort — the runner is killed mid-flight and the SLA ticker eventually reaps the stage. With it, cancellation is graceful.
- **MCP tool always returns a tool error on cancellation**, even when SIGKILL was needed. The agent sees "stage cancelled (subprocess killed after 30s grace)" rather than a successful exit.
- The backend's existing SLA timeout (#280's max_retries_snapshot + the orchestrator's category-D handling) is the long-tail safety net; the MCP tool doesn't need to coordinate with it.

## Q3: How does the MCP server resolve the runner binary?

### Options

a. **`fishhawk-runner` on PATH** — same as the CLI's resolution. PATH lookup; `FISHHAWK_RUNNER_BIN` env overrides; error with a clean message if neither resolves.
b. **Compiled-in** — `fishhawk-mcp` ships with the runner embedded. Tightly couples release cadences of the two binaries; the runner is its own release pipeline today.
c. **Operator-side env only** — no PATH fallback. Forces explicit configuration.

### Decision

**Option (a)**, mirroring the CLI's existing resolver at `cli/cmd/fishhawk/runner.go:46`. Order: `runner_binary` tool input > `FISHHAWK_RUNNER_BIN` env > `exec.LookPath("fishhawk-runner")`.

- Matches operator expectations — they've already configured `fishhawk-runner` to run `fishhawk runner start` from the CLI.
- (b) would couple two release pipelines that are independent today (`fishhawk-runner` has its own GitHub Actions workflow). Don't merge them prematurely.
- (c) is hostile to the dominant case (operator already has the binary on PATH).

## Q4: What's the auth boundary?

### Decision

Nothing new is needed.

- **Operator's `FISHHAWK_API_TOKEN`** is already in the MCP server's env (the operator passed it at `claude mcp add` time). Pass it through to the runner subprocess via `cmd.Env = append(os.Environ(), "FISHHAWK_API_TOKEN="+token)` — the same shape `fishhawk runner start` uses.
- **Claude Code's own auth** is the spawned agent's concern. The runner spawns claude-code as a sub-subprocess; that subprocess reads `~/.claude` for auth. MCP doesn't touch it.
- **Per ADR-022's addendum, runner-side identity stays read-only.** This tool runs operator-side (the MCP server holds an `fhk_*` apitoken); the spawned runner subprocess gets an `fhm_*` mcptoken via the existing `POST /v0/runs/{id}/mcp-token` flow. Two distinct token identities, same as today.

No new scopes, no new endpoints.

## Q5: One binary or two?

### Options

a. **Same binary, tool always registered, fails cleanly at call time.** `fishhawk_run_stage` is on every `fishhawk-mcp` deployment. When invoked, it tries to resolve `fishhawk-runner`; if that fails (or the operator's host can't spawn subprocesses), the tool returns a clear error.

b. **Same binary, tool registered conditionally at startup.** Server checks an env flag (`FISHHAWK_MCP_LOCAL=true`) or auto-detects (`fishhawk-runner` on PATH) and only registers the tool when local. Agents on a hosted deployment never see it.

c. **Separate binary `fishhawk-mcp-local`.** Bundles all `fishhawk-mcp` tools + the spawn tool. Two release artifacts.

### Decision

**Option (a) for v0.** When hosted MCP becomes a real concern, revisit and likely move to (c).

- We don't have a hosted MCP deployment today. Splitting binaries pre-emptively is speculative complexity.
- (b)'s conditional registration is invisible to the agent. The agent calls `fishhawk_run_stage`, gets "tool not found," and has no clue why. Failing at call time with "fishhawk-runner not on PATH; this tool requires local MCP execution" is more honest.
- The migration path from (a) → (c) is trivial: register-or-not is a one-line change at `tools.go::registerStartRun`-style level. The migration from (b) → (c) is the same trivial change. There's no lock-in cost to picking (a) now.
- Document the constraint in `docs/mcp/install.md`: this tool requires the MCP server to run on a host that can spawn subprocesses — which today means an operator's workstation; a future hosted deployment would surface a clean error.

## Consequences

- The MCP server gains the ability to spawn long-running subprocesses. This is a new operational shape and needs visible documentation (operator install path, troubleshooting section).
- The agent-driven local-loop dialogue inside Claude Code becomes structurally complete: `fishhawk_start_run` → `fishhawk_run_stage --stage plan` → `fishhawk_approve_plan` → `fishhawk_run_stage --stage implement`. No terminal handoffs.
- The runner needs a SIGTERM handler for clean cancellation. Without it, cancellation is best-effort (kill + SLA-ticker reap). This is a small, contained piece of work — filed as a dependency.
- The MCP integration-test surface grows by one cross-process scenario (MCP server → fake runner shim → backend). The fake-agent shim from #425 (local-loop CI smoke) is the natural fit; this ADR's impl issue inherits that test substrate.
- The MCP binary picks up an effective constraint: it must run on a host that can spawn `fishhawk-runner`. Today (operator workstation) this is always true; if we ever stand up a hosted MCP, the tool errors clearly.

## Out of scope

- **Runner SIGTERM handling** — filed as a separate prerequisite issue so the runner team / next runner-side PR can land it independently. The impl issue blocks on it for full graceful cancellation but can land with kill-only as a v0.5.
- **Streaming the runner's structured stdout into the MCP tool result** beyond progress notifications — the structured outcome is the final tool result; verbose runner logs stay in stderr (forwarded to the operator's terminal) and in the trace bundle that lands at the backend.
- **A `fishhawk_get_runner_status` polling tool** — explicitly rejected in Q1 in favor of progress notifications + the audit log.
- **Multi-stage orchestration as a single tool call** (e.g. one MCP call that runs plan, awaits approval, then runs implement) — that conflates the agent's decision-making with the runner's execution. The agent should orchestrate stage-by-stage; approval lives between the two calls.
- **Hosted MCP support for this tool** — out of scope until hosted MCP itself is. When it is, we move to Q5 option (c).

## Related

- ADR-019 (Fishhawk as coordination layer) — the agent-driven dialogue this ADR makes structurally complete.
- ADR-021 / #322 (MCP server v0 read-only) — the principle this tool sits in tension with; resolved by noting that this is operator-side (MCP server holds `fhk_*` token), not runner-side.
- ADR-022 / #388 (Pluggable runner backends + runner_kind) — the addendum constrains identity to read-only on the runner side; this ADR preserves that — the spawned runner subprocess still gets an `fhm_*` mcptoken.
- #427 — the discussion issue this ADR resolves.
- #425 — local-loop CI smoke test pattern the impl issue inherits.
- #426 — sibling: `fishhawk_start_run` field parity (the input half of the local-loop dialogue).
