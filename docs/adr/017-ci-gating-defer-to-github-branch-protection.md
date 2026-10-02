---
id: ADR-017
title: "CI gating: defer to GitHub branch protection, decouple approval from merge"
status: accepted
date: 2026-05-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/249
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-017: CI gating: defer to GitHub branch protection, decouple approval from merge

## Context

The workflow spec (\`.fishhawk/workflows.yaml\`) currently enumerates required CI checks per gate via \`blocking_checks: [ci_pass, fishhawk_audit_complete]\`. The customer also configures GitHub branch protection with its own required-status-check list. The two lists must agree; in practice they drift — PR #248 fixed exactly this (the spec said \`ci_pass\`, GitHub's check name was \`CI Pass\`, the gate broke silently).

Fishhawk's gate also today refuses approve when checks aren't green, coupling the human approval moment to CI state. This adds audit-log fidelity ("approved with these checks green at the time") at the cost of conflating two separate concerns: "this work matches the plan" and "the change is mergeable."

The dogfooding pass surfaced both problems together: the spec's enumerated list is a sync hazard, and the coupling means a passing approval can't happen until CI clears, even if the reviewer is ready.

## Industry research

Convergent design across mature PR automation tools:

- **Kodiak**: "Kodiak should only wait on status checks that are marked as required in branch protection settings." Refuses to operate without branch protection enabled. No tool-side enumeration.
- **Mergify**: legacy \`check-success=foo\` config + newer "Merge Protections" feature where Mergify posts itself as one rollup check; convergence direction is away from enumeration.
- **bors-ng**: explicit-list outlier; its complications (\`status_wait_success\`, optimistic-update) are evidence the explicit-list model doesn't survive contact with real CI.
- **Dependabot/Renovate**: use \`gh pr merge --auto\`, which inherently respects branch protection's required-check list. No separate enumeration.
- **GitHub auto-merge**: triggers when "all required reviews are met and all required status checks have passed" — required = whatever branch protection says.

Convergent failure modes: explicit enumeration drifts; "all checks must pass" treats optional integrations as load-bearing (CodeQL scans, deploy previews, third-party SaaS); pending checks are subtle and need cope mechanisms.

## Options

### Source of truth for required checks

- **(A) Keep \`blocking_checks\` in the spec**: explicit, but drifts.
- **(B) "All checks must pass"**: zero config, but treats every random integration as load-bearing.
- **(C) Defer to branch protection**: read \`required_status_checks.contexts\` from GitHub's branch protection / rulesets API at run-create time. Customer's curated list is the authoritative source.

### Fallback when branch protection isn't configured

- **(D) Refuse to dispatch**: secure-by-default. Operator must set protection before Fishhawk works.
- **(E) Fall back to internal-only**: dispatch using \`fishhawk_audit_complete\` as the only gate. Onboarding-friendly but lets a misconfigured repo skip CI gates entirely.

### Approval gate coupling

- **(F) Keep coupling**: gate refuses approve when checks aren't green. Audit moment is "approved-with-CI-green."
- **(G) Decouple**: reviewer approves Fishhawk based on plan/diff regardless of CI; GitHub branch protection blocks the merge until checks pass; audit moment is reconstructible from the chain ("approval at T1 + last green check at T2"). Matches how teams actually work.

### Fishhawk-internal checks

The \`fishhawk_audit_complete\` check is Fishhawk-derived, not GitHub CI. Per #231 we already publish it as a real GitHub Check Run. Customers can mark it required in branch protection just like any other check — meaning option (C) handles it transparently.

## Recommendation

**(C) + (D) + (G)**:

1. **Drop \`blocking_checks\` from the workflow spec.** Read GitHub branch protection's \`required_status_checks.contexts\` (and rulesets' equivalent) at run-create time as the authoritative list.
2. **Refuse to dispatch when no branch protection is set.** Strict, secure-by-default. Customer must configure protection before Fishhawk runs.
3. **Decouple approval from merge.** Fishhawk approval gate only checks "is the plan/diff approved." GitHub branch protection is the merge gate. High-autonomy workflows (\`routine_change\`) become "queue \`gh pr merge --auto\`; let GitHub do the rest."
4. **Bump App permissions** to include \`administration: read\` (or the newer \`repository_administration: read\` scope) so Fishhawk can query branch protection.

## Decision

**Recorded 2026-05-09.** Adopt (C) + (D) + (G).

- **Source of truth**: GitHub branch protection + rulesets. Read both, take union of \`required_status_checks.contexts\`. Cache per-run; re-read on \`branch_protection_rule\` or \`repository_ruleset\` webhooks.
- **Fallback**: refuse to dispatch when no branch protection covers the target branch. Webhook receiver writes a category-B audit entry with \`failure_reason="branch protection not configured for <branch>"\` and the run never starts. Empirically Kodiak's posture; safe by default.
- **Approval gate**: drops the \`checkBlockingChecks\` enforcement on the approval-handler path. Fishhawk approval becomes "is the plan reviewed and approved." Status-check state is recorded in the audit log via #228 ingestion as it always was, but doesn't gate the approval transition.
- **Fishhawk-internal checks**: published as real GitHub Check Runs (already done for \`fishhawk_audit_complete\` per #231). Customer marks them required in branch protection alongside their CI checks. Spec doesn't mention them.
- **Permission bump**: App manifest gains \`administration: read\`. Existing installs re-accept — pre-alpha posture per project lead.
- **\`routine_change\` workflow**: uses GitHub auto-merge after the agent opens the PR. Fishhawk doesn't run a separate merge step; the customer's branch protection + auto-merge handles the gating + timing.

## Consequences

**Easier**:
- One source of truth. The spec → branch protection drift class of bug (PR #248) is gone by construction.
- Customer onboarding is simpler: configure branch protection once.
- The \`routine_change\` flow simplifies — GitHub's machinery handles the merge.
- The "what about all checks pass" question is answered cleanly: defer to the curated list, never the union.

**Harder**:
- Existing installs must re-accept the permission bump. One-time pain.
- Customers without branch protection can't use Fishhawk until they configure it. Onboarding gets a hard gate. Not a problem at v0; could be a friction point at scale.
- Reading branch protection requires admin-level repo permission; some compliance-conscious customers may push back. Document the rationale.
- Decoupling approval from CI loses the "approved-with-CI-green" audit moment. Can be reconstructed from the chain (approval entry timestamp + most recent \`check_run\` ingest entry); not lost, just not point-in-time.
- Need to handle both branch protection (classic) and rulesets (newer) APIs; a customer might have both, with overlapping or conflicting contexts. Take the union.

## Children

Order roughly reflects build order: the manifest bump unblocks the backend read; the read unblocks the gate-removal; the gate-removal unblocks the spec drop; auto-merge and SPA rename land alongside the rest.

- [ ] #252 — App manifest: add `administration: read` permission
- [ ] #251 — Read branch protection at run-create time; snapshot on the run
- [ ] #253 — Drop checkBlockingChecks enforcement from the approval handler
- [ ] #254 — Drop `blocking_checks` from the workflow spec; bump spec to 0.2
- [ ] #255 — Routine_change: switch to `gh pr merge --auto` after the PR opens
- [ ] #256 — SPA: rename "blocking checks" → "required checks"; source from branch protection


## References

- Triggering bug: PR #248 (spec→display-name mismatch).
- Existing check-run ingestion: #228.
- Existing audit-complete derivation + Check Run publish: #229, #231.
- Industry research:
  - [Kodiak: Configuration Reference](https://kodiakhq.com/docs/config-reference) — defers to branch protection
  - [Mergify: Merge Protections](https://docs.mergify.com/merge-protections/) — single rollup check pattern
  - [bors-ng: status_wait_success RFC](https://bors.tech/rfcs/0360-add-a-new-configuration-option-status-wait-success) — coping with explicit-list limits
  - [Dependabot issue #2661](https://github.com/dependabot/dependabot-core/issues/2661) — "all checks pass" footgun
  - [GitHub Docs: Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)

Parent epic: #15 (ADRs).
