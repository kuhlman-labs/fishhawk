---
id: ADR-020
title: "Plan review surface: post the plan on the originating issue"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/321
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-020: Plan review surface: post the plan on the originating issue

## Context

Per ADR-019, Fishhawk becomes a coordination layer that surfaces actions where developers live. The plan-review surface is the most visible "destination" we have today: when a plan stage's artifact lands, the SPA renders it as a "plan document" with an approval panel, and reviewers must visit `/runs/<id>/stages/<plan-stage-id>` to read and approve.

The workflow spec already anticipates a GitHub-side surface — `persistence: [target: originating_issue, mode: rendered_comment, update_on_change: true]` is parsed in `backend/internal/spec/spec.go` but has no consumer. `issuecomment.Notifier.NotifyPlanReady` posts a *summary* of the plan to the issue today, but the canonical surface remains the SPA.

We need to choose the GitHub-side primary surface for plan review before E17 starts coding. The choice affects: how reviewers comment (threaded inline vs single-comment-thread), how approval signal lands (reaction / slash / PR action), and how plan-update-on-revision works.

## Options

### A. Plan-as-issue-comment-thread

The full plan document lands as a comment on the originating issue. Reviewers reply in the same thread; approval comes via `+1` reaction on the plan comment (or `/fishhawk approve` slash command, already wired). When the plan re-uploads, the comment edits in place.

**Pros**: The originating issue is where the work was framed; the conversation continues in its natural locus. Reuses subscriptions (`issue_comment`) + the existing notifier. Reactions are GitHub-native and require no PR. Reviewers see the whole conversation (trigger → plan → discussion → approval) in one place.

**Cons**: GitHub doesn't render inline code-style line-by-line review on issue comments. Reviewers commenting on specific plan steps reference them by paraphrase.

### B. Plan-as-draft-PR-body

Open a draft PR before code is written; the plan lives in the PR body / a `PLAN.md` committed to the branch. Reviewers comment line-by-line. Approval is GitHub's "approve" button on the PR. When the implement stage runs, it pushes code to the same branch and converts the PR to ready-for-review.

**Pros**: GitHub's PR review surface is the richest comment experience — line-level threads, suggestions, resolved/unresolved state.

**Cons**: Awkward. A PR "with no code, just a plan" is a misuse of the surface — PRs imply diffs, branch protection rules don't know what to do, the "Files changed" tab is empty or shows the synthetic plan-file. Also: it splits review across two PRs (plan-PR + implementation-PR) for the same logical change. Linear and Sentry-style products have tried PR-shaped plans; the consensus has drifted toward issue-shape.

### C. Hybrid

Plan-as-issue-comment-thread for the primary review surface; a non-merge-able "plan reference" PR auto-opened against a placeholder branch for line-by-line commenting if reviewers want it. Reviewers can pick the surface that matches the depth of feedback they want.

**Pros**: Optionality.

**Cons**: Two-surface complexity for marginal benefit. Approval-signal ambiguity (which surface wins?). v0 doesn't have a customer asking for line-level plan commenting yet.

## Recommendation

**A** — plan-as-issue-comment-thread.

The originating issue is the conversation's natural locus. Reactions are the lightest-weight approval signal and require zero new UI. The SPA's plan-document page becomes a read-only mirror of the comment (same flip ADR-018 did for the review stage). The existing `NotifyPlanReady` + `update_on_change` spec intent are the foundation; the impl is extension, not rebuild.

When a customer surfaces a real need for line-level plan commenting, file a follow-up — likely a SPA-rendered annotation surface backed by the audit log, not a forced PR shape.

## Decision

Adopt option **A**.

## Consequences

- `NotifyPlanReady` extends to post the full plan document body (today posts a summary). The plan's standard_v1 fields render as a markdown doc.
- `update_on_change: true` spec field becomes consumed: subsequent plan uploads edit the existing comment via `UpdateIssueComment` (filed as part of E20 / E17).
- New webhook subscription: reaction events on the plan comment. `+1` from a configured approver fires the approval path with `approval.Surface = github_reaction`.
- New audit category: `plan_approved_via_reaction`.
- SPA's plan-document page becomes a read-only summary, with a "View on GitHub" affordance pointing at the plan comment URL.
- Workflow spec doc (`docs/spec/workflow-v0.md`) updates to document the new flow + the `persistence.originating_issue` semantic.
- Existing `/fishhawk approve` slash command stays wired (provides a typing-fallback when reactions aren't enough — e.g., approval-with-comment).
- HTTP approval surface (`POST /v0/stages/{id}/approvals`) stays wired for the SPA + CLI (E18).

## Out of scope

- Line-level plan commenting via PR shape. File a follow-up if customers ask.
- Plan-as-PR for high-autonomy workflows (routine_change). Implement stages still open PRs; review stages still get them. Only the plan-review surface moves.
- Migration of in-flight plans. Existing runs whose plan is mid-review continue with the SPA flow; new runs use the new flow. Detection: the spec's `persistence.originating_issue` flag.

## Related

- ADR-019 — the umbrella coordination-layer decision this is a sub-decision of.
- ADR-018 / #311 — review-stage approval moved to PR merge. Direct precedent.
- ADR-017 / #249 — CI gating moved to branch protection. Same pattern.
- #234 — original "comment back on the issue" feature; this builds on it.
- E17 / #323 — the impl track this ADR unblocks.
