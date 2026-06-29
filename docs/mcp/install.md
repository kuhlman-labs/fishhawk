# fishhawk-mcp install

Operator-facing install path for the Fishhawk MCP server. Covers manual binary install + `claude mcp add` registration. The model decision lives in [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322); the lifecycle-write tools landed under [E22 / #389](https://github.com/kuhlman-labs/fishhawk/issues/389); the module source lives at `backend/cmd/fishhawk-mcp/`.

## Status

E19.7 / #347 shipped the release pipeline + v0 read-only tools. E22 added the lifecycle-write surface (start_run, cancel_run, retry_stage, approve_plan, reject_plan, list_runs). Direct binary download is the v0 install path; Homebrew / other package-manager distribution is out of scope.

## Prerequisites

- A Fishhawk API token (`fhk_*`). Issued by the backend's apitoken surface; for local dev, `fishhawkd token issue --subject <login> --scopes read:runs,read:audit,write:runs,write:approvals,write:stages,write:deploy,write:campaigns` mints a token with the full operator surface.
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
  --env FISHHAWK_RUNNER_BIN=/usr/local/bin/fishhawk-runner \
  -- /usr/local/bin/fishhawk-mcp
```

`--` separates Claude Code's flags from the binary path. The exact flag shape varies by Claude Code version — `claude mcp add --help` is authoritative. Some clients prefer a config file path; some take env vars inline.

`FISHHAWK_RUNNER_BIN` is required only when `fishhawk-runner` is not in system PATH and not co-located with `fishhawk-mcp`. When both binaries live in the same directory, the MCP server resolves `fishhawk-runner` automatically via its sibling-binary probe (`os.Executable` + `filepath.Dir`).

### 5. Smoke-test the registration

In a Claude Code session, ask:

> What's the status of run `<some-run-uuid>`?

The agent should call `fishhawk_get_run_status` and return the Run row + ordered stages + recent audit. If you get "tool not available" or similar, recheck `claude mcp list` and your env vars.

## Tool surface

Two audiences with different scopes:

- **Operator-side** (your terminal Claude Code with an `fhk_*` apitoken): full lifecycle. Read tools work with `read:runs,read:audit`; the write tools need `write:runs,write:approvals,write:stages,write:deploy,write:campaigns` per the table below.
- **Runner-side** (the agent inside a Fishhawk runner with an `fhm_*` mcptoken, scope `mcp:read`): read-only. Per ADR-021 / [ADR-022's addendum](https://github.com/kuhlman-labs/fishhawk/issues/388) — runner identity is read-only across all runner backends. The agent never approves its own work.

### Read tools

| Tool | What |
|---|---|
| `fishhawk_get_active_run` | Resolves "the run for the current context" from `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env. |
| `fishhawk_get_plan` | Returns the approved standard_v1 plan; walks `parent_run_id` for CI-retry chains. |
| `fishhawk_get_run_status` | Bundles Run + ordered stages + recent audit (time-descending) into one tool call. |
| `fishhawk_list_audit` | Filtered audit access (`category`, `stage_id`) with cursor pagination. |
| `fishhawk_verify_run` | Verify audit chain integrity for a run — checks every entry hash and chain link. `verified=false` is a halt condition before opening a PR. |

### Write tools (operator-side only)

| Tool | What | Required scope |
|---|---|---|
| `fishhawk_start_run` | Create a new run; mirrors `fishhawk run start`. Local-runner inputs (`working_dir`, `workflow_spec`, `issue`, `runner_kind`) auto-discover the spec + fetch the issue payload — see [Local-runner mint](#local-runner-mint) below. Optional `idempotency_key` for safe re-submit. | `write:runs` |
| `fishhawk_cancel_run` | Cancel a running run; mirrors `fishhawk run cancel`. Idempotent on re-cancel. | `write:runs` |
| `fishhawk_retry_stage` | Re-fire a failed stage per its category; mirrors `fishhawk run retry`. Categories A/C re-dispatch; B / gate-rejected D surface as `retry_not_applicable`. | `write:stages` |
| `fishhawk_approve_plan` | Approve the plan stage of a run (resolves stage from run id); mirrors `fishhawk plan approve`. | `write:approvals` |
| `fishhawk_reject_plan` | Reject the plan stage with optional rationale; mirrors `fishhawk plan reject`. | `write:approvals` |
| `fishhawk_list_runs` | Enumerate runs with filters (`repo`, `workflow_id`, `state`) + cursor pagination. Each run's `issue_context` (issue body + all comments) is omitted by default to stay within the tool-result token cap; pass `include_issue_context: true` to re-include it. | `read:runs` |
| `fishhawk_run_stage` | Drive one stage of a local-runner run by spawning `fishhawk-runner` as a subprocess; mirrors `fishhawk runner start`. Events stream as MCP `notifications/progress` when the client provides a progress token; the final result carries the full event list and post-run stage state. Cancellation: SIGTERM + 30s grace + SIGKILL; the runner handles SIGTERM cooperatively ([#435](https://github.com/kuhlman-labs/fishhawk/issues/435)) — exits with code 130 and emits a `runner_cancelled` event. Requires the `fishhawk-runner` binary to resolve on the MCP server's host — see [ADR-024](https://github.com/kuhlman-labs/fishhawk/issues/433). | `write:runs` |

**Auth posture** (today, v0):

- `fhm_*` mcptokens calling `fishhawk_approve_plan` / `fishhawk_reject_plan` are rejected by the backend's role check (`checkApproverAuthorization`) when `RoleResolver` is wired — the runner's `mcp:run:<id>` subject won't match any team in the gate's approver list.
- Scope enforcement is active at the wire level for all write paths. An `fhm_*` mcptoken calling any write tool receives HTTP 403 `insufficient_scope` (implementing [#402](https://github.com/kuhlman-labs/fishhawk/issues/402)).

**Runner-side scopes** (issued by the backend at stage start):

- All mcptokens carry `mcp:read` (baseline; always present).
- `write:retries` is NOT in the default set and is NOT issued via `fishhawkd token issue`. It is included in the `fhm_*` mcptoken only when the workflow spec sets `executor.agent_self_retry: true` on the executing stage. Operators opt in at the spec level. This scope allows the in-runner agent to call `POST /v0/stages/{id}/retry` (via `fishhawk_retry_stage`) to re-open its own failed stage without operator intervention — subject-bound to the token's run; cross-run retries are rejected with `cross_run_retry`.

## Local-runner mint

For the local-runner flow ([E22 / #389](https://github.com/kuhlman-labs/fishhawk/issues/389)), the agent inside Claude Code can mint a real, stage-bearing run on the operator's workstation without leaving the chat. The MCP server's `fishhawk_start_run` accepts the same convenience inputs the CLI does:

| Input | What | Set by |
|---|---|---|
| `working_dir` | Absolute path of the checkout. The MCP server walks up to the `.git` boundary looking for `.fishhawk/workflows.yaml` and ships the bytes inline, computing `workflow_sha` from the discovered file. | Agent (typically the cwd it's working in). |
| `workflow_spec` | Inline YAML body, if the agent already has it. Skips the disk walk. | Agent. |
| `spec_file` | Explicit spec path, overrides `working_dir` auto-discovery. | Agent (rare; mostly for test scenarios). |
| `issue` | GitHub issue number, `#N`, or `https://github.com/owner/repo/issues/N`. The MCP server shells to `gh issue view` and ships the title/body/url/number inline so the prompt builder reads the cache instead of needing an installation_id. Best-effort: a missing `gh` emits a warning on the tool result and the run proceeds without the cache. | Agent. |
| `issue_context` | Pre-fetched issue payload (alternative to `issue`); only valid with `trigger_source=github_issue`. | Agent — when the agent already fetched the issue itself. |
| `runner_kind` | `github_actions` (default) or `local`. The local-runner flow uses `local` so the dispatcher skips the workflow_dispatch hop and waits for `fishhawk runner start` to drive each stage. | Agent. |

The composition matches the CLI's `fishhawk run start --working-dir … --issue … --runner-kind local`. With `fishhawk_run_stage` ([ADR-024](https://github.com/kuhlman-labs/fishhawk/issues/433) / #434), the dialogue inside Claude Code can be entirely agent-driven — no terminal handoff:

1. Agent calls `fishhawk_start_run` with `working_dir`, `issue`, `runner_kind=local`.
2. Agent calls `fishhawk_run_stage --stage plan` (the MCP server spawns `fishhawk-runner` and streams events).
3. Agent calls `fishhawk_approve_plan`.
4. Agent calls `fishhawk_run_stage --stage implement`.
5. Agent calls `fishhawk_verify_run` with the run id. A `verified=false` result is a halt — do not open a PR; file an incident.

The `fishhawk_run_stage` tool requires the `fishhawk-runner` binary on the MCP server's host (`PATH` lookup, overridable via `FISHHAWK_RUNNER_BIN` env or the tool's `runner_binary` input). The MCP server runs locally on an operator's workstation today, so this is always satisfied; a future hosted MCP deployment will surface a clean tool error.

`stage_id` is optional ([#602](https://github.com/kuhlman-labs/fishhawk/issues/602)): when omitted, the tool resolves it from `(run_id, stage)` by listing the run's stages and matching on stage type, so you no longer hand-copy a stage UUID. Pass `stage_id` explicitly only to disambiguate or for back-compat — when supplied it must match a stage of the requested type, or the call errors rather than spawning against the wrong stage. A v0 run has at most one stage per type, so the requested type normally resolves uniquely; the >1 case is reported as an ambiguous error naming the duplicate ids rather than silently picking one.

Cancellation: cancelling the `fishhawk_run_stage` tool call sends `SIGTERM` to the runner, waits 30 seconds, then escalates to `SIGKILL`. The runner handles SIGTERM cooperatively (#435) — `ctx.Done()` propagates to the long-running calls (agent invocation + trace/plan/PR uploads), the deferred cancel-emit writes a `runner_cancelled` JSONL line on stdout, and the process exits with code 130 (`128 + SIGINT` convention). The bundle that was packed up to the cancellation point still ships best-effort, so the backend receives whatever events the agent produced.

## Runner integration

[E19.8 / #348](https://github.com/kuhlman-labs/fishhawk/issues/348) wires `fishhawk-mcp` into the runner so the in-runner Claude Code agent has Fishhawk awareness mid-execution. The runner fetches an `fhm_*` mcptoken via the signed `POST /v0/runs/{id}/mcp-token` endpoint at stage start, stamps it onto the agent's env (`FISHHAWK_API_TOKEN`), and the agent can call the read tools to introspect its own context. Write tools are not on the runner's surface (per ADR-022's addendum).

## Troubleshooting

- Run `fishhawk doctor` first — it checks backend reachability, token validity, spec presence, runner-binary resolution, MCP registration, git state, and gh CLI auth in one pass and prints a remediation hint for every failing rung.

| Symptom | Likely cause |
|---|---|
| `FISHHAWK_API_TOKEN is required` on startup | The env var is unset or empty. Set it (see step 3). |
| Claude Code says the tool isn't available | The MCP add step didn't take. Re-run `claude mcp add fishhawk` and verify with `claude mcp list`. After rebuilding the binary, restart Claude Code so it re-spawns the MCP subprocess. |
| `fishhawk: HTTP 401 (...)` from any tool | Token expired or invalid. Reissue via the backend's API-token surface. |
| `fishhawk: HTTP 403 (insufficient_scope)` from a write tool | The token doesn't carry the required write scope (see the Write tools table). Reissue with `--scopes` including the right write scopes. |
| `fishhawk: HTTP 404` from `fishhawk_get_plan` | Run id valid but the plan stage hasn't terminated — the tool returns a structured `no_plan_yet` response, not an error. Other 404s usually mean the run id is wrong. |
| `no plan stage on run …` from `fishhawk_approve_plan` | The run's workflow doesn't have a plan stage (e.g. `routine_change`). Approve at the stage level directly via the CLI / SPA. |
| `fishhawk-runner not on PATH` from `fishhawk_run_stage` | The binary could not be resolved via any rung of the resolution chain (input → env → sibling → PATH). Remediate in order of preference: (1) install `fishhawk-runner` in the same directory as `fishhawk-mcp` — the sibling-binary probe resolves it automatically; (2) set `--env FISHHAWK_RUNNER_BIN=<path>` in `claude mcp add` (see step 4); (3) pass `runner_binary` per tool call as a last resort. |
| `stage type "…" not found in run …` from `fishhawk_run_stage` | The run has no stage of the requested `stage` type — the error lists the run's available stage types. Check that `stage` matches the run's workflow (e.g. `routine_change` has no plan stage) and that the prior stage actually created the one you're asking to run. |
| `stage type "…" is ambiguous in run …` from `fishhawk_run_stage` | The run has more than one stage of the requested type (unusual for v0). The error names the duplicate stage ids; pass the intended one as `stage_id` explicitly to disambiguate. |

## See also

- [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model decision (server vs slash skill).
- `backend/cmd/fishhawk-mcp/README.md` — module-level README.
- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
