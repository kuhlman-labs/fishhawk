# fishhawk CLI

Command-line interface for the Fishhawk control plane. Wraps the HTTP API documented in `docs/api/v0.openapi.yaml` so users can drive runs from a terminal.

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/cli`) so it can be released independently of the backend and runner. Per ADR-014 (#78), the multi-module workspace lets each component carry its own version tag.

## Layout

- `cmd/fishhawk/` — the binary entrypoint. Subcommand dispatch in `main.go`, per-command flags in `run.go`, validate logic in `validate.go`.
- `internal/httpclient/` — typed wrapper around the backend API. Marshals `CreateRunInput`, decodes `Run`, surfaces `*APIError` for non-2xx responses.
- `internal/spec/` — workflow-spec validator. Embeds `workflow-v0.schema.json` (mirrored from `docs/spec/`; the schema-sync diff in CI fails if the copies drift) and runs JSON Schema validation locally so users iterate on errors before opening a PR.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E6.1 (#55), E6.2 (#33), E6.3 (#34), E6.4 (#35), E6.5 (#36) shipped: scaffold + `run start`, `run status`, `run list`, `run cancel`, `run open`, `validate`. E18.1 (#332), E18.2 (#333), E18.3 (#334), E18.4 (#335), E18.5 (#336) added `plan approve`, `plan reject`, `run retry`, `audit list`, `audit tail`.

## Subcommands

```
fishhawk run start    --repo R --workflow W --workflow-sha S [--trigger-ref REF]
fishhawk run status   <run-id> [--output text|json]
fishhawk run list     [--repo R] [--workflow W] [--state S] [--limit N] [--cursor C]
fishhawk run cancel   <run-id>
fishhawk run open     <run-id> [--print-url]
fishhawk run retry    <stage-id> [--output text|json]
fishhawk plan approve <run-id> [--reason R] [--output text|json]
fishhawk plan reject  <run-id> [--reason R] [--output text|json]
fishhawk audit list   <run-id> [--category C] [--stage UUID] [--limit N] [--cursor X] [--output text|json]
fishhawk audit tail   <run-id> [--interval D] [--output text|json] [--max-polls N]
fishhawk diagnose     <run-id> [--output text|json]
fishhawk validate     [path]                   # default: .fishhawk/workflows.yaml
fishhawk version
```

`diagnose` prints a run's **product-facts-only** diagnostic bundle (`GET /v0/runs/{id}/diagnostics`): run id, stage states, the failing stage's category + audit surface, audit sequence range, build versions + git SHAs, workflow spec hash, and runner kind. It is pure read — the bundle carries no diffs, paths, prompts, or free text, so it is safe to attach to an upstream Fishhawk product report.

`run retry` takes a **stage** id, not a run id — retry is stage-scoped per the state machine. Pick the failed stage from `fishhawk run status <run-id> --output json` (`.stages[].id`).

`audit list` outputs NDJSON (one entry per line) when `--output json` is set so a long page can be piped through `head`/`tail` without breaking the parser.

`audit tail` polls the audit endpoint on a configurable interval (default 2s, minimum 500ms) and prints new entries as they land. It exits cleanly on Ctrl-C. There's no server-side SSE today — if streaming demand grows we'd add one and migrate the client.

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

    # Approve the plan stage on a run from the terminal (ADR-019 / #320)
    fishhawk plan approve <run-id> --reason "scope looks right"

    # Reject — recording a reason is encouraged but not required
    fishhawk plan reject <run-id> --reason "scope too wide; split the migration"

    # Inspect the audit log without leaving the terminal
    fishhawk audit list <run-id>
    fishhawk audit list <run-id> --category approval_submitted --output json | jq .

    # Follow a run's audit log in a side terminal
    fishhawk audit tail <run-id> --interval 1s

## See also

- `docs/api/v0.openapi.yaml` — the contract this CLI consumes.
- `docs/api/v0.md` — human-readable companion.
- `docs/MVP_SPEC.md` §5.1.4 — CLI component definition.
