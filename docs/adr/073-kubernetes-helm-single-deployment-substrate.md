---
id: ADR-073
title: "Kubernetes and the Helm chart are the single deployment substrate; retire the unexercised ECS path"
status: accepted
date: 2026-07-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/2298
supersedes: []
superseded_by: []
applies_to: ["infra/terraform/**"]
---

# ADR-073: Kubernetes and the Helm chart are the single deployment substrate; retire the unexercised ECS path

## Context

Beta topology is settled as **hybrid** (2026-07-27): Fishhawk operates the hosted **Mode 2** multi-tenant service, and compliance-conscious partners self-host **Mode 1** in their own perimeter. Mode 1 is not a separate build — it is the same `fishhawkd` binary with `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` set, and the Helm chart already serves both modes plus local (`kubernetes.md`: *"The chart serves both ADR-057 deployment modes; only the values differ."*).

That leaves one question: **which substrate does Fishhawk's own hosted service run on?** Two prod-capable paths exist today.

### The two paths, and their actual state

| | Artifact | Status (verified 2026-07-27) |
|---|---|---|
| **AWS ECS Fargate** | `infra/terraform/` (alb, ecs, rds, dns, iam, secrets, logs, migrations, network, security) + `backend-deploy.yml` | **Never deployed.** `backend-deploy.yml` has **zero** runs. `infra-apply.yml` has run three times — 2026-05-27, 06-22, 07-14 — and **failed every time**. Terraform has never successfully applied. |
| **Kubernetes / Helm** | `deploy/helm/fishhawk/` (values, values-local, values-prod) | Exercised locally via `scripts/dev k8s` (ADR-034 M1 Docker-Desktop path). `values-prod.yaml` is a posture demonstration, explicitly *"Not copy-paste-deployable as-is."* |

**Container images are real on both paths.** `backend-build.yml` succeeds on every push, publishing cosign-signed `ghcr.io/kuhlman-labs/fishhawkd:main` and `:sha-<short>`; `backend-release.yml` cuts `:vX.Y.Z` at a tag. The image supply chain is not in question — only what runs it.

So there is **no migration and no sunk operational investment**. This is a choice between two unproven paths, and the ECS path has three failed applies behind it.

### The dogfooding argument is unusually strong for this product

`docs/METHODOLOGY.md` commits to *"Fishhawk is built using Fishhawk"* as a public, verifiable claim. Under the hybrid topology, **Mode 1 partners install the Helm chart**. If Fishhawk's own service runs a substrate no customer ever touches, the first real production test of the customer artifact is a partner's install — and the operational experience Fishhawk accumulates does not transfer to supporting them.

The asymmetry is already visible: the ECS path had a deploy workflow written for it and never ran; the chart has an explicitly non-deployable production values file. Neither is exercised in production, but only one of them is also what customers install.

### Portability

ADR-062 specifies N regional cells. Kubernetes travels across regions, clouds and on-prem; ECS does not leave AWS. A partner with a cloud or on-prem requirement forces this question anyway.

## Options

1. **ECS Fargate for hosted, Helm for customers.** The status quo on paper. Managed control plane (no cluster to patch or upgrade), and ECS's deployment circuit-breaker gives auto-rollback as a platform feature. **Rejected**: it institutionalises two production substrates, two runbooks and two sets of failure modes — with the customer-facing one being the less-exercised. Its practical advantage is also largely hypothetical here, since the path has never successfully deployed.

2. **Kubernetes + Helm as the single substrate.** One artifact for hosted cells, Mode 1 self-hosted, and local. Every chart defect hits Fishhawk before it hits a partner. Costs a managed-cluster bill and cluster lifecycle ownership, and requires rebuilding the progressive-delivery/auto-rollback behaviour ECS provides natively.

3. **Keep both, converge later.** Defers the decision at the cost of maintaining two paths through beta — exactly when attention is scarcest and when Mode 1 partners begin installing the chart. Rejected: "converge later" gets strictly more expensive once prod holds customer data.

## Recommendation

**Option 2 — Kubernetes and the Helm chart as the single deployment substrate**, for hosted cells, Mode 1 self-hosted, and local development.

The decisive facts are that there is nothing to migrate, and that one of the two candidates is independently required regardless — Mode 1 partners install the chart whether or not Fishhawk uses it. Choosing the chart makes the required artifact the exercised one; choosing ECS means maintaining a second substrate to avoid using the first.

**Retire the ECS path rather than leaving it in place.** `infra/terraform/`'s ECS-specific resources and `backend-deploy.yml` are non-working code that reads as authoritative infrastructure — a trap for a future reader, human or agent, and a standing invitation to drift. Some of the Terraform is substrate-independent (VPC, RDS, DNS, secrets) and should be kept or adapted; the ECS/ALB task-definition machinery should go.

**Bring the chart to genuine production posture as beta work.** `values-prod.yaml` being a non-deployable example is acceptable for a demonstration and not acceptable for the artifact a design partner installs.

### Open sub-decisions

