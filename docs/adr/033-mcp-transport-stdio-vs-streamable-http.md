---
id: ADR-033
title: "MCP transport: stdio-only vs. add streamable-HTTP (relationship to the #655 gateway)"
status: accepted
date: 2026-06-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/843
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-033: MCP transport: stdio-only vs. add streamable-HTTP (relationship to the #655 gateway)

## Context

`fishhawk-mcp` (`backend/cmd/fishhawk-mcp`) currently runs on a single transport: **stdio** (`mcp.StdioTransport{}`, main.go). The MCP server is a **thin, stateless proxy** — every tool round-trips to `fishhawkd` over its HTTP API (`FISHHAWK_BACKEND_URL`, default `http://localhost:8080`), authenticated by a bearer token passed via the `FISHHAWK_API_TOKEN` env var. Each client (Claude Code, Codex, …) spawns its **own** `fishhawk-mcp` subprocess; the harness owns that process lifetime.

This works well for the single-operator local dogfood loop, but two real frictions have surfaced:

1. **Multi-client / multi-agent access.** Connecting a second agent (Codex) alongside Claude Code means a second spawned subprocess, not a shared endpoint. There is no way for several agents to share one MCP surface.
2. **Reconnect-after-rebuild friction.** Because the harness owns a per-client subprocess, a `fishhawk-mcp` rebuild requires a manual `/mcp` reconnect to go live (hit 3× in a single session). A long-lived service would not have this problem.

Separately, the product's hosted direction (`app.fishhawk.[tld]`) and the parked **#655 (MCP gateway + per-agent non-human identity + short-lived scoped credentials)** imply a future where agents connect to a *remote* MCP, not a co-located binary.

The MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`) supports a **streamable-HTTP** transport alongside stdio, and tool registration is transport-agnostic, so supporting both from one codebase is mechanically small. The hard part is the auth/security model that stdio currently provides for free (token via env, never on the wire; zero network listener; run-bound MCP tokens scoped to their own run).

## Options

- **(a) Stdio-only (status quo).** Keep stdio as the sole transport. Simplest, zero network surface, idiomatic for local tools. Multi-agent = multiple subprocesses; no remote access; reconnect friction stays.
- **(b) Add a localhost-only HTTP transport (dev-ergonomics slice).** `--transport http --addr 127.0.0.1:PORT`, reusing the single `FISHHAWK_API_TOKEN`, no TLS, bound to loopback only. Gives a shared local endpoint (multiple agents, no per-client subprocess) and removes the reconnect-after-rebuild friction. Does NOT attempt multi-tenant identity. Contained; explicitly not for remote/hosted use.
- **(c) Full HTTP gateway with per-agent identity (= pull #655 forward).** Streamable-HTTP listener with per-request bearer validation, TLS, per-agent non-human identity, and short-lived scoped credentials so run-bound token scoping survives a shared listener. The hosted/multi-tenant answer. Largest scope; subsumes/anchors #655.
- **(d) Dual-transport, staged.** Keep stdio as the default; add (b) now as a flag for local shared/dev use; defer (c) to #655 for anything past localhost. The transport flip and the auth/identity work are decoupled.

## Recommendation

**(d) — dual-transport, staged.** Keep stdio default; add an opt-in localhost-only HTTP transport (b) as a contained dev-ergonomics win; gate any past-localhost exposure behind the #655 identity/credential work (c). Rationale: the mechanical transport addition is cheap and immediately useful (shared local endpoint + no reconnect dance), while the genuinely hard and security-sensitive part (per-agent identity, scoped creds, TLS) is exactly #655 and should not be bolted on implicitly by exposing a bare listener. A bare HTTP listener reusing one operator token must therefore be **loopback-only and clearly marked not-for-remote** until #655 lands.

## Decision

**Adopt (d) — dual-transport, staged.** Decided 2026-06-09 (operator-confirmed).

Concretely:
- **stdio stays the default transport.** Existing consumers (Claude Code, Codex per-client subprocess) are unchanged.
- **Add an opt-in localhost-only HTTP transport now (option b)** as a contained dev-ergonomics slice. It is explicitly **single-operator, one run-scoped token, one shared loopback endpoint — NOT multi-tenant.**
- **Defer anything past loopback (option c) to #655.** A shared remote/multi-tenant listener requires per-agent non-human identity, short-lived scoped credentials, and TLS; that is #655's scope and must not be bolted onto (b) implicitly. #655 remains the gate for remote exposure.

Resolved sub-questions:
1. **Is (b) worth doing now? — Yes.** Reconnect-after-rebuild friction is real and recurring (#894, #901, plus earlier this session), and the Codex reviewer/executor adapters (#840/#844) make a shared local MCP surface a near-term want. The transport addition is cheap (transport-agnostic tool registration).
2. **Flag surface + loopback enforcement.** `--transport stdio|http` (default **stdio**); `--addr` only meaningful with `--transport http`, default `127.0.0.1:<port>`. **Hard-enforce loopback**: reject/clamp any `--addr` that resolves to a non-loopback IP, so a bare listener cannot be accidentally exposed. **Require the bearer per request even on loopback** — loopback is not a trust boundary on a multi-user host; validate `Authorization: Bearer <FISHHAWK_API_TOKEN>` per request rather than trust the socket.
3. **Run-bound-token coexistence.** A shared HTTP listener reusing the single `FISHHAWK_API_TOKEN` means every client on that listener shares that one run-bound token's scope. This does **not** weaken per-run scoping for the single-operator local loop (still one token, one run), but it cannot offer *different* run-scoped tokens per client — that genuinely needs per-request identity, which is (c)/#655. (b) must be documented as "single-operator local shared endpoint; not multi-tenant."
4. **Sequencing with #655.** (b) is a **migration path, not throwaway scaffolding** — the transport-selection + HTTP-handler wiring (b) adds is exactly what (c) builds on. (c) layers per-request bearer validation, per-agent non-human identity, short-lived scoped credentials, and TLS on the same listener.

**Impl issue:** #927 (fishhawk-mcp localhost-only HTTP transport).

## Consequences

- **If (d):** small additive change for (b); stdio consumers unaffected (default unchanged); a new, clearly-scoped loopback HTTP mode; #655 remains the gate for remote/multi-tenant. New (modest) surface area: an HTTP handler path + its tests, and docs distinguishing "local shared" from "hosted."
- **If we stay (a):** no new surface, but the multi-agent and reconnect frictions persist and the hosted story is deferred entirely to #655.
- Either way, this ADR records that **HTTP-MCP is not merely a transport flip** — anything past loopback pulls in per-agent identity + scoped credentials (#655).

## Related
#655 (MCP gateway + per-agent identity + scoped creds — the hosted/multi-tenant anchor), the mcp-release signed-binary distribution (E19.7/#347), ADR-021 (read-only MCP surface origins), ADR-031 (#710). Surfaced 2026-06-07 connecting a Codex agent to the local MCP.

Parent epic: #389
