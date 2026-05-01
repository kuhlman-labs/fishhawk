# fishhawk/runner

The GitHub Action that runs an agent under a Fishhawk workflow stage and ships the signed trace bundle back to the backend. Customers reference the action as:

    uses: kuhlman-labs/fishhawk/runner@v0.1

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/runner`) so it can be tagged independently of the backend and the CLI — the customer-facing version pin is on the runner alone. See [ADR-014 (#78)](https://github.com/kuhlman-labs/fishhawk/issues/78) for the multi-module rationale.

## Layout

- `action.yml` — composite action manifest. Defines inputs, sets up the Go toolchain, invokes the binary.
- `cmd/fishhawk-runner/` — the binary entrypoint. Flag parsing in `flags.go`, dispatch in `main.go`.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E5.1 (#52) — module scaffold. The binary parses its inputs, emits a single JSON startup log line, and exits 0. Customers pinning `v0.1` see a no-op success until later issues land:

- E5.2 (#29) — Claude Code invocation harness
- E5.3 (#30) — full trace capture + bundling
- E5.4 (#31) — plan validation against `standard_v1` (reuses `backend/internal/plan`)
- E5.5 (#53) — post-hoc constraint enforcement on stage output
- E5.6 (#32) — signed trace shipping to backend (uses `backend/internal/signing` + `backend/internal/tracestore`)
- E5.7 (#54) — versioned, signed releases of `fishhawk/runner` with SBOM

## Inputs (action.yml)

| Input | Description |
|---|---|
| `run-id` | Workflow run identifier (UUID, supplied by backend dispatch). |
| `backend-url` | Fishhawk backend URL the runner ships its trace bundle to. |
| `workflow` | Workflow ID matching a key under `workflows:` in `.fishhawk/workflows.yaml`. |
| `stage` | Stage ID within the workflow (e.g. `plan`, `implement`, `review`). |

## Build and test

From the repo root (workspace-aware):

    go build ./runner/...
    go test -race ./runner/...

Or from this directory directly:

    go build ./...
    go test ./...

## Local invocation

The same binary the action runs can be invoked locally for development:

    go run ./cmd/fishhawk-runner \
      --run-id 11111111-2222-3333-4444-555555555555 \
      --backend-url http://localhost:8080 \
      --workflow feature_change \
      --stage plan

Today this just emits a JSON log line and exits. As E5.2+ land, the same invocation will run the configured agent end-to-end.

## See also

- `docs/MVP_SPEC.md` §5.1.2 — runner component definition.
- `docs/MVP_SPEC.md` §5.3 — trust model (signing, supply-chain, ephemeral keys).
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle, where the runner sits in the dispatch flow.
