---
id: ADR-011
title: "Pricing model (per-engineer vs. per-run)"
status: proposed
issue: https://github.com/kuhlman-labs/fishhawk/issues/75
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-011: Pricing model (per-engineer vs. per-run)

## Context

Pricing model affects how customers value Fishhawk and how Fishhawk's revenue scales with usage. Per MVP_SPEC §10 #4, leaning per-engineer.

## Options

- **Per-engineer / month** — predictable for buyers; scales with team growth; aligns with how sister tools (Linear, Vercel, etc.) are priced.
- **Per workflow run** — usage-aligned; harder for buyers to forecast; can disincentivize use.
- **Hybrid** — base seat fee + usage tier above a threshold.

## Recommendation

Per-engineer for v0 paid tier. Most buyer-friendly, easiest to communicate.

## Decision

_To be recorded._

Status confirmed by the Captain 2026-10-23 as **proposed**: open and deferred ("This ADR remains parked for *market* signal").

Revisit trigger set by the Captain 2026-11-14: Fishhawk stays free, Apache-2.0 and local-only through the solo-captain alpha, with no billing. This record stays **proposed** until beta entry, or the first inbound request to pay for Fishhawk or to have it hosted, whichever comes first. The hypothesis to test then is that the paid offer is the hosted control plane and team features, within ADR-013's designated-enterprise boundary.

## Consequences

_To be recorded._

## Spec reference

`docs/MVP_SPEC.md` §10 #4

## Target deadline

Day 60


---
Parent epic: #15
