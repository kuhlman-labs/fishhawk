---
id: ADR-019
title: "Fishhawk as coordination layer, not destination"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/320
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-019: Fishhawk as coordination layer, not destination

## Context

Fishhawk has been building a destination-shaped UX: a custom SPA renders plans, hosts approvals, owns the audit-log viewer. Developers and operators live somewhere else — GitHub PRs and conversations, terminals, Claude Code — and have to alt-tab to Fishhawk to act. Plan approval, run retry, audit inspection, and run-state visibility are all SPA-primary; CLI is partial; Claude Code is blind; GitHub gets ad-hoc notifications but no live status surface.

ADR-017 (#249) moved CI gating onto branch protection. ADR-018 (#311) moved review-stage approval onto PR merge. Both share a pattern: when there's a natural surface elsewhere developers live, Fishhawk records but doesn't host the primary action. This ADR extends that pattern across the rest of the product.

Concretely: in dogfooding the demo loop, the author repeatedly finds himself bouncing between an issue thread, the SPA, the CLI, and Claude Code to do one logical thing ("review and approve this plan", "check what stage we're at", "see what the agent did"). Each surface has a partial view. The friction is the product asking the operator to come to it.

## Industry research

Convergent design across coordination-shaped DX tools:

- **Dependabot / Renovate**: state lives on GitHub (PRs + comments). No separate UI to "approve" — the merge button is the approval. Configuration lives in the repo. The product is the integration; there's no destination.
- **CircleCI / GitHub Actions**: ship their own dashboards but the workflow status renders directly in the PR's checks UI; the dashboard is for triage, not routine approval.
- **Linear / Jira**: maintain a destination UI because the work itself happens there (writing tickets), but their GitHub integrations post live status to PRs and issues so engineers don't context-switch for routine view-state queries.
- **Anthropic Claude Code**: ships MCP servers (`claude_ai_Gmail`, etc.) that expose third-party state into the agent's reasoning context. Agents don't visit other UIs; they query.

The convergent pattern: **state lives once, surfaces multiply**. Routine actions are surfaced wherever the operator is; the source-of-truth UI is for triage, not daily flow.

## Options

### A. Stay destination-shaped

The SPA is the canonical action surface; CLI / GitHub get partial views. Lowest engineering cost; highest operator-friction cost. Doesn't match how the team actually works (this ADR's author bouncing between surfaces is the canary).

### B. Coordination layer

The SPA stays for triage + read-only summaries; every action it surfaces must also be reachable from CLI, GitHub, and Claude Code. Fishhawk records + audits; surfaces multiply. Mirrors ADR-017 / ADR-018 at scope.

### C. Pure-coordination (no SPA)

Remove the SPA entirely; surfaces are GitHub + CLI + Claude Code only. Most aggressive interpretation. Probably right for v1+, but v0 still needs a destination for novel surfaces (audit-log search, cross-run analytics) that don't have a natural home elsewhere yet.

## Recommendation

**B**.

Specifically, every operator action that exists in the SPA today must be reachable from the surfaces below by the end of v0:

| Action | SPA | CLI | GitHub | Claude Code |
|---|---|---|---|---|
| View run status | ✓ | ✓ | gap | gap |
| Approve plan | ✓ | gap | partial (slash) | gap |
| Reject plan | ✓ | gap | partial (slash) | gap |
| Retry stage | ✓ | gap | gap | gap |
| List audit entries | ✓ | gap | gap | gap |
| See current state on issue | n/a | n/a | gap (ad-hoc only) | gap |
| See active plan from agent context | n/a | n/a | n/a | gap |

The decision creates four impl-track epics, one per surface gap area:

- **E17** / #323 — Plan-as-canonical-review-on-GitHub (depends on ADR-020)
- **E18** / #324 — CLI action-verb parity (no ADR; pure additive surface)
- **E19** / #325 — Claude Code awareness via MCP server (depends on ADR-021)
- **E20** / #326 — Issue-side live status comment (no ADR; sticky-comment-that-edits pattern)



## Decision

Adopt option **B**.

## Consequences

- The SPA stops being the assumed primary surface. Existing pages stay as read-only summaries; new actions land in CLI/GitHub/MCP first, with the SPA reflecting them.
- The audit log becomes more important — it's the single source of truth across surfaces. Audit categories grow to cover surface-of-origin (e.g. `Surface=cli` / `Surface=github_reaction` / `Surface=mcp_tool` on approvals).
- API surface widens because every action needs a bearer-token-callable backend endpoint. Most already exist (the exploration found that all `/v0/*` endpoints accept bearer auth); a few need to be added or extended.
- State-machine concurrency story tightens: four surfaces can race on one action. The existing approval idempotency (#238) + audit-log dedup patterns extend; tests need to cover the cross-surface case.
- Notification model beyond GitHub stays out of scope here. ADR-015 / #79 owns Slack / email / other notification channels.
- A future v1+ may delete the SPA's action surfaces entirely (option C). v0 keeps them for triage.

## Out of scope

- The four impl-track epics own their own design + execution. This ADR establishes the framing only.
- Cross-installation dashboard / design-partner aggregate views (E11's concern).
- Reworking the SPA's information architecture beyond the read-only-summary flips each epic owns.

## Related

- ADR-017 / #249 — CI gating defers to branch protection. Same pattern.
- ADR-018 / #311 — review-stage approval defers to PR merge. Same pattern.
- ADR-020 / #321 — plan review surface model choice (E17 prerequisite). Resolved.
- ADR-021 / #322 — Claude Code integration shape (E19 prerequisite). Resolved.
- ADR-015 / #79 — out-of-band notification model (Slack etc.). Adjacent; not blocked on this.
