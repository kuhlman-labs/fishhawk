---
id: ADR-016
title: "IaC tool — Terraform vs AWS CDK"
status: accepted
date: 2026-05-05
issue: https://github.com/kuhlman-labs/fishhawk/issues/165
supersedes: []
superseded_by: []
applies_to: ["infra/terraform/**"]
---

# ADR-016: IaC tool — Terraform vs AWS CDK

## Context

[ADR-009](https://github.com/kuhlman-labs/fishhawk/issues/73) chose ECS Fargate as the deploy target. The hosted infrastructure (VPC, subnets, ALB, ECS cluster, RDS, Secrets Manager, IAM, CloudWatch) needs IaC. ADR-009's "consequences" section flagged the choice between Terraform and AWS CDK as deferred. E13.7 (#148) is the first PR that needs an answer.

## Options

- **Terraform** (HCL): mature, declarative, language-agnostic, simple `apply` model, large module ecosystem. Anyone with HCL literacy can read the diff at review time. State management is explicit (S3 backend + DynamoDB lock).
- **AWS CDK** (TypeScript): full programming language with types; generates CloudFormation under the hood. Same language as the frontend, so the contributor surface is unified. Heavier toolchain (Node + tsc + cdk synth + cdk deploy). State managed by CloudFormation (no S3+DynamoDB to bootstrap).
- **Pulumi** (TypeScript or Go): cross-cloud, real programming language. Drawbacks: smaller ecosystem than Terraform; requires Pulumi service or self-hosted backend.
- **Raw CloudFormation** (YAML): native, no extra tooling. Verbose for complex resources; painful import-from-existing story.

## Recommendation

**Terraform.** Reasons:

- HCL diffs are easier to review at PR time than synthesized CloudFormation. The Terraform plan output is the source of truth.
- The reviewer pool for fishhawk is broader than the contributor pool for the frontend; HCL is the lowest-common-denominator IaC language and matches what most operations engineers expect.
- AWS-only is fine for v0 (we're committed to ECS Fargate), but cloud-portability is a v1+ concern; Terraform keeps that door open.
- State-store bootstrap (S3 bucket + DynamoDB lock table, created out-of-band by hand) is a one-time setup cost and well-documented.
- Existing GitHub Actions modules (`hashicorp/setup-terraform`, etc.) make CI integration straightforward.

## Decision

**Recorded 2026-05-05.** Use Terraform for all hosted infrastructure under `infra/terraform/`.

- **Terraform version**: pin via a `versions.tf` `required_version`. Floor at `~> 1.5` (matches what ships with current macOS Homebrew); bump as features land.
- **Provider versions**: AWS `~> 5`. Pin in `versions.tf`.
- **State**: S3 backend with DynamoDB state-lock table. Bucket + table created out-of-band by the operator (chicken-and-egg); `infra/terraform/README.md` documents the bootstrap.
- **Workspace model**: one workspace per environment (`dev`, `staging`, `prod`). Variables file per env (`prod.tfvars`).
- **CI deploy**: GitHub Actions OIDC → IAM role assumption (no long-lived AWS keys per ADR-009). The deploy workflow runs `terraform apply` with the image tag passed in as a variable.

## Consequences

**Easier**:
- E13.7 (#148) starts with Terraform from day one.
- Operators can `terraform plan` against prod to preview drift without going through CI.
- Existing staging/prod state is greppable via `terraform state list`; debugging a misconfiguration doesn't require parsing CloudFormation events.

**Harder**:
- Initial bootstrap requires creating the state bucket + lock table by hand (and explaining how) before the first `terraform apply`. CDK's CloudFormation-state model would skip this.
- Cross-module ergonomics in HCL are noisier than typed TypeScript (no real refactoring tools, no static type-checked outputs).

**Other decisions this constrains**:
- E13.7 (#148) lays down infra/terraform/ in the structure described in the next slice's PR.
- E13.4 (#61) secrets management — secrets land in AWS Secrets Manager, referenced from the ECS task definition's `secrets` array. Terraform creates the Secrets Manager entries; operator populates the values out-of-band (so secret values never leave the AWS account).

## Spec reference

[ADR-009](https://github.com/kuhlman-labs/fishhawk/issues/73) "Mitigate with Terraform or AWS CDK from day one."

## Target deadline

Day 7 — **met (Day 6)**.

---
Parent epic: #15
