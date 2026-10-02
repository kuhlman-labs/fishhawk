---
id: ADR-060
title: "MCP session-survival shim: stdio supervisor as gateway phase 0"
status: accepted
date: 2026-07-13
issue: https://github.com/kuhlman-labs/fishhawk/issues/1920
supersedes: []
superseded_by: []
applies_to: ["cmd/fishhawk-mcp-shim/**"]
---

# ADR-060: MCP session-survival shim: stdio supervisor as gateway phase 0

## Context

Every scripts/dev rebuild that touches fishhawk-mcp requires a manual /mcp reconnect because the Claude Code harness owns the stdio subprocess and keeps serving the OLD binary (8+ reconnects in the 2026-07-12/13 dogfood session; #1369). The 2026-07-13 spike set (recorded on #1369) established the decisive facts: (a) Claude Code does NOT re-initialize a restarted streamable-HTTP MCP session — the stale Mcp-Session-Id 404s and upstream explicitly closed the fix request 'not planned' (anthropics/claude-code#60949; docs: not-found errors never retried); go-sdk v1.6.1 sessions are an in-memory map with no pluggable store. (b) Claude Code honors notifications/tools/list_changed ACROSS turns (autoRefresh default-true wiring verified in the 2.1.207 bundle; mid-turn additions fail per anthropics/claude-code#31893). (c) go-sdk stdio framing is newline-delimited JSON, so a byte-preserving line passthrough is protocol-safe, and stdio servers are never auto-restarted by the client. (d) fishhawk-mcp is a stateless proxy (env-only config, no caches) and its handshake ALREADY advertises tools.listChanged (go-sdk defaults ToolCapabilities{ListChanged:true}; no explicit Capabilities override). Scope decision already recorded on #655 (2026-07-13): the parked gateway stays parked until after E44/ADR-057 tenancy; this ADR's mechanism is designed as that gateway's PHASE 0 so nothing is throwaway.

## Options

1. HTTP-transport-as-default-dev-loop (restart fishhawk-mcp --transport http in place): DEAD — the client will not resume the session (spike B, #60949 'not planned'); a restart still costs a manual /mcp. 2. Exec-self re-exec (the running fishhawk-mcp execs the rebuilt binary inheriting fds): REJECTED — go-sdk v1.6.1 cannot adopt an already-initialized session, so the synthetic-initialize resume logic would live inside the very binary being swapped; a bad build bricks the session with no fallback. 3. Stdio supervisor shim (cmd/fishhawk-mcp-shim): a small, rarely-changing passthrough binary the harness owns; spawns bin/fishhawk-mcp as a child, passes ndjson frames verbatim, records the client's initialize, watches the child binary for CONTENT (sha) change, quiesces in-flight requests, swaps the child, replays initialize with a synthetic id, synthesizes notifications/tools/list_changed, and respawns a crashed child. 4. Status quo (reconnect banner): the friction being retired.

## Recommendation

Option 3. It is the only mechanism the spike evidence permits; it isolates the swap logic in a stable binary (solving exec-self's chicken-and-egg); it upgrades crash behavior for free (today a crashed fishhawk-mcp also costs a /mcp; under the shim it is a transparent respawn); and with the child-transport behind a seam it is the #655 gateway's session-holding kernel — unparking #655 later means swapping the stdio child for a streamable-HTTP upstream and layering identity/credentials on the E44 tenancy schema.

## Decision

ACCEPTED 2026-07-13 (founder-directed). Implement cmd/fishhawk-mcp-shim per option 3 with these ratified design points: byte-preserving ndjson passthrough parsing only initialize and request ids; sha-compare (not mtime) swap trigger; bounded in-flight quiesce before swap (defer to next idle on timeout, never kill mid-request); initialize replay with synthetic id, response swallowed; synthesize tools/list_changed after every swap (schema updates land by the next operator turn — the across-turns limitation is accepted and documented); crash respawn with backoff plus the same replay; child transport behind a seam accepting a future streamable-HTTP upstream (gateway phase 0 for #655); post-swap verifiability via the GitSHA-stamped handshake and a version-returning tool call. The scripts/dev _signal_mcp_activation reconnect banner is retired when the shim is the registered server (kept as fallback otherwise). Accepted residuals: a schema-NEW tool invoked in the same turn as a swap fails once (#31893); shim-binary changes themselves still need one manual /mcp (rare by design).

## Consequences

New seldom-changing binary cmd/fishhawk-mcp-shim + one-time operator re-registration of the claude MCP server entry to point at the shim. scripts/dev builds the shim and drops the reconnect nag when the shim is registered. In-runner per-stage MCP consumers are unaffected (fresh spawn per stage already picks up new binaries). fishhawk-mcp itself stays unchanged except docs. When #655 unparks (after E44), the shim's transport seam becomes the gateway's upstream connector — the identity/credential substance lands on top rather than rebuilding session survival. Risk: the shim is on the critical path of every MCP interaction; mitigated by its deliberately tiny surface, fake-child unit tests for replay/quiesce/swap/respawn, and the banner fallback if it is ever unregistered.
