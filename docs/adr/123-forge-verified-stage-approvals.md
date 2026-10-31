---
id: ADR-123
title: "Amend ADR-113: stage approval gates admit only forge-verified credentials, on every channel"
status: accepted
date: 2026-10-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/4750
supersedes: []
superseded_by: []
amends: ["ADR-113"]
applies_to: []
---

# ADR-123: Amend ADR-113: stage approval gates admit only forge-verified credentials, on every channel

## Context

Amendment to ADR-113 (#4628), **v2**. Status: accepted (ratified by the Captain, 2026-10-27). v1 was reviewed by claude-fable-5-1 and gpt-6-astra (both: revise), and v2 folds in both reviews.

#4116 made `approvals.members` a subject-shape rule: a gate admits a principal whose provider-qualified subject (`github:<login>` / `gitlab:<user>`) is listed. The rule never checks how the presenting credential was minted. `identity.SubjectListed` checks subject shape only, and `checkApprovalPredicates` never reads the credential kind. A static API token can carry a forge-qualified subject: one minted by `fishhawkd token issue --subject`, or a child token minted through `POST /v0/tokens` under the caller's own subject. Such a token therefore satisfies a members leg, even though the refusal hint says a static token "never matches" (#4633, found by both ADR-113 v5 reviews).

**The comment channel.** The v1 review found a second, separate gap. The issue-comment approval channel (`/fishhawk approve|reject` and reply-comment approvals) records the webhook sender as the approver and advances the stage directly, without the approvals predicate or quorum. On workflow-v2 gates its only check is the legacy `approvers` role list, which v2 removed. It is tracked separately as a security fix. This record states the rule the fix implements.

ADR-113 D2(a) already requires `auth_method == "oauth"` for ADR ratification, and notes that this is stricter than the plan gate today. The plan for #4633 (run c23228ac) proposes a credential-kind rule for every approval-row gate. Its allow-list is wider than D2(a)'s, so the planner flagged it as a new architectural decision.

## Options

**A. Status quo: subject shape only.** A static token carrying a listed forge subject keeps approving, and the hint text is corrected to say so. Rejected: a static token's subject is only a label, so this undercuts the reason #4116 required forge identities.

**B. oauth only on every gate, mirroring D2(a).** Rejected: it refuses the browser session and the OAuth authorization-server (fho_) token, which are both forge-verified by construction. That would lock the web UI and the /mcp OAuth path out of every members gate.

**C. A forge-verified allow-list on every stage approval gate, with every channel that records an approval held to the same predicate (recommended).** The allow-list is `session`, `oauth` and `oauth_as`; everything else is refused.

## Recommendation

C, as an amendment to ADR-113. D2(a) keeps its stricter oauth-only rule for ratification, and option C is the general floor for stage approval gates.

## Decision

**Accepted 2026-10-27:** ratified by the Captain (v2).

The decision:

1. **The allow-list, and how kinds are derived.** On every stage approval gate that declares `approvals.members`, a request-borne submission's members leg admits a listed principal only when the credential that authenticated the request is a forge-verified kind. Kinds are derived ONCE, by a function in `backend/internal/identity` that takes the trusted middleware markers as arguments, in this order:
   1. **`session`:** a cookie session authenticated the request (`SessionID` set; the middleware ignores a bearer once a cookie session resolves). Sessions are minted only by the forge sign-in callback.
   2. **`oauth_as`:** the bearer is an audience-validated authorization-server (fho_) token. This is checked BEFORE `auth_method`, because an AS identity deliberately carries an empty `auth_method`, and its consent requires a forge session.
   3. **`mcp_run`:** an `mcp:run:` subject.
   4. **From an api token's `auth_method`:**
      - `oauth`: minted by `POST /v0/tokens/login` from a forge access token that the forge resolved to that subject at mint time. `fishhawkd token issue` and `POST /v0/tokens` cannot produce it.
      - `static`.
      - `unrecorded`: empty.
   5. **`unknown`:** anything else.
2. **Refusals.** Every kind except `session`, `oauth` and `oauth_as` is refused with 403 `approver_predicate_unmet`, `details.reason = credential_not_forge_verified`, naming `details.credential_kind`. The rejection audit row records `predicate_snapshot.members_credential`. This is a separate field from ADR-055's `auth_method` enum, and the `predicate_snapshot` OpenAPI description gains it.
3. **Scope.**
   - **Which gates.** The rule applies to `ApprovalRepo`-backed STAGE gates. ADR-113 D2 ratification keeps its own (a)/(b) credential rules, and the shared approver evaluation used by ratification does not carry this leg.
   - **Direct and delegated human-kind submissions.** The leg applies to the presenting credential's own subject.
   - **Delegated agent-kind submissions.** Their principal is the seated captain. They are ADMITTED, NEVER COUNTED, unchanged here: they never count toward a human quorum. ADR-121's prohibition on delegated tier-I adoption is preserved.
4. **Every channel that records a stage approval meets the same predicate.**
   - **The issue-comment channel** (slash commands and reply comments). The forge authenticates the comment's sender, so it needs no presenting-credential classification. But it MUST satisfy the gate's full effective approvals predicate (members, quorum, author exclusion) before it records or advances anything. Until that parity exists, comment approvals and rejections on ANY workflow-v2 gate are REFUSED with a reply naming the supported channels. This holds whether or not the gate declares `approvals` (the v2 schema permits a gate without one), and fails closed when the cached spec cannot be read or no role resolver is configured. A v0/v1 gate keeps its legacy `approvers` role check.
   - **Paths that read the forge are unchanged:** merge-settled review gates, the ADR-112 forge-approval fold and merge-queue eligibility read identities from the forge itself.
5. **Relation to #4116 and ADR-113 D2(a).** This adds a credential leg to #4116's subject-shape rule and leaves the rule itself unchanged. D2(a) (oauth-only for ratification) stays stricter, and this amendment does not loosen it.

## Consequences

- **Live approvers.**
  - **Who is refused.** An approver who uses a static token on a members gate is refused after deploy and must switch to `fishhawk token login` or the forge browser session.
  - **What the inventory showed.** On 2026-10-27, every approval in the last 30 days by the listed forge principal carried an `oauth` stamp.
  - **Before deploy,** the inventory runs over every OPEN gate, using `approval_submitted.auth_method` on the audit chain, and over every active credential of a listed principal. A human whose only credential is static gets a working replacement login first.
  - **No comment-channel approval** appears in the daemon logs.
- **Residual: what the `oauth` stamp proves.** The `oauth` mint accepts ANY forge access token that the forge resolves to the subject (a personal access token, or a token from another OAuth app), not only one issued to this deployment's client. That is #4627, still open, and ADR-113's #4627 prerequisite is unchanged. The assurance is the forge's own, as in ADR-113's residual.
- **Residual: counting is subject-only.** Approval rows record no credential kind, so the count-time filter stays subject-only. A static-credential approval recorded before deploy, on a gate that is still open, can still count. This is an explicit grandfathering decision, bounded by the pre-deploy open-gate inventory.
- **Residual: a process holding an admitted human credential is indistinguishable from the human** (ADR-113).
- **Inventory command.** The `fishhawkd approver-members` inventory gains the same credential leg, so the impact check and the gate agree.
- **Ordering.**
  - **The comment-channel refusal (Decision 4)** ships first, as its own security fix.
  - **The credential leg (Decisions 1–3)** is implemented in #4633 (run c23228ac). Its PR merges once its implementation matches this v2 text.
- **Record metadata.** amends: ["ADR-113"]; supersedes: []; applies_to: []. ADR-113 gains `amended_by: ["ADR-123"]` and keeps `status: accepted`.
