---
id: ADR-083
title: "Captain semantics: approvals stay predicate-governed; the captain record routes pages, owns the digest, and confirms delegation, and changes hands by a recorded two-step handover"
status: accepted
date: 2026-09-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/3751
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-083: Captain semantics: approvals stay predicate-governed; the captain record routes pages, owns the digest, and confirms delegation, and changes hands by a recorded two-step handover

## Context

ADR-080 #3693 defines the captain as "the human who commands a repository: ratifies direction, sets delegation, decides at gates. **A role, not a person** — resolved through the existing approval predicates." Charter §2 makes change of command an alpha requirement (T11): "a handover is a recorded event with a brief". E76 #3697 carries it. Before its children can be written, "captain" needs a meaning in code, because today there is none.

**What exists (verified 2026-09-27):**

- **Approvals are predicate-governed.** A gate's `approvals: {count, not, min_permission, member_of, members}` (`docs/spec/workflow-v2.md` §approvals; E39 #1705 / ADR-055) is enforced in `server/approvals.go` `checkApprovalPredicates` and `quorum.go` (`eligibleApprover`, `resolvePredicates`). Forge predicates fail closed; an agent never counts toward a human quorum. **`members` is parsed but never enforced** (filed as E39.13 #3745). On this repository the effective rule is "any non-agent holder of a `write:approvals` token".
- **No owner concept anywhere.** There is no repos table (a repo is a TEXT column on `runs`), no owner on `installations`, and `account_members.role` is per account, not per repo. `operator.yaml` `conventions.escalation_contact` is free prose with no consumer.
- **Pages are addressed to nobody.** They are one-line issue comments without an @-mention (`issuecomment/ping.go`), plus `drive_run`'s decision-required stop in whichever session is driving (ADR-040 D4).
- **The delegation principal is whoever holds the token.** Locally the MCP server uses the human's own token. Delegated approvals record `operator-agent/delegated` with `on_behalf_of` (`approvals.go` ~1078/2379).
- **Delegation is declared, not confirmed.** Tiers and escalations live in `.fishhawk/workflows.yaml`, human-authored and forbidden to agents. Nothing re-confirms configuration. The closest precedents are acknowledgement categories, grooming dispositions, and E58.3 #2272's planned lower-only `min()` at admission.
- **Identities.** Static token subjects (`brett@local-mcp`) are not provider-qualified. `fishhawk token login` mints `github:<login>`.
- **Consumers waiting on a captain identity:** gate notifications (E60.3 #2292), escalation paging at every tier (E77.6 #3740), the per-captain digest watermark (E75.6 #3734), the per-workflow delegation read (E76.1 #3747), and the bridge (E40).

## Options

**D1 — what the captain record means.**
1. **A label only.** Harmless, but the transfer is decorative.
2. **Sole approver.** Every approval path (REST, campaign driver, issue-comment channel, the future E40 decision surface) would depend on a new runtime record outside the standing orders. An absent captain wedges every gate, which makes the one-person risk *worse* and contradicts "a role resolved through predicates".
3. **Hybrid.** Approvals stay predicate-governed exactly as today. The captain record decides who is paged, whose read watermark the digest uses, who may confirm delegation, and the default `on_behalf_of` principal for the operator agent. An approval by someone other than the captain is valid and is flagged on the record, not refused.

**D2 — how the role changes hands.**
1. **Two-step handover:** the outgoing captain names a successor; the successor accepts.
2. **Claim:** anyone satisfying the repository's strictest gate predicate may claim.
3. **Account admin assigns.**

**D3 — identity key.** Provider-qualified subjects only, or accept static subjects as well.

**D4 — lowering delegation on handover.**
1. **Record the confirmation; lowering files an issue** proposing the human-authored `workflows.yaml` change.
2. A lower-only runtime ceiling composed by `min()` at admission.
3. Both.

**D5 — delegation before the incoming captain confirms.** Continue as declared but flagged "unconfirmed", or clamp to `low`.

## Recommendation

**D1 option 3, D2 option 1 with a fallback claim, D3 accept both with explicit verification, D4 option 1 for alpha, D5 continue and flag.** Binding rules:

1. **Approvals do not change.** Gate eligibility is the gate's predicate and nothing else. The captain record is never an input to quorum, and an absent captain can never block a gate. Fixing `members` (#3745) is independent and proceeds on its own.
2. **One captain per repository at a time.** Recorded on the account's global chain (`AppendGlobalChained`), keyed by repository: `captain_handover_offered`, `captain_assigned` (on acceptance), `captain_relinquished`, `captain_claimed`. The current captain is derived from the chain, never from a mutable table alone. Register every category.
3. **Handover is two-step.** The outgoing captain offers to a named successor; the successor accepts; each step is audited. An offer can be withdrawn before acceptance.
4. **Fallback claim, loudly.** When there is no captain (never assigned, or relinquished), anyone satisfying the repository's strictest human gate predicate may claim. The claim is audited and paged to the previous captain if one exists. On a deployment where that predicate reduces to "any non-agent token holder", the claim records that it was **unverified**. **An agent identity can never offer, accept, claim or hold the role.**
5. **Identity.** A provider-qualified subject is preferred. A static subject is accepted and carried with `identity_verified: false`, visible on every surface that shows the captain.
6. **What the record does:** routes pages (E60.3, E77.6 address the current captain); keys the digest watermark (E75.6); is the default `on_behalf_of` principal the operator agent acts for; shows on the bridge. Where an approver is not the captain, the approval is valid and the hand-off notes it.
7. **Delegation confirmation.** On acceptance, the incoming captain is asked, per workflow, to **confirm** (recording `delegation_confirmed` with workflow, tier, and the content hash of the E76.1 #3747 view) or **lower** (filing an `autonomy:low` issue proposing the `workflows.yaml` edit). Nothing raises, and no agent edits `.fishhawk/**`. Until confirmed, delegation continues as declared and every hand-off and page on that workflow says "unconfirmed since <handover>". A lower-only runtime ceiling (D4 option 2) is deferred; revisit it with E58.3 #2272.

## Decision

**Accepted (2026-09-27). Ratified by Brett (repository maintainer) on the explicit instruction "ratify both ADRs" after reviewing this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt the Recommendation as binding: D1 option 3 (approvals stay predicate-governed; the captain record routes pages, keys the digest watermark, confirms delegation and is the operator agent's default principal, and never gates an approval), D2 option 1 with the audited fallback claim, D3 provider-qualified identity preferred with static subjects carried as `identity_verified: false`, D4 option 1 for alpha (confirm, or lower by filing an `autonomy:low` issue; nothing raises; no agent edits `.fishhawk/**`), D5 continue-and-flag. Rules 1–7 bind the implementation. An agent identity can never offer, accept, claim or hold the role.

**Scope of this ratification:** the semantics above. It changes no approval rule; fixing `members` (#3745) proceeds independently.

## Consequences

- E76 children: the recorded transfer (rules 2–5); the handover brief composing E75.6's digest over the window since the last `captain_assigned`, plus open pages, parked clarifications, campaigns, waivers, escalations and delegation (E76.1); delegation confirmation (rule 7, `autonomy:low`); a bridge surface; and a handover walk by a second person with their own provider-qualified identity. The walk must not rely on `members` being unenforced.
- Consumers gain a single identity to address: E60.3 #2292, E77.6 #3740, E75.6 #3734, E40.
- Out of scope: multi-captain concurrency, fleet-level assignment, raising delegation on handover (E82), and any change to who may approve.
- Reversal: stop reading the captain record. Pages fall back to unaddressed issue comments, and the chain keeps the history.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- #3703 [E82] Earned autonomy: the crew's record recommends delegation changes the captain ratifies — low confidence (0.27); shared: captain, changes, delegation, record

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"epic_suggestion":{"number":3703,"url":"https://github.com/kuhlman-labs/fishhawk/issues/3703","title":"[E82] Earned autonomy: the crew's record recommends delegation changes the captain ratifies","score":0.26666666666666666,"confidence":"low","basis":"captain, changes, delegation, record"},"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2019} -->
