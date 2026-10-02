---
id: ADR-072
title: "Customers bring their own model credentials (BYOK): inference cost sits with the customer, Fishhawk charges for the governance layer"
status: accepted
date: 2026-07-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/2296
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-072: Customers bring their own model credentials (BYOK): inference cost sits with the customer, Fishhawk charges for the governance layer

## Context

ADR-010 (#74, marketplace billing) and ADR-011 (#75, pricing model) were both filed on Day 1 and parked to the design-partner phase. **Neither asks who pays for inference** — ADR-010 is about payment rails, ADR-011 about pricing dimension. That question sits underneath both, and there is now data that makes it urgent.

### The cost data changes ADR-011's recommendation

ADR-011 recommends per-engineer pricing as "most buyer-friendly, easiest to communicate." That was written with no cost history. There are now **894 costed runs**: median **$6.76 per issue**, IQR $3.43–$14.69, max $104.04.

A 10-engineer team pushing 200 issues/month costs roughly **$1,350/month in inference**. At a plausible $50/seat that is $500 revenue against $1,350 COGS — break-even needs about **$135/engineer/month** before any margin, at one plausible usage level, with a long tail above it.

The structural problem: **seats do not move, usage does.** Per-engineer pricing with bundled inference scales costs against a variable revenue is blind to. The same team could run 50 issues or 800.

**Per-engineer is a good instinct that only works if Fishhawk does not bear inference cost.**

### A governance-specific argument against marginal pricing

ADR-011 notes per-run pricing "can disincentivize use" and treats it as a forecasting inconvenience. For a *governance* product it is a correctness problem: **if governing a change costs money at the margin, someone under deadline pressure routes around it** — precisely what `METHODOLOGY.md` §"No founder bypass under pressure" exists to prevent. Governance must be cheap at the margin or people avoid it.

### What exists today

Model credentials are **deployment-scoped environment variables**: `FISHHAWKD_ANTHROPIC_API_KEY`, plus the region-scoped `FISHHAWKD_MODEL_API_KEY` / `FISHHAWKD_MODEL_BASE_URL` pair introduced for regional cells (ADR-062), which already implements careful withholding — the default key is deliberately never sent to a non-default endpoint.

There is **no per-account credential storage**. `accounts` and `account_members` exist; no secrets table does.

Inference is consumed on two paths: the **runner** (stage agents, customer-side or Fishhawk-hosted) and the **backend** (the `anthropic` reviewer adapter, which runs control-plane-side).

## Options

1. **Bundled** — Fishhawk buys inference and prices it into a seat or platform fee. Rejected on the arithmetic above: unbounded margin exposure against a variable revenue cannot see, worsening exactly as customers adopt more deeply.

2. **BYOK** — the customer supplies model credentials and pays their provider directly; Fishhawk charges for the governance layer. **Recommended.**

3. **Metered pass-through** — Fishhawk buys inference and rebills it with margin. Removes the exposure but adds billing machinery, disputes over estimated-vs-actual token costs, and makes Fishhawk a reseller of something it does not add value to. It also re-introduces the marginal-cost bypass incentive above.

4. **Hybrid: BYOK by default, bundled as a convenience tier.** Defensible later; premature now. It reintroduces the margin problem for whichever tier bundles, and there is no evidence yet that buyers want it.

## Recommendation

**Option 2, BYOK.** Four reasons specific to this product rather than general:

1. **The plumbing largely exists.** Credentials are already externally supplied and already flow to the runner; ADR-062 already built region-scoped credential/endpoint pairing with withholding semantics.
2. **The value sold is governance, not inference.** Fishhawk is explicitly *not* a coding agent (`README.md`). Charging for the envelope while the customer buys their own tokens matches what is actually delivered.
3. **It strengthens the residency story.** ADR-057 made data residency a hard requirement; BYOK keeps model calls under customer control, in their region, on their contract.
4. **E11's target buyers frequently require it.** Compliance-conscious organizations often hold negotiated provider agreements and will not route inference through a vendor.

**Consequence for ADR-011:** with inference off Fishhawk's books, its per-engineer recommendation becomes viable rather than dangerous. That ADR stays parked for market signal, but its blocking risk is removed.

**Pair it with generous included volume** on whatever the governance-layer price becomes, so the marginal cost of governing one more change is effectively zero.

### Open sub-decisions

- **Credential scope.** Per-account encrypted storage is the right end state; per-cell/per-deployment environment configuration ships sooner and may be sufficient for a handful of design partners.
- **The reviewer path.** Reviews run **backend-side**. Charging them to the customer's key is consistent but means the control plane holds and uses customer credentials; Fishhawk absorbing review cost is cleaner security but reintroduces a small bundled cost.
- **Model allow-list.** The deployment currently validates a resolved model against an allowed set. Under BYOK the customer's key also determines reachable models — whose list governs, or is it the intersection.
- **Cost tracking semantics.** `cost_usd_total` stays computed from token counts × the `pricing` table, but becomes **attribution** (what this cost *you*) rather than billing. Arguably more valuable to the customer, and it makes E59's forecasting a customer-facing feature rather than an internal margin tool.

## Decision

**Accepted (2026-07-27).** Adopt **Option 2 — BYOK**. The customer supplies model credentials and pays their provider directly; Fishhawk charges for the governance layer and buys no tokens. Three sub-decisions were settled; where they refine the Recommendation, **these govern**.

### 1. Credential scope: per-cell for beta, per-account storage after

Design partners get their own cell with their key in environment configuration, reusing the ADR-062 region-scoped pattern that already exists. Per-account encrypted storage lands when partner count outgrows per-cell isolation.

**Consequence: BYOK is NOT beta-blocking.** Under per-cell isolation it requires essentially no code — set that cell's `FISHHAWKD_ANTHROPIC_API_KEY` (and the runner's key) to the customer's credential and all inference is already on their contract. It is a configuration and documentation change.

