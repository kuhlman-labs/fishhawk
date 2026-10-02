---
id: ADR-010
title: "Marketplace billing path (GitHub vs. direct)"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/74
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-010: Marketplace billing path (GitHub vs. direct)

## Context

Customers install Fishhawk from GitHub Marketplace (MVP_SPEC §10 #8). Billing can flow through GitHub or be handled directly. Each path has compliance and conversion implications.

## Options

- **GitHub Marketplace billing** — friction-free for customers (charged on their existing GitHub bill); GitHub takes a cut; less control over plans, trials, and discounts.
- **Direct billing (Stripe)** — full control over plans/trials; introduces customer-facing payment friction; can run alongside Marketplace listing.
- **Hybrid** — Marketplace for trial / SMB; direct for enterprise.

## Recommendation

Through GitHub for early-stage convenience, per MVP_SPEC §10 #8. Revisit for enterprise pricing.

## Decision

_To be recorded._

## Consequences

_To be recorded._

## Spec reference

`docs/MVP_SPEC.md` §10 #8

## Target deadline

Day 45


---
Parent epic: #15
