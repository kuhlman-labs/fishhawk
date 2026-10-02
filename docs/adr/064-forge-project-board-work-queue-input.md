---
id: ADR-064
title: "Forge project board as declarative work-queue input and derived status projection"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2139
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-064: Forge project board as declarative work-queue input and derived status projection

## Context

Operators today organize agent work on a forge project board (GitHub Projects #7) by hand. A 2026-07-23 backlog-triage session quantified the friction: ~150 hand-rolled GraphQL mutations, the undocumented `singleSelectOptions` field-mutation workaround, the 100-item pagination cap, user-owned-project auth (App installation tokens cannot reach user-owned Projects v2 — #1114, why `FISHHAWKD_PROJECTS_TOKEN` exists), and issues silently landing `boarded:false`. This is a friction class the product exists to absorb, and it recurs for every tenant.

Proposal under consideration: make the board a first-class product surface — agents use it to coordinate work, and human operators get insight into what agents are working on.

Existing machinery this must compose with: the audit chain is the sole source of truth for run state; work-management conventions (`fishhawk_file_issue`, #1005) already encode board placement with a best-effort `boarded:false` degradation; campaigns assemble their wave DAG from issue-level `depends_on` edges (ADR-047); #2051 adds the no-epic explicit-issue-list campaign variant; E40 (#1712) builds the decision-centric attention queue. Timing pressure: E45's forge-agnostic work-item abstraction and the ADR-057 provider model are being shaped now — the board seam is cheap to reserve in those interfaces today and a refactor later (same lesson as folding the provider discriminator into ADR-057 before it shipped).

## Options

1. **Board as live coordination bus** — agents read and write the board to coordinate in-flight work. Rejected direction: the board is unaudited, non-transactional, rate-limited, truncates item lists at 100, and can disagree with the audit chain (split-brain — an agent acting on a stale column acts on stale run state).

2. **Directional split: declarative input + derived projection.**
   - Board → Fishhawk, at selection time only: a board view/column expresses human intent about what to work on next; e.g. "work the Up Next column in priority order" becomes a campaign source (dovetails with #2051's explicit-issue-list variant). In-flight coordination stays on the campaign/`depends_on`/lineage-lock machinery.
   - Fishhawk → board, as output: run state projected to board Status (run active → In Progress, parked at gate → In Review/Blocked, merged → Done), best-effort and eventually consistent, following the notifier/issue-comment-surface pattern and the `boarded:false` degradation posture.
   - Spec shape: additive `workflow-v1.x` `board:` block — provider discriminator, project ref, field mappings (tenant's Status option names → run states; priority field).

3. **Fishhawk-native board UI** — build our own board surface. Rejected direction: competes with both the forge board (work inventory) and E40's attention queue (decisions); duplicates an inventory surface tenants already have.

## Recommendation

Option 2. Constraints to honor: (a) forge asymmetry is real — GitHub App installation tokens cannot reach user-owned Projects v2 while GitLab group boards sit inside the E45 group-scoped OAuth app, so the feature needs per-tenant capability detection and graceful degradation, never an assumed-available surface; (b) the board is never read back for in-flight decisions — projection only; (c) keep the board a projection so it does not compete with E40. Sequencing: implementation is post-alpha; the only near-term action is reserving the board seam in E45's work-item abstraction and the ADR-057 provider model while those are still being shaped.

## Decision

**Accepted (2026-07-26).** Adopt Option 2 — the directional split (declarative input, derived projection). Three forks were settled at ratification, and a finding at ratification substantially narrows the remaining scope. Where these refine the Recommendation, **they govern**.

### 0. Finding: the projection half is ALREADY DELIVERED

`work-management-v0` already carries every element this ADR proposed as a new `board:` block:

| Proposed here | Already shipped in `work-management-default.yaml` |
|---|---|
| provider discriminator | `provider: github_projects` |
| project ref | `project: {owner, owner_type, number}` |
| field mappings | `states:` — canonical states → the tenant's Status option names (#1012) |
| run state → board Status | `transitions:` — 7 run/campaign/issue lifecycle events |

Parsed into `Conventions` at `backend/internal/workmgmt/conventions.go:59,353` with tests, including the property this ADR wanted: the board-sync hook moves a card **only from the transition's expected source state**, so a human's manual placement is never overridden. `campaign_started → up_next` (#1816) and `issue_closed`/`issue_reopened` (#1817) landed after this ADR was filed.

**So the Fishhawk → board projection is done for GitHub Projects.** What remains open is (a) the board → Fishhawk **input** direction, and (b) per-forge capability detection for the asymmetry named in the Recommendation.

### 1. Configuration extends work-management-v0 — NO workflow-spec `board:` block

This **supersedes the Recommendation's spec shape.** The ADR proposed an additive `workflow-v1.x` `board:` block; that is now rejected on two grounds:

- It would **duplicate** the provider / project / states config work-management-v0 already carries, creating two surfaces that can disagree about the same board.
- The workflow spec is the operator's **governance** surface — autonomy, gates, approvals, permissions — and ADR-067 (#2210) and ADR-066 (#2209) were just ratified specifically to make it legible. Adding work-tracking integration to it dilutes exactly what that consolidation bought.

Board integration lives in `work-management-v0`, next to the filing conventions and board placement it already owns. The selection config (source view/column, ordering) is an additive field there.

### 2. Reserve seams only; the campaign-selection feature is post-alpha

Near-term work is confined to what is cheap now and expensive as a later refactor, per the Recommendation's own sequencing:

- Grow the `workmgmt` provider abstraction from **create-only** (`File(ctx, req)` — `backend/internal/workmgmt/provider.go:25`) to include **read/list**.
- Document the **per-forge capability matrix**: GitHub user-owned Projects v2 requires a UAT/PAT with `project` scope because App installation tokens cannot reach it (#1114); `gh project item-list` caps at 100 items so a reader must enumerate via search/issue queries, never board item lists; GitLab group boards sit inside the E45 group-scoped OAuth app.

The board-view-as-campaign-source feature itself is **deferred post-alpha**.

**Implementation lands as children of E45 (#1852)**, the forge-agnostic work-item abstraction — not a new epic. The whole point of acting now is that E45 and the ADR-057 provider model are still being shaped; a separate epic would decouple this from the interfaces it needs to influence.

### 3. The never-read-back invariant is enforced TECHNICALLY, not by convention

The read capability exists only on the campaign-assembly / operator-selection path. **No board-read tool is exposed to agents through the MCP surface.** An invariant that depends on an agent choosing not to reach for a surface decays the first time one is prompted differently — and the board is precisely what an agent would reach for to coordinate. Enforcing it by construction keeps the audit chain the sole source of truth for run state, rather than making that a behavioural expectation.

Named approver: repository maintainer (human).

## Consequences

Operators get board-level insight into agent work without Fishhawk growing a competing UI; campaigns gain a human-curated selection source; the hand-rolled board-maintenance friction class is absorbed by the product; a per-forge capability matrix (GitHub Projects v2 vs GitLab boards, token classes) must be documented and detected per tenant; agents remain forbidden from using the board as a coordination bus, keeping the audit chain authoritative.
