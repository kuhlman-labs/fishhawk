# fishhawk/runner

The GitHub Action that runs an agent under a Fishhawk workflow stage and ships the signed trace bundle back to the backend. Customers reference the action as:

    uses: kuhlman-labs/fishhawk/runner@v0.1

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/runner`) so it can be tagged independently of the backend and the CLI — the customer-facing version pin is on the runner alone. See [ADR-014 (#78)](https://github.com/kuhlman-labs/fishhawk/issues/78) for the multi-module rationale.

## Layout

- `action.yml` — composite action manifest. Defines inputs, sets up the Go toolchain, invokes the binary.
- `cmd/fishhawk-runner/` — the binary entrypoint. Flag parsing in `flags.go`, dispatch in `main.go`.
- `internal/agent/` — the agent abstraction (`Invoker`, `Invocation`, `Result`, `Event`).
- `internal/agent/claudecode/` — adapter for Anthropic's Claude Code CLI.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E5.1 (#52) shipped the scaffold. E5.2 (#29) wires the Claude Code invocation harness: when `prompt-file` is supplied, the runner invokes Claude Code with `--print --output-format stream-json`, captures each event, and emits the trace as JSON Lines on stdout. Trace bundling and signed upload land later:

- E5.3 (#30) — full trace capture + bundling (replaces stdout JSONL with `*.jsonl.gz` + manifest/trailer)
- E5.4 (#31) — plan validation against `standard_v1` (reuses `backend/internal/plan`)
- E5.5 (#53) — post-hoc constraint enforcement on stage output
- E5.6 (#32) — signed trace shipping to backend (uses `backend/internal/signing` + `backend/internal/tracestore`)
- E5.7 (#54) — versioned, signed releases of `fishhawk/runner` with SBOM

## Inputs (action.yml)

| Input | Required | Description |
|---|---|---|
| `run-id` | yes | Workflow run identifier (UUID, supplied by backend dispatch). |
| `backend-url` | yes | Fishhawk backend URL the runner ships its trace bundle to. |
| `workflow` | yes | Workflow ID matching a key under `workflows:` in `.fishhawk/workflows.yaml`. |
| `stage` | yes | Stage ID within the workflow (e.g. `plan`, `implement`, `review`). |
| `prompt-file` | no | Path to a file containing the constructed prompt. When unset the runner exits 0 without invoking the agent — useful for exercising the dispatch path before E5.2+ are wired upstream. |
| `working-dir` | no | Agent working directory; defaults to the runner's CWD. |
| `max-tokens` | no | Hard cap on agent tokens (input + output); 0 means no cap. |
| `timeout` | no | Wall-clock cap on the agent invocation, e.g. `15m`. Default 15m. |

The Claude Code API key is supplied via the `ANTHROPIC_API_KEY` environment variable, which customers populate from their GitHub Secrets. v0.x will replace this with a Fishhawk-issued ephemeral key (MVP_SPEC §5.3).

## Build and test

From the repo root (workspace-aware):

    go build ./runner/...
    go test -race ./runner/...

Or from this directory directly:

    go build ./...
    go test ./...

## Local invocation

The same binary the action runs can be invoked locally for development:

    # Dispatch-path probe (no agent invocation)
    go run ./cmd/fishhawk-runner \
      --run-id 11111111-2222-3333-4444-555555555555 \
      --backend-url http://localhost:8080 \
      --workflow feature_change \
      --stage plan

    # With the Claude Code harness (E5.2+)
    echo "Summarize the README" > /tmp/prompt.txt
    ANTHROPIC_API_KEY=sk-... go run ./cmd/fishhawk-runner \
      --run-id 11111111-2222-3333-4444-555555555555 \
      --backend-url http://localhost:8080 \
      --workflow feature_change \
      --stage plan \
      --prompt-file /tmp/prompt.txt \
      --max-tokens 50000 \
      --timeout 5m

When `--prompt-file` is set the runner emits one JSON event per stdout line; the structured runner log lines (`runner_started`, `runner_completed`) go to stderr.

## See also

- `docs/MVP_SPEC.md` §5.1.2 — runner component definition.
- `docs/MVP_SPEC.md` §5.3 — trust model (signing, supply-chain, ephemeral keys).
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle, where the runner sits in the dispatch flow.
