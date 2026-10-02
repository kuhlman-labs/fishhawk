---
id: ADR-037
title: "Agent-facing waits follow the MCP Tasks / long-running-operation contract (durable handle + authoritative poll)"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/879
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-037: Agent-facing waits follow the MCP Tasks / long-running-operation contract (durable handle + authoritative poll)

**Status: Decided (2026-06-08).** Establishes the shape of every agent-facing *wait* operation on Fishhawk's MCP surface. Relates to ADR-033 (#843, MCP transport) and ADR-036 (#874, gate fail-safe). Instances: #878 (`await_review`), #880 (`run_stage`).

## Context

Agents wait on multi-minute Fishhawk operations — a stage running (`run_stage`, 6–13 min observed), an agent review landing (`await_review`, ~3.5–4.5 min measured), a run reaching terminal (review-gate / merge resolution; decomposition children). **Load-bearing constraint: MCP is the primary interface — we cannot assume an agent has a shell, the `fishhawk` CLI, or any host-specific async/background mechanism.** Today the surface mixes a synchronous long-block (`run_stage`; `await_review`'s 120s default vs ~4 min real latency) with ad-hoc polling, forcing agents to either burn a turn blocking (hitting transport/host timeouts — a disconnect can lose a 13-min result) or hand-roll `sleep` loops.

## Options

Research into MCP and the broader API ecosystem (sources below) shows a clear convergence plus several explicitly-rejected alternatives:

1. **Naive long-block (just a bigger timeout).** Rejected — and explicitly rejected by the MCP Tasks designers: exceeds transport/host timeouts, blocks agent parallelism, disconnect loses the work.
2. **Split start/check/get tools.** Rejected: relies on prompt engineering ("the agent forgets to check back"), duplicative.
3. **Webhooks / server push as the source of truth.** Rejected: needs a client-callable endpoint (impractical for many agents/desktops); push is best-effort, never authoritative.
4. **Event via harness agent re-invocation, or a backgrounded CLI.** Rejected *for this system*: those are Claude-Code *harness* affordances (background-task completion; a shell + the `fishhawk` binary), NOT MCP primitives — they fail an MCP-only agent, violating the constraint.
5. **Long-running-operation (LRO) / MCP "Tasks" pattern.** **Chosen.** The call returns a durable handle immediately; the caller polls the handle to a terminal status (authoritative), notifications optional ("poll for truth, listen for speed"). MCP standardized this as **Tasks (SEP-1391, the 2025-11-25 revision; experimental)**; the broader world matches it — Google AIP-151 (an `Operation` handle "≈ a Future/Promise", poll to done), Azure Async Request-Reply, MS Graph LRO (202 + operation + poll).

## Decision

Align Fishhawk's agent-facing waits to the LRO/Tasks contract:

- **Durable handle returned immediately** — Fishhawk already has them (`run_id` / `stage_id`).
- **Authoritative poll-to-terminal** — `get_run_status` is the poll, the audit chain is the source of truth — with **well-defined terminal statuses** per operation; notifications are best-effort only.
- **Server-suggested `pollInterval`** so agents poll on the right cadence instead of guessing sleeps.
- **Idempotent dispatch** — Fishhawk already has `idempotency_key` on `start_run` — so a retried request dedupes to the same handle.
- **Native MCP Tasks (`invocationMode:async`) when the client supports it; SYNCHRONOUS (with progress keep-alive) as the negotiated fallback** — capability-negotiated, so a Tasks-capable agent gets fire/poll/parallelize/recover and a sync-only agent still works.
- **Fail-safe (per ADR-036 #874):** resolve on ANY terminal outcome, with a backstop so nothing strands. `input_required` (MCP Tasks' human-in-the-loop status) maps directly to Fishhawk's human gates (plan approval / review).

Any convenience block (e.g. `await_review`) is a thin, **calibrated, idempotent, non-stranding** wrapper over the poll — never the primary mechanism, never an uncalibrated long-block.

## Consequences

- Portable to ANY MCP agent with no shell/CLI/side-channel assumption — satisfies the load-bearing constraint.
- Fishhawk mostly has the bones already; the work is *aligning to the contract*, not inventing a channel.
- **Two-phase delivery.** **Near-term (buildable now, no external dep):** server-suggested `pollInterval` + a crisp terminal-status contract on the handle + idempotent dispatch + bless poll-the-handle + recalibrate `await_review`. **Native-Tasks half is gated on** (a) MCP Tasks leaving experimental / the harness supporting it, and (b) the transport decision ADR-033 (#843) — streamable-HTTP's resumable SSE lets long polls/streams survive disconnects.
- Instances: `await_review` #878, `run_stage` #880; "await run terminal" (review-gate/merge resolution + decomposition `children_settled`) folded in. CI / check-run "green" waiting stays GitHub-native (`gh pr checks --watch`) — out of scope.

Sources: WorkOS "MCP Async Tasks: long-running workflows for AI Agents" (workos.com/blog/mcp-async-tasks-ai-agent-workflows); MCP SEP-1391 Long-Running Operations (github.com/modelcontextprotocol/modelcontextprotocol/issues/1391); Google AIP-151 (google.aip.dev/151); Azure Async Request-Reply (learn.microsoft.com/azure/architecture/patterns/asynchronous-request-reply); MCP Progress utility (modelcontextprotocol.info/specification/draft/basic/utilities/progress/).

Parent epic: #389
