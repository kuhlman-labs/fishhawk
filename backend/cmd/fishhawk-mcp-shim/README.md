# fishhawk-mcp-shim

Stdio **session-survival supervisor** for the [`fishhawk-mcp`](../fishhawk-mcp/) MCP server ([ADR-060 / #1921](https://github.com/kuhlman-labs/fishhawk/issues/1921)).

## Why

Claude Code owns the MCP server subprocess and does **not** reconnect a restarted stdio server. A `scripts/dev reload` that rebuilds `fishhawk-mcp` leaves the live session pointed at the old binary until the operator runs `/mcp` by hand (see [ADR-021 gotcha](../fishhawk-mcp/README.md) and the dev-loop reconnect banner). The shim closes that gap: it sits between the client and `fishhawk-mcp`, watches the child binary for a rebuild, and hot-swaps it under the live session so the tool set refreshes with no manual reconnect.

## How it works

The shim spawns `fishhawk-mcp` as a child over pipes and passes newline-delimited JSON-RPC frames **byte-verbatim** in both directions. It parses only (a) the client's `initialize` request (recorded with the child's response) and (b) message ids, for in-flight request tracking.

- **Content poller** — a sha-256 poll (never mtime; a reload rebuild can be a byte-identical no-op) over the child binary path, with a settle debounce so a half-written `go build -o` output never triggers a swap.
- **Swap** — on a confirmed content change the shim quiesces (waits for zero in-flight client requests up to `--quiesce-timeout`; on timeout it defers the swap to the next idle moment — it never kills a child mid-request), SIGTERMs the old child (SIGKILL-escalated to its process group after a grace period), spawns the new binary, replays the recorded `initialize` with a synthetic collision-proof id (swallowing the response), sends `notifications/initialized`, and synthesizes `notifications/tools/list_changed` upstream so the client re-reads the tool set.
- **Crash recovery** — a crashed child is respawned with capped exponential backoff through the same replay path; any requests orphaned by the crash get synthesized JSON-RPC error responses so the client is never stranded.

The child connection sits behind a small `childTransport` seam so a later phase can substitute a streamable-HTTP upstream — the [#655](https://github.com/kuhlman-labs/fishhawk/issues/655) gateway phase-0 constraint.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--child` | sibling `fishhawk-mcp` next to the shim executable | path to the `fishhawk-mcp` child binary |
| `--poll-interval` | `2s` | how often to poll the child binary for a content change |
| `--quiesce-timeout` | `30s` | how long to wait for zero in-flight requests before deferring a swap |

## Registration

**One-time operator re-registration (required).** To move a live session off the manual-reconnect treadmill you must re-point the harness's `fishhawk` MCP server entry at the shim **instead of** `fishhawk-mcp` — a one-time step per host. If a plain `fishhawk-mcp` entry already exists, remove it and re-add pointed at the shim binary:

```sh
claude mcp remove fishhawk    # drop the existing plain fishhawk-mcp entry, if any
claude mcp add fishhawk --command /path/to/bin/fishhawk-mcp-shim \
  --env FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN
```

The registration mirrors the sibling [`fishhawk-mcp`](../fishhawk-mcp/README.md#install-operators) form — `--command` with the token wired through `--env` — just pointed at the shim binary. With the standard `bin/` layout no `--child` flag is needed. Run `/mcp` once after re-registering so the session picks up the shim; from then on child rebuilds hot-swap with no further `/mcp`.

The shim adds no auth of its own: it passes `FISHHAWK_API_TOKEN` / `FISHHAWK_BACKEND_URL` straight through to the child, so the child needs them in its environment. Wire `FISHHAWK_API_TOKEN` via `--env` as above (and `FISHHAWK_BACKEND_URL` too when it is not the default `http://localhost:8080`).

### `scripts/dev` integration

The shim is wired into the dev loop ([#1922](https://github.com/kuhlman-labs/fishhawk/issues/1922)) as the fifth rebuild-matrix binary with its **own** trigger glob (`backend/cmd/fishhawk-mcp-shim/` only — deliberately not the `backend/internal/plan|spec` shared-lib case, so it rebuilds rarely). `scripts/dev reload` (`--all`) rebuilds `bin/fishhawk-mcp-shim` alongside the others; a running shim keeps its open inode, so the on-disk rebuild is inert until a manual `/mcp` (see the accepted residual below).

`scripts/dev up`/`reload` select exactly one closing banner:

- **schema-major bump** — a `.fishhawk/workflows.yaml` `version:` MAJOR change; unconditional (#1422).
- **shim rebuilt** — `fishhawk-mcp-shim` source changed: a distinct banner telling you to run `/mcp` once (the shim swaps its child, not itself).
- **reconnect** — `fishhawk-mcp` changed and the shim is **not** registered: the legacy manual-reconnect banner.
- **auto-swap** — `fishhawk-mcp` changed and the shim **is** registered: a one-line note that the shim hot-swaps the rebuilt child automatically, verifiable via a version-returning tool call reflecting the new GitSHA.

Registration is detected via the `FISHHAWK_MCP_SHIM_REGISTERED` env override (sourced from `.env`; `1`/`true` or `0`/`false`) winning over a best-effort `claude mcp get fishhawk` probe; an absent or errored `claude` CLI degrades to not-registered, which keeps the manual banner (the fail-safe direction).

## Accepted residuals

- **A same-turn schema-new tool fails once.** Claude Code honours `notifications/tools/list_changed` across turns but not mid-turn ([anthropics/claude-code#31893](https://github.com/anthropics/claude-code/issues/31893), verified in the ADR-060 spike set). A tool whose schema is brand-new in the just-swapped binary fails once within the turn it was announced, then works.
- **Shim-binary changes still need one manual `/mcp`.** The shim swaps the *child*; a rebuild of the shim itself is still owned by the harness, so a change to `fishhawk-mcp-shim` needs a one-time `/mcp` reconnect like any MCP server change.
