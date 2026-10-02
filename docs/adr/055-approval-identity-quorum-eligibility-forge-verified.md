---
id: ADR-055
title: "Approval identity: quorum + eligibility predicates + forge-verified identity (GitHub/GitLab-agnostic)"
status: accepted
date: 2026-07-07
issue: https://github.com/kuhlman-labs/fishhawk/issues/1698
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-055: Approval identity: quorum + eligibility predicates + forge-verified identity (GitHub/GitLab-agnostic)

## Context

Every human gate today authorizes against `roles.{role}.members` — a literal handle list in `.fishhawk/workflows.yaml` (`["@your-github-handle"]` since #1695) — matched against the bearer token's `subject`. Three structural problems:

1. **Subjects are asserted, not authenticated.** `fishhawkd token issue --subject <s>` records whatever string the operator typed. Whoever holds a token *is* its subject. For a product whose positioning is *keeper of the record*, the identities in the record are only as good as the authentication behind them — an approval row naming an unverified subject is not evidence.
2. **Named individuals don't genericize.** E36 (#1639) had to replace the founder's handle with a placeholder; every external repo must edit people into its spec, and staffing changes are spec changes. Compliance frameworks don't ask "did Brett approve" — they ask "was it approved by N people with authority who weren't the author."
3. **"Human" is not distinguishable from "agent holding a human's token."** The operator-agent acts under an operator token (ADR-040 `delegated:true`); nothing structurally prevents delegated acts from satisfying a gate meant for a human.

A fourth constraint is forward-looking: **Fishhawk will support multiple git sources** (GitLab planned). Identity is currently implicitly GitHub-shaped (`@handle`, `@org/team`). Whatever we adopt must not braid GitHub into the spec schema, the token model, or the audit rows.

Fishhawk should not become an identity provider. The forge (GitHub, GitLab) already authenticates users, models permissions, and models groups; Fishhawk's job is to *verify against it* and *record faithfully*.

## Options

**A. Status quo + validation.** Keep `members` lists; validate handles exist on the forge at spec-parse time. Fixes typos only. Still asserted subjects, still named individuals, still no human/agent distinction.

**B. Pure quorum.** Spec says `min_human_approvals: N`; any authenticated subject counts; identities recorded. Portable and record-centric — but with asserted subjects, anyone who can mint tokens can stuff the quorum. The record faithfully records meaningless identities. Rejected alone; quorum is the right *spec* shape but needs an authn floor and an eligibility boundary.

**C. Quorum + eligibility predicates + forge-verified identity.** Three separated concerns:
   - **Authentication — delegate to the forge.** Identity is a `(provider, subject)` pair (`github:alice`, `gitlab:bob`), established by minting *user-bound* tokens through the forge's OAuth flow (GitHub device flow; GitLab equivalent), so a token's subject is a forge-verified login, not a typed string. Static operator tokens keep working but their approvals record `auth_method: static`; a workflow can require better.
   - **Authorization — predicates, not people.** The gate declares *how many* and *who is eligible* in forge-neutral vocabulary: `count: 2`, `min_permission: maintain` (mapped per provider: GitHub maintain/admin ⇔ GitLab Maintainer/Owner), `member_of: <org/team | group>`, plus separation-of-duties: `not: [author, agent]` (an approver may not be the change author; a `delegated:true` operator-agent act never counts toward a *human* quorum — the machinery to distinguish exists since ADR-040). Membership/permission resolve against the forge **at decision time**, and the resolution is **snapshotted into the approval row** ("at 19:04Z, github:alice held maintain"). Literal `members:` allow-lists remain valid as one predicate form (back-compat + the small-team case).
   - **Record — richer approval rows.** `identity{provider, subject}`, `auth_method` (oauth | static | artifact), `channel` (interactive | api | delegated), the predicate-evaluation snapshot, timestamp, artifact hash approved. No cryptographic "proof of human" is claimed — the channel is recorded and workflows may require `channel: interactive` where it matters; that is the honest contract.

**D. External IdP (OIDC/SAML).** Enterprise-grade, heavier, duplicates what the forge already knows about repo authority. Deferred — but the provider interface in C must be shaped so an OIDC provider slots in later without schema change.

## Recommendation

Option C, phased:

- **Phase 1 (authn floor + quorum):** `IdentityProvider` interface (`VerifyUser` via OAuth device/web flow, `PermissionLevel(repo, subject)`, `ResolveMembership(ref)`), GitHub implementation first; user-bound tokens (subject = verified forge login, provider-qualified); approval rows gain `identity/auth_method/channel`; spec gates gain `approvals: {count: N, not: [author, agent]}`. #1119's UAT work is subsumed here.
- **Phase 2 (eligibility):** `min_permission` / `member_of` predicates with decision-time resolution + snapshot; provider vocabulary mapping table documented per forge.
- **Phase 3 (optional):** artifact-anchored approvals — the approval *is* a forge-native act (PR review / MR approval) that Fishhawk verifies via API and records; and/or an OIDC provider for non-forge identity.

Schema impact is additive within workflow-v1 (`approvals` block optional; `roles.members` remains valid — treat as a `members` predicate). Token + audit-row changes are additive. The auth-change checklist (AGENTS.md) applies at each phase: impact inventory via `token migrate` before any gate starts *requiring* oauth-verified subjects.

## Decision

**Ratified 2026-07-07 (founder). Option C is adopted**, with the following bindings:

1. **Static tokens: record-and-degrade.** Static bearer tokens keep working at every surface; their approvals record `auth_method: static`. An individual workflow gate MAY require oauth-verified approvals. No forced migration; the auth-change checklist applies if/when any gate tightens.
2. **Preset default gate:** the generic presets ship `approvals: {count: 1, not: [author, agent]}` in place of the placeholder handle list. `roles.members` remains valid as an allow-list predicate.
3. **Implementation scope: Phases 1+2**, tracked by epic **#1705 [E39]** (children #1706–#1711). Phase 3 (artifact-anchored approvals, OIDC provider) is deferred until a design-partner need materializes.
4. **Forge-agnostic from the first commit:** identity is a `(provider, subject)` pair behind an `IdentityProvider` interface; GitHub is the first provider; GitLab must be implementable with no schema change.

## Consequences

- The spec becomes fully portable (E36 goal): no person named in a preset, ever; presets ship `approvals: {count: 1, not: [author, agent]}` instead of a placeholder handle.
- Approval rows become evidence-grade: verified identity + authority snapshot + channel, consumable by the E9 export/verifier chain unchanged (additive fields).
- GitLab support becomes an `IdentityProvider` + vocabulary-mapping implementation, not a redesign; the same seam later serves OIDC.
- Static tokens degrade gracefully but visibly (`auth_method: static` in the record); operators see exactly which approvals carry weaker authn.
- New failure modes to design for: forge API unavailable at decision time (fail closed for gate decisions, with retry guidance in next_actions); permission drift between approval and merge (the snapshot records what was true at decision time — that is the contract).

## Relations

Related: ADR-040 (#997, operator-agent delegation — `delegated` acts excluded from human quorum), #1119 (Projects-token UAT flow — subsumed by Phase 1), E36 #1638 (portable presets are the forcing function), E9/#27 (export + external verifier consume the richer rows), ADR-050 #1540 (agent-identity posture on the runner side).
