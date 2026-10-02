---
id: ADR-062
title: "Regional-cells control plane: fishhawk-directory service, directory-first region pinning, per-cell inference config"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2099
supersedes: []
superseded_by: []
applies_to: ["directory/**"]
---

# ADR-062: Regional-cells control plane: fishhawk-directory service, directory-first region pinning, per-cell inference config

## Context

E44.7 (#1831) realizes ADR-057's data-residency requirement via regional cells. The within-cell tenancy foundation is merged (#1825–#1830: schema, endpoints, login gate, authz, RLS, per-account audit chain #2096). The net-new surface — a global account→region directory, cross-cell routing, onboarding region pinning, region-scoped model inference — has no existing design in code or docs. The #1831 plan agent parked with three architecture questions the codebase cannot settle. This ADR records the operator-selected design so implementation can proceed; it is filed as PROPOSED for ratification by the project owner, drafted by the operator-agent during campaign `f779f8cd` (run `bc47d2c4`). Relates to epic #1824 / issue #1831.

**Amendment A1 (2026-07-22, plan-review arbitration on run `bc47d2c4`):** the directory store maps `(provider, account_key) → home_region` ONLY; `region → cell_base_url` resolves EXCLUSIVELY from the directory's env config (single source of truth — no per-account cell URL column). The directory→cell handoff is an HMAC-SHA256-signed parameter set `(provider, account_key, home_region, expires_at, nonce)` with a shared env secret; the cell validates signature + expiry, accepts a pin only when the account's `home_region` is NULL or equal (first-write-wins replay bound), and rejects any pin whose region differs from the cell's own configured home region. Redirects preserve the original path and query. Routed surfaces are GET-only by construction. The handoff codec lives once, in the public `directory/pkg/handoff` package, imported by the cell.

## Options

**Q1 — Directory realization + routing transport**
- (a) New minimal `directory/` Go module shipping a separate `fishhawk-directory` binary with its own tiny Postgres holding ONLY (provider, account_key/slug → home_region) + install-state nonce table; routes login and the App-install callback via HTTP 302 (path+query preserved) to the cell resolved from region→cell_base_url env config.
- (b) A "directory mode" of fishhawkd — less new infra, but couples the global plane to a regional binary and muddies DB authority.
- (c) Static config, no DB — cannot record post-deploy bindings; breaks self-serve onboarding.
- Reverse-proxy routing instead of 302 — hides cell hostnames but passes request bodies (potentially resident data) through the global plane, undercutting the residency goal.

**Q2 — Onboarding region-pin write protocol**
- (a) Directory-first: onboarding hits the directory, which assigns and records (slug → home_region) — region from operator config or explicit onboarding input, defaulting to the enterprise's GHEC data-residency region when discoverable (discovery deferred to a follow-up) — then 302s into the chosen cell, which stamps `accounts.home_region` from the signed handoff (authoritative-on-write, never re-derived, cell self-check per Amendment A1). No cell→directory write-back path exists.
- (b) Cell-first with write-back: closer to today's single-cell flow, but resident data can land before its region is authoritatively known, and it needs an authenticated cross-plane write API plus reconciliation on write-back failure — a residency violation window, not a cosmetic risk.

**Q3 — Region-scoped model inference**
- (a) Per-cell config: each cell deploys with its region's model endpoint/base-URL + reviewer keys (region-scoped serve.go config); selection stays process-level. Cell-per-tenant = a cell configured with one account.
- (b) Per-account region→endpoint map resolved in-cell at request time: only needed if the one-region-per-cell invariant is ever relaxed; adds a registry, a request-time lookup, and a fail-closed path today's model does not need.

## Recommendation

Adopt (a) for all three, as amended by A1: separate minimal directory binary with 302 routing (path+query preserved); directory-first region pinning with the directory as sole placement authority and the signed-handoff + cell-self-check validation chain; per-cell inference config. Each alternative either violates the residency invariant outright (proxy routing, cell-first pinning) or contradicts the regional-cell model (in-cell per-account endpoint maps). Delivery: decomposition under #1831, with per-region deploy topology (helm values, buckets) carved out as a separate human-led infra issue.

## Decision

**Accepted (2026-07-26), ratified retroactively.** Adopt the Recommendation as amended: the `fishhawk-directory` service, directory-first region pinning, and per-cell inference configuration.

Ratified by the founder after the fact: the first implementation slice, **E44.7 #1831** (regional cells — per-region stores, account→region directory, region-scoped model inference), had already merged against this Recommendation under the ADR's own "implementation proceeds against the Recommendation" clause. Ratification records the decision the shipped code already assumes; no amendment was required, so no slice re-plans.

Process note: an ADR whose implementation ships before its Decision is recorded inverts the intended order. The clause that permitted it was deliberate and bounded, but the decision should be recorded at the point the first slice is dispatched rather than retroactively — otherwise the audit trail shows code merged against an unratified decision. Worth a convention: an ADR permitting implementation-ahead-of-ratification names the slice that forces the ratification deadline.

Named approver: repository maintainer (human).

## Consequences

- New `directory/` module, binary, Dockerfile + go.work wiring, and deploy target; the directory holds slug↔region metadata only, minimizing non-resident data.
- No cell ever writes global state; a cell can be rebuilt/moved without directory schema changes.
- A cell cannot serve two regions; relaxing that later requires a new ADR (per-account endpoint resolution).
- backend takes a compile-time dependency on the public `directory/pkg/handoff` codec (one implementation of the signed handoff; drift between planes is impossible by construction).
- Per-region deploy topology stays human-led (values-&lt;region&gt;.yaml, per-region buckets, .github release wiring).

---

## Amendment A2 (2026-07-22) — corrections from the abandoned first implementation

Run `bc47d2c4` implemented this ADR and was abandoned (PR #2104 closed). The code is discarded; these findings are not. The re-plan of #1831 MUST carry them as binding conditions.

**A2.1 — The pin must be consumed on the paths the directory actually redirects to.** A1 requires the redirect to preserve the original request path (`/v0/onboarding/start`, `/v0/install/callback`, `/v0/login`). The first implementation then mounted the only cell-side verifier at a *separate* `/v0/onboarding/region-pin` endpoint, which no redirect ever targets — so `accounts.home_region` was never stamped in the real flow, and the cross-boundary test passed only because its helper hand-substituted the verifier path. **Decision:** the cell verifies and consumes the `fh_*` handoff parameters via middleware on the routed onboarding surfaces themselves, not on a bespoke endpoint. Path preservation and pin consumption must hold simultaneously. The cross-boundary test MUST drive a Location string emitted by the real directory router — never a hand-built path.

**A2.2 — Nonce roles must be separated and the persisted one must cross the boundary.** The first implementation stored an install-state nonce at `/onboarding/start`, then minted a *different* nonce for the handoff pin and preserved only the caller's original `state`, so the stored nonce appeared nowhere in the redirect and `ConsumeInstallState` could only ever return `ErrNotFound`. **Decision:** state explicitly which nonce serves which purpose (handoff replay bound vs OAuth install-state), and whichever is meant to be consumed later MUST appear in the redirect. A test must assert the persisted nonce crosses the redirect boundary and is consumed — not merely that one row exists.

**A2.3 — The first-write-wins bound must hold by construction.** The cell's `Pin` was check-then-act (read, compare in Go, then an upsert whose `ON CONFLICT` overwrote `home_region` unconditionally), so two concurrent pins could both observe NULL and race. **Decision:** enforce it in SQL — a conditional update (`WHERE home_region IS NULL OR home_region = $n`) or a locking transaction — mirroring the directory's own atomic `AssignRegion`. A concurrent test is required; sequential and fake-backed tests do not discharge this.

**A2.4 — A missing cell region must fail closed.** A cell with a database and a handoff secret but no `FISHHAWKD_HOME_REGION` accepted signed pins for *any* supported region, turning a configuration omission into cross-region persistence. **Decision:** an unset cell region disables the pin surface entirely (refuse, don't accept). The residency self-check may never degrade open.

**A2.5 — `/v0/onboarding/start` needs an authorization decision, not just documentation.** As shipped it was fully unauthenticated: any caller could permanently pin an arbitrary `(provider, account_key)` to a region of their choosing, with the directory signing the attacker's input and no move path afterwards (residency squatting). Documenting the trust assumption was accepted as the minimum in the abandoned run; it is not sufficient. **Decision:** gate the assignment endpoint on an operator credential or a forge-verified identity before any deployment, and treat reachability-as-access-control as an explicitly recorded, time-bound exception if it is retained at all.

**A2.6 — The handoff codec has exactly one owning slice.** Two slices independently implemented `directory/pkg/handoff` with incompatible APIs, producing an unresolvable fan-in conflict (root cause: #2103). **Decision:** in any decomposition, exactly one slice creates the codec and every other slice consumes it; the operator must not add a shared implementation path to the plan-level scope where multiple slices inherit it.
