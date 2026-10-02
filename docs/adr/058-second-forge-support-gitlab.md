---
id: ADR-058
title: "Second-forge support: GitLab repositories, work items, and GitLab CI"
status: accepted
date: 2026-07-12
issue: https://github.com/kuhlman-labs/fishhawk/issues/1851
supersedes: []
superseded_by: ["ADR-061"]
applies_to: []
---

# ADR-058: Second-forge support: GitLab repositories, work items, and GitLab CI

## Context

Fishhawk is single-forge GitHub across three surfaces the product treats as one — repositories, work items, and CI. Supporting GitLab means giving each surface a second implementation. The three surfaces are at **very different levels of readiness**, and that asymmetry is the central fact this ADR has to plan around:

| Surface | Current abstraction | GitLab effort |
|---|---|---|
| **Work items / tracker** | **Real provider abstraction.** `workmgmt.Provider` registry (`backend/internal/workmgmt/provider.go`) with a provider-neutral `WorkItem` model and **two shipping impls: `github_projects` and `jira`**. Optional capability interfaces (`Transitioner`, `NumberDiscoverer`, `EpicChildrenQuerier`). | **Low–moderate.** Write a third sibling provider + a `gitlabclient`. The `Apply` conventions engine, MCP tools, and HTTP handlers are already provider-neutral. |
| **Repo / git / PR / webhook / forge-auth** | **Hardcoded to GitHub.** No `Forge`/`SCM`/`VCS` interface exists. `githubclient.Client` is a ~3,150-line concrete struct (~40 REST methods) consumed by ~17 packages that each pin GitHub types (`RepoRef`, `MergeMethod`, `DispatchInputs`, `installationID int64`). `githubapp.TokenProvider` is keyed by GitHub-App `installationID int64`. The webhook receiver hardcodes GitHub's HMAC scheme, headers, and event shapes. The **runner pushes to GitHub and opens PRs against a hardcoded `api.github.com`** (`runner/internal/gitops/pr.go`). | **High.** This is a new-interface effort, not a config swap. The forge abstraction doesn't exist yet. |
| **CI / dispatch backend** | **No abstraction.** `runner_kind` is a closed enum (`github_actions`, `local`) consulted by scattered `if` guards, not a dispatcher interface. Dispatch = `workflow_dispatch` REST call; inbound = `workflow_run`/`check_run` webhook parsing; status out = GitHub check runs. **The one forge-neutral seam is the runner↔backend HTTP contract** (signed trace upload + `runner_kind` self-report) — that seam is exactly what makes the `local` runner work, and the runner binary is already backend-agnostic. | **Moderate–high.** New dispatch call, new inbound webhook ingest, new status publisher, and a `.gitlab-ci.yml` job template. The runner binary itself is largely reusable. |

Three roadmap facts constrain the design:

