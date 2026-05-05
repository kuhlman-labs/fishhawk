# fishhawk

The workflow and governance layer for agent-driven software development.

Agents do the work. Your team approves the work. Fishhawk holds the record.

## Status

Pre-alpha, but the v0 build is largely landed:

- **Backend control plane (`fishhawkd`)** — REST API, run/stage state machine on Postgres, signed audit log, policy evaluator, approval gating with SLA timeouts, retry semantics, GitHub App webhook receiver. ([`backend/`](backend/README.md))
- **Runner action (`fishhawk/runner`)** — published as `kuhlman-labs/fishhawk/runner@runner/vX.Y.Z`. Cosign-signed releases with SBOMs. ([`runner/`](runner/README.md))
- **CLI (`fishhawk`)** — `validate`, `run start`, `run status`, `run open`. ([`cli/`](cli/README.md))
- **Web UI** — plan review, approval, audit log per run, retry on failures. ([`frontend/`](frontend/README.md))
- **Audit-log verifier** — standalone binary that re-verifies an exported chain offline. ([`verifier/`](verifier/README.md))
- **Hosted infrastructure (Terraform)** — VPC, RDS, ECS Fargate, ALB, OIDC-based deploys. Two cost profiles: dev (~$15/mo, no NAT/no ALB) and prod (~$85/mo, full HA-eligible). ([`infra/terraform/`](infra/terraform/README.md))
- **CI/CD** — every push to `main` builds + signs the backend image; tagged releases auto-deploy via GitHub Actions OIDC.

What's still ahead is the methodology commitment in [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md): on Day 21 of the v0 build (~2026-05-20), Fishhawk starts shipping its *own* changes through Fishhawk. Until then, the workflow spec at [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml) is a public commitment, not yet a running system. Day 21 is the gating event after which every PR carries a link to its workflow run and audit log.

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
make up                     # docker compose: Postgres :5432, MinIO :9000/:9001
make migrate                # apply backend migrations
make dev-backend            # run fishhawkd on :8080
make dev-frontend           # in another terminal: Web UI on :5173 (proxies /v0)
make validate               # validate .fishhawk/workflows.yaml with the CLI
make test                   # all Go modules (-race) + Web UI vitest
make lint                   # golangci-lint v2 + eslint + tsc --noEmit
make coverage               # reproduce the CI 80% gate
```

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

Per-component details live in each subdirectory's README:

| Component | README |
|---|---|
| Backend (`fishhawkd`) | [`backend/README.md`](backend/README.md) — `serve`, `migrate`, `token issue`, env-var reference |
| Web UI | [`frontend/README.md`](frontend/README.md) — `pnpm dev`, route layout, OAuth wiring |
| Runner action | [`runner/README.md`](runner/README.md) — used in GitHub Actions, not typically run locally |
| CLI | [`cli/README.md`](cli/README.md) — `fishhawk validate`, `fishhawk run start/status/open` |
| Verifier | [`verifier/README.md`](verifier/README.md) — `fishhawk-verify` against an exported audit log |
| Infrastructure | [`infra/terraform/README.md`](infra/terraform/README.md) — bootstrap, dev vs prod profiles, CI deploy flow |

## Following along

Watch the repository. Substantive changes land as PRs against `main`; once Fishhawk is self-hosting, every PR will carry a link to its workflow run and audit log.

## Contributing

The project is in pre-alpha. Issues and discussion are welcome. Feature PRs are not yet being accepted while the v0 abstractions are still being fixed in place. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Security

Please report vulnerabilities responsibly. See [`SECURITY.md`](SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

---

Built in Lithia, Florida.
