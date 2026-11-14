---
id: ADR-057
title: "Multi-tenancy model and tenant isolation for hosted deployment"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/1823
supersedes: ["ADR-061"]
superseded_by: []
amended_by: ["ADR-105"]
applies_to: []
---

# ADR-057: Multi-tenancy model and tenant isolation for hosted deployment

## Context

We are preparing to support Fishhawk beyond the current single-operator posture (`backend/cmd/fishhawkd/token.go:40`). Two customer deployment modes are in scope, and the tenancy design must serve both:

- **Mode 1 — Self-hosted (BYO).** The customer runs their own Fishhawk deployment and connects it to *their* GitHub Enterprise (Server or Cloud) or Org. One deployment == one customer == effectively single-tenant. Isolation is the deployment boundary; the customer owns infra, DB, secrets, and region.
- **Mode 2 — Hosted (SaaS).** We host Fishhawk. A GitHub Enterprise Admin installs the App, and the deployment is **tenanted to their GitHub Enterprise account** (default granularity). One deployment serves many customers; isolation and **data residency** must be enforced in software.

The same core serves both: Mode 1 is the multi-tenant core constrained to a single implicit account; Mode 2 is the pooled, region-celled configuration. We do not build two products.

**Framing: the data plane is already installation-aware; the control plane assumes a single tenant.**

Already per-installation (works today):
- `runs.installation_id` (BIGINT, indexed) from the webhook payload — `migrations/0005_runs_installation_id.up.sql`, `backend/internal/webhook/dispatcher.go`.
- GitHub token cache keyed by installation — `backend/internal/githubapp/cache.go:42`.
- Notifier mints the run's installation token per post — `backend/internal/issuecomment/notifier.go`.
- **Execution runs on the customer's own infra** (their GitHub Actions VM, in their region), using **their** repo secrets for LLM keys — never fishhawkd's (`runner/action.yml`, `onboarding/templates/fishhawk.yml`).
- **Invariant #1: customer source code never reaches the backend** (`docs/ARCHITECTURE.md:324`). The backend holds only traces, plans, metadata, audit logs, tokens. This bounds a logical-isolation defect's blast radius to metadata — not source.

Single-tenant assumptions to change for Mode 2 (the control plane):
1. **No tenant/account boundary on identity.** `api_tokens` / `mcp_tokens` carry only a `subject` string, no account column (`migrations/0008`, `0023`). `Identity` (`middleware.go:62`) has no tenant field.
2. **Authorization is scope-based, not tenant-scoped.** `hasScope(id, "write:runs")` never checks "does this caller own this run's account."
3. **No query enforces isolation.** Point reads are `WHERE id = $1`; `ListRuns` has no account filter (`backend/internal/run/queries.sql`); ticker scans are global.
4. **One global audit hash chain** (`migrations/0009_audit_entries_global_chain.up.sql`) — must become per-account (decided below).
5. **All secrets/endpoints are process-global** — one App key, one webhook secret, one reviewer-LLM key set, one operator-repo authz realm, hardcoded `github.com`/`api.github.com` endpoints, read once at boot (`serve.go`, `auth/github_oauth.go`, `githubapp/client.go`).

### The frontend GitHub-OAuth login hole (must fix before ANY hosted exposure — both modes)

**Currently exploitable.** `handleGitHubCallback` (`backend/internal/server/auth.go:75`) exchanges the OAuth code, fetches the profile, and calls `AuthRepo.SignIn` (`backend/internal/auth/postgres.go:31`) which **upserts the user and mints a session unconditionally** — no membership/allowlist/permission gate. The `OperatorRepo`/`OperatorMinPermission` gate (`server.go:172`) guards only the token-*mint* endpoint, not the cookie-session callback. Cookie sessions (`TokenID == ""`) **bypass scope enforcement** (`middleware.go:118`); reads do no identity scoping (`runs.go:1500`). Net: any GitHub account can sign in and read/write data. Closing this is a precondition of both modes.

## Decisions (founder-directed)

