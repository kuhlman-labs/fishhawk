---
id: ADR-076
title: "MCP over HTTP served by fishhawkd, with fishhawkd as the OAuth authorization server — supersedes ADR-033's loopback gate"
status: accepted
date: 2026-07-31
issue: https://github.com/kuhlman-labs/fishhawk/issues/2387
supersedes: ["ADR-033"]
superseded_by: []
applies_to: []
---

# ADR-076: MCP over HTTP served by fishhawkd, with fishhawkd as the OAuth authorization server — supersedes ADR-033's loopback gate

## Context

Three separate frictions have now been hit repeatedly, and they have been treated as one problem when they are two.

**1. Onboarding.** Registering the MCP server in any client means inlining a long-lived `fhk_` bearer into that client's config. `claude mcp get` prints it in the clear. Rotation means editing every client config. There is no expiry. Onboarding a second repository is a token-copy ritual — the friction that prompted this ADR.

**2. Reconnect-after-rebuild.** The harness owns a per-client `fishhawk-mcp` subprocess, so a rebuild needs a manual `/mcp`. ADR-060's shim (#1922) hot-swaps the child and largely retired this, but only for child rebuilds; a shim-binary change still needs a reconnect.

**3. Multi-client / multi-agent.** Each client spawns its own subprocess. There is no shared endpoint, so a second agent means a second process, not a second consumer.

