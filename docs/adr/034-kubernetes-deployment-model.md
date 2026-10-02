---
id: ADR-034
title: "Kubernetes deployment model — relationship to ECS, deps, secrets, worker model"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/845
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-034: Kubernetes deployment model — relationship to ECS, deps, secrets, worker model

> Anchor for the Kubernetes deployment-model issue cluster. Records the decisions and frames the one deferred call (k8s vs. ECS).

## Context

Fishhawk needs a Kubernetes deployment model. Short term (alpha) we deploy to a **local Docker-Desktop k8s cluster**; longer term we want the **foundations for a production k8s deployment** — without finishing one yet.

A production path **already exists**: AWS ECS Fargate + Terraform (`infra/terraform/`, `.github/workflows/backend-deploy.yml`, image `ghcr.io/kuhlman-labs/fishhawkd`, shipped under #168). This work introduces a **parallel** model and must not implicitly strand the ECS path.

## Decisions already made (operator, 2026-06-07)
- **Packaging:** Helm chart, values-per-env (`values-local.yaml` vs `values-prod.yaml`). Chart at `deploy/helm/fishhawk/`.
- **Scope now:** local fully working + prod-ready *hooks* (externalizable DB/S3, ingress+TLS templates, secrets strategy, multi-replica hazard resolved). Defer HPA / full observability / a real prod cluster.
- **Stateful deps:** in-cluster postgres+MinIO for local (mirror `docker-compose.yml`); external endpoints for prod via values.

## The deferred decision (this ADR)
**Does k8s SUPERSEDE the ECS/Terraform path, COEXIST with it (ECS = our hosted prod, k8s = self-hosters + local), or stay self-host/local-only?** Resolve once the local chart is proven and the hosted direction firms up. Until then ECS stays intact and the chart is built prod-capable but not adopted as the hosted prod target.

Sub-decisions to record here: in-cluster-vs-external deps policy; secrets strategy (k8s Secret local + External-Secrets hook prod, see child); the background-worker singleton model (single-replica all-in-one vs api+worker split / leader election, see child).

## Options
- (a) Coexist / multi-target — ECS hosted prod, k8s for self-host + local.
- (b) k8s supersedes ECS — deprecate Terraform/ECS over time.
- (c) Self-host/local-only — k8s never becomes our hosted prod; ECS stays the hosted target.

## Recommendation
Defer between (a)/(b)/(c) until the local chart + prod-foundation children land and alpha settles; build the chart so any of the three remains reachable. Lean (a) as the least-regret default.

## Decision
_TBD._

## Consequences
- The child issues build a prod-capable chart regardless of this decision; only the *adoption as hosted prod* is gated here.
- ECS (#168) and the hosted-cluster tickets (#182 secrets backend, #388 ADR-022 runner backends, #650 ADR-029 egress, #655 MCP gateway, #843 ADR-033 MCP transport) remain the surrounding hosted context.

## Related
#168 (ECS path), #182, #388, #650, #655, #843. Child cluster: the `[E22.X]` k8s issues filed alongside this ADR.

Parent epic: #389
