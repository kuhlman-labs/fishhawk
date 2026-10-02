---
id: ADR-013
title: "Apache 2.0 / BSL boundary for enterprise modules"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/77
supersedes: []
superseded_by: []
applies_to: ["LICENSE.md", "cli/**", "runner/**", "verifier/**", "docs/spec/**", "backend/internal/account/**", "backend/internal/identity/**"]
---

# ADR-013: Apache 2.0 / BSL boundary for enterprise modules

## Context

Per MVP_SPEC §8 (D61–90), the OSS repo is Apache 2.0 on the core, BSL on enterprise modules. The split needs to be defined: which packages are under which license? Which directories?

## Options

- **All-Apache** — friendliest to OSS, hardest to monetize hosted features.
- **Apache core / BSL enterprise modules** — per spec direction; needs clear file-tree boundary.
- **Source-available only (BSL everywhere)** — simplifies licensing; loses OSS positioning.

## Recommendation

Apache core / BSL enterprise (matches spec). Define the boundary as a path-based rule in `LICENSE.md` or per-directory `LICENSE` files.

## Decision

**Accepted (2026-07-26).** **Declare the boundary now; defer the relicensing.** The enterprise/core boundary is defined and the codebase is structured along it, but **everything remains Apache-2.0** until the first designated-enterprise surface ships to a paying customer.

> Not legal advice. Executing the deferred relicensing should be reviewed by counsel before it happens.

### 0. Findings that constrain this decision

**The forcing function has partly already fired.** Several enterprise-shaped surfaces are already in-tree and already Apache-2.0: the `directory` module (ADR-062 regional cells), the `pricing` module, `backend/internal/account`, `backend/internal/identity`, and the output of **23 closed E44 multi-tenancy children**. Code already published under Apache-2.0 **cannot be retroactively relicensed** — anyone may fork today's HEAD and use it under Apache indefinitely. A BSL boundary can only ever bind future versions.

That constraint applies **equally to splitting now and to deferring**, which is what makes deferral cheap: it gives up nothing that splitting today would have preserved.

**The ADR's Day 60 target (2026-06-28) passed four weeks ago.** Recording this decision now closes that gap.

### 1. Posture

The product is pre-alpha with no customers, no revenue, and no enterprise feature anyone is paying for. Adoption is the scarce resource; license friction at this stage costs the thing we most need and protects nothing that exists. So:

- **Now:** the boundary is declared in `LICENSE.md` as a path-based rule, and modules are structured along that seam. Everything ships Apache-2.0.
- **Trigger:** BSL is applied to the designated paths when the **first designated-enterprise surface ships to a paying customer** — not on a date, and not on a feature merely existing. That trigger must be re-reviewed (with counsel) when it fires rather than executed automatically.

Declaring the boundary without relicensing is the substance of this decision: it makes the split an architectural seam maintained from the start instead of a retrofit performed under commercial pressure.

### 2. Designated enterprise surfaces

- **Multi-tenancy and tenant isolation** (E44 / ADR-057): workspace-scoped tenancy, Postgres RLS, per-account audit chains, enterprise membership login gate.
- **Regional cells / directory service** (ADR-062): the `directory` module, region pinning, per-cell inference config.
- **Billing / pricing**: the `pricing` module and any marketplace or billing integration.
- **SSO/SAML and compliance export**: MVP_SPEC §298 already names SSO/SAML a v1+ enterprise-tier feature; ADR-054 (#1582) governs the export posture.

Of these, only **SSO/SAML and compliance export** are genuinely greenfield — the other three already exist under Apache. Those two are where the boundary can bind cleanly from first commit, and should be built behind the seam from the start.

### 3. Permanently Apache-2.0, regardless of posture

These do **not** move to BSL, ever:

- **The external verifier** (`/verifier/**`). A verifier that cannot be independently obtained, read and run would undermine the entire "keeper of the record" claim ADR-056 (#1699) was just ratified to support. Third-party verifiability is worthless if the verification tool is restricted.
- **The workflow spec and schemas** (`docs/spec/**`). The interoperability surface — restricting it would prevent anyone building tooling against Fishhawk workflows.
- **The runner** (`/runner/**`). It executes inside customer infrastructure; a restricted license on code running in someone else's CI is the highest-friction and least enforceable place to put one.
- **The CLI** (`/cli/**`). The onboarding surface; friction here lands before anyone has decided whether they want the product.

### 4. Structural obligation created now

`directory` and `pricing` are already separate `go.work` modules — clean seams that need no work. **Multi-tenancy is not**: it lives inside `backend/internal/account` and `backend/internal/identity`, and `identity` is partly shared with core authentication. Keeping the seam meaningful requires that new enterprise-surface work land behind it rather than diffusing through `backend/internal/`, and that the core/enterprise split inside `identity` be made explicit.

### Related parked decisions

ADR-010 (#74, marketplace billing) and ADR-011 (#75, pricing model) are parked to the design-partner phase and will inform when the relicensing trigger actually fires. This ADR does not depend on either.

Named approver: repository maintainer (human).

## Consequences

_To be recorded._

## Spec reference

`docs/MVP_SPEC.md` §8 (D61–90)

## Target deadline

Day 60


---
Parent epic: #15
