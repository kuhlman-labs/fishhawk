# fishhawk-mcp

Model Context Protocol server that exposes Fishhawk run / plan / audit state to Claude Code (and any other MCP-compatible client) per [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322).

Two audiences share one surface:

- The **in-runner Claude Code agent** reads its own run's state mid-execution — what's the active plan, what audit entries fired for the current retry, what constraints apply. Closes the agent-is-blind-to-Fishhawk-state gap that motivated ADR-019.
- The **interactive Claude Code session** — an engineer asking "what's the status of my current run" — gets the answer through natural language without a CLI alt-tab.

All v0 tools are read-only. Action verbs (approve, retry, cancel) stay in the CLI / SPA / GitHub. A future v0.x or v1 may add write-side tools.

## Status

E19.2 / #342 shipped scaffolding + handshake. E19.3–E19.6 landed the v0 tool surface (all read-only per ADR-021):

- `fishhawk_get_active_run` (E19.3 / #343) — resolves "the run for the current context" from `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env.
- `fishhawk_get_plan` (E19.4 / #344) — returns the approved standard_v1 plan; walks `parent_run_id` up to 8 levels for CI-retry chains.
- `fishhawk_get_run_status` (E19.5 / #345) — bundles Run + ordered stages + recent audit (time-descending) into one call. The agent's "where are we" query.
- `fishhawk_list_audit` (E19.6 / #346) — filtered audit access (category, stage_id) with cursor pagination. Mirrors the CLI's `fishhawk audit list`.

E19.7 / #347 wires the binary into the release pipeline next.

## Build

From the repo root (workspace-aware):

```sh
go build ./backend/cmd/fishhawk-mcp/...
```

The binary lands at `./fishhawk-mcp` when built explicitly:

```sh
go build -o fishhawk-mcp ./backend/cmd/fishhawk-mcp
```

## Configuration

Two env vars; both honored from the OS environment when the binary launches.

| Variable                | Required | Default                 | Notes                                                                                                                                                                                                  |
| ----------------------- | -------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `FISHHAWK_API_TOKEN`    | **yes**  | —                       | Bearer token. Generated via the backend's API-token surface. No "anonymous" mode — unlike the CLI's dev path, MCP tools always round-trip the API and running unauthenticated would be a silent permission bug. |
| `FISHHAWK_BACKEND_URL`  | no       | `http://localhost:8080` | Same fallback as the CLI. Trailing slash is stripped.                                                                                                                                                  |

The binary exits non-zero on startup if `FISHHAWK_API_TOKEN` is missing.

## Wiring into Claude Code

After building, register the binary as an MCP server in your Claude Code config:

```sh
claude mcp add fishhawk --binary $(pwd)/fishhawk-mcp
```

Provide the env vars via your shell or the `claude mcp` config (path varies by client version — see Claude Code's docs).

Once registered, the agent has the Fishhawk tool surface available alongside other MCP servers. Interactive sessions can ask "what's the status of my Fishhawk run" and the agent picks the right tool.

## Distribution (planned)

E19.7 / #347 wires the binary into the GitHub Release pipeline alongside `fishhawk` (CLI) and `fishhawk-runner`. Once landed, operators can `claude mcp add` from a published archive rather than building locally.

## See also

- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
- `cli/internal/httpclient` — typed wrappers the MCP server reuses (or a thin local copy if cross-module reuse becomes awkward — final call inside individual tool PRs).
- [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model-decision ADR.
