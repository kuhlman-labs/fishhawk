---
id: ADR-021
title: "Claude Code integration: ship a Fishhawk MCP server"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/322
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-021: Claude Code integration: ship a Fishhawk MCP server

## Context

Per ADR-019, Fishhawk becomes a coordination layer surfacing actions where developers live. Claude Code is the surface where the actual agent work happens — both in customer runners (`runner/cmd/fishhawk-runner/main.go` invokes Claude Code as the implementation agent) and in interactive sessions where engineers ask Claude Code to do work locally.

Today the agent is intentionally blind to Fishhawk state. The runner constructs a prompt server-side via `GET /v0/stages/{id}/prompt`, hands it to Claude Code, and post-processes the trace. The agent operates in a single-shot context with no way to query "what's my run id, what's the audit history so far, what's the active plan, what constraints apply." Operators using Claude Code interactively (writing a plan locally, say) have no Fishhawk awareness at all — they alt-tab to the SPA.

We need to decide the integration shape before E19 starts coding. The choice affects: who the audience is (in-runner agent vs interactive developer vs both), how state flows (read-only context vs action-taking), and where the integration lives in the codebase.

## Options

### A. Claude Code slash skill (operator-facing only)

Ship a `/fishhawk` skill in the user's `~/.claude/skills/` directory that wraps the CLI. Slash invocations (`/fishhawk status`, `/fishhawk plan`) call out to `fishhawk run status` etc. and surface the output in the Claude Code session.

**Pros**: Tiny scope; reuses the CLI completely. No new module.

**Cons**: Only helps interactive operators. The in-runner agent gets nothing. Slash commands are operator-triggered; the agent can't consume them as context for its own reasoning.

### B. MCP server (agent + operator)

Ship a `fishhawk-mcp` binary that implements the Model Context Protocol. Tools expose read-only Fishhawk state: `fishhawk_get_active_run`, `fishhawk_get_plan`, `fishhawk_get_run_status`, `fishhawk_list_audit`. Operators add it once via `claude mcp add fishhawk`; the in-runner agent has the same server available via the runner's environment.

**Pros**: Serves both audiences with one surface. The agent in a run can query its own state ("what's my plan?", "what audit entries fired for the last retry?") and reason from that. Operators get the same context in their interactive Claude Code sessions.

**Cons**: New module + new distribution channel (binary in releases, MCP add docs). Auth shape needs careful thought — bearer tokens leaked into agent environments are a real risk; we may need scoped or short-lived tokens.

### C. Both

Ship the MCP server (B) AND a slash skill (A). The skill wraps the MCP for the human-interactive case; the MCP serves the agent.

**Pros**: Best of both. The skill gives operators a one-line invocation; the MCP gives the agent structured tools.

**Cons**: Two artifacts to maintain. Probably over-engineered for v0 — the MCP server alone serves both audiences (just with a slightly less ergonomic UX for interactive use, since the agent has to know to call the tool).

## Recommendation

**B** — ship the MCP server.

The agent-in-runner case is the higher-leverage audience. A retry handler today can read the full audit log of its parent run, but the Claude Code agent that's actually doing the retry implementation can't. Closing that loop unlocks better agent reasoning — the agent can see what was attempted, what failed, what the constraints are, and adjust.

Interactive operators benefit too: an engineer asking Claude Code "what's the status of my current run" via natural language gets the answer through the same tools, no slash command syntax to memorize.

A slash skill can ship later as a thin wrapper if reviewers want one-line invocation; the MCP is the load-bearing piece.

### Tool surface (initial)

| Tool | Reads | Notes |
|---|---|---|
| `fishhawk_get_active_run` | `GET /v0/runs?repo=…&trigger_ref=…` or env-based lookup | Resolves "the run for this context" from `GITHUB_REPOSITORY` + current branch / PR URL. |
| `fishhawk_get_plan` | `GET /v0/stages/{plan-stage-id}/artifacts` (or `/v0/runs/{id}/plan`) | Walks `parent_run_id` for retry chains. |
| `fishhawk_get_run_status` | `GET /v0/runs/{id}` + `/v0/runs/{id}/stages` | Current stage + state + recent activity. |
| `fishhawk_list_audit` | `GET /v0/runs/{id}/audit?category=…&stage=…` | Filtered audit list. |

All tools are **read-only** in v0. Action verbs (approve, retry, cancel) stay in the CLI + SPA + GitHub for now. Agents proposing actions can articulate them; humans take them.

### Auth shape

`FISHHAWK_API_TOKEN` env var. For interactive use, the operator generates a token via the existing API-token surface. For in-runner use, the runner provisions a scoped, short-lived token at stage-start (similar to the existing signing-key flow per #218) — flag this as a sub-design under E19.1 since the token-shape question has security implications worth pinning before code lands.

### Distribution

New binary `fishhawk-mcp` built from `cmd/fishhawk-mcp/` (or a new top-level `mcp-server/` module — module-layout decided in E19.2). Released via the same GitHub Release pipeline as `fishhawk` and `fishhawk-runner`. Docs page: `claude mcp add fishhawk --binary <path-to-fishhawk-mcp>`.

## Decision

Adopt option **B**. Start with read-only tools; ship a slash skill later only if reviewers ask.

## Consequences

- New module: `cmd/fishhawk-mcp/` (or top-level `mcp-server/`) in the Go workspace.
- New release artifact: `fishhawk-mcp` binary published per release.
- New audit category considered for tool invocations: `mcp_tool_called` (debated in E19.1 — may be too chatty; could log instead).
- The runner's agent invocation expanded to include `FISHHAWK_API_TOKEN` in the environment, scoped to the run's lifetime.
- Backend API surface stays read-only-from-MCP; existing `/v0/runs/*`, `/v0/stages/*`, `/v0/audit/*` endpoints are sufficient.
- A future v0.x or v1 may add MCP tools that take actions (approve, retry). Defer; v0 is read-only.

## Out of scope

- A Claude Code slash skill (`/fishhawk status`). Optional follow-up; skip unless reviewers ask.
- Other MCP clients (Cursor, Continue.dev). The MCP protocol is client-agnostic; we publish the server, anyone implements MCP can consume it. Test in Claude Code first.
- Agent actions via MCP (write-side tools). Read-only v0 is the right starting point.

## Related

- ADR-019 — the umbrella coordination-layer decision this is a sub-decision of.
- E19 / #325 — the impl track this ADR unblocks.
- #218 — the runner's signing-key flow; the scoped-token shape may follow the same pattern.
