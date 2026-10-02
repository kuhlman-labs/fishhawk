---
id: ADR-050
title: "Acceptance-agent egress + credential posture (runner-embedded default-deny proxy)"
status: accepted
date: 2026-07-01
issue: https://github.com/kuhlman-labs/fishhawk/issues/1540
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-050: Acceptance-agent egress + credential posture (runner-embedded default-deny proxy)

## Context

The acceptance stage (ADR-049 / #1519) introduces the one agent in Fishhawk that deliberately assembles the **lethal trifecta**: agent-controlled code execution + network access + credentials, all in one process. Every other agent is missing a leg — `review` has no network and no write authority; `implement` has no network to an untrusted running system. The running app the acceptance agent drives can render **attacker-controlled data** into what the agent observes, so the agent must be treated as potentially prompt-injected.

What exists today vs. the gap:
- **Have:** env-var credential isolation — `runner/cmd/fishhawk-runner/gateenv.go` runs gate subprocesses under a default-deny allow-list and explicitly denies `GITHUB_TOKEN`/`GH_TOKEN`/API keys.
- **Don't have:** any **network egress** control. The runner is a GitHub Action; its network is whatever CI permits. Egress filtering is net-new capability, and it is the crux of this decision.

ADR-049 mandated a **new Rule-of-Two row** for the acceptance agent (advisory, no write authority, plus a scoped network-egress allowance) but deferred the mechanism to this ADR. Invariant #1 (customer source/runtime never reaches the backend) still holds because the agent runs customer-side; this ADR governs what that customer-side agent may reach and hold. Tracked under epic E31 (#1528).

## Options

The decision is primarily about the **egress-enforcement mechanism**; the credential-minimization and blast-radius decisions are settled uniformly across options.

### Option A — Runner-embedded default-deny egress proxy *(recommended)*
The runner action stands up a filtering HTTP(S) proxy and forces the acceptance agent through it (`HTTPS_PROXY` + deny direct sockets), with a default-deny allow-list.
- **Pros:** strongest containment — a prompt-injected agent physically cannot reach a non-allow-listed host; portable across CI providers; the security boundary is owned by Fishhawk, not the customer.
- **Cons:** real engineering (TLS CONNECT handling, DNS pinning to prevent rebinding, proxy lifecycle in the action).

### Option B — CI-native network policy
Rely on the customer's runner network controls (GitHub-hosted egress rules / self-hosted firewall).
- **Pros:** cheapest for us.
- **Cons:** non-portable; puts a security-critical boundary in the customer's hands; unverifiable by Fishhawk. Rejected for a security control.

### Option C — Documented customer-responsibility only
Ship the posture + advisory guidance, defer hard enforcement.
- **Pros:** fastest.
- **Cons:** the trifecta is contained only by convention in v1. Rejected as the enforcement mechanism, but see the phasing note in Consequences.

## Recommendation

Adopt **Option A** — a runner-embedded default-deny egress proxy — as the enforcement mechanism, together with the following uniform posture:

1. **Egress (primary control).** Default-deny. Allow-list exactly three destination classes: (1) the declared **target instance host(s)**, (2) the **model API endpoint** (Anthropic/OpenAI), (3) the **Fishhawk backend** (signature-authed trace-bundle ship). Nothing else. The target host is declared in the workflow spec via a new **egress-allowance grammar** and is the only customer-controlled entry.

2. **Credentials (minimize the third leg).** The agent gets its **model API key** (unavoidable) and **customer-supplied target-instance test credentials** — injected only into the acceptance invocation env, scoped to the target, never sent to the backend. It gets **no MCP/Fishhawk API token** (acceptance ships its verdict via the signature-authed trace upload, which needs no token). Explicitly denied: repo write tokens, deploy credentials, and the broad Fishhawk API token, kept off the invocation env just as `gateenv.go` keeps them off gate subprocesses.

3. **Blast-radius backstop.** The agent is **advisory with zero write authority** (the ADR-049 Rule-of-Two row). Combined with the egress lock, a fully-compromised agent can at worst emit a **wrong verdict** — no exfil path (egress), no write path (authority). A reject is `must_page_human`; a wrong pass is caught by the observable-evidence requirement (verdict + confirmable artifacts, never verdict alone) and human arbitration.

4. **Rule-of-Two table.** Record the new acceptance row in `docs/ARCHITECTURE.md` §6 with these egress/credential/authority constraints.

## Decision

**Accepted (2026-07-01).** Adopt Option A: a runner-embedded default-deny egress proxy is the enforcement mechanism for the acceptance agent, with the egress allow-list (target + model + backend), the minimized credential set (model key + scoped target creds; no MCP token; repo/deploy/broad-API tokens denied), and the advisory zero-write blast-radius backstop as specified in the Recommendation.

Implementation is tracked by **E31.4 (#1532)** under epic **E31 (#1528)**; the runner acceptance executor (E31.7 / #1535) MUST NOT land before this posture is enforced. Human-led (autonomy:low) given the security criticality. Named approver: repository maintainer (human).

## Consequences

**Positive**
- Contains the lethal trifecta with a Fishhawk-owned, portable, verifiable control rather than customer convention.
- Preserves Invariant #1 (agent stays customer-side; only redacted verdict/evidence cross).
- Dropping the MCP token removes an entire credential leg with no functional loss.

**Negative / risks**
- **Net-new proxy engineering** in the runner action: TLS CONNECT tunneling, DNS pinning against rebinding, allow-list config from the spec, proxy lifecycle. This is the bulk of E31.4 and is security-critical (human-led).
- **Spec surface grows** — a new egress-allowance grammar (declaring permitted target host(s)) in the workflow schema; must be validated and kept minimal.
- **Residual model-endpoint channel.** The model API endpoint is necessarily allow-listed; a determined injection could attempt to smuggle data through model prompts. Out of scope here; the egress lock plus zero-write authority bound the practical risk.
- **Phasing.** If the full proxy cannot land in the first pass, the fallback is to ship the **egress-allowance grammar in the spec now** and enforce via documented customer-responsibility (Option C) as an interim, so hardening to the proxy later needs no spec change. The interim state must be called out explicitly — the trifecta is not technically contained until the proxy lands.

**Relations:** implements the deferred security posture of ADR-049 (#1519); enforced by E31.4 (#1532); gates E31.7 (#1535).
