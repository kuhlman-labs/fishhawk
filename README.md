# fishhawk

The governed, auditable workflow for agent-driven software development.

Agents do the work. Your team approves the work. Fishhawk holds the record.

## Status

Pre-alpha. The v0 build has largely landed, and Fishhawk now develops itself through Fishhawk:

- **Backend control plane (`fishhawkd`)** — REST API, run/stage state machine on Postgres, signed audit log, policy evaluator, approval gating with SLA timeouts, retry semantics, GitHub App webhook receiver. ([`backend/`](backend/README.md))
- **Runner action (`fishhawk/runner`)** — runs the agent (Claude Code or Codex) on the customer's CI, captures the signed trace, and validates the plan against its schema. Releases ship as `kuhlman-labs/fishhawk/runner@runner/vX.Y.Z` through a pipeline that cosign-signs each release and attaches an SBOM; the first public tag has not been cut yet. ([`runner/`](runner/README.md))
- **CLI (`fishhawk`)** — `validate`, `run` (start/status/open), `plan`, `audit`, `export`, `doctor`, `token`, `deploy`, `campaign`, among others; the component README documents the full command set. ([`cli/`](cli/README.md))
- **MCP server (`fishhawk-mcp`)** — exposes run, plan, and audit state to Claude Code (and any MCP client) over the Model Context Protocol; the surface self-hosted runs are driven through. ([`backend/cmd/fishhawk-mcp/`](backend/cmd/fishhawk-mcp))
- **Web UI** — plan review, approval, audit log per run, retry on failures. ([`frontend/`](frontend/README.md))
- **Audit-log verifier** — standalone binary that re-verifies an exported chain offline. ([`verifier/`](verifier/README.md))
- **Hosted infrastructure** — Terraform on AWS (VPC, RDS, ECS Fargate, ALB, OIDC-based deploys; dev ~$15/mo and prod ~$85/mo profiles, [`infra/terraform/`](infra/terraform/README.md)) plus a Helm chart for Kubernetes ([`deploy/helm/fishhawk/`](deploy/helm/fishhawk), quickstart in [`docs/deploy/kubernetes.md`](docs/deploy/kubernetes.md)).
- **CI/CD** — every push to `main` that touches backend code builds + signs the backend image; tagged releases are wired to auto-deploy via GitHub Actions OIDC.

Fishhawk now ships its *own* changes through Fishhawk. Since Day 22 of the v0 build (2026-05-21), substantive changes flow through a workflow run defined by [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml): a human approves the plan, constraints are enforced on the implementation, and the PR is opened by Fishhawk itself — stamped with its run and stage IDs (the *"Opened by Fishhawk for run …"* footer on recent PRs). This is the methodology commitment in [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md), today held by convention rather than enforced by the product. The audit log behind Fishhawk's own development is published as a public artifact in [`docs/compliance/`](docs/compliance/) — a machine-verifiable export plus a human-readable agent-changes report, both re-verifiable offline with the standalone `fishhawk-verify` binary.

