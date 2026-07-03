# fishhawk

The governed, auditable workflow for agent-driven software development.

Agents do the work. Your team approves the work. Fishhawk holds the record.

## Status

Pre-alpha. The v0 build has largely landed, and Fishhawk now develops itself through Fishhawk:

- **Backend control plane (`fishhawkd`)** — REST API, run/stage state machine on Postgres, signed audit log, policy evaluator, approval gating with SLA timeouts, retry semantics, GitHub App webhook receiver. ([`backend/`](backend/README.md))
- **Runner action (`fishhawk/runner`)** — runs the agent (Claude Code or Codex) on the customer's CI, captures the signed trace, and validates the plan against its schema. Published as `kuhlman-labs/fishhawk/runner@runner/vX.Y.Z`; cosign-signed releases with SBOMs. ([`runner/`](runner/README.md))
- **CLI (`fishhawk`)** — `validate`, `run` (start/status/open), `plan`, `audit`, `export`, `doctor`. ([`cli/`](cli/README.md))
- **MCP server (`fishhawk-mcp`)** — exposes run, plan, and audit state to Claude Code (and any MCP client) over the Model Context Protocol; the surface self-hosted runs are driven through. ([`backend/cmd/fishhawk-mcp/`](backend/cmd/fishhawk-mcp))
- **Web UI** — plan review, approval, audit log per run, retry on failures. ([`frontend/`](frontend/README.md))
- **Audit-log verifier** — standalone binary that re-verifies an exported chain offline. ([`verifier/`](verifier/README.md))
- **Hosted infrastructure** — Terraform on AWS (VPC, RDS, ECS Fargate, ALB, OIDC-based deploys; dev ~$15/mo and prod ~$85/mo profiles, [`infra/terraform/`](infra/terraform/README.md)) plus a Helm chart for Kubernetes ([`deploy/helm/fishhawk/`](deploy/helm/fishhawk), quickstart in [`docs/deploy/kubernetes.md`](docs/deploy/kubernetes.md)).
- **CI/CD** — every push to `main` builds + signs the backend image; tagged releases auto-deploy via GitHub Actions OIDC.

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
cp .env.example .env        # populate later for GitHub App / OAuth (see below)
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

## Following along

Watch the repository. Substantive changes land as PRs against `main`, each opened by Fishhawk and stamped with its workflow run and stage IDs.

## Contributing

The project is in pre-alpha. Issues and discussion are welcome. Feature PRs are not yet being accepted while the v0 abstractions are still being fixed in place. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Security

Please report vulnerabilities responsibly. See [`SECURITY.md`](SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
