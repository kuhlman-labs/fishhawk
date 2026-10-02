---
id: ADR-051
title: "Release governance: evidence-derived release notes + delegating publish"
status: accepted
date: 2026-07-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/1579
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-051: Release governance: evidence-derived release notes + delegating publish

## Context

The SDLC loop now covers track → plan → implement → review → accept → deploy (E31 complete). Release/publish is the remaining gap. Three signed, tag-triggered release pipelines already exist (backend-release.yml, mcp-release.yml, runner-release.yml: cosign keyless signatures, SPDX SBOMs, GitHub Releases) but their release-notes bodies are STATIC files (.github/release-notes/<mod>.md). Meanwhile the audit chain uniquely knows what shipped and why: per merged PR it holds the approved plan, both reviewer verdicts, acceptance outcome + evidence, deferred concerns, and rolled-up cost (cost.go's merged-PR aggregation). Nothing consumes that for release artifacts. Evidence-derived release notes — every changelog line linked to its plan/review/acceptance evidence — are something only Fishhawk's provenance can generate trustworthily, and a natural extension of the audit-log-as-central-artifact positioning (MVP_SPEC line 198).

## Options

**Option A — new `publish` stage type.** A first-class workflow stage for publication. Pros: uniform stage semantics. Cons: schema+orchestration work for what is mechanically a delegation; violates the governance-envelope posture (MVP §9: Fishhawk does not own pipeline mechanics); the existing release pipelines already do the publishing well.

**Option B — no new stage type: backend release-evidence service + `release_notes` artifact, publication stays delegated (RECOMMENDED).** A backend service assembles merged-run evidence between two refs/releases into a ReleaseEvidence model; a renderer produces evidence-linked notes persisted as a new `release_notes` artifact kind; the existing `release` workflow's delegating deploy stage triggers the tag pipelines unchanged; after the pipeline publishes, the backend updates the GitHub Release body via the App API (avoiding the human-led .github/** surface entirely) and records a `release_published` audit entry. Mirrors ADR-038's delegating posture exactly. A semver-bump recommendation derives from the release's recorded change classes; the operator decides.

**Option C — external tooling (changelog generators).** Pros: zero product work. Cons: loses the evidence linkage that is the entire differentiated value; generic commit-message changelogs are strictly worse than what the audit chain can produce.

## Recommendation

Option B. No new stage type, no pipeline ownership: an evidence-assembly service + `release_notes` artifact + post-publish GH Release body update via the App, keeping publication delegated to the existing signed pipelines. Implementation epic: E33 (filed alongside this ADR) with six children covering the evidence query, artifact+renderer, publish integration, semver hint, operator verbs, and an end-to-end dogfood release.

## Decision

**ACCEPTED 2026-07-09 — Option B** (founder decision, recorded by the operator-agent). No new stage type; publication stays delegated to the existing signed tag pipelines. Ship the backend release-evidence service, the `release_notes` artifact kind + evidence-linked renderer, the post-publish GitHub Release body update via the App API with a `release_published` audit entry, and the advisory semver-bump recommendation. Rationale: the delegating posture mirrors ADR-038 and the MVP §9 governance-envelope — Fishhawk owns the gates and the record, not pipeline mechanics — and evidence-derived notes are the audit chain's differentiated value made visible.

Implementation conditions accepted with the decision: (1) the post-publish Release-body update must be operator-retriable and surfaced in `next_actions` from day one (E33.3), never a silent best-effort side-effect; (2) evidence assembly renders loop-bypassing PRs honestly with reduced evidence and says so — never fabricates (E33.1/E33.2); (3) the releases-write App permission is inventoried before enabling (founder action at E33.3). Implementation: E33 (#1583), children #1586–#1591.

## Consequences

Positive: completes the SDLC loop's release arc using the product's unique asset; release notes become audit evidence themselves; per-release cost reporting falls out of existing data. Negative/risks: GH Release body updates need a releases-write App permission (inventory before enabling); evidence assembly spans refs→PRs→runs and must handle runs that bypassed the loop (human-led PRs appear with reduced evidence — render honestly, never fabricate); notes for repos with mixed loop/non-loop history will be partial by construction and must say so.
