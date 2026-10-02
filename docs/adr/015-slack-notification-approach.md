---
id: ADR-015
title: "Slack notification approach (v0.x scope)"
status: accepted
date: 2026-06-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/79
supersedes: []
superseded_by: []
applies_to: ["backend/internal/issuecomment/**", "docs/issue-comment-surfaces.md"]
---

# ADR-015: Slack notification approach (v0.x scope)

## Context

Slack integration is v0.x scope (MVP_SPEC §9). But identity surface and notification scaffolding designed in v0 affect how cleanly Slack lands later. Decision now: how do we ensure v0 doesn't paint us into a corner?

## Options

- **Defer entirely until v0.x; design nothing now** — risk: notification system in v0 is GitHub/email-shaped and Slack later requires refactor.
- **Design notification routing abstraction in v0; Slack adapter in v0.x** — minor cost now, smooth path later.
- **Ship a thin Slack notifier in v0** — scope creep; risks delaying core v0.

## Recommendation

Design notification routing abstraction in v0 with email + GitHub-comment adapters. Slack adapter is a v0.x addition that drops in.

## Decision

**Introduce a `Channel` routing abstraction in v0; Slack is a v0.x drop-in adapter (option B).** Decided 2026-06-09 (operator-confirmed).

**State at decision time (verified in-repo):** v0 did NOT build the routing abstraction the original recommendation assumed. What exists is a single, GitHub-issue-comment-shaped surface in `backend/internal/issuecomment` (cataloged in `docs/issue-comment-surfaces.md`: sticky status, plan-on-issue full/summary, CI-retry, budget alert, slash-command replies, run-rejected). There is **no email adapter** and **no multi-channel router** — surfaces are method-per-notification on the server, keyed by a stable `(audit category, kind)` taxonomy.

**Decision:**
- **Introduce a `Channel` interface in v0** and retrofit `backend/internal/issuecomment` behind it, with **GitHub-comment as the first (and currently only) channel.** The existing `(category, kind)` taxonomy in `docs/issue-comment-surfaces.md` is the routing key.
- **Slack lands in v0.x as a drop-in `Channel` adapter** — no refactor of the notification core required at that point.
- **Do NOT ship a Slack notifier in v0** (rejected: scope creep, risks delaying core v0).
- **Email is not a v0 deliverable.** It was never built and nothing in the dogfood loop needs it. It can become an additional `Channel` adapter later if a design partner requires it, but it is explicitly not planned work now.

**Rationale:** the live cost is a bounded refactor (one channel, stable routing key already exists), and it removes the "v0 notification system is GitHub-shaped and Slack later requires a core refactor" risk the ADR was written to avoid. This supersedes the original recommendation, which assumed an email adapter would also be built in v0.

**Impl issue:** #932 (retrofit `issuecomment` behind a `Channel` routing abstraction).

## Consequences

- A `Channel` seam exists in v0; the GitHub-comment surfaces become the first channel behind it.
- Slack (v0.x) and any future channel (email, etc.) drop in as adapters without touching the notification core.
- Minor v0 cost: the retrofit refactor + its tests + a `docs/ARCHITECTURE.md` "Where to look" entry for the channel abstraction.

## Spec reference

`docs/MVP_SPEC.md` §7.2 (presence), §9 (v0.x)

## Target deadline

Day 21

---
Parent epic: #15
