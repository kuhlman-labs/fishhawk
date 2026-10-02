---
id: ADR-001
title: "Cloud target (AWS / GCP / Other)"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/65
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-001: Cloud target (AWS / GCP / Other)

## Context

The v0 spec (MVP_SPEC §10 #2) defers the cloud target. Fishhawk is hosted-only in v0 (§9). Choice between AWS, GCP, or another provider drives later infra decisions: managed Postgres (RDS / Cloud SQL), object storage (S3 / GCS), container runtime (ECS / Fargate / Cloud Run / GKE), secrets, observability.

## Options

- **AWS** — most mature primitives, broadest ecosystem, RDS+S3+ECS/Fargate well-trodden. More operator burden than GCP.
- **GCP** — Cloud Run is the cleanest container target in the industry, GCS solid, Cloud SQL is fine. Smaller ecosystem; some enterprise procurement friction.
- **Hetzner / Fly / Railway / smaller provider** — cheaper, simpler ops at v0 scale; risk to the audit-grade "we know where data lives" story for compliance-conscious customers.

## Recommendation

Selected based on enterprise-compliance positioning and ecosystem maturity.

## Decision

**Recorded 2026-04-30: AWS.**

Primary services:

- **RDS Postgres** for queryable audit/run/stage metadata (per E2.1 / #22 schema design)
- **S3** for trace bundle storage with content-addressing (ADR-003)
- **ECS Fargate** for the `fishhawkd` container runtime (ADR-009)
- **AWS Secrets Manager** for GitHub App private key, signing-key root, etc.
- **CloudWatch** for log shipping; structured slog JSON output already emitted by the backend
- Region: TBD (likely `us-east-1` or `us-east-2`); deferred until first design partner geography is known

## Consequences

**Easier**
- Enterprise compliance procurement: AWS is the default expectation in fintech/healthtech (the v0 ICP).
- Plenty of audit-friendly primitives: KMS for envelope encryption, IAM for least-privilege, CloudTrail for AWS-side audit logs (separate from Fishhawk's own audit log, but useful for incident response).
- Wide tooling support: Terraform, the AWS CDK, official Go SDK with mature audit-friendly defaults.

**Harder**
- Higher day-2 ops burden than GCP. Mitigation: choose Fargate (managed runtime, no nodes) over EKS, and aggressively prefer managed services over self-hosted ones.
- Slightly higher infra cost vs. Hetzner-class providers at v0 scale. Acceptable given the compliance positioning.

**Other decisions this constrains**
- ADR-003 (object storage) → S3.
- ADR-009 (deployment target) → ECS Fargate.
- Future ADRs on observability and secrets default to AWS-native services unless we have a specific reason to deviate.

## Spec reference

`docs/MVP_SPEC.md` §10 #2 (deadline Day 3)

## Target deadline

Day 3 — **met**.

---
Parent epic: #15
