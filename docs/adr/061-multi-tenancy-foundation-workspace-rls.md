---
id: ADR-061
title: "Multi-tenancy foundation: workspace-scoped tenancy with Postgres RLS isolation"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/2069
supersedes: ["ADR-058"]
superseded_by: []
applies_to: []
---

# ADR-061: Multi-tenancy foundation: workspace-scoped tenancy with Postgres RLS isolation

## Context

Fishhawk is single-tenant today: one dogfood repo, a single global `FISHHAWKD_OPERATOR_REPO` mint-authz anchor, and no tenant scoping on any row, query, token, or request path. The alpha is **multi-tenant from day one** (multiple design partners concurrently). For a governance/audit product, tenant isolation is not a feature — it is the core trust proposition; a single cross-tenant leak is existential. Isolation must therefore be a foundation laid BEFORE surface area (UI, more workflows) expands, because retrofitting tenant-scoping onto every access path later is both expensive and leak-prone. This ADR settles the tenant model, isolation mechanism, request→tenant resolution, identity binding, and migration so the work can decompose into an epic and run as a campaign.

Consolidates the prior tenancy threads: #1756 (mint-anchor tenancy design, E39.12), ADR-058 ("tenant = forge account" — superseded here by the workspace model), ADR-057 (run-row credential-ref, GitLab), and E39 (approval identity, which becomes workspace-scoped).

## Decisions (settled with founder)

1. **Tenant = a first-class Fishhawk `workspace`** that OWNS one or more forge installations (GitHub App installs / GitLab OAuth grants). A workspace is the trust/identity/billing boundary and is decoupled from any single forge account — one customer with a GitHub org AND a GitLab group is ONE workspace. (Chosen over the simpler "tenant = forge account/installation" of ADR-058.)
2. **Isolation = shared schema + `tenant_id` (workspace_id) on every tenant-scoped row + Postgres Row-Level Security.** RLS makes isolation a DATABASE invariant: a forgotten `WHERE tenant_id=` in app code still cannot leak. App-layer scoping is retained as defense-in-depth ON TOP of RLS. (Chosen over app-layer-only scoping and over DB-per-tenant.)

## Design (proposed — the Recommendation the founder ratifies below)

### Tenant model
- `workspace` table (id, name, slug, created_at, …) — the tenant.
- `workspace_installation` mapping: `(workspace_id, forge, installation_id/account)`. An installation belongs to EXACTLY ONE workspace — that uniqueness is the forge-level isolation boundary (an org's install can't be claimed by two tenants).
- `workspace_membership`: identity (`github:user`, oauth subject — the E39 identity shape) → workspace + role. Membership is what E39 approval eligibility/quorum evaluates against, per-workspace.

### Isolation / RLS
- Add `tenant_id uuid` (FK → workspace) to every tenant-scoped table: runs, stages, campaigns, campaign_items, audit_entries, plan/artifacts, api_tokens, scope_amendments, concerns, deliveries, and the rest. A completeness sweep classifies EVERY table as tenant-scoped vs global (e.g. schema-version/system tables) — a missed table is the failure mode, so this classification is explicit and test-enforced.
- RLS policy per tenant-scoped table: `USING (tenant_id = current_setting('fishhawk.current_tenant')::uuid)`. The app sets `SET LOCAL fishhawk.current_tenant = <workspace_id>` per request-transaction after resolving the tenant.
- A dedicated migration/admin DB role carries `BYPASSRLS` for cross-tenant system work (migrations, reconciliation) — narrowly gated and audited; the request-path role never has it.
- sqlc-generated queries are transparent to RLS (the DB enforces the filter), so they need no per-query rewrite — a major reason RLS beats app-layer-only. App-layer `tenant_id` filters are still added on the hot read paths as defense-in-depth + for index selectivity.

### Request → tenant resolution (three surfaces)
- **Webhook:** carries the installation id / repo → `workspace_installation` → workspace.
- **REST API:** the operator token is workspace-bound (`api_tokens.workspace_id`) → resolve from the token.
- **MCP:** the operator token authenticating the MCP connection is workspace-bound → resolve from the token (the founder's dogfood connection binds to workspace 0).
- A single early middleware resolves the tenant and sets the RLS session var; a request that cannot resolve a tenant is rejected before any tenant-scoped query runs.

### Identity & auth (ties to E39 / #1753 / mint-authz)
- `api_tokens` gain `workspace_id`; token mint + verify become workspace-scoped.
- The mint-authz min_permission predicates that today anchor to the global `FISHHAWKD_OPERATOR_REPO` (quorum.go) become per-workspace: each workspace has its own operator-repo/config anchor.
- `resolveOperatorRepoToken` (#1753) resolves the App installation token per workspace-installation.
- E39 approval quorum/eligibility evaluates identity permission WITHIN the request's workspace — an approver in workspace A can never satisfy a gate in workspace B.

### Migration (multi-step, on live dogfood data)
1. Create `workspace`, `workspace_installation`, `workspace_membership`.
2. Create "workspace 0" (the founder dogfood workspace); map the existing kuhlman-labs installation + founder identity to it.
3. Add nullable `tenant_id` to every tenant-scoped table; backfill = workspace 0.
4. Make `tenant_id` NOT NULL; add indexes.
5. Add RLS policies (permissive-then-enforcing), grant `BYPASSRLS` to the admin role only, flip the request role to the RLS-enforced role.

## Phased rollout (future epic children — each a campaign item)
- **P1 Tenant tables + resolution plumbing:** workspace/installation/membership tables, the tenant-resolution middleware, and the RLS session-var plumbing — tenant_id nullable, RLS not yet enforcing.
- **P2 tenant_id sweep + backfill:** add tenant_id across all tables (with the completeness classification/test), backfill workspace 0, NOT NULL + indexes.
- **P3 RLS enforcing:** policies on every tenant-scoped table, BYPASSRLS admin role, request role flipped — with a cross-tenant leak test suite (attempt A-reads-B, assert empty).
- **P4 Identity/auth workspace-scoping:** api_tokens.workspace_id, per-workspace mint-authz anchor, #1753 per-installation token, E39 workspace-scoped quorum.
- **P5 Workspace management surface:** create workspace, attach installation, invite/roles — the operator/API surface (and later the UI panel).

## Consequences
- **Positive:** isolation enforced at the DB layer (leak-resistant by construction); the workspace abstraction supports cross-forge customers and future billing/SSO; sqlc queries mostly unchanged.
- **Costs/risks:** a live-data migration touching every table; RLS requires `tenant_id` indexes for performance; the `BYPASSRLS` admin role is a sharp edge (must be narrowly used + audited); the tenant-scoped-vs-global table classification must be complete (a missed table = a silent global table = a leak) — enforced by a completeness test; the MCP stdio model's per-connection workspace binding needs care.

## Out of scope (non-goals)
Billing/metering; self-serve signup UX; a cross-tenant admin console; DB-per-tenant physical isolation (explicitly rejected in favor of RLS). ADR-058's "tenant = forge account" is superseded by the workspace model here.

## Decision
_(Founder ratifies: the two settled decisions above are accepted; this section confirms the proposed design + P1–P5 phasing, or amends it. On acceptance this ADR decomposes into a `[E<n>] Multi-tenancy` epic with P1–P5 as depends_on-chained children, driven as a campaign.)_

## Relations
Supersedes ADR-058's tenant definition. Related: #1756, ADR-057, E39 (#1705).