1. **Default tenant granularity = GitHub Enterprise account.** A tenant (`account`) maps to a GitHub Enterprise account and owns 1+ orgs/installations beneath it. (Org-only tenancy remains a supported finer grain for customers without an enterprise, but the default onboarding provisions at the enterprise level.)
2. **Audit chain is per-account.** Replace the single global hash chain (`migrations/0009`) with a per-account append-only chain (`prev_hash` chains within `account_id`). Enables clean per-tenant compliance export and is a data-residency prerequisite.
3. **Login membership gate = GitHub Enterprise membership.** Sign-in is allowed only for identities that are members of the tenant's GitHub Enterprise; resolved via the identity provider's enterprise-membership API. No membership → no session.
4. **EMU (Enterprise Managed Users) is supported.** Identity resolution handles IdP-provisioned, short-code-suffixed EMU logins and data-resident GHEC (`<slug>.ghe.com`) OAuth/API endpoints.
5. **Data residency is a hard requirement.** A tenant's data (Postgres rows, per-account audit chain, S3 trace bundles, and model-inference endpoints) must reside in the tenant's chosen region.

## Architecture: Approach D within regional cells

**Mode 2 hosted = pooled multi-tenant *inside each region cell*, tenants pinned to a home region.**

- **Regional cells.** One Fishhawk stack + Postgres + S3 trace bucket per supported region (start with the GHEC data-residency regions — US, EU, Australia — config-driven so more come online without code change). `docs/ARCHITECTURE.md §5.2` bucket-per-environment becomes bucket-per-region.
- **Pooled within a region (Approach D).** Inside a cell, tenants share Postgres with `account_id` scoping on every root table, threaded through queries + authz, backstopped by **Postgres RLS**. Because source never touches the backend and compute is offloaded, pooled logical isolation is an acceptable posture for the metadata we hold.
- **Tenant pinned to a home region.** At onboarding (Enterprise Admin installs the App), the enterprise account is bound to a region; all its rows, audit chain, and trace bundles live only in that cell.
- **Thin global directory (control plane).** A minimal, metadata-only global service maps `enterprise account/slug → home region` and routes the install callback and each login to the correct cell. Kept deliberately tiny (slug↔region only) to minimize what lives outside a resident region; treat its own storage region per policy.
- **Per-account GitHub endpoints.** Data-resident GHEC enterprises use region-specific `api.<slug>.ghe.com` OAuth/REST endpoints, so the GitHub base URL becomes **per-account config** (`accounts.github_base_url` / `oauth_base_url`), superseding the hardcoded `github.com` defaults. Mode 1 sets these per-deployment (GHES); Mode 2 sets them per-account.
- **Region-appropriate model inference.** Plan/implement-review agents that process trace/plan text must call a model endpoint in the tenant's region (or the trace/plan data leaves the region). Reviewer-key + model-endpoint selection becomes region-scoped.
- **Runner/execution** already runs on the customer's own infra in their region — no change; keeps source and heavy compute out of our cells entirely.

**Cell-per-tenant (dedicated infra)** remains available for enterprises that require physically isolated infrastructure or as a near-zero-code interim for the first hosted design partner — it is the regional-cell design with a cell size of one.

**Mode 1 self-hosted** trivially satisfies residency (customer picks their region/infra): ship the same core configured to a single implicit account, configurable GHES/GHEC endpoints, and the enterprise-membership login gate scoped to their enterprise.

## Consequences

Preconditions of hosting (file first):
1. **Frontend login gate:** enterprise-membership check in `handleGitHubCallback` before `SignIn`; `Identity.AccountID`; deny-with-no-session; `/v0/auth/me` account context; SPA 403 handling; close the cookie-session scope-bypass.
2. **Configurable GitHub/OAuth endpoints** (`auth/github_oauth.go`, `githubapp/client.go`, `serve.go`) — per-deployment (Mode 1) and per-account (Mode 2), incl. EMU / `<slug>.ghe.com`.

