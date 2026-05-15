# fishhawk-mcp install

Operator-facing install path for the Fishhawk MCP server. Covers manual binary install + `claude mcp add` registration. The model decision lives in [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322); the module source lives at `backend/cmd/fishhawk-mcp/`.

## Status

E19.7 / #347. Direct binary download is the v0 install path; Homebrew / other package-manager distribution is out of scope.

## Prerequisites

- A Fishhawk API token. The backend's API-token surface (E13 / forthcoming) issues these against a GitHub-authenticated identity. Until the surface lands, dev backends with stubbed auth accept any non-empty value — operators set `FISHHAWK_API_TOKEN=dev` to start.
- A reachable Fishhawk backend. `https://app.fishhawk.[tld]` for production installs; `http://localhost:8080` for local dev.
- Claude Code installed (any version supporting `claude mcp add`).

## Install

### 1. Download the binary for your platform

Grab the release for your target from the latest `mcp/vX.Y.Z` tag at <https://github.com/kuhlman-labs/fishhawk/releases>:

| Platform | Filename |
|---|---|
| Apple Silicon Mac | `fishhawk-mcp-<version>-darwin-arm64` |
| Intel Mac | `fishhawk-mcp-<version>-darwin-amd64` |
| Linux x86_64 | `fishhawk-mcp-<version>-linux-amd64` |
| Linux ARM64 | `fishhawk-mcp-<version>-linux-arm64` |

Rename to `fishhawk-mcp` and `chmod +x`:

```sh
# Pick the right asset for your platform; replace vX.Y.Z with the latest release tag.
curl -fSL \
  "https://github.com/kuhlman-labs/fishhawk/releases/download/mcp/vX.Y.Z/fishhawk-mcp-vX.Y.Z-darwin-arm64" \
  -o /usr/local/bin/fishhawk-mcp
chmod +x /usr/local/bin/fishhawk-mcp
```

### 2. Verify the binary

Each release ships `SHA256SUMS` + `SHA256SUMS.sig` + `SHA256SUMS.pem` signed by the GitHub Actions OIDC identity. Verify before trusting the binary on shared infrastructure:

```sh
# Download SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.pem from the release.
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/mcp-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
sha256sum -c SHA256SUMS
```

A passing verify means the file came from this repo's release workflow on this tag — no managed PGP key in the loop. See [ADR-009 / #75](https://github.com/kuhlman-labs/fishhawk/issues/75) for the broader supply-chain story.

### 3. Export the environment

```sh
export FISHHAWK_API_TOKEN="<your token>"
# Optional; defaults to http://localhost:8080.
export FISHHAWK_BACKEND_URL="https://app.fishhawk.example.com"
```

`FISHHAWK_API_TOKEN` is **required**. There is no "anonymous" mode — every tool round-trips the API and running unauthenticated would be a silent permission bug, not a developer convenience.

### 4. Register with Claude Code

```sh
claude mcp add fishhawk \
  --command /usr/local/bin/fishhawk-mcp \
  --env FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN \
  --env FISHHAWK_BACKEND_URL=$FISHHAWK_BACKEND_URL
```

The exact flag shape varies by Claude Code version — `claude mcp add --help` is authoritative. Some clients prefer a config file path; some take env vars inline.

### 5. Smoke-test the registration

In a Claude Code session, ask:

> What's the status of run `<some-run-uuid>`?

The agent should call `fishhawk_get_run_status` and return the Run row + ordered stages + recent audit. If you get "tool not available" or similar, recheck `claude mcp list` and your env vars.

## Tool surface

All read-only per ADR-021. Action verbs (approve, retry, cancel) stay in the CLI / SPA / GitHub — agents articulate proposed actions; humans take them.

| Tool | What |
|---|---|
| `fishhawk_get_active_run` | Resolves "the run for the current context" from `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env. |
| `fishhawk_get_plan` | Returns the approved standard_v1 plan; walks `parent_run_id` for CI-retry chains. |
| `fishhawk_get_run_status` | Bundles Run + ordered stages + recent audit (time-descending) into one tool call. |
| `fishhawk_list_audit` | Filtered audit access (`category`, `stage_id`) with cursor pagination. |

## Runner integration

E19.8 wires `fishhawk-mcp` into the runner's container image so the in-runner Claude Code agent has Fishhawk awareness mid-execution. Until that ticket lands, operators only see the MCP server in interactive Claude Code sessions on their dev machines.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `FISHHAWK_API_TOKEN is required` on startup | The env var is unset or empty. Set it (see step 3). |
| Claude Code says the tool isn't available | The MCP add step didn't take. Re-run `claude mcp add fishhawk` and verify with `claude mcp list`. |
| `fishhawk: HTTP 401 (...)` from any tool | Token expired or invalid. Reissue via the backend's API-token surface. |
| `fishhawk: HTTP 404` from `fishhawk_get_plan` | Run id valid but the plan stage hasn't terminated — the tool returns a structured `no_plan_yet` response, not an error. Other 404s usually mean the run id is wrong. |

## See also

- [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model decision (server vs slash skill).
- `backend/cmd/fishhawk-mcp/README.md` — module-level README.
- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
