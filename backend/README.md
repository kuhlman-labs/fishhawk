# fishhawkd

The Go service that orchestrates workflow runs in Fishhawk. Owns the
workflow run / stage state machine, the policy evaluator, approval state,
the audit log writer, the GitHub App webhook receiver, and the REST API
consumed by the CLI and the Web UI.

This directory is its own Go module. It is tied into the repo via
`go.work` at the root so it can be tagged and released independently of
the runner action and the CLI. See
[ADR-014](https://github.com/kuhlman-labs/fishhawk/issues/78) for the
multi-module rationale.

## Layout

- `cmd/fishhawkd/` — the binary entrypoint.
- `internal/` — packages private to this module. The bulk of backend
  logic lives here as it lands.

## Build and test

From the repo root (workspace-aware):

    go build ./backend/...
    go test ./backend/...

Or from this directory directly:

    go build ./...
    go test ./...

## Status

E3.2 (#42) — HTTP server with graceful shutdown, middleware stack, and
the `/healthz` endpoint. The middleware order, outermost first, is
recovery → request ID → logging → auth stub → mux. Auth is a stub that
sets `Identity{Subject: "anonymous"}` until E4 (#4) lands real auth.

Subsequent issues under epic E3 (#3):

- E3.3 (#43) — run/stage state machine.
- E3.4 (#44) — policy evaluator.
- E3.5 (#45) — approval state + SLA tracking.
- E3.6 (#46) — REST API surface for CLI + UI.
- E3.7 (#47) — GitHub App webhook receiver wiring.

## Run

    go run ./backend/cmd/fishhawkd
    curl http://localhost:8080/healthz

Override the listen address with `--addr` or `FISHHAWKD_ADDR`.

Larger context: `docs/MVP_SPEC.md` §5.1.1 (component) and §5.2 (execution
flow).