- **Managed Kubernetes flavour** — EKS versus a cheaper-control-plane managed offering. This is mostly a cost and operational-comfort question, and it is the one where the founder's inputs (AWS spend, on-call comfort) matter more than the architecture.
- **Environment split** — today's `dev` / `prod` GitHub environments exist only as workflow configuration. What tiers actually get provisioned, and what gates each.
- **Progressive delivery / rollback** — what replaces ECS's deployment circuit-breaker. Helm rollback, a progressive-delivery controller, or accepting manual rollback for beta.
- **How much Terraform survives** — VPC/RDS/DNS/secrets are substrate-independent; ECS/ALB is not.

## Decision

**Accepted (2026-07-27).** Adopt **Option 2** — Kubernetes and the Helm chart are the single deployment substrate for hosted cells, Mode 1 self-hosted, and local development. Four sub-decisions were settled; where they refine the Recommendation, **these govern**.

### 1. EKS — stay on AWS

The surviving Terraform (VPC, RDS, DNS, secrets) is already AWS, RDS Postgres remains the store, and `infra/terraform/iam.tf` already models the GitHub Actions OIDC deploy role. Leaving AWS would mean rewriting all of that to save control-plane cost.

Accepted cost: roughly **$73/month per cluster control plane before nodes, per regional cell** (ADR-062). That is a real per-cell tax and it should inform how many cells beta actually needs — the answer for beta is very likely **one**.

### 2. Environment tiers: `dev` and `prod`

- **`dev`** — auto-deploys from `main` on every push. This is the **continuous exercise of the Helm chart** that the dogfooding argument depends on; without it, choosing Kubernetes buys the artifact-convergence benefit without the feedback that makes it worth having.
- **`prod`** — deploys from a `backend/v*` tag, gated by the `prod` GitHub environment's required-reviewers rule. Carries Fishhawk's own dogfood account and, later, hosted Mode 2 partners.

This matches the environment structure already written into `backend-deploy.yml` and `infra-apply.yml`, so the workflow topology survives even though the ECS implementation does not. Two clusters or two namespaces on one cluster is an implementation choice; the cost note above argues for namespaces at beta scale.

A partner-shaped `staging` tier was considered and rejected for now: a third environment to provision, pay for and keep in sync, at pre-revenue scale. Revisit when prod carries partner data and a bad release becomes a customer-facing incident rather than a self-inflicted one.

### 3. Rollback: `helm upgrade --atomic --wait`, documented and drilled

`--atomic` rolls back automatically when readiness gates fail, covering what ECS's deployment circuit-breaker would have covered, with **no new components** in a cluster nobody has yet operated in production. A progressive-delivery controller (Argo Rollouts / Flagger) is strictly more capable and is deferred — revisit when there is production traffic worth canarying.

**The drill is not optional and is not the same as the automation.** Before partners, exercise a real rollback including the case `--atomic` does not solve: **a rolled-back image running against a forward-migrated database.** That is the classic failure and the one a readiness gate cannot catch.

### 4. Remove the ECS path; keep the substrate-independent Terraform

**Delete** `ecs.tf`, `alb.tf`, `migrations.tf`, and `.github/workflows/backend-deploy.yml`. **Keep and adapt** `network.tf`, `rds.tf`, `dns.tf`, `secrets.tf`, `iam.tf`.

Leaving it deprecated-but-present was rejected: non-working infrastructure that reads as authoritative is a trap for the next reader, human or agent — and `infra-apply.yml` still triggers on `infra/terraform/**` pushes, where it has already failed three times (2026-05-27, 06-22, 07-14). A dead path that runs on every infra change is worse than no path.

**Unchanged:** the image supply chain. `backend-build.yml` and `backend-release.yml` publish cosign-signed images to ghcr and are substrate-independent. They stay exactly as they are.

Named approver: repository maintainer (human).

## Consequences

One deployment artifact serves hosted cells, self-hosted partners and local development, so chart defects surface in Fishhawk's own operations before they reach a partner — which makes the METHODOLOGY dogfooding commitment true of deployment, not only of code changes. Support quality improves for Mode 1 partners because Fishhawk runs what they run. Regional cells (ADR-062) become portable across clouds and on-prem.

Costs and new work:

- **Cluster lifecycle becomes Fishhawk's responsibility** — provisioning, version upgrades, capacity. Fargate would have absorbed this.
- **Auto-rollback must be rebuilt.** ECS's deployment circuit-breaker is a platform feature; on Kubernetes it is Helm rollback, a progressive-delivery controller, or a manual runbook.
- **The chart must reach production posture**, including a real secrets story, ingress/TLS, and a migration hook proven under failure — not only the happy path `scripts/dev k8s` exercises.
- **Removing the ECS path is itself work**, and touches `.github/workflows/**` and `infra/terraform/**`, both of which are human-led surfaces.
- Nothing changes for the image supply chain: `backend-build.yml` / `backend-release.yml` and the cosign signing posture are substrate-independent and stay as they are.
