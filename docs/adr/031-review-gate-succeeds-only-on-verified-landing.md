---
id: ADR-031
title: "Review gate succeeds only on verified landing (platform-neutral, delivery-agnostic)"
status: accepted
date: 2026-06-03
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

## Decision

**Accepted (2026-06-03).** Status confirmed by the Captain (repository maintainer) on 2026-10-23 from the record's own source: "**Status: Decided (2026-06-03).**".

## Amendment A1 (accepted 2026-11-14) — verified landing governs code-delivery gates; decision 4 is the queue-owned merge plus forge approval

This amendment sets the scope of the landing rule and replaces decision 4 for merges Fishhawk dispatches. It is one of three companion amendments: ADR-017 Amendment A1 and ADR-018 Amendment A1 state the same scope for the merge gate and for review-stage approval.

**1. Scope of the landing rule.** The Decision's lead rule governs **code-delivery gates**: a review stage succeeds only when the change has verifiably landed. A code-delivery gate authorizes a change to land on the repository: the merge gate of ADR-146's families 1 (planned change) and 2 (PR adoption) (`docs/adr/146-workflow-catalogue.md:63-64`). For those gates the lead rule and decisions 1 to 3 stand.

Other gates resolve under their own effect-specific contracts, as ADR-146 D1 names them (`:62`):
- tracker mutations: the approval of the proposed mutations (family 3, `:65`);
- a decision record: ratification under ADR-113 (family 4, `:66`);
- a deployment target: the deployment approval (family 5, `:67`).

This does not remove ADR-139 D9's verified-record-landing condition on `adr_change` confirmation (`docs/adr/139-adr-change-workflow.md:65`). `backlog_grooming`'s confirm stage is an example of a review-typed stage resolved under its own contract (`backend/internal/server/review_gate_admission.go:159-206`).

**2. Verified, not asserted, on every gate.** No gate succeeds because an operator or an agent asserts that it succeeded. Each gate resolves only on what it declares and the server checks:
- for a code-delivery gate, verified landing (decision 1);
- for any other gate, its own recorded, attributed decision, checked against that gate's approval rules.

A human confirmation that is the gate's declared decision, as on `backlog_grooming`'s confirm stage, is that gate's check, not an override (`.fishhawk/workflows.yaml:357-364`). No gate gains a force-succeed or manual-override path.

**3. A merge outside Fishhawk.** "The queue does not block external merges." (`docs/adr/093-fishhawk-owned-merge-queue.md:96`). A code-delivery gate whose pull request is merged on the forge outside Fishhawk still resolves by landing, through decision 2's webhook-or-poll path. The checks that settle a merged gate still apply. Among them:
- The ADR-036 implement-review hold (`backend/internal/server/merge_members.go:51-53`).
- The members check. A review gate that declares `approvals.members` is held when no listed identity merged or approved the pull request (`backend/internal/server/merge_members.go:50-72`).
- The reconciler's lineage re-check. A merge whose branch carries a foreign commit is held under ADR-035. That check has documented skips and fail-open cases (`backend/internal/mergereconciler/README.md:43-54`).

**4. Decision 4, for a merge Fishhawk dispatches.** Decision 4 said "the merge is the approval". For a merge Fishhawk dispatches, three acts now authorize the merge:
- **The merge intent.** This is the operator's `merge_run`, or the delegated `may_merge` arm where the declared autonomy allows it.
- **The dispatch.** The merge is dispatched under ADR-109's queue ownership and its shared per-run dispatch ledger (`docs/adr/109-merge-queue-owns-the-merge.md:19-21`, `:42-47`).
- **The forge approval.** A forge approval qualifying under ADR-112 R1, including its permitted clean-advance chain (`docs/adr/112-queue-merge-forge-approval-predicate.md:45-63`).

Where a protected path needs more (ADR-121 D4.2), that record decides. Landing is still what resolves the gate. There is still no separate Fishhawk approve-click on a code-delivery review gate.

For a merge made on the forge outside Fishhawk, decision 4 stands as written: the merge is the approval, subject to section 3.

**5. Families explain the scope; they do not select it at runtime.** ADR-146 says of its catalogue that "no validator rule follows from it" (`docs/adr/146-workflow-catalogue.md:71`). No runtime check reads a family. A running gate is recognised by its own declaration, and this amendment does not change how.

**6. Consequences.** "The ADR-018 `/approvals` 409 stays." now holds for code-delivery review gates (ADR-018 Amendment A1).

**What stands.**
- Decisions 1 to 3 stand within the scope above.
- The implementation discipline, Phase 2, and the absence of a `manual` or assertion provider all stand.
- The partial supersession of ADR-018's resolution mechanism stands.

- **Record metadata:** an in-body amendment of ADR-031. No new edge. ADR-031 keeps `supersedes: ["ADR-018"]` and `status: accepted`.