Core tenancy + residency:
3. **Schema:** `accounts` (enterprise key, `home_region`, `github_base_url`, granularity), `installations` mapping, `account_members`; add `account_id` to `runs`, `campaigns`, `refinement_*`, `api_tokens`, `audit_entries`; backfill from `installation_id`.
4. **Per-account audit chain:** re-key `prev_hash` chaining and the append-only integrity checks by `account_id`; per-account compliance export.
5. **Authz:** account dimension on `Identity`; `requireAccountOwnsRun`-style checks on every run/stage/audit handler.
6. **Queries + RLS:** `account_id` in `ListRunsFilter`, ticker/reconciler scans, point reads; Postgres RLS on tenant-scoped tables.
7. **Regional cells:** per-region Postgres + S3 buckets; the global `account → region` directory + cell routing; region-scoped model endpoints/keys.
8. **EMU/SSO:** intersects the existing v1 SSO/SAML item — resolve EMU identities in the membership gate; coordinate scope with that item.
9. **CLI/local runs:** map operator tokens to an account (the single implicit account in Mode 1).
10. **Packaging/ops:** self-hosted distribution (image + Helm, `docs/deploy/kubernetes.md`) with a documented single-tenant profile; per-region hosted deploy topology.

Risks: a missed `account_id` predicate is a cross-tenant leak within a cell (RLS mitigates); cross-region data leakage via the global directory or a mis-selected model endpoint (residency); backfilling `installation_id → account_id`; EMU/GHEC endpoint variance. Docs to update on landing: `docs/ARCHITECTURE.md` §5/§8 (regional cells, per-account audit chain), `docs/MVP_SPEC.md` (promote multi-tenancy + deployment modes + residency out of v1-deferred), `docs/api/v0.openapi.yaml` (account context on `/auth/me`), `docs/deploy/` (self-hosted + regional hosted guides).

## Status

**Accepted (founder-directed).** Decisions above are locked. Next step: break the Consequences into child implementation issues under a multi-tenancy epic, sequencing the two hosting preconditions (login gate, endpoint configurability) first.

## Decision

**Accepted (date not stated).** Status confirmed by the Captain (repository maintainer) on 2026-10-23 from the record's own source: "**Accepted (founder-directed).** Decisions above are locked.".