ADR-033 (#843) decided **dual-transport, staged**: stdio default, an opt-in loopback HTTP transport, and *anything past loopback gated behind #655* (gateway + per-agent non-human identity + short-lived scoped credentials). #655 is PARKED except its credential piece, unparked into ADR-075/#2312 for the Kubernetes runner's forge credentials — a different concern (agents authenticating **to the forge**, not **to fishhawkd**).

That gate was right at the time. This ADR revisits it because (a) alpha means an external team self-hosts and onboards their own operators, making onboarding a first-hour experience rather than a dogfood annoyance, and (b) the MCP authorization specification has since settled, which changes what "do it properly" costs.

### What already exists, verified in the tree

- **Identity federation.** `fishhawk token login` runs **GitHub's** device flow (`login/device/code` → `login/oauth/access_token`), hands the GitHub token to `POST /v0/tokens/login`, and fishhawkd **re-verifies it server-side** before minting an `fhk_`. `GET /v0/tokens/login` advertises the configured client_id so the operator needs nothing out of band.
- **A credential store.** `cli/internal/credstore` — `$XDG_CONFIG_HOME/fishhawk` (else `~/.config/fishhawk`), `0600`/`0700`, keyed by backend URL, carrying subject, scopes, a `Provider` field, and an `ExpiresAt` field that is currently unused.
- **A resolution ladder,** in the CLI only: explicit flag → credstore → empty (`cli/cmd/fishhawk/run.go`).
- **Two token families with prefix-routed authenticators.** `fhk_` (operator, long-lived, sha256-hashed at rest) and `fhm_` (backend-minted, run-bound, 60-minute TTL, where ADR-021 makes the TTL the load-bearing protection).
- **What does NOT exist:** any OAuth *authorization server* surface. fishhawkd is a relying party on GitHub and a token minter. There is no authorization endpoint, no token endpoint, no AS metadata, no PRM.

### What the MCP authorization spec requires

Read against the current draft:

- Authorization is OPTIONAL, but HTTP transports **SHOULD** conform. **STDIO transports SHOULD NOT** follow the spec and **should retrieve credentials from the environment** — so the stdio + env path is spec-endorsed permanently, not a stopgap.
- The MCP server acts as an OAuth 2.1 **resource server** and **MUST** implement Protected Resource Metadata (RFC 9728).
- **"The implementation details of the authorization server are beyond the scope of this specification. It may be hosted with the resource server or a separate entity."**
- The AS **MUST** implement OAuth 2.1 and **MUST** provide RFC 8414 or OIDC Discovery metadata; AS and clients **SHOULD** support **Client ID Metadata Documents**. **Dynamic Client Registration is deprecated**, retained only for backwards compatibility.
- Clients **MUST** send RFC 8707 `resource` on authorization and token requests; servers **MUST** validate the token was issued **for them as audience**.
- **"MCP servers MUST NOT accept or transit any other tokens"**, and clients **MUST NOT** send tokens not issued by that server's AS.

**The decisive consequence:** a GitHub token cannot be accepted directly, and PRM cannot simply point at GitHub, because GitHub will not mint audience-bound tokens for a customer's Fishhawk resource. In Mode 1 self-hosted we also cannot require every customer to stand up Keycloak or Auth0. **Therefore fishhawkd must be the authorization server.** The identity half is already built — this is an AS with federated identity, which is a well-trodden shape, not a novel one.

### The distinction that has been conflated

Transport and auth are orthogonal, and only one of them fixes onboarding:

| Friction | Fixed by |
|---|---|
| Secret inlined in every client config | **auth model** |
| Reconnect-after-rebuild; multi-agent; per-client subprocess | **transport** |

Remote HTTP MCP with bearer auth — what Sentry and Linear ship as their non-OAuth path — fixes the transport frictions and **re-creates the onboarding one over HTTP**, since the client config still carries a bearer. ADR-033 already said "HTTP-MCP is not merely a transport flip"; this is the concrete form of that claim.

## Options

**(a) Status quo.** stdio only, `fhk_` in each client config. No new surface. All three frictions persist; every alpha operator meets the token-copy ritual in their first hour.

**(b) Credstore resolution only.** Give `fishhawk-mcp` the ladder the CLI already has: `FISHHAWK_API_TOKEN` → credstore → actionable error naming `fishhawk token login`. Onboarding becomes one `fishhawk token login`, then `claude mcp add` with no secret anywhere. Small, no new network surface, no ADR strictly required. Fixes onboarding only; transport frictions remain; the stored credential is still long-lived.

**(c) HTTP transport with bearer auth, no OAuth.** Fixes reconnect and multi-agent. Does **not** fix onboarding. Would also mean shipping a network listener whose only auth is a static shared token — the thing ADR-033 explicitly refused to expose past loopback.

**(d) MCP over HTTP served by fishhawkd, with fishhawkd as the OAuth AS.** The MCP surface becomes a route on the existing fishhawkd listener rather than a separate exposed binary; fishhawkd implements PRM, AS metadata, an authorization endpoint with PKCE, a token endpoint with refresh, CIMD client registration, `resource` handling and audience-bound tokens; the forge stays the identity provider behind the authorization endpoint's login step. stdio + env remains, spec-endorsed, as the headless/CI path.

**(e) (d) plus per-agent non-human identity.** Full #655. Each agent gets its own identity rather than acting under an operator's. Needed for Mode 2 hosted multi-tenant; not needed for a self-hosted single-team alpha.

## Recommendation

**(d), with (b) landing first as an independently useful slice.**

The reasoning:

1. **Only (d) fixes all three frictions,** and it is the shape that serves the hosted direction rather than being thrown away at beta. ADR-033 assumed the HTTP listener would be a new exposed service on `fishhawk-mcp`; serving the MCP surface **from fishhawkd** changes that calculus materially. In Mode 1 the customer's fishhawkd is **already** reachable from the operator's machine, the bearer middleware already prefix-routes `fhk_`/`fhm_`, and the MCP server is described in ADR-033 itself as "a thin, stateless proxy" that round-trips everything to fishhawkd. Collapsing the proxy removes a deployment unit rather than adding one.

2. **The AS work is smaller than "build an authorization server" implies,** because identity federation, server-side verification, scope restriction at mint, hashed-at-rest storage and prefix-routed authentication all exist. What is genuinely new is the OAuth front door: authorization endpoint + PKCE + `iss` (RFC 9207), token endpoint + refresh, two metadata documents, CIMD, and audience binding. It is real work and it is security-sensitive — it should not be hand-waved — but it is additive to a working identity system, not greenfield.

3. **(b) first is not a detour.** It is the only piece that helps *today*, it is needed regardless because stdio + env stays the permanent headless path, and it converts `ExpiresAt` from a dormant field into the seam where short-lived credentials later land.

4. **(e) stays out.** Per-agent NHI answers "which agent acted", which matters when multiple tenants share one deployment. In alpha the operator is the principal and `fhm_` already gives run-bounded agent credentials. Pulling NHI forward would roughly double the surface for no alpha-visible gain.

### Sequencing, each slice shippable

1. **Credstore resolution in `fishhawk-mcp`** — kills the secret in client config.
2. **MCP surface as a route on fishhawkd**, bearer-authenticated with existing `fhk_`/`fhm_`, HTTP transport alongside stdio — kills reconnect and multi-agent. Not yet spec-OAuth; still requires a bearer, so it is *not* an onboarding regression only because slice 1 already landed.
3. **PRM + AS metadata + authorization/token endpoints + PKCE + `iss` + `resource`/audience binding + CIMD** — makes it spec-compliant, so Claude Code and any conformant client self-onboard with a browser consent and no secret. The identity step behind the authorization endpoint supports **both GitHub and GitLab** as federated identity providers (GitLab SaaS + self-managed via a group-scoped OAuth app, per ADR-058/#1851; provider discriminator per the ADR-057 amendment). fishhawkd remains the sole AS — clients see one issuer regardless of forge.
4. **Short-lived access tokens with refresh**, activating `ExpiresAt`.

## Decision

**Ratified 2026-07-31 by the operator.**

- **Adopt (d), with (b) landing first.** The four slices above are the plan of record.
- **The MCP surface moves into fishhawkd** (open question 1 resolved): a route on the existing listener — no second deployment unit, no second TLS story, reusing the existing prefix-routed bearer middleware. `fishhawk-mcp` remains as the stdio binary; the signed-binary distribution (E19.7/#347) now covers the stdio path only.
- **Slice 3 gates alpha** (open question 5 resolved): alpha ships with spec-compliant OAuth onboarding, not just slices 1–2. The operator explicitly accepted the schedule cost as worth it for the project.
- **GitLab is a co-equal federated identity provider in slice 3** (scope amendment at ratification). This is the *identity* half of GitLab support only. The *forge* half — webhook admission, MR flow, notifier, run lineage against a GitLab repo — is ADR-058/#1851 and is tracked there, not here.
- **Combined validation:** the E36 second-repo live walk (#1642) is re-targeted to a GitLab-hosted repository, validating outside-this-repo operation and GitLab functionality in one walk. That walk therefore depends on BOTH this ADR's slices and the ADR-058 forge implementation; the dependency is recorded on #1642.

This **supersedes ADR-033's gate** — "defer anything past loopback to #655" — and replaces it with: past-loopback exposure requires **spec-compliant OAuth with audience-bound tokens**, which is slice 3, and does **not** require per-agent NHI, which stays #655. ADR-033's other decisions (stdio default; transport-agnostic tool registration; loopback hard-enforcement for the bare-bearer HTTP mode) stand.

## Consequences

- Onboarding becomes `claude mcp add --transport http <fishhawkd>/mcp` plus a browser consent. No secret in any config.
- One MCP surface for all clients; no per-client subprocess; the reconnect dance ends.
- fishhawkd becomes a security-critical OAuth AS. That is a real, permanent obligation: PKCE correctness, redirect-URI validation, `iss` validation, CIMD document validation, refresh-token rotation and revocation. It deserves the `autonomy:low` treatment throughout and should not be driven at high autonomy.
- GitLab operators onboard identically to GitHub operators: the AS federates to either forge for identity, so the client-side experience is forge-agnostic.
- Every existing `fhk_` keeps working — stdio + env is spec-endorsed and stays the headless/CI path. No forced migration.
- `fhm_` fits without redesign: fishhawkd is both AS and RS, so run-bound tokens are already audience-correct by construction; they become AS-issued tokens with a restricted scope set.
- Mode 2 multi-tenant inherits the AS rather than needing one built later, and #655 narrows to just per-agent NHI.
- Slice 3 on the alpha critical path adds security-sensitive work to the alpha schedule; the getting-started guide (#2262) documents the OAuth path as the primary onboarding.

## Open questions

1. ~~Does the MCP surface move into fishhawkd?~~ **Resolved at ratification: yes, into fishhawkd.**
2. **Scope vocabulary.** PRM advertises `scopes_supported` and the spec wants least-privilege challenges. Today's operator default set is coarse. **Deferred to slice 3 design:** decide whether MCP scopes mirror the existing set or get a narrower MCP-specific vocabulary.
3. **CIMD hosting.** Client ID Metadata Documents require the *client* to host an HTTPS metadata document. Whether Claude Code and Codex do so today needs checking; if not, pre-registration is the fallback and DCR is deprecated. **Research task, owner: operator-agent, before slice 3 planning.**
4. **Interaction with ADR-057 / E44 workspaces.** In Mode 2 the AS must issue tokens scoped to a workspace (tenant = forge account, with a provider discriminator). **Deferred to slice 3 design; the GitLab amendment makes this sharper, not softer — the token model must carry the provider discriminator from day one.**
5. ~~Does slice 3 gate alpha?~~ **Resolved at ratification: yes, slice 3 gates alpha.**

## Related

#843 (ADR-033 — the staged transport decision this amends), #655 (gateway + per-agent NHI + scoped creds; NHI stays parked here), #1851 (ADR-058 GitLab forge support — slice 3's GitLab identity work aligns with it; the GitLab *forge* run-loop work the re-targeted #1642 walk needs lives there), #2310/#2312 (ADR-075 run-scoped **forge** credentials — adjacent but distinct: agents authenticating to the forge, not to fishhawkd), #347 (signed MCP binary distribution, now stdio-only), #2262 (getting-started guide, which documents whatever onboarding path this settles), #1922 (ADR-060 shim, which mitigated but did not remove the reconnect friction), ADR-021 (read-only MCP surface origins and the `fhm_` TTL rationale), ADR-057/#1823 (deployment modes), #1642 (E36 live walk — re-targeted to a GitLab repo as the combined validation).

Surfaced 2026-07-31 while planning a second-repo onboarding for the E36 live walk (#1642). Ratified 2026-07-31.