### 2. All inference on the customer's key, including backend-side reviews

Reviews run control-plane-side, and they bill to the customer's key like everything else. Fishhawk buys no tokens at all — the cleanest possible story, and zero retained inference cost.

**This carries a real security consequence, and the per-cell decision defers it exactly.** The control plane holds and *uses* customer credentials for outbound calls, so the blast radius of a control-plane compromise includes customer keys. Under **per-cell isolation that blast radius is one customer** — a cell holds one key and serves one tenant, which is why decisions 1 and 2 compose safely today.

**The risk arrives with per-account storage**, when one control plane holds many customers' credentials and uses them for outbound calls. That is a materially different threat model from passing a key through to a runner, and it is a **binding constraint on the future storage work**: encrypted at rest, never logged, never in a trace bundle, rotatable, revocable, and scoped so a single compromise does not yield every tenant's key. Record it there rather than rediscovering it.

### 3. Model allow-list: intersection, enforced asymmetrically

A model must be in the deployment's allowed set **and** reachable by the customer's key. Fishhawk retains the ability to exclude models its prompts do not handle or that have no `pricing` table entry (which would silently contribute $0 to cost attribution).

**In practice the two halves are enforced at different times**, and this should be designed for rather than discovered: the deployment allow-list is checkable **up front**; customer key access cannot be enumerated without attempting a call, so it surfaces **at call time**. The failure message must make clear which side refused — "this deployment does not permit model X" and "your provider credential cannot reach model X" are different problems with different fixes, and the customer can only act on the second.

### Consequence for ADR-011 (#75)

With inference off Fishhawk's books, per-engineer pricing becomes viable rather than dangerous. That ADR stays parked for market signal, but the margin risk that the cost data exposed is removed. Pair whatever price emerges with **generous included volume**, so the marginal cost of governing one more change stays effectively zero and nobody has a financial reason to route around a gate.

### Consequence for beta

Design-partner billing is trivial: partners bring a key, are charged nothing, and the entire pricing decision defers until there is usage data from real teams rather than from one founder. **E59.3 (advisory plan-gate forecast) stays out of beta scope** — partners see their own provider bill, so cost visibility is valuable but not blocking.

Named approver: repository maintainer (human).

## Consequences

Fishhawk's revenue decouples from inference cost, removing the margin exposure that made per-engineer pricing dangerous and eliminating the incentive to price governance at the margin. The residency story improves. Design-partner billing during beta becomes trivial — partners bring a key and are charged nothing — which defers the entire pricing decision until there is usage data from real teams rather than from one founder.

Costs and new surface:

- **Per-account credential storage** does not exist and is real work: encrypted at rest, rotatable, revocable, never logged, never in a trace bundle.
- **New customer-facing failure modes**: an invalid, expired, rate-limited or quota-exhausted customer key fails runs, and the error must be actionable by the customer rather than looking like a Fishhawk fault.
- **Fishhawk loses direct control of model choice as a cost lever** — ADR-070's Tier 3 model-selection idea becomes advice to the customer rather than an action Fishhawk takes.
- **Support burden shifts**: "my runs are failing" may now be a provider quota problem, and diagnosis must make that obvious.
- Onboarding gains a step — the getting-started guide (#2262) and the partner playbook (#2257) both need it.
