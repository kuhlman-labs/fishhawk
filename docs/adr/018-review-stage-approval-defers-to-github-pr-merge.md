---
id: ADR-018
title: "Review-stage approval defers to GitHub PR merge"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/311
supersedes: []
superseded_by: ["ADR-031"]
applies_to: []
---

# ADR-018: Review-stage approval defers to GitHub PR merge

## Context

The review-stage detail page today still surfaces an in-Fishhawk approval panel: the workflow spec declares `gates: [approval]` with an `approvers.any_of: [founder]` list, the SPA renders that as an action surface, and slash commands `/fishhawk approve` / `/fishhawk reject` advance the stage. Meanwhile GitHub branch protection independently gates the merge — required reviewers, required status checks, the `fishhawk_audit_complete` Check Run. The two surfaces overlap on a single moment ("can this PR ship?") with no clear allocation.

ADR-017 / #249 already moved the **merge gate** to branch protection: CI checks left the Fishhawk spec; the approval handler stopped refusing-on-CI; the merge-readiness signal lives on GitHub. The review-stage approval gate is the last piece of the old model still firing in Fishhawk's UI. The plan-stage approval is different — it gates intent before any code is written, and there's no GitHub-side equivalent.

PR #307 made this concrete: a reviewer approves the PR on GitHub, GitHub blocks merge until CI clears, the PR merges. Fishhawk's review-stage gate never fires from the PR-side actions; it sits in `awaiting_approval` until someone separately clicks the SPA's approve button or comments `/fishhawk approve`. The labeler has to act twice for one decision.

## Options

### A. Keep the review-stage approval as today

The workflow author can declare role-based approvers in the spec; Fishhawk enforces independently of branch protection. Two approval surfaces overlap; the user acts twice; the audit log records both events.

**Pros**: Workflow-spec `approvers.any_of: [<role>]` is enforceable by Fishhawk's role resolver — finer-grained than branch protection's required-reviewers if the team's policies don't map to a flat list.

**Cons**: Drift hazard (branch protection's approvers and Fishhawk's `approvers` list must stay in sync); duplicate UI; labeler-friction.

### B. Review-stage approval becomes informational; PR merge IS the success signal

Webhook listens for `pull_request.closed` with `merged: true` → looks up the Fishhawk run by `runs.pull_request_url` → transitions the review stage to `succeeded` and writes an audit row naming the merger. Webhook also listens for `pull_request_review.submitted` to record approver/reviewer events in the audit log (who approved on the PR, what they said, when). The SPA's review-stage panel renders the same data as a read-only summary.

The workflow spec's `gates: [approval]` for review stages becomes advisory: Fishhawk records the declared approver list but doesn't enforce it independently. Teams that want strict approver enforcement configure branch protection's required-reviewers — that's where the merge gate already lives.

**Pros**: Single source of truth (GitHub PR), fewer surfaces, the labeler acts once. Aligns with ADR-017's direction. Audit log captures merge-time facts (merger, reviewers, decisions) ingested from PR-side events. Workflows that don't require approval (auto-merge, `routine_change`) work naturally — the merge event still fires.

**Cons**: The workflow author loses Fishhawk-side enforcement of `approvers.any_of: [<role>]` for review stages. That role-mapping moves to branch protection (where it conceptually belongs, given ADR-017). One transition: existing v0 spec examples that show `approvers.any_of: [founder]` on review stages become informational, which could confuse early readers — needs spec doc updates.

### C. Hybrid: review-stage gate optional, governed by a spec flag

Add `gates.approval.source: github | fishhawk` (or similar) so workflows can choose. Default `github`; opt-in `fishhawk` for teams that want the strict role-list.

**Pros**: Migration path for teams that already lean on `approvers`.

**Cons**: Schema complexity for a knob most workflows won't touch. The "I want stricter approvers than branch protection allows" case is rare; teams that need it can configure branch protection accordingly.

## Recommendation

**B**: review-stage approval defers to GitHub; PR merge is the success signal; PR review events feed the audit log.

Mirrors ADR-017's logic exactly: when GitHub already has an authoritative surface for a gate (merge readiness / approval), Fishhawk records but doesn't enforce. The role-mapping (`founder → @kuhlman-labs`) stays for plan stages — where Fishhawk's vote is independent and meaningful, and GitHub has no equivalent — but doesn't apply to review stages.

For workflows that don't require approval (auto-merge, `routine_change`), the merge event still drives the transition. For workflows that do require approval, branch protection's required-reviewers enforces it; Fishhawk records who reviewed.

## Decision

Adopt option **B**.

## Consequences

- New webhook handlers for `pull_request.closed` (with `merged=true`) and `pull_request_review.submitted`. The receiver already subscribes to `pull_request` events.
- Review-stage approval is removed from the in-Fishhawk approval surface. The approval API rejects calls against review stages; `/fishhawk approve` against an issue whose run is in review stage is a no-op (or a help message). Plan stage approval continues unchanged.
- SPA review-stage detail page becomes read-only — surfaces the PR + required checks + merger / approvers from audit events. No approval panel.
- Workflow spec docs note the `approvers` field is informational for review stages (still parsed + stored on the stage row for audit visibility, just not enforced).
- Audit log gains two new categories: `pr_approved_on_github` (from PullRequestReview events) and `pr_merged` (from pull_request.closed). The `approval_submitted` category continues to record plan-stage approvals.
- Plan stage approval gate unchanged. `/fishhawk approve` / `/fishhawk reject` continue to apply to plan stages.

## Out of scope (file follow-ups if needed)

- ARCHITECTURE.md update lands with the impl PRs, not here.
- Long-running review stages that span multiple PR re-pushes: existing `runs.parent_run_id` threading already handles this for new dispatches; the review stage closes on merge regardless.
- Reject paths: a PR closed without merging fires `pull_request.closed` with `merged=false`. Open question whether that transitions review stage to `cancelled` / `failed` / stays awaiting (handled in the impl issue).

## Related

- ADR-017 / #249 — branch protection owns the merge gate. This ADR is the next step.
- #238 — slash-command approval surface (continues for plan stages).
- #229 / #231 — `fishhawk_audit_complete` Check Run; the audit story stays as-is.
- #216 — `runs.pull_request_url` is the lookup key for the new webhook handlers.

## Impl issues

- #312 — Webhook: pull_request.closed + pull_request_review handlers
- #313 — Approval handler: stop enforcing in-Fishhawk review-stage approval
- #314 — Frontend: review-stage detail read-only summary


## Resolution (2026-05-14)

All impl issues landed:

- #315 / #312 — webhook handlers for `pull_request.closed` + `pull_request_review.submitted`
- #317 / #313 — backend approval prune (HTTP + slash both refuse review-stage submissions; `409 review_stage_managed_by_github`)
- #318 / #314 — SPA review-stage detail page is read-only; `ReviewActivityPanel` renders PR-side audit categories
- #319 / #316 (follow-up) — closed-without-merging cancels the review stage + writes `pr_closed_without_merge`

Audit-log categories introduced: `pr_merged`, `pr_approved_on_github`, `pr_review_submitted`, `pr_closed_without_merge`. Reopen handling stays out of scope; the v0 escape hatch is re-trigger via `/fishhawk run`.
