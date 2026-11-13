---
id: ADR-143
title: "Human-only actions are refused on agent-drivable channels (amends ADR-084)"
status: accepted
date: 2026-11-12
issue: https://github.com/kuhlman-labs/fishhawk/issues/5231
supersedes: []
superseded_by: []
amends: ["ADR-084"]
applies_to: []
---

# ADR-143: Human-only actions are refused on agent-drivable channels (amends ADR-084)

**Status: accepted (ratified 2026-11-12).**

## Context

Several actions are reserved to a human. ADR-040 fixes fail-closed paging, "anything not delegated pages the human" (Decision 2), and runs operator-agent actions under a distinct `operator-agent/<role-spec-version>` subject (Decision 4). Other records reserve particular actions: ADR-083 (#3751) makes delegation confirmation and lowering a captain act and decides captain identity in its rules 4 and 5; ADR-084 rule 5 says a server-synthesized concern is "never waivable by an agent"; ADR-101 D3 lists "delegation confirm or raise" and "every captain verb" as non-delegable; ADR-089 D1 says "Attaching is a captain action"; ADR-117 D1 says a judgement is "written ONLY by a human-initiated verb". The grooming, upkeep and comms dispositions are reserved by their handlers' operator-only ladder (`backend/internal/server/grooming_dispositions.go:284`, `upkeep_dispositions.go:263`, `comms_dispositions.go:365`) (outside D2; exclusion (iv)).

The server enforces these from the credential's subject, with several predicates (files in `backend/internal/server/` unless named). `isAgentSubject` (`actor.go:43`) recognises an operator-agent, run-bound or first-officer-seat subject. With a `delegated` test it guards the server-check clear (`concern_human_only.go:59-66`) and delegation confirm and lower, which refuse an agent or delegated actor (`backend/internal/delegationconfirm/delegationconfirm.go:381`). The captain verbs are in the same family: the handler passes `ActorIsAgent` and `ActorIsDelegated` (`captain.go:544-546`), and the captain package refuses either (`backend/internal/captain/transition.go:88`). Alone it guards seat attach and retire (`first_officer_seat.go:189`, `:474`) and resolving a concern with operator evidence (`resolve_concerns.go:149`). The disposition ladders refuse run-bound and operator-agent tokens (`grooming_dispositions.go:291-299`), and their last rung, `write:approvals` (`grooming_dispositions.go:300`), is "enforced UNCONDITIONALLY: no cookie-session bypass" (`grooming_dispositions.go:281-282`). A cookie identity carries no scopes (`middleware.go:58-66`), so the ladders refuse every cookie session. Every other subject is treated as a human. `actor.go:40-42` states the residual: "an agent session presenting a human operator's token is indistinguishable from that human and is treated as one".

A credential's subject does not say who is using it. An MCP server configured with a human-bound credential and driven by an agent session presents a human subject and declares nothing, so every guard admits it. This was found on this repository's own deployment on 2026-11-12 (#5228). ADR-084 Amendment A1 (#3752) decides the server-check case only. This ADR decides the rule for a closed list of human-only actions.

The same weakness reaches the CLI. The stdio MCP server resolves its bearer from `FISHHAWK_API_TOKEN` or the same credential store the CLI uses (`backend/cmd/fishhawk-mcp/main.go:137`), and sends a plain `Authorization: Bearer` request whose extra headers are caller-set (`backend/internal/mcpserver/client.go:7273-7285`). A CLI call and a stdio MCP call made with the same stored login are the same request to the server. The `/mcp` route re-dials REST with the caller's own bearer (`backend/internal/server/mcproute.go:462-475`) and refuses a cookie identity (`:520-524`). Amendment A1 names the CLI as a human channel and leaves "what makes a CLI call human" to this record.

## Options

- **A. Leave it to the deployment.** Document that an agent session must hold an operator-agent credential. Nothing changes in the product, and the control holds only where the setup is right.
- **B. Enforce by channel as well as identity.** Refuse human-only actions on agent-drivable channels whatever subject the credential carries, and decide what makes each remaining channel human.
- **C. Step-up on every human-only action.** Require a proof per action (a fresh forge-verified proof), on every channel.

## Recommendation

Option B, decided as a credential-kind test for a closed list of actions. A per-action proof on bearer channels is a direction, not decided here.

- **D1. The credential-kind test.** A D2 action is admitted only when the request's credential kind, derived per ADR-123 D1 from server-set markers, is `session`: a cookie session minted by the forge sign-in callback. The existing derivation is `approvalCredentialKind` (`backend/internal/server/approval_credential.go:10`).
  - Any request not on a `session` credential is refused for a D2 action, whatever subject it carries and whichever client presented it. That covers `static`, `oauth`, `oauth_as`, `mcp_run`, `unrecorded` and `unknown` bearers, through stdio MCP, HTTP `/mcp`, the CLI and direct REST.
  - D1 is a second, stricter tier (`session` only) beside the forge-verified tier (`session`, `oauth`, `oauth_as`) that ADR-123, ADR-117 A1.3, ADR-126 D5 and ADR-139 use. Those records keep their tier.
  - The server never reads a channel from a header, user agent, terminal claim or any other caller-set value, and the absence of an MCP marker grants nothing.
  - D1 relies on four properties, which this record makes requirements: no MCP transport accepts or forwards a session cookie (`/mcp` refuses one today, `backend/internal/server/mcproute.go:518-524`); no bearer credential can be exchanged for a session; a session is minted only by the forge sign-in callback (ADR-123 D1); and a session request for a D2 action passes the server's CSRF protection (`backend/internal/server/csrf.go:180`), with no exemption for a D2 route.
  - The check is at the action's handler, so it holds on every ingress.
  - The existing subject and `delegated` tests stay where they exist today. Their predicates differ per action (Context), and none is widened or narrowed. D1 adds a refusal and never admits a request that an action's existing guard refuses.
  - The refusal names the Web UI session as the remedy, produces no side effect and records no row for the action as taken; whether a refusal row is written follows the guard's existing rule.
- **D2. The D1 actions are a closed list, in this record,** listed in one registry the guards and the completeness test read. Every D2 action is in it from the first change; only its enforced mark varies. The server-check entry carries one mark for all its verbs, and a verb added later that clears a server-check concern is enforced from the change that adds it. A mark is set by the action's enabling change and is cleared only by an amendment of this record; a test pins the set of enforced entries so that removing one is a visible change. The list grows only by amendment. It names:
  - clearing a server-check concern by any verb (waive, bulk waive, defer, resolving it with operator evidence, and any later verb that clears one): ADR-084 rule 5 and Amendment A1.

  Recorded exclusions, each with its reason:
  - (i) ADR ratification: ADR-113 D2(a) admits only an `oauth` api token and refuses a cookie session, the reverse of D1, and D2(b) admits an SSH signature whatever credential submits it (ADR-139 keeps both), so D1 cannot apply to it without an amendment of ADR-113. A stronger proof for it is ADR-113 Direction H.
  - (ii) Stage approval gates, including the human-executor review gate, production-intent approvals and protected-change approvals: ADR-123 sets the credential rule for a gate that declares `approvals.members`. A gate that declares none keeps only its subject test and the Context's residual.
  - (iii) The escape-closure attestation: ADR-117 A1.3 decides its credential kinds.
  - (iv) Actions whose guard refuses only run-bound and operator-agent tokens: grooming, upkeep and comms dispositions (`grooming_dispositions.go:284`, `upkeep_dispositions.go:263`, `comms_dispositions.go:365`), campaign hotfix and review rerun (`campaign_hotfix.go:127-135`, `review_rerun.go:247-254`). Whether D1 applies to them is a separate decision. For the dispositions it also needs a session authorization predicate, because their last rung requires a bearer scope that cookie sessions do not carry (`grooming_dispositions.go:300`).
  - (v) Any action whose own decided record states a credential kind or a channel for it, including ADR-126 D5 security dispositions and ADR-139's human acts (rulings, waivers, errata, scoped-round orders, grants).
  - (vi) Labelling a concern's judgement, and resolving with operator evidence a concern that is not a server-check concern: ADR-117 D1 sets no credential rule for them, and Amendment A1 reaches only server-check concerns. ADR-117 D1 names their human verbs, including the MCP verb `fishhawk_label_concern` (`docs/adr/117-live-review-effectiveness.md:91`).
  - (vii) The five captain seat verbs (`backend/internal/server/captain.go:23`), delegation confirm and lower (`backend/internal/delegationconfirm/delegationconfirm.go:381`), and first-officer seat attach and retire (`first_officer_seat.go:189`, `:474`). ADR-083 (#3751) decides captain identity. Rule 5 accepts a static subject ("A static subject is accepted and carried with `identity_verified: false`"), rule 4 accepts an unverified claim, and seat attach defers to the sitting captain's subject (`first_officer_seat.go:180-185`). Applying D1 to them amends ADR-083, whose record must be backfilled first (ADR-113 D3).

  A listed action is never excluded. Where an action is both in the list and within an exclusion's words, the list governs.

  Extending D1 to any excluded action is an amendment of its governing record, or, for exclusion (iv), the separate decision named in Directions. A test enumerates every handler that refuses a caller on `isAgentSubject` or `operatorrole.IsTokenSubject`, or that refuses every request marked delegated (a handler that only evaluates a delegation condition for such a request is not one), and fails when its action is neither in the list nor in the exclusions.
- **D3. Until a proof of presence is in effect, the CLI is not a human channel for the D2 actions.** A server-synthesized concern is cleared from the Web UI cookie session only. A CLI call is a bearer call, so D1 refuses it for a D2 action.
  - The server cannot tell a CLI call from a stdio MCP call: both present the same stored bearer credential in the same request (Context).
  - This answers the question ADR-084 Amendment A1 left to this record, "what makes a CLI call human". It narrows A1's "or the CLI" for the server-check clear verbs (D2), which is the ADR-084 amendment this record declares.
  - A terminal-bound confirmation and an ordinary forge-verified signature are not proofs an agent's shell cannot produce. Restoring a bearer path for a D2 action needs a server-verifiable, action-bound human proof.
  - A proof of presence is the condition that restores a bearer channel. This direction names ADR-144 (#5239). The direction is that, once an amendment adopts a proof for a D2 action, any channel that carries a valid proof for that action qualifies, the CLI included. Until that amendment is ratified no proof is in effect, and where it admits an MCP transport it also amends ADR-084 Amendment A1's refusal and declares that. It is adopted by an amendment of this record, or by the record that decides the proof, declaring that amendment.
  - No bearer CLI verb is added for a D2 action.
  - The Web UI cookie session is the human channel.
- **D4. Human-bound credentials in agent clients.** A human-bound credential in an MCP client stays a supported setup (ADR-076), and under D1 it never admits a D2 action. An operator agent acting under delegation keeps ADR-040 Decision 4's distinct operator-agent subject. This record does not decide per-agent identity for MCP clients.
  - The condition that an MCP client holds a credential with a non-agent subject is reported to the operator without exposing credential values. Where the report names enforcement, it reads it from the connected server, and reports every D2 action as unprotected when it cannot. Form, severity and placement are the implementation's (#5227).
  - An ordinary human CLI login alone is not a failure.
- **D5. No new agent path.** This ADR adds no way for an agent to take a human-only action. A recorded, scoped grant that would let one do so is a separate decision.

**Directions (not decided here).**
- A proof of presence for the D2 actions (ADR-144, #5239), after which any channel carrying a valid proof qualifies: an amendment of this record.
- Extending D1 to the captain, delegation and seat actions (exclusion (vii)): an amendment of ADR-083 once its record is backfilled on main. It states how a static-subject incumbent hands over to a forge-session identity while the existing path still works.
- Extending D1 to stage approval gates, ADR ratification or the escape-closure attestation: an amendment of ADR-123, ADR-113 or ADR-117 respectively.
- Extending D1 to labelling and to resolve-with-evidence of other concerns: an amendment of ADR-117.
- Whether D1 applies to the actions in exclusion (iv), with a session authorization predicate for the dispositions: a separate decision.
- Whether issuing and revoking a charter lease (ADR-101 D4, which also attaches the lease-scoped seat, D8) take D1: decided with the captain, delegation and seat actions (exclusion (vii)) by the ADR-083 amendment above, before those verbs ship.

## Decision

**Accepted 2026-11-12** by the Captain (manual ratification path; approver, erratum and hashes in the ratification comment). **Option B**, i.e. D1–D5 as stated in the Recommendation: a credential-kind test (`session` only) for clearing a server-check concern by any verb (D1, D2), with the captain, delegation and seat actions excluded pending an ADR-083 amendment; the CLI not a human channel for those clears until a proof of presence is adopted by amendment (D3, narrowing ADR-084 Amendment A1's CLI arm for now); human-bound credentials in MCP clients supported but never admitting a D2 action (D4); and no new agent path (D5), with the rest as named directions. It amends ADR-084 only as "What this amends" states. This record builds nothing by itself; D1 takes effect when the enabling change Enablement describes lands.

## Consequences

- The server-check clear verbs are the D2 actions. #5228 is the issue that carries D1 for them, applying A1's 'over an agent-drivable channel (any MCP transport)' as D1's credential-kind test. A CLI path for any D2 action is refused under D3.
- Propose-and-confirm (#5229) is not assumed here. A confirm step for a queued waive is a clear of a server-check concern and is admitted only as D1 says.
- Today the Web UI clears a server-check concern by waive or defer only, from the attention queue's decision panel (`frontend/src/attention/decision-panel.tsx:234`, `:242`), which lists an open concern only while its run has an implement or review stage awaiting approval (`backend/internal/server/attention.go:343-353`, `:390-416`). A server-check concern open at any other time has no Web UI path. The Web UI has no bulk-waive or resolve-with-evidence call and does not mark a concern as a server check. D1 is therefore enforceable for the server-check clears once the enabling change shows that every open server-check concern is reachable from that panel, or names the run state in which it is not. The Web UI's waive and defer are the session path for the D2 action, so D1 is enforced for all four verbs by one enabling change: bulk waive and resolve-with-evidence refuse a server-check concern on any non-session credential from that change, and a human clears such a concern by waive or defer from the session. A Web UI bulk or resolve surface, which #5229's confirm surface may provide, is a convenience and not a prerequisite.
- An operator agent that today acts under a human-bound credential loses every D2 action once D1 takes effect for it. That is the intended effect.
- ADR-084 Amendment A1's CLI arm is narrowed for now, as D3 says. A1's Web UI arm and the rest of rule 5 stand.
- **Residuals.** A cookie session proves the session's forge identity, not human presence. A session cookie is a bearer secret: a process that can read the browser's cookie store, or drive the browser, sends a `session` request with its CSRF token and acts as that human. On a single-operator host D1 therefore rests on how the browser profile is protected, which the product cannot observe, and this record does not claim to prevent it. The excluded actions (D2) keep the credential-holder residual their own records state. For every action outside D2, listed in an exclusion or not, an agent session on a human-bound credential is treated as that human. That includes each action whose delegation condition is evaluated only when the request declares itself delegated (ADR-040 Decision 2): the paging contract holds for such a session only by the deployment remedy below. D1 closes only the paths the credential kind separates.
- **Enablement and migration.** D1 takes effect for an action when the change enabling it lands. That change provides an end-to-end authorized Web UI session path for the action, passes the positive validation case, and marks the action's entry as enforced in that registry. Until then the action keeps its present guard and the Context's residual stands for it. The deployment remedy is an operator-agent credential for every agent session.
  - A direct REST call with a bearer is refused like any other bearer call.
  - MCP tools for D2 actions stay registered and return the refusal with its remedy.
  - Enablement for each action carries the impact inventory, migration path and safe-to-ship statement that `docs/review/security-conventions.md:15` requires for a change that tightens handler-side authorization.
  - A human without a Web UI session has no path to a D2 action, and that is accepted. So is a deployment with no forge sign-in configured: it has no path to clear a server-check concern once D1 is enforced for that verb. Each enabling change says so in its safe-to-ship statement.

**What this amends.** ADR-084: rule 5's enforcement for the server-check clears (D1, D2), and Amendment A1's "a human clears it through the Web UI session or the CLI": the CLI arm is narrowed, until a proof of presence is in effect, to no CLI path (D3). The reason is that the server cannot tell a CLI call from a stdio MCP call. Both present the same stored bearer credential in the same request (Context), so A1's refusal "over an agent-drivable channel (any MCP transport)" cannot be enforced while a CLI bearer is admitted. A1's Web UI arm and the rest of rule 5 stand. ADR-083 (#3751) is not changed: exclusion (vii) leaves its actions under their present guards. Not changed: ADR-040 (Decision 4 is relied on), ADR-076 (per-agent identity stays out; its reasoning point 4), ADR-089, ADR-101, ADR-113 D2, ADR-117 (D1 and A1.3, with the direction above), ADR-123, ADR-126 and ADR-139. ADR-018 and ADR-031 are not amended: this record changes neither their merge-as-approval invariant nor the verified-landing resolution mechanism.

**Related:** ADR-018 and ADR-031 (only as the subject of the note above), ADR-040, ADR-076, ADR-083 (#3751), ADR-089, ADR-101, ADR-113, ADR-117, ADR-123, ADR-126, ADR-139; #5227, #5228, #5229.

- **Record metadata.** amends: ["ADR-084"]; supersedes: []; applies_to: []

### Validation

- For each D2 action whose entry is marked enforced, a handler test, run through the real middleware:
  - admits a human subject on a `session` credential;
  - refuses the same non-agent subject on `static`, `oauth`, `oauth_as`, `unrecorded` and `unknown` credentials, with authenticated legacy-token fixtures through the real middleware; separately refuses an authenticated run-bound `mcp_run` credential clearing a server-check concern on its own run;
  - pins each action's preserved agent-subject and delegated-request refusals, including a session-authenticated request marked delegated where that flag is refused;
  - shows that the credential check never authorizes an agent;
  - covers direct REST for every such action, and stdio MCP and HTTP `/mcp` forwarding wherever an MCP tool exposes the action;
  - shows that a forged or omitted channel header grants nothing;
  - observes committed state and audit effects both ways;
  - refuses a whole bulk batch with no partial effect;
  - has a RED counterfactual with the check removed.
- The four D1 properties, in five checks: (a) a session cookie on `/mcp` reaches no D2 action; (b) a test pins that the session creator has one caller, the forge sign-in callback; (c) no route mints a session from a bearer; (d) no supported MCP transport forwards a session cookie; (e) a session request for an enforced D2 action without a valid CSRF token is refused.
- A test pins the set of enforced D2 entries, so that clearing a mark is a visible change.
- The D4 report, where it names enforcement, reads it from the connected server, and a case where it cannot read it reports every D2 action as unprotected.
- The D2 completeness test: it enumerates every handler that refuses a caller on `isAgentSubject` or `operatorrole.IsTokenSubject`, or that refuses every request marked delegated (a handler that only evaluates a delegation condition for such a request is not one), and fails when its action is neither in the D2 list nor in its exclusions.
