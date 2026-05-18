# fishhawk-mcp install

Operator-facing install path for the Fishhawk MCP server. Covers manual binary install + `claude mcp add` registration. The model decision lives in [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322); the lifecycle-write tools landed under [E22 / #389](https://github.com/kuhlman-labs/fishhawk/issues/389); the module source lives at `backend/cmd/fishhawk-mcp/`.

## Status

E19.7 / #347 shipped the release pipeline + v0 read-only tools. E22 added the lifecycle-write surface (start_run, cancel_run, retry_stage, approve_plan, reject_plan, list_runs). Direct binary download is the v0 install path; Homebrew / other package-manager distribution is out of scope.

## Prerequisites

- A Fishhawk API token (`fhk_*`). Issued by the backend's apitoken surface; for local dev, `fishhawkd token issue --subject <login> --scopes read:runs,read:audit,write:runs,write:approvals,write:stages` mints a token with the full operator surface.
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
  --env FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN \
  --env FISHHAWK_BACKEND_URL=$FISHHAWK_BACKEND_URL \
  -- /usr/local/bin/fishhawk-mcp
```

`--` separates Claude Code's flags from the binary path. The exact flag shape varies by Claude Code version — `claude mcp add --help` is authoritative. Some clients prefer a config file path; some take env vars inline.

### 5. Smoke-test the registration

In a Claude Code session, ask:

> What's the status of run `<some-run-uuid>`?

The agent should call `fishhawk_get_run_status` and return the Run row + ordered stages + recent audit. If you get "tool not available" or similar, recheck `claude mcp list` and your env vars.

## Tool surface

Two audiences with different scopes:

- **Operator-side** (your terminal Claude Code with an `fhk_*` apitoken): full lifecycle. Read tools work with `read:runs,read:audit`; the write tools need `write:runs,write:approvals,write:stages` per the table below.
- **Runner-side** (the agent inside a Fishhawk runner with an `fhm_*` mcptoken, scope `mcp:read`): read-only. Per ADR-021 / [ADR-022's addendum](https://github.com/kuhlman-labs/fishhawk/issues/388) — runner identity is read-only across all runner backends. The agent never approves its own work.

### Read tools

| Tool | What |
|---|---|
| `fishhawk_get_active_run` | Resolves "the run for the current context" from `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env. |
| `fishhawk_get_plan` | Returns the approved standard_v1 plan; walks `parent_run_id` for CI-retry chains. |
| `fishhawk_get_run_status` | Bundles Run + ordered stages + recent audit (time-descending) into one tool call. |
| `fishhawk_list_audit` | Filtered audit access (`category`, `stage_id`) with cursor pagination. |

### Write tools (operator-side only)

| Tool | What | Required scope |
|---|---|---|
| `fishhawk_start_run` | Create a new run; mirrors `fishhawk run start`. Optional `idempotency_key` for safe re-submit. | `write:runs` |
| `fishhawk_cancel_run` | Cancel a running run; mirrors `fishhawk run cancel`. Idempotent on re-cancel. | `write:runs` |
| `fishhawk_retry_stage` | Re-fire a failed stage per its category; mirrors `fishhawk run retry`. Categories A/C re-dispatch; B / gate-rejected D surface as `retry_not_applicable`. | `write:stages` |
| `fishhawk_approve_plan` | Approve the plan stage of a run (resolves stage from run id); mirrors `fishhawk plan approve`. | `write:approvals` |
| `fishhawk_reject_plan` | Reject the plan stage with optional rationale; mirrors `fishhawk plan reject`. | `write:approvals` |
| `fishhawk_list_runs` | Enumerate runs with filters (`repo`, `workflow_id`, `state`) + cursor pagination. | `read:runs` |

**Auth posture** (today, v0):

- `fhm_*` mcptokens calling `fishhawk_approve_plan` / `fishhawk_reject_plan` are rejected by the backend's role check (`checkApproverAuthorization`) when `RoleResolver` is wired — the runner's `mcp:run:<id>` subject won't match any team in the gate's approver list.
- `fhm_*` mcptokens calling the other write tools (`start_run`, `cancel_run`, `retry_stage`) are **not yet** gated on scope at the handler — that enforcement is tracked at [#402](https://github.com/kuhlman-labs/fishhawk/issues/402). Until that lands, the read-only-runner property holds by convention (no production code path in the runner calls write tools) rather than wire enforcement.

## Runner integration

[E19.8 / #348](https://github.com/kuhlman-labs/fishhawk/issues/348) wires `fishhawk-mcp` into the runner so the in-runner Claude Code agent has Fishhawk awareness mid-execution. The runner fetches an `fhm_*` mcptoken via the signed `POST /v0/runs/{id}/mcp-token` endpoint at stage start, stamps it onto the agent's env (`FISHHAWK_API_TOKEN`), and the agent can call the read tools to introspect its own context. Write tools are not on the runner's surface (per ADR-022's addendum).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `FISHHAWK_API_TOKEN is required` on startup | The env var is unset or empty. Set it (see step 3). |
| Claude Code says the tool isn't available | The MCP add step didn't take. Re-run `claude mcp add fishhawk` and verify with `claude mcp list`. After rebuilding the binary, restart Claude Code so it re-spawns the MCP subprocess. |
| `fishhawk: HTTP 401 (...)` from any tool | Token expired or invalid. Reissue via the backend's API-token surface. |
| `fishhawk: HTTP 403 (insufficient_scope)` from a write tool | The token doesn't carry the required write scope (see the Write tools table). Reissue with `--scopes` including the right write scopes. |
| `fishhawk: HTTP 404` from `fishhawk_get_plan` | Run id valid but the plan stage hasn't terminated — the tool returns a structured `no_plan_yet` response, not an error. Other 404s usually mean the run id is wrong. |
| `no plan stage on run …` from `fishhawk_approve_plan` | The run's workflow doesn't have a plan stage (e.g. `routine_change`). Approve at the stage level directly via the CLI / SPA. |

## See also

- [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model decision (server vs slash skill).
- `backend/cmd/fishhawk-mcp/README.md` — module-level README.
- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