For the canonical scope and the technical realization, see [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) and [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## What Fishhawk is

An opinionated workflow engine for agent-driven software changes, a policy enforcement layer for what agents can and cannot do, and an immutable audit trail of agent activity, plans, approvals, and outcomes. Tool-agnostic, agent-agnostic, opinionated about process.

## What Fishhawk is not

A coding agent. A project management tool. A CI/CD platform. A general-purpose workflow engine. See [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) §1 for the full framing.

## Running locally

Prerequisites:

- [Go 1.25+](https://go.dev/dl/) (the workspace targets `~> 1.25`)
- [Node 22+](https://nodejs.org/) and [pnpm 10+](https://pnpm.io/) (for the Web UI)
- [Docker](https://www.docker.com/) (for the local Postgres + MinIO stack)
- Optional: [`golangci-lint v2`](https://golangci-lint.run/) for linting; `actionlint` for workflow files

The repository ships a [`Makefile`](Makefile) that wraps the common loops. Run `make help` to see every target. The quickstart:

```sh
cp .env.example .env        # populate later for GitHub App / OAuth / trace storage (see below)
make up                     # docker compose: Postgres :5432, MinIO :9000/:9001
make minio-init             # one-time per fresh stack: create the fishhawk-traces bucket
make migrate                # apply backend migrations
make dev-backend            # run fishhawkd on :8080
make dev-frontend           # in another terminal: Web UI on :5173 (proxies /v0)
make validate               # validate .fishhawk/workflows.yaml with the CLI
make test                   # all Go modules (-race) + Web UI vitest
make lint                   # golangci-lint v2 + eslint + tsc --noEmit
make coverage               # reproduce the CI 80% gate
```

The Makefile auto-loads `.env` if present, so credentials and overrides flow into `make dev-backend` without manual `source` plumbing.

Trace storage is opt-in: after `make minio-init`, uncomment the trace-storage block in `.env` (`FISHHAWKD_S3_BUCKET` and friends) so `fishhawkd` can reach MinIO. Without it, `/v0/runs/{id}/trace` responds 503 — everything else works.

Contributors driving Fishhawk through its own workflow loop use `scripts/dev` instead (see [`AGENTS.md`](AGENTS.md)); the Makefile is the plain local path.

If you'd rather run things by hand, the Makefile targets are thin wrappers over these commands:

```sh
docker compose up -d
export FISHHAWKD_DATABASE_URL='postgres://fishhawk:fishhawk@localhost:5432/fishhawk?sslmode=disable'
go run ./backend/cmd/fishhawkd migrate up
go run ./backend/cmd/fishhawkd serve   # http://localhost:8080/healthz

cd frontend && pnpm install && pnpm dev   # http://localhost:5173

go run ./cli/cmd/fishhawk validate ./.fishhawk/workflows.yaml
```

A plain `go test ./...` from the root won't work — the repo is a multi-module Go workspace. The Makefile's `test-go` target loops over every module in `go.work`; the equivalent loop:

```sh
for m in $(go work edit -json | jq -r '.Use[].DiskPath'); do
  (cd "$m" && go test -race ./...)
done
```

Without a GitHub App configured, the backend logs warnings on startup and the OAuth + webhook endpoints respond 503 — runs, plans, and the audit log still work. To wire up Web UI sign-in and GitHub events, see the **Local development** section in [`docs/github-app/README.md`](docs/github-app/README.md).

Per-component details live in each subdirectory's README:

| Component | README |
|---|---|
| Backend (`fishhawkd`) | [`backend/README.md`](backend/README.md) — `serve`, `migrate`, `token issue`, env-var reference |
| Web UI | [`frontend/README.md`](frontend/README.md) — `pnpm dev`, route layout, OAuth wiring |
| Runner action | [`runner/README.md`](runner/README.md) — used in GitHub Actions, not typically run locally |
| CLI | [`cli/README.md`](cli/README.md) — `fishhawk validate`, `run`, `plan`, `audit`, `export`, `doctor` |
| Verifier | [`verifier/README.md`](verifier/README.md) — `fishhawk-verify` against an exported audit log |
| Infrastructure | [`infra/terraform/README.md`](infra/terraform/README.md) — bootstrap, dev vs prod profiles, CI deploy flow |

## Defining a workflow

Fishhawk reads `.fishhawk/workflows.yaml` from the repository it governs. `fishhawk init` scaffolds one from an autonomy preset (`--preset low|medium|high`, default `medium` — see [`docs/spec/workflow-preset.md`](docs/spec/workflow-preset.md)), and `fishhawk validate` checks it against the workflow schema. A trimmed version of the medium preset, showing the plan → implement → review shape most work uses:

```yaml
version: "1.0"

workflows:
  feature_change:
    description: >-
      Default workflow for feature work. Human approves the plan and the
      PR; the agent does the implementation.
    drive: true                    # fishhawkd auto-advances mechanical transitions
    operator_agent:                # what the operator agent may decide alone
      may_approve: clean_dual_approval
      may_route_fixup: convergent_concerns
      may_retry: infra_flake
      must_page_human: [reviewer_reject, plan_rejection, scope_amendment]
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

This document validates as-is (`fishhawk validate`). The full preset adds budgets, a second heterogeneous reviewer, and runtime policy; this repository's own [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml) is the fuller real-world reference, including an acceptance stage and a delegating deploy workflow. The schema and field reference live in [`docs/spec/`](docs/spec/).

### The operator contract

A run is a negotiation between two parties: agents propose (a plan, a diff), and an **operator** decides at each gate — approve or reject the plan, route review concerns back as a fix-up, approve and merge the PR. The `operator_agent` block above delegates the mechanical share of that role to an operator agent under named conditions: approve on a clean dual approval, route convergent review concerns to a fix-up, retry an infrastructure flake. Everything listed in `must_page_human` — and merging — stays with a person.

The operator's behavioral contract (gate procedures, escalation posture, prohibitions) ships with the product as `operator-role-v0` — see [`docs/spec/operator-role.md`](docs/spec/operator-role.md) and the shipped default [`docs/spec/operator-role-default.yaml`](docs/spec/operator-role-default.yaml). A repository does not write its own role spec; it may add a thin overlay at `.fishhawk/operator.yaml` for local conventions (merge ritual specifics, escalation contacts). Authority stays in the workflow's `operator_agent` knobs.

## Following along

Watch the repository. Substantive changes land as PRs against `main`, each opened by Fishhawk and stamped with its workflow run and stage IDs.

## Contributing

The project is in pre-alpha. Issues and discussion are welcome. Feature PRs are not yet being accepted while the v0 abstractions are still being fixed in place. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Security

Please report vulnerabilities responsibly. See [`SECURITY.md`](SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
