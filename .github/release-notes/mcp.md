<!--
  Header for MCP releases. The mcp-release workflow appends
  GitHub's auto-generated changelog (commits since the previous
  mcp/v* tag) underneath this body via
  `generate_release_notes: true`.

  To customize per-release: edit this template before tagging, or
  open the GitHub Release after publication and revise the body.
-->

## Fishhawk MCP server

Model Context Protocol server that exposes Fishhawk run / plan / audit state to Claude Code (and any other MCP-compatible client). Read-only per [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322); see [`docs/mcp/install.md`](../docs/mcp/install.md) for the operator-facing install path.

Two audiences share one surface:

- The in-runner Claude Code agent reads its own run's state mid-execution.
- The interactive Claude Code session — a developer asking "what's the status of my Fishhawk run" — gets the answer through natural language.

## What's in this release

- `fishhawk-mcp-<version>-darwin-arm64` — Apple Silicon Mac.
- `fishhawk-mcp-<version>-darwin-amd64` — Intel Mac.
- `fishhawk-mcp-<version>-linux-amd64` — Linux x86_64.
- `fishhawk-mcp-<version>-linux-arm64` — Linux ARM64 (incl. the runner container's arm64 build).
- `mcp-<version>.sbom.spdx.json` — SPDX-JSON SBOM produced by [`anchore/sbom-action`](https://github.com/anchore/sbom-action). Covers the backend module's whole dependency tree, since the MCP binary lives under `backend/cmd/fishhawk-mcp/`.
- `SHA256SUMS` — sha256 of every artifact above.
- `SHA256SUMS.sig` + `SHA256SUMS.pem` — keyless [cosign](https://docs.sigstore.dev/cosign/overview/) signature + Fulcio certificate chain. Issued by the GitHub Actions OIDC identity for this repo + workflow.

## Verifying the release

```sh
# Download SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.pem from this release.
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/mcp-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
sha256sum -c SHA256SUMS
```

A passing verify means the file came from this repo's mcp-release workflow on this tag — no managed PGP key in the loop.

## Tool surface

All read-only per ADR-021:

| Tool | What |
|---|---|
| `fishhawk_get_active_run` | Resolves "the run for the current context" from `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env. |
| `fishhawk_get_plan` | Returns the approved standard_v1 plan; walks `parent_run_id` for CI-retry chains. |
| `fishhawk_get_run_status` | Bundles Run + ordered stages + recent audit (time-descending) into one tool call. |
| `fishhawk_list_audit` | Filtered audit access with cursor pagination. |

## Compatibility

The server speaks the v0 Fishhawk HTTP API (`docs/api/v0.openapi.yaml`) over bearer auth, and MCP protocol spec 2025-11-25 (via `github.com/modelcontextprotocol/go-sdk` v1.6.0) over stdio.
