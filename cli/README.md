# fishhawk CLI

Command-line interface for the Fishhawk control plane. Wraps the HTTP API documented in `docs/api/v0.openapi.yaml` so users can drive runs from a terminal.

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/cli`) so it can be released independently of the backend and runner. Per ADR-014 (#78), the multi-module workspace lets each component carry its own version tag.

## Layout

- `cmd/fishhawk/` — the binary entrypoint. Subcommand dispatch in `main.go`, per-command flags in `run.go`, validate logic in `validate.go`.
- `internal/httpclient/` — typed wrapper around the backend API. Marshals `CreateRunInput`, decodes `Run`, surfaces `*APIError` for non-2xx responses.
- `internal/spec/` — workflow-spec validator. Embeds `workflow-v0.schema.json` (mirrored from `docs/spec/`; the schema-sync diff in CI fails if the copies drift) and runs JSON Schema validation locally so users iterate on errors before opening a PR.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E6.1 (#55), E6.2 (#33), E6.3 (#34), E6.4 (#35), E6.5 (#36) shipped: scaffold + `run start`, `run status`, `run list`, `run cancel`, `run open`, `validate`.

## Subcommands

```
fishhawk run start  --repo R --workflow W --workflow-sha S [--trigger-ref REF]
fishhawk run status <run-id> [--output text|json]
fishhawk run list   [--repo R] [--workflow W] [--state S] [--limit N] [--cursor C]
fishhawk run cancel <run-id>
fishhawk run open   <run-id> [--print-url]
fishhawk validate   [path]                   # default: .fishhawk/workflows.yaml
fishhawk version
```

## Global flags

| Flag | Env | Default |
|---|---|---|
| `--backend-url` | `FISHHAWK_BACKEND_URL` | `http://localhost:8080` |
| `--token` | `FISHHAWK_TOKEN` | `""` (dev backends with stubbed auth) |
| `--timeout` | — | `60s` |

`--token` will become required once `/v0/tokens` lands; for now most backends accept anonymous calls via the `authStub` middleware.

## Build and test

From the repo root (workspace-aware):

    go build ./cli/...
    go test -race ./cli/...

Or from this directory directly:

    go build ./...
    go test ./...

## Local invocation

    # Start a run
    fishhawk run start \
      --backend-url http://localhost:8080 \
      --repo kuhlman-labs/fishhawk \
      --workflow feature_change \
      --workflow-sha $(git rev-parse HEAD)

    # Watch its state
    fishhawk run status <run-id>

    # Pipe a machine-readable Run into jq (handy for demo / status loops)
    fishhawk run status <run-id> --output json | jq .state

    # List recent runs
    fishhawk run list --state running --limit 25

## See also

- `docs/api/v0.openapi.yaml` — the contract this CLI consumes.
- `docs/api/v0.md` — human-readable companion.
- `docs/MVP_SPEC.md` §5.1.4 — CLI component definition.
