# fishhawk

The workflow and governance layer for agent-driven software development.

Agents do the work. Your team approves the work. Fishhawk holds the record.

## Status

Very early. This repository is public from day one as a deliberate commitment to the methodology described in [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md): Fishhawk is built using Fishhawk, and the record of how it gets built is open.

There is no working software here yet. The repository currently contains:

- The v0 specification: [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md)
- The brand foundations: [`docs/BRAND_FOUNDATIONS.md`](docs/BRAND_FOUNDATIONS.md)
- The methodology Fishhawk holds itself to: [`docs/METHODOLOGY.md`](docs/METHODOLOGY.md)
- The (placeholder) workflow spec for Fishhawk's own development: [`.fishhawk/workflows.yaml`](.fishhawk/workflows.yaml)

The 90-day plan in [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) targets day 21 as the milestone where Fishhawk begins shipping its own changes through Fishhawk. Until then, the workflow spec is a public commitment, not a running system.

## What Fishhawk is

An opinionated workflow engine for agent-driven software changes, a policy enforcement layer for what agents can and cannot do, and an immutable audit trail of agent activity, plans, approvals, and outcomes. Tool-agnostic, agent-agnostic, opinionated about process.

## What Fishhawk is not

A coding agent. A project management tool. A CI/CD platform. A general-purpose workflow engine. See [`docs/MVP_SPEC.md`](docs/MVP_SPEC.md) §1 for the full framing.

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