1. **The identity leg already anticipates GitLab.** ADR-055 / E39 (#1705) shipped a backend `IdentityProvider` interface whose scope explicitly states *"the interface must remain implementable for GitLab without schema change, but no GitLab provider ships in this epic."* #1756 (E39.12) adds *"forge-neutral through the IdentityProvider seam (GitLab: group membership / project access level map cleanly)."* So **one of the four forge legs (identity/approvals) is already seam-ready**; the other three are not.

2. **ADR-022 / #388 already scoped runner-backend pluggability.** `runner_kind` was designed as an audit dimension meant to grow ("K8s extension is cheap — add `k8s` to the CHECK constraint"). The precedent is: *backend assigns `runner_kind` at dispatch* (Option 2, not runner self-declaration). `gitlab_ci` is a natural new value on that axis.

3. **ADR-057 / #1823 (multi-tenancy) is GitHub-Enterprise-shaped.** Tenant = GitHub Enterprise account; per-account GitHub endpoints (E44 #1826/#1827). A second forge widens "tenant" and "account" beyond GitHub Enterprise. **These two efforts collide and must be sequenced deliberately**, not designed in isolation.

Invariant #1 (customer source never reaches the backend) is *unaffected and naturally satisfied* on GitLab: the runner executes on GitLab CI / customer infra and ships only signed traces + metadata. That is a point in GitLab's favor — the hardest compliance invariant ports for free.

### The load-bearing fork: forge auth

GitHub App **installation** is threaded through the entire repo/git surface as `installationID int64` — in `githubapp.TokenProvider.Token(ctx, installationID)`, in every `githubclient` method, in the webhook `Event` envelope, and in `server/workitems.go`'s inline installation resolution. **GitLab has no GitHub-App-installation equivalent.** Its analogs are OAuth applications, and Personal / Project / Group access tokens. Generalizing the credential model away from `installationID int64` toward a forge-neutral repo-scope is the keystone that blocks everything else on the repo/git surface — and it is the same generalization #1756 already wants for mint-authz tenancy.

## Options

### Overall strategy

- **Option A — Big-bang `Forge` interface.** Extract one unified interface (branch/ref ops, PR/MR, commit status, webhook ingest, forge-auth) and adapt all ~17 consumers at once, GitHub and GitLab landing together. *Rejected as the primary path:* enormous single refactor, high regression surface against the live dogfood loop, and it forces the ADR-057 tenancy collision to be resolved up front.

- **Option B — Incremental, seam-by-seam, credential-model first (recommended).** Introduce the forge abstraction one surface at a time, GitHub remaining the only implementation until a GitLab consumer forces each seam. Land the **forge-credential generalization** (drop the `installationID int64` assumption) as the first child, mirroring how ADR-055 already carved `IdentityProvider`. Copy the proven `workmgmt` registry pattern to a `forge` registry. Each phase is independently shippable and independently revertible.

- **Option C — Defer until a paying GitLab customer.** Keep GitHub-only; only preserve the `IdentityProvider` GitLab-implementability posture already committed in ADR-055. *Rejected unless the roadmap says GitLab is not near-term* — but worth stating as the honest "do nothing structural yet" baseline.

### Per-surface options (within Option B)

**Work items (easiest):** add `backend/internal/workmgmt/gitlab/provider.go` + a `gitlabclient`, a `gitlab` connection block in `conventions.go` + the JSON schema + `Target.GitLab`, and a registration in `workmgmt_wiring.go`. Decide how GitLab maps onto the canonical-state / epic-children model (GitLab epics are a separate Premium-tier entity; issues use `#N` like GitHub, so `depends_on` body-markers and `#N` refs largely port). The server's inline `s.cfg.GitHub` installation resolution in `workitems.go` must be made forge-optional (GitLab has no installation).

**Repo/git/PR/webhook (hardest):**
- Extract a `Forge` interface from the `githubclient` method surface actually consumed (ref create + SHA read, push-credential, PR/MR create/edit/close/list/auto-merge, commit-status, branch protection, compare/diff, repo-scope resolution). Adapt the ~17 consumers off pinned `githubclient.*` types.
- New forge-credential abstraction replacing `githubapp.TokenProvider`'s `installationID int64` shape.
- New GitLab webhook receiver (`X-Gitlab-Token`, GitLab event JSON) parallel to the GitHub HMAC receiver.
- Runner: an MR opener parallel to `gitops/pr.go` (GitLab `POST /projects/:id/merge_requests`, `source_branch`/`target_branch`), a GitLab branch in `detectRunnerKind` (`GITLAB_CI`/`CI_PIPELINE_ID`), and a GitLab token-broker fallback. Git push over HTTPS with `http.<host>.extraheader` already works for any host. **ADR-035 lineage/tree-ownership logic is git-level and mostly forge-agnostic** — branch naming and push mechanics aside, it ports.

**CI/dispatch:**
- Introduce the missing dispatcher seam: a `RunnerBackend`/`Dispatcher` interface over `{dispatch a stage, publish status, ingest completion webhook}` with `github_actions`, `gitlab_ci`, `local` as implementations — replacing today's scattered `runner_kind` guards (`orchestrator.fireDispatch`, `host_dispatch_guard`).
- `gitlab_ci` `runner_kind` value + CHECK-constraint migration (the ADR-022 growth path).
- GitLab dispatch = pipeline trigger / pipelines API; inbound = Pipeline + Job + MR webhooks; status out = GitLab **commit statuses** (`POST /projects/:id/statuses/:sha`) — GitLab has no "check runs."
- A `.gitlab-ci.yml` job template analog of `runner/action.yml` + the customer `fishhawk.yml`.

## Recommendation

**Adopt Option B**, sequenced against ADR-057, in this order:

1. **Phase 0 — Scope + coordinate with ADR-057.** The scope forks are now settled (see "Scope decisions" below). Do *not* start extraction until the ADR-057 "account/installation" seam and this ADR's "forge" seam are reconciled — they are the same seam viewed from two angles.
2. **Phase 1 — Forge-credential generalization (keystone).** Replace `installationID int64` with a forge-neutral repo-scope credential provider. Unblocks both GitLab repo/git work *and* #1756 mint-authz tenancy. Ship with GitHub as the only implementation (pure refactor, zero behavior change) so it lands safely against the dogfood loop.
3. **Phase 2 — Work-item GitLab provider.** Lowest-risk first GitLab surface; proves the end-to-end forge-config path (`.fishhawk` conventions → GitLab issue) and forces the per-repo conventions loader (`workitems.go` `conventionsLoader` is still a `Default()` stub).
4. **Phase 3 — `Forge` interface extraction + GitLab repo/git/PR + webhook receiver.** The big one. Gate behind Phase 1.
5. **Phase 4 — Dispatcher seam + `gitlab_ci` runner backend + `.gitlab-ci.yml` template.** Builds on ADR-022; reuses the backend-agnostic runner binary.

Rationale: this front-loads the one refactor that unblocks the most (credential model), delivers a working GitLab surface early (work items) to de-risk config/onboarding, and defers the largest refactor (forge extraction) until the credential seam exists to hang it on. It also keeps GitLab out of the critical path of ADR-057's tenancy work while forcing the two to share the account/credential seam rather than build it twice.

### Scope decisions (settled 2026-07-12)

The four ratification forks are resolved as follows:

1. **Deployment targets: GitLab.com SaaS *and* self-managed.** Both are in scope. Per-account / per-deployment base-URL configuration is therefore required from the start — directly analogous to the GHES/EMU endpoint work (E44 #1826) and reusing that pattern (`accounts.<forge>_base_url` / OAuth base). No GitLab.com-only shortcut that hardcodes `gitlab.com`.
2. **Credential model: a GitLab OAuth application** as the primary path — the closest available equivalent to the GitHub App. **Honest caveat to record in the design:** GitLab has *no* exact GitHub-App-installation analog (no "install once, mint a per-installation token across many repos"). The closest faithful shape is an **OAuth application authorized at the group/account level**, with the forge-credential provider brokering a token *per forge-account/group scope* rather than per GitHub installation. Group/Project access tokens (bot users) are the fallback where OAuth-app authorization isn't available (notably some self-managed configurations). Phase 1's abstraction must therefore key on a forge-neutral *account/group scope*, not `installationID int64`.
3. **Tenancy: "tenant" generalizes to *forge account*.** ADR-057's `account` concept becomes forge-neutral (`{provider, account_id}`), not "GitHub Enterprise account." **This is the highest-leverage, most time-sensitive consequence:** ADR-057 is being implemented *now* (E44 preconditions, tenancy schema #1823). A `provider` discriminator must be injected into ADR-057's `accounts` / `installations` schema and its endpoint-config columns *before that schema ossifies as GitHub-only* — retrofitting a forge dimension after the tenancy tables and RLS policies ship is a migration we can avoid entirely by co-designing now. Phase 1 of this ADR and ADR-057's foundation are the same seam and must land together.
4. **Timeline: short-term, not immediate.** Option C (defer / do nothing structural) is rejected. Option B proceeds, but the *only* piece that is urgent-now is the forge-account/credential generalization in ADR-057's schema (fork 3) — because that window closes as ADR-057 lands. The GitLab implementations themselves (Phases 2–5) are short-term but not on today's critical path.

The single actionable directive from these answers: **co-design the forge-account discriminator and forge-neutral credential scope into ADR-057's tenancy schema before it ships**, so GitLab becomes an additive provider later rather than a schema migration.

## Decision

**Accept Option B (incremental, credential-model-first) with the scope above.** GitLab is a committed short-term target across all three surfaces (repositories, work items, GitLab CI), SaaS and self-managed, authenticated via a group/account-scoped OAuth application, with "tenant" generalized to forge account. The forge-account discriminator and forge-neutral credential scope are to be folded into ADR-057's in-flight tenancy schema now; the GitLab provider implementations follow per the phased plan. Phased children to be filed against a new epic once this ADR is ratified.

Ratified 2026-07-12 (operator/founder). Children filed under [E45] #1852: #1854 (discriminator, MERGED via PR #1931), #1855, #1856, #1857, #1858, #1859, #1860, #1861. Closing this ADR per git-flow step 7; the epic tracks delivery.

## Consequences

- **If adopted:** Fishhawk gains a genuine multi-forge architecture. The `workmgmt` registry pattern generalizes to a `forge` registry; `runner_kind` grows per ADR-022 as designed; the credential-model refactor (Phase 1) also pays down #1756's mint-authz tenancy debt. Invariant #1 holds unchanged.
- **Coupling paid down regardless of GitLab:** extracting the `Forge` interface and the dispatcher seam improves testability and removes ~17 packages' direct dependence on the concrete `githubclient` even before a second forge ships.
- **Cost is concentrated, not spread:** work-items are cheap, but repo/git/PR/webhook/auth is a real interface-extraction project touching the live dogfood loop. Sequencing (credential-model first) is what keeps each step revertible.
- **Time-sensitive dependency on ADR-057 (now the load-bearing consequence).** With "tenant = forge account" settled and ADR-057 in active implementation, the forge-account `provider` discriminator and forge-neutral credential scope must land in ADR-057's `accounts`/`installations` schema, endpoint-config columns, and RLS policies *before they ship*. Miss this window and GitLab becomes a tenancy-table migration instead of an additive provider. This is the one action that cannot wait for the rest of the phased plan.
- **Not doing it** leaves GitLab customers unservable and leaves the `installationID int64` assumption embedded, which #1756 already flags as a tenancy blocker independent of GitLab.

## Relations

- ADR-022 / #388 — pluggable runner backends + `runner_kind` (the CI-backend growth path this ADR extends).
- ADR-055 / #1698, E39 / #1705 — `IdentityProvider` interface, already committed to remain GitLab-implementable (the one seam-ready forge leg).
- #1756 (E39.12) — mint-authz tenancy beyond a single operator repo; shares Phase 1's credential-model generalization ("GitLab: group membership / project access level map cleanly").
- ADR-057 / #1823 — multi-tenancy (GitHub-Enterprise-shaped); must be co-sequenced with this ADR's account/credential seam. E44 #1826 (configurable GitHub/OAuth endpoints) is the direct precedent for self-managed-GitLab endpoint config.
- E36 / #1638 — external-repo alpha readiness; genericizes fishhawk-specific assumptions (a precursor that reduces GitLab-porting friction).
- ADR-050 / #1540 — acceptance-agent egress chosen for portability "across CI providers" (a design choice that already anticipated non-GitHub CI).
- Invariant #1 (`docs/ARCHITECTURE.md` §6) — customer source never reaches the backend; satisfied for free on GitLab CI.
