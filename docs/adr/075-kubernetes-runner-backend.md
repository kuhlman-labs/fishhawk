---
id: ADR-075
title: "Kubernetes runner backend: run agent stages in the customer's own cluster instead of their CI platform"
status: accepted
date: 2026-07-28
issue: https://github.com/kuhlman-labs/fishhawk/issues/2310
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-075: Kubernetes runner backend: run agent stages in the customer's own cluster instead of their CI platform

## Context

Three runner kinds ship today: `local` (host-spawned), `github_actions`, and `gitlab_ci` (dispatch-only, dormant until #2043). All three run agent stages on **someone else's compute** — the operator's laptop, or the customer's CI platform.

Customers will want a fourth: **agent stages running as Jobs in their own Kubernetes cluster**, with no CI platform involved.

### The seam is already built

ADR-022 (#388) shipped the pluggable dispatcher. `runnerbackend.Backend` is **three methods** — `Kind()`, `HostDispatched()`, `TriggerStage(ctx, TriggerParams)` — over a forge-neutral `TriggerParams`. Adding a backend is a small, well-bounded piece of work.

**More importantly, the pull primitive already exists.** From `runnerbackend/local.go`: a backend reporting `HostDispatched() == true` causes its stages to **park at `awaiting_host_dispatch`** rather than being fired, and *"the host-dispatch marker endpoint (or an MCP spawn verb calling it) flips `awaiting_host_dispatch → dispatched` at the moment of the spawn."*

That is precisely the shape a controller inside a customer's cluster needs: poll for parked stages, spawn a Job, mark it dispatched. **No new state machine, and no inbound access to the customer's cluster.**

### Two decisions since then make this more attractive

- **ADR-072 (BYOK)** removes the model-credential question: the customer's key is a Secret in their own cluster.
- **ADR-074 (runner-hosted reviewers)** raises job count per run from roughly three to seven, since each declared reviewer becomes its own dispatched job.

### What is actually missing: forge credentials

Today the CI platform supplies the token the runner uses to clone, commit, push and open a PR — GitHub Actions hands it an App token, GitLab CI a job token. **A bare Kubernetes Job has no CI platform to supply one.** So `fishhawkd` must mint and hand a **short-lived, scoped forge credential**.

That is one of the three ideas in **#655 (PARKED: MCP gateway + per-agent non-human identity + short-lived scoped credentials)** — specifically the credential-lifecycle piece, which #655 itself notes *"pairs with the hosted runner's secret handling."* This ADR is what makes that piece load-bearing rather than speculative.

### An honest note on the motivation

The usual framing is "save CI minutes." The arithmetic is weaker than it sounds: run wall-clock is median 0.39h (p90 1.4h) *including gate waits*, so actual runner-minutes are perhaps 10–25 per run — cents at standard rates, against **$11.94 of inference per issue**. CI cost is low single-digit percent of run cost.

It strengthens with large runners (2–16× rates) and at volume. But the durable drivers are different and hold regardless of any price sheet: **capacity control**, **network access to internal services** (a customer's `verify` command may need their internal registry, package mirror or database), **security policy**, and simply not running agent workloads on a third party's compute. Those should lead when this is described to customers.

## Options

1. **Do not build it — customers use their CI platform.** Rejected: it forecloses customers whose `verify` commands need in-network access, whose policy forbids third-party compute for code-touching workloads, or who are CI-capacity- rather than cost-constrained. It also means a self-hosted Mode 1 customer must still depend on a hosted CI platform, which undercuts the self-hosting story.

2. **In-cluster push only.** `fishhawkd` creates Jobs directly via an in-cluster ServiceAccount. Nearly free for **Mode 1 self-hosted**, where `fishhawkd` is already in the cluster — but useless for hosted Mode 2, since Fishhawk's control plane cannot and should not reach into a customer's cluster.

3. **A pull controller in the customer's cluster.** A Fishhawk-supplied controller polls for `awaiting_host_dispatch` stages, spawns a Job, and calls the marker endpoint. Works for **both** modes — a Mode 1 user simply runs it beside `fishhawkd` — and needs no inbound access. This is how self-hosted CI runners generally work.

4. **Both**: in-cluster push for Mode 1, pull controller for Mode 2. Optimal per mode; two code paths and two deployment stories to document and support.

## Recommendation

**Option 3 — a pull controller, as the single mechanism.**

It covers both deployment modes with one component and one code path. A Mode 1 user runs the controller beside `fishhawkd`, where polling is a local call; the mild inefficiency versus direct Job creation is a fair price for not maintaining two dispatch paths — the same principle applied to the ECS/Helm split in ADR-073 and the reviewer split in ADR-074.

The backend registers with `HostDispatched() == true`, reusing the `local` park semantics verbatim rather than inventing a state.

### Scope the credential work to what this needs

Unpark **#655** for its **credential-issuance piece only**: short-lived, run-scoped forge tokens issued to a runner Job. The MCP gateway and per-agent non-human identity are separable and remain out of scope here — #655 itself calls driver-agent connectivity and executor-agent concerns *"independent axes."*

### Synergies worth planning around

- **ADR-063 / E51 gets easier on this substrate.** A Job *is* a container, and ADR-063's hard prohibition on exposing the host Docker socket is naturally satisfied. Some of E51's containment work is redundant here.
- **E51.4 (#2137) already owns the hard part** — daemon-dependent gate commands and the `scripts/test verify` testcontainers collision are the same DinD/nesting problem this substrate hits.

### Open sub-decisions

- **Runner image ownership.** The Job needs the `fishhawk-runner` binary **and** the agent CLIs at compatible versions (E32.13 / #1743's `agent_version` ranges). Does Fishhawk publish an opinionated image, ship a base for customers to extend, or both?
- **Credential blast radius.** How short is short-lived, what scope, and what happens when a Job outlives its token mid-run.
- **Observability.** CI platforms give log UIs for free. Logs stay in the customer's cluster, stream to Fishhawk, or both.
- **Queueing and concurrency.** CI platforms handle this; a controller needs its own answer, and it interacts with the per-tenant caps in E60.5 (#2294).

## Decision

**Accepted (2026-07-28).** Adopt **Option 3** — a pull controller in the customer's cluster, as the single mechanism for both deployment modes. It registers with `HostDispatched() == true`, reusing the `local` park semantics (`awaiting_host_dispatch` + the host-dispatch marker endpoint) verbatim rather than inventing a state.

A Mode 1 user runs the controller beside `fishhawkd`, where polling is a local call. The mild inefficiency versus direct in-cluster Job creation is a fair price for one code path — the same principle applied to the ECS/Helm split (ADR-073) and the reviewer split (ADR-074).

Four sub-decisions were settled; where they refine the Recommendation, **these govern**.

### 1. Fishhawk publishes an opinionated runner image

One supported image carrying the `fishhawk-runner` binary plus the agent CLIs at versions validated against the current release. **Version compatibility becomes Fishhawk's problem, not the customer's** — the `agent_version` ranges from E32.13 (#1743) are declared against an image Fishhawk controls, which is what makes them enforceable rather than aspirational.

Accepted cost: Fishhawk owns the CLI upgrade cadence and the compatibility matrix across releases. Given CLIs release near-daily (the 2026-07-08 `claude` auto-update that motivated #1743), this is a standing maintenance obligation, not a one-time build.

Customers needing a specific toolchain (a `verify` that requires Java 21, or an internal package mirror) are a real case this does **not** serve. Documenting an extension path is a reasonable follow-up; it is not in this decision.

### 2. Run-scoped forge tokens, minted at dispatch, delivered via the Job

`fishhawkd` mints a token scoped to the run's repository and run branch, with a TTL sized to the stage budget, handed to the Job as a Secret at creation. **The controller never holds a standing forge credential** — so compromising the controller does not yield repository write access, which it would under the alternative.

**Open and load-bearing: what happens when a stage outruns its TTL.** A long implement stage, or one that enters several fix-up rounds, can outlive a conservatively-scoped token — and the failure would surface as a confusing push rejection late in a run rather than as an obvious credential expiry. Renewal, generous TTL, or explicit mid-run re-mint: decide it in the epic, do not let it be discovered.

### 3. Logs stay in the customer's cluster

Fishhawk receives the signed trace bundle it already receives from every runner; raw Job logs stay in the cluster for the customer's own tooling.

Streaming logs to the control plane was rejected because it **contradicts the perimeter argument that motivates this entire backend** — build output is customer data, and this runner exists to keep it in-cluster. Accepting a harder support story is the consistent choice; the trace bundle is what Fishhawk is entitled to see.

### 4. #655 unparks for credential issuance only

Unpark the piece this ADR makes load-bearing: **short-lived, run-scoped forge credential issuance**. The **MCP gateway** and **per-agent non-human identity** remain parked — #655 itself frames driver-agent connectivity and executor-agent concerns as *"independent axes,"* and only the latter is on this path.

Named approver: repository maintainer (human).

## Consequences

Customers gain a runner topology with no third-party compute in the path: agent stages run in their cluster, on their credentials, with access to their internal network. Combined with ADR-072 (BYOK) and ADR-074 (runner-hosted reviewers), a hosted Mode 2 customer reaches the point where **no customer code and no customer credential is processed on Fishhawk infrastructure** — the control plane orchestrates and records only.

Costs and new surface:

- **A new distributed component.** A controller in customer clusters is something to version, upgrade, support and debug remotely — with the usual skew problem when a customer runs an old one.
- **Credential issuance becomes load-bearing**, pulling a piece of #655 out of parked status onto the critical path.
- **Observability regresses before it improves.** CI platforms supply log UIs, retries and queue visibility for free; all of it must be rebuilt or deliberately delegated to the customer's cluster tooling.
- **The runner image becomes a supported artifact** with an agent-CLI version-compatibility story (#1743).
- **Support surface widens**: "my run failed" can now mean a cluster problem, an RBAC problem, an image-pull problem or a quota problem, none of which Fishhawk can see directly.
