---
id: ADR-114
title: "Refinement sessions are optionally repo-scoped at open, and drafting/preview resolve that repo's conventions per repo (not the shipped default)"
status: accepted
date: 2026-10-22
issue: https://github.com/kuhlman-labs/fishhawk/issues/4642
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-114: Refinement sessions are optionally repo-scoped at open, and drafting/preview resolve that repo's conventions per repo (not the shipped default)

## Status

ACCEPTED (ratified by the Captain 2026-10-22). Amended by A1 (token callers), ratified by the Captain 2026-10-23. Filed by the operator on 2026-10-22, from the #4626 plan (run 5b7e4eed), which declared a new architectural decision.

## Context

ADR-110 (#4605) moves repository conventions out of the shipped work-management default (guard (i), #4606/#4626). Today a refinement (`draft_epic`) session has no repository until its file arm pins one, so drafting and preview can only use the deployment-wide default. Once the default is neutral, a session needs to know its repository earlier, or this repository's draft rendering changes.

## Options

- **A.** Keep sessions repo-less, and render drafts with the neutral default until filing. This repository's preview loses its title formats and labels.
- **B.** Make a session optionally repo-scoped at open (`refinement_drafts.repo`, nullable and persisted across revisions), and resolve drafting and preview conventions through the per-repo loader for that repo. A session with no repo uses the shipped default.
- **C.** Require a repo at open for every session.

## Recommendation (as planned in #4626)

**Option B**, with these rules:
- The open-time repo is persisted. The file arm REFUSES a repo that differs from it (409 `refinement_filing_repo_mismatch`, a planner-chosen rule pending confirmation).
- The cookie caller's repo visibility gates both the open arm and the read of a repo-scoped session.
- The loader falls back to the shipped default only on fetch not-found. A fetch error or an unparseable declared file refuses (`refinement_conventions_unavailable`).
- Open question for the Captain: whether a pre-fetch fall-through (no accounts row, unregistered forge, no App installation) may silently use the default, or must degrade with a named reason (ADR-110 rule: a missing declaration refuses or degrades by name, never falls back silently).

## Decision

**Accepted 2026-10-22** by the Captain (repository maintainer): **Option B**, with the Recommendation's rules, including the 409 `refinement_filing_repo_mismatch` refusal when the file arm names a repo other than the session's open-time repo.

The open question is settled by ADR-110: a pre-fetch fall-through (no accounts row, unregistered forge, no App installation) DEGRADES WITH A NAMED REASON and never silently uses the default. The shipped default applies only to a session with no repo, or when the fetch succeeded and the repository declares nothing.

## Consequences

- A migration adds `refinement_drafts.repo`.
- The refinement API gains an optional repo at open, plus new error codes.
- This repository's draft_epic output stays pinned by a golden test.

Related: ADR-110 (#4605), ADR-052, ADR-057, #4626, #4606.

## Amendment A1: token callers (ratified by the Captain 2026-10-23)

**Context.** ADR-114's visibility rule names cookie callers only. Bearer, CLI and MCP token callers follow the product-wide token posture: `repoFilterFor` returns no repository filter for a token, because the repository-permission mirror is keyed on human forge subjects (ADR-057 repository visibility, #2071). The #4626 implement review (#4678 item 1) found that a token with `write:approvals` can therefore open a session naming any repository the App can see. The response reveals whether the App is installed there and the shape of that repository's work-management declaration.

**Options.**
- **A.** State that refinement follows the product-wide token posture, and document the probe.
- **B.** Gate tokens on refinement's open and file arms. This needs a token-to-repository scoping model that does not exist, and it is inconsistent with every other surface.
- **C.** Scope tokens to repositories product-wide. That belongs to a product-wide ADR, not here.

**Decision.** Option A.
- Refinement's open, read and file arms apply to token callers exactly the token posture every other surface applies. ADR-114 adds no refinement-specific token rule.
- The refinement README's Visibility bullet names the probe: a token caller learns a repository's App-installation status and the shape of its declaration.
- This decision does not cover a token reaching a repository outside its own workspace account. If one can, that is a tenancy defect under ADR-057, fixed there, not accepted here.

**Consequences.** Documentation only (the refinement README), tracked in #4678 item 1, which first verifies that an operator token cannot name a repository outside its account. Product-wide token repository scoping, if wanted, is a separate ADR.
