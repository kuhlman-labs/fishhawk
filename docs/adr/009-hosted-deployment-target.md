---
id: ADR-009
title: "Hosted deployment target"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/73
supersedes: []
superseded_by: []
applies_to: ["backend/Dockerfile"]
---

# ADR-009: Hosted deployment target

## Context

`fishhawkd` is a stateful Go service that needs to handle webhook spikes from GitHub, talk to Postgres + S3, and serve API + UI traffic. Deployment target affects operational complexity, autoscaling behavior, and how secrets are wired.

## Options

- **Kubernetes (GKE / EKS)** — most flexible; substantial day-2 ops burden for a solo founder.
- **Cloud Run / ECS Fargate** — managed container runtime; fast scaling; minimal ops. Some constraints on long-lived connections.
- **Single VM (EC2 / GCE)** — simplest mental model; manual ops, no autoscale, single point of failure.
- **App Platform (Fly.io / Render / Railway)** — fastest to ship; lock-in concerns.

## Recommendation

Cloud Run (if GCP per ADR-001) or ECS Fargate (if AWS). Managed container runtime is the right altitude for v0.

## Decision

**Recorded 2026-04-30: ECS Fargate.**

Stack:

- **ECS service** running the `fishhawkd` task definition (1+ replicas, min 2 in production for rolling deploys).
- **Application Load Balancer** in front for TLS termination, WebSocket support (if needed later), and target-group health checks driven by `GET /healthz`.
- **VPC** with public ALB subnets and private Fargate subnets; database access only via VPC endpoints / RDS in private subnets.
- **CloudWatch Logs** for container stdout/stderr (the slog JSON output is structured-friendly).
- **AWS Secrets Manager** injection via Fargate task definition `secrets`.
- **GitHub Actions OIDC** for IAM role assumption from CI — no long-lived AWS keys.

Container image build:

- Multi-stage `Dockerfile` at `backend/Dockerfile` produces a distroless static-binary image (~25 MB).
- Built and pushed by the same GitHub Actions CI that already runs lint/test (E13.5 / #62).

## Consequences

**Easier**
- Zero node ops: Fargate handles capacity. Solo-founder day-2 burden is minimal.
- Deploys via `aws ecs update-service` are atomic (rolling) and roll back on health-check failure automatically.
- No need to learn EKS / Kubernetes for v0. If we hit Fargate's limits (long-lived connections, very high RPS, gpu workloads), migrating to EKS later is mechanical.

**Harder**
- Fargate has higher per-vCPU cost than EC2 self-managed nodes. Acceptable at v0 scale (single-digit tasks).
- Long-lived WebSocket connections capped by ALB idle timeout. Not a v0 need; revisit if real-time UI features land.
- ECS task definitions are verbose. Mitigate with Terraform or AWS CDK from day one.

**Other decisions this constrains**
- E13.3 (#60) container build + deploy pipeline.
- E13.4 (#61) secrets management uses Secrets Manager.
- v1+ multi-region requires a higher-level orchestration layer; out of scope for now.

## Spec reference

`docs/MVP_SPEC.md` §5.1.1, §9 (hosted-only v0)

## Target deadline

Day 7 — **met**.

---
Parent epic: #15
