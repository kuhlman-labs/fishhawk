# fishhawk

The governed, auditable workflow for agent-driven software development.

Agents do the work. Your team approves the work. Fishhawk holds the record.

Fishhawk is an opinionated workflow engine for agent-driven software changes: it defines the stages a change moves through (plan → implement → review), enforces policy on what an agent can and cannot do, gates the work behind human approvals, and keeps an immutable, signed audit trail of every plan, approval, and outcome. It is tool-agnostic and agent-agnostic — it is **not** a coding agent, a CI/CD platform, or a general-purpose workflow engine.

Fishhawk develops itself through Fishhawk: since Day 22 of the v0 build (2026-05-21), substantive changes flow through a workflow run defined by [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml), and the audit log behind that development is published in [`docs/compliance/`](docs/compliance/).

> **Status: pre-alpha.** The v0 control plane, runner, CLI, MCP server, and Web UI have landed. See [Documentation](#documentation) for the full component map. Feature PRs are not yet being accepted while the v0 abstractions settle — see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Quickstart

Local setup brings up the backend against a Postgres + MinIO stack and validates a workflow.

**Prerequisites:** [Go 1.25+](https://go.dev/dl/), [Docker](https://www.docker.com/), and — only for the Web UI — [Node 22+](https://nodejs.org/) with [pnpm 10+](https://pnpm.io/).

The [`Makefile`](Makefile) wraps the common loops (`make help` lists every target):

```sh
cp .env.example .env        # optional: populate later for GitHub App / OAuth / trace storage
make up                     # docker compose: Postgres :5432, MinIO :9000/:9001
make migrate                # apply backend migrations
make dev-backend            # run fishhawkd on :8080 — http://localhost:8080/healthz
make validate               # validate .fishhawk/workflows.yaml with the CLI
```

The Web UI (plan review, approvals, per-run audit log) is optional:

```sh
make dev-frontend           # in another terminal: http://localhost:5173 (proxies /v0)
```

Without a GitHub App configured, the OAuth and webhook endpoints respond 503 — runs, plans, and the audit log still work. Trace storage is opt-in: run `make minio-init` once, then uncomment the trace-storage block in `.env`. To wire up sign-in and GitHub events, see [`docs/github-app/README.md`](docs/github-app/README.md).

### A sample workflow

Fishhawk reads `.fishhawk/workflows.yaml` from the repository it governs. `fishhawk init` scaffolds one from an autonomy preset (`--preset low|medium|high`); the trimmed `feature_change` workflow below shows the plan → implement → review shape most work uses, and validates as-is with `fishhawk validate`:

```yaml
version: "1.0"

workflows:
  feature_change:
    description: >-
      Default workflow for feature work. Human approves the plan and the
      PR; the agent does the implementation.
    drive: true                    # fishhawkd auto-advances mechanical transitions
    on_ci_failure:
      max_retries: 1

    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:                 # advisory agent review before the human gate
          agents:
            - provider: claudecode
              model: claude-opus-4-8
          human: 1
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval         # one approval, not the author or an agent
            approvals:
              count: 1
              not: [author, agent]

      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:                  # run your test entrypoint before the PR opens
            command: "make test"
            timeout: "15m"
            max_iterations: 1
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
        constraints:               # enforced on the implementation, not advisory
          - max_files_changed: 45
          - forbidden_paths: [".github/workflows/**", ".fishhawk/**"]
          - required_outcomes: [tests_added_or_updated, ci_green]

      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
```

The full preset adds budgets, a second heterogeneous reviewer, and runtime policy; this repository's own [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml) is the fuller real-world reference. The schema and field reference live in [`docs/spec/`](docs/spec/).

## Connect the MCP server

The MCP server (`fishhawk-mcp`) exposes run, plan, and audit state — and the local-runner drive loop — to any MCP client over the Model Context Protocol. Connecting it to your CLI lets an agent start runs, read plans, and drive stages without leaving the chat.

**1. Build the binaries** (co-locate them so the MCP server auto-resolves the runner):

```sh
go build -o bin/fishhawk-mcp ./backend/cmd/fishhawk-mcp
go build -o bin/fishhawk-runner ./runner/cmd/fishhawk-runner
```

**2. Issue an operator token** against your local backend and export it:

```sh
go run ./backend/cmd/fishhawkd token issue \
  --subject <your-login> \
  --scopes read:runs,read:audit,write:runs,write:approvals,write:stages
export FISHHAWK_API_TOKEN="<the fhk_ token printed above>"
export FISHHAWK_BACKEND_URL="http://localhost:8080"   # default; override for hosted
```

`FISHHAWK_API_TOKEN` is required — every tool round-trips the API. There is no anonymous mode.

**3a. Register with Claude Code:**

```sh
claude mcp add fishhawk \
  --env FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN \
  --env FISHHAWK_BACKEND_URL=$FISHHAWK_BACKEND_URL \
  -- "$(pwd)/bin/fishhawk-mcp"
```

Verify with `claude mcp list`, then ask the agent for the status of a run — it should call `fishhawk_get_run_status`. The exact flag shape varies by version; `claude mcp add --help` is authoritative.

**3b. Register with the Codex CLI** — add a `[mcp_servers.fishhawk]` block to `~/.codex/config.toml`:

```toml
[mcp_servers.fishhawk]
command = "/absolute/path/to/bin/fishhawk-mcp"
env = { FISHHAWK_API_TOKEN = "fhk_...", FISHHAWK_BACKEND_URL = "http://localhost:8080" }
```

Recent Codex CLI versions also accept `codex mcp add fishhawk -- /absolute/path/to/bin/fishhawk-mcp` (see `codex mcp --help`). Confirm with `codex mcp list`.

> **Tip — surviving rebuilds:** Claude Code does not reconnect a restarted stdio MCP server, so rebuilding `fishhawk-mcp` under a live session needs a manual `/mcp`. Register the [`fishhawk-mcp-shim`](backend/cmd/fishhawk-mcp-shim/README.md) supervisor instead — it hot-swaps the rebuilt child with no reconnect.

The full install path (pre-built release binaries, cosign verification, the complete tool surface, and troubleshooting) lives in [`docs/mcp/install.md`](docs/mcp/install.md).

## Documentation

| Doc | What |
|---|---|
| [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) | v0 scope and framing — what Fishhawk is and is not. |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Stack, run/stage lifecycle, storage, invariants. Read before designing anything cross-component. |
| [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md) | Autonomy tiers (low/medium/high) and the operator contract. |
| [`docs/spec/`](docs/spec/) | JSON Schemas + reference for the workflow spec and the plan artifact. |
| [`docs/mcp/install.md`](docs/mcp/install.md) | MCP server install, tool surface, and troubleshooting. |
| [`docs/api/v0.md`](docs/api/v0.md) | REST API surface (`docs/api/v0.openapi.yaml` is the source of truth). |
| [`docs/deploy/kubernetes.md`](docs/deploy/kubernetes.md) | Helm-chart quickstart for a Kubernetes deployment. |
| [`docs/BRAND_FOUNDATIONS.md`](docs/BRAND_FOUNDATIONS.md) | Voice, naming, positioning. |
| [`AGENTS.md`](AGENTS.md) | Build/test/lint gates and the contributor workflow loop. |

### Components

| Component | README |
|---|---|
| Backend control plane (`fishhawkd`) | [`backend/README.md`](backend/README.md) — REST API, state machine, audit log, policy, approvals. |
| Runner action (`fishhawk/runner`) | [`runner/README.md`](runner/README.md) — runs the agent on CI, captures the signed trace. |
| CLI (`fishhawk`) | [`cli/README.md`](cli/README.md) — `validate`, `run`, `plan`, `audit`, `export`, `doctor`, `token`. |
| MCP server (`fishhawk-mcp`) | [`backend/cmd/fishhawk-mcp/README.md`](backend/cmd/fishhawk-mcp/README.md) — run/plan/audit state over MCP. |
| Web UI | [`frontend/README.md`](frontend/README.md) — plan review, approval, per-run audit log. |
| Audit-log verifier | [`verifier/README.md`](verifier/README.md) — re-verify an exported chain offline. |
| Infrastructure | [`infra/terraform/README.md`](infra/terraform/README.md) — Terraform on AWS; Helm chart under [`deploy/helm/`](deploy/helm/fishhawk). |

## Following along

Watch the repository. Substantive changes land as PRs against `main`, each opened by Fishhawk and stamped with its workflow run and stage IDs.

## Contributing

The project is in pre-alpha. Issues and discussion are welcome. Feature PRs are not yet being accepted while the v0 abstractions are still being fixed in place. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Security

Please report vulnerabilities responsibly. See [`SECURITY.md`](SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
