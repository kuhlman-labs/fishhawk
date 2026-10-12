---
id: ADR-099
title: "Crew role naming: operator drives, first officer oversees (amends ADR-080 glossary), nautical titles only for command and oversight, functional names for working roles"
status: accepted
date: 2026-10-12
issue: https://github.com/kuhlman-labs/fishhawk/issues/4331
supersedes: ["ADR-080"]
superseded_by: []
applies_to: []
---

# ADR-099: Crew role naming: operator drives, first officer oversees (amends ADR-080 glossary), nautical titles only for command and oversight, functional names for working roles

## Context

ADR-080 (#3693, accepted 2026-09-26) set the crew vocabulary. Its glossary defines **First officer** as "The operator agent (ADR-040)". The charter, `docs/BRAND_FOUNDATIONS.md` and `docs/MVP_SPEC.md` followed it.

Two later ADRs use the same title for a different role:
- **ADR-089** (#4149, accepted 2026-10-08) uses "first officer" for a **structurally read-only overseer seat**, distinct from the driver.
- **ADR-097** (#4299, accepted 2026-10-11) makes the operator the orchestrator, with the first officer keeping a standing watch.

Neither amended ADR-080's glossary. The record therefore uses one title for two roles. The charter also lists the operator as "first officer" and has no row for the overseer seat at all.

Separately, crew role names mix two styles. Some are nautical: captain, first officer, chief engineer, security officer. The rest are functional: planner, implementer, reviewers, acceptance, operator, groomer, product manager, release manager, ops, architect, historian, comms. Code identifiers are already functional (the crew-message role enum: `planner`, `security`, `reviewer`, …).

## Options

- **A. Keep ADR-080's meaning.** The operator stays "first officer", and the overseer seat is renamed. This contradicts two accepted ADRs and the captain's usage.
- **B. Adopt ADR-089/097's meaning, and set a naming rule (RECOMMENDED).** Nautical titles cover command and oversight only. Working roles take functional names.
- **C. Theme every role** (boatswain, purser, …). This cuts against the brand voice (direct, technical, generic nautical words only) and against discoverability.

## Recommendation

Option B.

- **D1 Glossary amendment to ADR-080.** **Operator** is the agent that drives runs and campaigns (ADR-040, ADR-097). **First officer** is the structurally read-only crew seat that keeps a standing watch over runs and the host, and raises evidence-backed findings (ADR-089, ADR-097). ADR-080's "First officer = the operator agent" row is superseded.
- **D2 Role naming rule.** Nautical titles are reserved for command and oversight: **captain** and **first officer**, plus the framing nouns crew, bridge, log and standing orders. Every working crew role takes a plain functional name: planner, implementer, reviewer, acceptance, operator, groomer, product manager, release manager, ops, architect, historian, security, upkeep, comms. Identifiers in code, the API and the workflow spec always use the functional lowercase name (`first_officer`, `security`, `upkeep`).
- **D3 Renames.** Chief engineer (E79) becomes **upkeep**. Security officer (E80) becomes **security**.
- **D4 Where it lives.** The rule goes in `docs/BRAND_FOUNDATIONS.md` § Product vocabulary. The charter crew table and `docs/MVP_SPEC.md` reflect D1 and D3.

## Decision

**Accepted (2026-10-12). Ratified by the Captain as recommended.** Adopt Option B with D1–D4 as written:

- **Operator** drives runs and campaigns.
- **First officer** is the read-only overseer seat.
- Nautical titles are reserved for command and oversight (captain, first officer). Working roles take functional names, and identifiers are always the functional lowercase name.
- Chief engineer is renamed **upkeep**; security officer is renamed **security**.

ADR-080's "First officer = the operator agent" vocabulary row is superseded. The documentation change shipped as PR #4332 (67a5c4c8).

## Consequences

- One title, one role. The charter's crew table gains the first officer, carried by E85, beside the shipped operator.
- Working roles read as what they do. The metaphor stays where it explains authority.
- Epic titles for E79 and E80 are updated to match.
- This partially supersedes ADR-080's vocabulary table (the First officer row) and leaves the rest of ADR-080 unchanged.

## Relations

Amends ADR-080 (#3693). Related: ADR-040 (#997), ADR-089 (#4149), ADR-097 (#4299), E79 #3700, E80 #3701, E85 #4141.