Amendment A1 supersedes ADR-061 (#2069), which was folded into it (confirmed by the Captain 2026-10-23).

**Amendments A1 and A2 (transcribed 2026-11-14).** Both were ratified as comments on this issue ([A1](https://github.com/kuhlman-labs/fishhawk/issues/1823#issuecomment-5013715614), [A2](https://github.com/kuhlman-labs/fishhawk/issues/1823#issuecomment-5016114662)) and are copied below verbatim, at the Captain's direction, so that the record carries them.

## Amendment A1 (accepted 2026-07-18) — tenant granularity is a forge-agnostic workspace

Supersedes Decision #1 ("default tenant granularity = GitHub Enterprise account") and reconciles ADR-058's "tenant = forge account" into one model.

**New tenant definition:** a tenant (the `accounts` row) is a **forge-agnostic Fishhawk workspace** that owns one or more forge **installations** — GitHub App installs (org/enterprise) AND GitLab OAuth grants (group/account) — across providers. One customer with both a GitHub org and a GitLab group is ONE tenant. The workspace, not any single forge account, is the trust / identity / billing / residency boundary.

**Why now:** GitLab is committed (ADR-058 / E45), so the tenant model must not be GitHub-Enterprise-intrinsic. #1854 (E45.1) already folded a forge `provider` discriminator into the `installations` table, so the multi-forge-installation shape is partly in place.

**What does NOT change (the isolation core is forge-independent):** shared-Postgres + `account_id`/`tenant_id` + Postgres RLS (Approach D), the per-account audit chain (#1828), tenant-scoped queries + RLS (#1830), regional cells + data residency (#1831). These operate on the tenant id regardless of which forge(s) the tenant owns — no amendment needed.

**What DOES change (children amended — see per-issue comments):**
- **#1825 (schema):** `accounts` is a forge-agnostic workspace, not "keyed to a GitHub Enterprise account." Move the per-forge endpoint config (`github_base_url`/`oauth_base_url`) OFF the `accounts` row and ONTO the per-installation row (a workspace spans forges → a single per-account base URL is wrong); `installations` carries `(provider, account/group ref, installation_id, base_url)`. `account_members` materializes from whichever forge(s) the workspace owns.
- **#1826 (endpoints):** generalize to per-installation FORGE endpoints — GitLab base/OAuth URLs too, resolved by the installation's `provider`.
- **#1827 (login gate):** forge-agnostic membership gate — resolve the OAuth login to its workspace(s) via the membership of the LOGIN'S forge (GitHub Enterprise membership OR GitLab group membership), keyed on the provider the login came through.
- **#1829 (authz):** `Identity.AccountID` is the workspace/tenant id (forge-agnostic); ownership checks are workspace-scoped, provider-independent.
- **#1832 (EMU/SSO):** EMU stays GitHub-specific; add GitLab SSO/group-sync identity resolution as the sibling forge path behind the same membership-resolution seam.

Terminology: the table may keep the name `accounts` (redefined as the workspace) to minimize churn, or be renamed `workspaces` — implementer's call; the SEMANTICS are the workspace model above.

Origin: the operator-drafted duplicate ADR-061 (#2069, closed) surfaced the forge-agnostic-vs-GH-Enterprise granularity gap; founder ratified the forge-agnostic workspace model 2026-07-18.

## Amendment A2 (founder-ratified 2026-07-19) — workspace admission & in-workspace visibility model

Settles the two authorization layers the A1 workspace model left open. Three layers, kept distinct:

**1. Identity (authn):** forge OAuth; an identity is `(provider, subject)`. A user may later hold multiple *linked* forge identities (linking itself is a follow-on).

**2. Workspace admission — native rows + JIT auto-join.** `account_members` is the AUTHORITATIVE, revocable, role-carrying workspace grant. No forge can be authoritative for a multi-forge workspace (union-of-forges over-admits and makes revocation impossible without forge-side removal). Bootstrap via **per-installation auto-join policies**: a workspace admin declares "members of org/group X may auto-join as role R"; at login the live forge check the user's OAuth token CAN answer (`/user/orgs` for GitHub; group membership for GitLab later) is evaluated against the policies, and a match UPSERTS an **audited** `account_members` row (`origin=auto_join`) then admits. Auto-joined rows are **re-verified against the predicate at each login** (predicate fails → row no longer admits); invited rows (`origin=invited`) are explicit admin grants that persist until revoked, and admit **without** any forge call (DB-only — forge API availability never locks out invited members). **The enterprise-membership API is NOT required for admission:** GitHub App installations are org-scoped, so policies anchor to orgs the user token can verify; enterprise tenants are workspaces owning several org installations (invite or org-scoped auto-join).

**3. In-workspace visibility — repo-scoped least-privilege.** Invariant #1 keeps *source* off the backend, but traces/plans/review comments DO carry code excerpts and file paths — so workspace-wide member visibility would leak restricted-repo content. A non-admin member sees runs/campaigns/audit **only for repos where their forge permission ≥ read**, via a per-user mirrored repo-ACL cache (login refresh + TTL; webhook invalidation later), enforced at the query layer ON TOP of tenant RLS. Workspace `admin` sees all workspace data + settings. Approval *eligibility* remains E39's live per-approval forge check. **Cross-forge:** visibility for a forge's data resolves via the user's linked identity for THAT forge; no linked identity → default deny for that forge's repos.

**Child impact:** #1827 revised — admission = native row OR auto-join match (live org check as the bootstrap predicate only); enterprise-API claims dropped. NEW child files repo-ACL mirroring + read-path filtering (see E44.10). #1829 unchanged (account-scoped ownership; the visibility child layers within-account repo filtering on top). #1830 RLS unchanged (tenant boundary).
