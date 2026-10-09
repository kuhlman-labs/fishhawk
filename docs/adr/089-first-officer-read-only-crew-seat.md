---
id: ADR-089
title: "First officer: a structurally read-only crew seat whose evidence-backed findings gate actions"
status: accepted
date: 2026-10-08
issue: https://github.com/kuhlman-labs/fishhawk/issues/4149
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-089: First officer: a structurally read-only crew seat whose evidence-backed findings gate actions

## Context

On 2026-10-08, campaigns 8bed6782 and 7ae8f7a5 ran under an improvised **bridge crew / away team** model:
- One operator-agent session **drove** the campaign: it planned, approved, dispatched and merged.
- A second session **oversaw** it read-only. It watched host health, cross-checked plans against the tree and the other in-flight runs, executed counterfactuals and consolidated fan-in builds, and handled incidents.

The overseer caught things the review agents and the loop missed:
- A full host disk (Go build cache at 660 GB): Docker VM EIO, Postgres down, fishhawkd 503.
- A control that a runner-executed counterfactual showed to be untested (#3901's directory-arm `-mmin` floor).
- Cross-run file overlaps that dictated merge order.
- A stale issue premise (#3901 written before the container gate moved GOCACHE).
- A 30 s inline board sweep (#4136).
- Live no-wedge inventories (#4116).

E85 (#4141) productizes the model. Tiers 1–2 (ops monitor, runner-executed counterfactuals, consolidated fan-in gate) are deterministic product behaviour. This ADR governs Tier 3: the judgement part, a **first officer** crew role. It extends ADR-081 (crew messages: a closed set of typed, role-addressed messages on the audit chain) and ADR-040 (the operator-agent role contract).

The improvised version had three structural weaknesses that this ADR must remove:
1. Findings travelled over ad-hoc cross-session messages, so the driver could ignore one with no record.
2. Addressing was by terminal-session socket name, and session renames broke delivery twice in one day.
3. The read-only boundary was a convention the overseer chose to keep, not something the product enforced.

## Options

- **A. Status quo: a second operator session by convention.** Zero build cost. Unenforced boundary, unrecorded findings, fragile addressing.
- **B. Fold the work into the review agents.** Reviewers are diff-only and scoped to one run. They cannot execute mutations, see other in-flight runs, or observe the host, which is most of what the overseer contributed.
- **C. A dedicated crew role with a structurally read-only seat** whose findings ride the existing crew-message and concern surfaces.

## Recommendation

Option C, with the decision points below.

## Decision

**Accepted (2026-10-08). Ratified by Brett (repository maintainer) on the explicit instruction "ratify the adr and approve and file".** The agent records the maintainer's decision; it is not self-approval. The decision points were drafted by the overseer session from the E85 brief and are open to amendment by a superseding or amending ADR.

**D1 Attachment and lifecycle.**
- A first-officer seat attaches to ONE campaign. A run-scoped seat for a single non-campaign run is permitted.
- At most one seat per campaign or run.
- Attaching is a captain action. A delegated operator agent may attach only where its `operator_agent` contract grants it.
- The seat retires automatically when its campaign or run reaches a terminal state. Retiring revokes its token.

**D2 Identity and scopes (structural, not conventional).**
- Each seat runs under its own minted token: `actor_kind=agent`, subject `first_officer/<campaign-or-run-id>`.
- **Granted:** `read:runs`, `read:audit`, and the crew-message write scope (ADR-081). It may also request read-only *verification jobs* (D3).
- **Withheld:** `write:approvals`, `write:stages` (dispatch), `write:runs`, `write:campaigns`, `write:deploy`, every merge, reap or retry verb, and all tracker writes.
- Enforcement is the token-scope check plus the operatorrole contract (ADR-040). A prohibited call fails with a permission error and an audit row. It is never a silent no-op.

**D3 Verification jobs.**
- The seat may request a verification job: a counterfactual, a consolidated fan-in build, or an allow-listed read-only inventory.
- The runner executes it in a scratch clone, never a lineage worktree, under the verify lock and the configured gate isolation.
- A job may not push, commit to a run branch, change run or stage state, or file issues. It returns a runner-observed result recorded on the audit chain.
- Read-only inventories, starting with `fishhawkd approver-members` (#4116), run through an allow-listed wrapper under a SELECT-only database role. The wrapper refuses any subcommand not on its list.

**D4 Findings and blocking.**
- Findings use ADR-081's `finding` type, role-addressed from `first_officer` to the captain.
- A finding is **blocking** when it cites machine evidence the product itself produced: a runner-observed verification result (e.g. a counterfactual that stayed GREEN, a fan-in FAIL) or an inventory result.
- A finding resting on judgement alone is **advisory**.
- Blocking findings surface in `fishhawk_get_gate_view` with the review concerns and prevent the gated action until dispositioned. Advisory findings surface but do not block.

**D5 Disposition.**
- Findings are dispositioned through the existing concern surface: accept, waive-with-reason (the reason must be non-empty), or defer-to-issue. No parallel mechanism is introduced.
- The seat cannot disposition its own findings.
- Each disposition is recorded on the audit chain with its actor.

**D6 Budget.**
- Each seat has a token-spend cap per campaign. The default is 10 % of the campaign's agent spend, with a configurable hard cap.
- Verification jobs run in their own concurrency group, `local-verify:<host>` with limit 1, so they never consume an implement slot or preempt one.
- When the cap is exhausted, the seat posts one `notice` and stops requesting jobs. It does not fail the campaign.

**D7 Addressing.**
- The seat is addressed by role on its campaign: `first_officer` on campaign `<id>`, resolved server-side. Delivery rides the audit chain (ADR-081), not a process or socket.
- Restarting, renaming or replacing the underlying agent session does not break delivery or lose undelivered messages.

**D8 Personas (ADR-084).** The first officer is not a reviewer persona. It casts no vote in a review round and does not count toward reviewer quorum. It may read persona remits as reference documents.

**D9 What stays human.** Destructive host remediation (clearing caches, restarting Docker, killing processes), tier, identity and approver rulings, and merges remain human or driver actions. The seat may only *recommend* them, as an advisory finding or a notice.

## Consequences

- ADR-081's closed crew-role enum gains `first_officer`. It is addressable and is not an implement-stage role (invariant #8 is unaffected).
- New verification job kind and `local-verify` concurrency group in the runner and fishhawkd. New token-mint path for seats. Gate view and concern surfaces gain finding-sourced entries.
- The two-terminal-session convention is retired once E85's capstone walk (E85.8) passes. Until then, sessions SHOULD follow the D2 and D9 boundaries by convention.
- **Risk:** blocking findings could stall gates if the seat over-reports. Mitigations: blocking requires product-produced evidence (D4), every blocking finding is dispositionable with a reason (D5), and spend is capped (D6).
- Implementation: E85.6 (#4141 epic), after E85.1–E85.4 deliver the deterministic tiers it builds on.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2357} -->
