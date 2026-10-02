---
id: ADR-031
title: "Review gate succeeds only on verified landing (platform-neutral, delivery-agnostic)"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/710
supersedes: ["ADR-018"]
superseded_by: []
applies_to: []
---

# ADR-031: Review gate succeeds only on verified landing (platform-neutral, delivery-agnostic)

**Status: Decided (2026-06-03).** Amends/supersedes the review-gate-resolution *mechanism* of ADR-018 (which becomes the `github_merge` provider); keeps its invariant.

## Context

A `feature_change` run is plan → implement → **review**. Per **ADR-018**, review resolution is GitHub-merge-driven: `/v0/stages/{id}/approvals` returns `409 review_stage_managed_by_github` for review stages, and the stage succeeds only on the `pull_request.closed` (merged) webhook. The invariant ADR-018 protects is **"the change was accepted into the codebase"** — implemented with one *delivery* (webhook) on one *platform* (GitHub).

Two problems: (1) the **local dogfood loop parks at review forever** (#702) — no webhook ingress, and `push_and_open_pr=false` runs carry no `pull_request_url` to poll; ~9 runs accumulated `running` at `review awaiting_approval`. (2) the model is **GitHub-coupled**, against the platform-neutrality goal.

A rejected draft added a standalone operator-side "resolve review" endpoint — a second authority that lets a run reach `succeeded` without the change actually landing. That was rejected as weakening the invariant.

## Decision

**A review stage succeeds if and only if the proposed change has *verifiably landed* in the canonical codebase** — and `succeeded` therefore *always* means "it is now part of the codebase." Concretely:

1. **Verified landing, never an assertion.** Resolution keys off a real, queryable VCS fact (the change is merged / on the target branch), not an operator's word. There is **no force-succeed / manual-override** path. A review that never lands ends as **`cancelled`** (operator abandons) or **`failed`** (e.g., PR closed-unmerged) — never `succeeded`. The honest record is the point.
2. **Delivery-agnostic.** The landing signal may arrive by **webhook** (production, public ingress) *or* by **poll** (local / no ingress) — the same idempotent internal resolution path (both transports can fire; resolving an already-resolved review is a no-op).
3. **Platform-neutral by a per-platform "has this landed?" query.** Every VCS has a verifiable landing fact: GitHub PR `merged`, GitLab/Gitea/Gerrit merge, trunk-based "commit reachable from the default branch", etc. The model abstracts *this query*, not an acceptance assertion. GitHub-merge becomes the default provider, not the coupling.
4. **The human gate *is* the merge.** The reviewer reviews on the PR and merges; the merge is the approval — no separate approve-click, no second authority. (This is already the ADR-018 GitHub flow; ADR-031 makes it the universal model and adds poll-delivery so the local loop participates.)

**Implementation discipline: concrete-now / abstract-when-needed.** The ADR records the neutral *model*; the *code* ships the single concrete `github_merge` path now. The `ReviewResolver` interface is extracted only when a real second backend exists — a one-implementation interface is premature abstraction.

## Consequences

- **Phase 1 (now — #702 re-scoped): concrete `github_merge`-by-poll.** The local loop defaults `push_and_open_pr=true` (runs carry `pull_request_url`); a **merge-status reconciler** (poller, mirroring `reactionpoller`/`dispatchwatchdog`) resolves the review gate on the run's PR reaching `merged` and **fails** it on closed-unmerged. Shares the existing `pull_request.closed` resolution path — must be **idempotent**. No spec change. Makes the local loop behave like production (review-on-PR → merge → resolve), and clears the parked runs.
- **Operator-loop shift (accepted, desired — "mimic the product"):** with `push_and_open_pr=true` the runner commits (StageScoped — excludes drift) + opens the PR *before* operator review, so scope-drift recovery moves from "git-add before commit" to "push a follow-up commit to the PR branch." Phase 1 should cover this.
- **Phase 2 (deferred until a real non-GitHub backend):** extract the `ReviewResolver` interface + provider(s) + a `review.resolution` config — **deployment-level default with optional per-workflow override** (decided then, when there's an actual choice).
- No `manual`/assertion provider; no new force-succeed endpoint. The ADR-018 `/approvals` 409 stays.

## Related

ADR-018 (amended), #702 (Phase 1), Phase-2 ticket (filed separately), #574, #698 (`/redrive` operator-only precedent), ADR-027 (#545).

Parent epic: #389
