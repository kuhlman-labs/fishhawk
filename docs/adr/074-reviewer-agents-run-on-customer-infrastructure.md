---
id: ADR-074
title: "Reviewer agents run on customer infrastructure: fishhawkd orchestrates and records but never executes an agent"
status: accepted
date: 2026-07-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/2306
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-074: Reviewer agents run on customer infrastructure: fishhawkd orchestrates and records but never executes an agent

## Context

Fishhawk executes agents in two places, and only one of them is the customer's.

| Path | Share of spend | Executes on | Credential |
|---|---|---|---|
| Stage agents (plan, implement, acceptance) | **85%** | the runner — customer's CI or their own machine | the customer's, in their environment |
| **Reviewers** (plan-review, implement-review) | **15%** | **`fishhawkd` itself** | whatever is in `fishhawkd`'s environment |

Verified in `backend/cmd/fishhawkd/serve.go`: `planReviewerSet.For()` constructs reviewer adapters inside the backend process. The `anthropic` provider issues an SDK call from the control plane; `claudecode` and `codex` **spawn CLI subprocesses of `fishhawkd`**. `grep` over `runner/` finds no review path at all. The withholding logic in the same file states the direction of travel plainly — a region-scoped key without a region endpoint is refused because it *"would send the region-scoped credential **and the review text** to the SDK's global default endpoint."*

**In Mode 1 self-hosted this is invisible**: `fishhawkd` runs inside the customer's perimeter, so reviewers already execute on customer infrastructure with the customer's key. **In hosted Mode 2 it is the whole problem**: the review prompt — carrying the full diff and the approved plan — crosses into Fishhawk's cluster, and the review runs on Fishhawk's infrastructure.

### Why this surfaced now

Three recent decisions converge on it:

- **Hybrid beta topology (2026-07-27)**: Fishhawk operates hosted Mode 2 while compliance-conscious partners self-host Mode 1. Reviews are the one behaviour that differs between the two modes.
- **ADR-072 (BYOK)**: customers supply model credentials. Reviews were the sole path that would have required Fishhawk to *hold* one — driving E61's per-account credential storage and, with it, a control plane holding and using many tenants' keys.
- **Operator requirement (2026-07-27)**: partners will want to control all infrastructure where agents run. Not most of it.

### Perimeter is not the same claim as region

Regional cells (ADR-062) place Fishhawk's infrastructure in the customer's **region**, satisfying geographic residency. They do not place processing inside the customer's **perimeter**. For the compliance-conscious buyers E11 targets — where audit is the reason they are there — *"in your region, on our infrastructure"* and *"on your infrastructure"* are materially different claims, and only the second supports "your code never leaves your environment."

### Relationship to #655 (PARKED: MCP gateway + per-agent identity)

**This does not fall under #655**, and that ticket draws the distinction itself: *"Driver-agent connectivity (this) is distinct from executor-agent pluggability … they are independent axes."* #655 governs agents connecting **to** `fishhawkd` (gateway, non-human identity, short-lived scoped credentials). This governs where **executor** agents run.

The two are **reinforcing**. #655's stated vision is *"deploy `fishhawkd` centrally (the shared record) and let many agents interact with it"* — reviewers running **inside** `fishhawkd` contradict that shape. And no new identity work is required first: the runner already ships signed verdicts to `POST /v0/runs/{run_id}/acceptance` behind `requireRunAccount(memberWrite, …)`, so reviews reuse a proven credential path.

## Options

1. **Status quo — reviewers stay in `fishhawkd`.** Rejected: it puts customer code on Fishhawk's infrastructure in hosted mode, makes Mode 1 and Mode 2 behave differently, and forces either per-account credential storage or Fishhawk paying for a cost the customer configures.

2. **Keep reviewers in `fishhawkd`; Fishhawk's key pays.** Removes the credential-storage requirement but not the execution-locus problem, and creates **customer-controlled COGS** — `reviewers.agents[]`, model, and codex `reasoning_effort` are all declared in the customer's own `workflows.yaml`, so a customer could configure five reviewers at maximum effort and Fishhawk would absorb it. Bounding that would mean capping customer governance configuration to protect Fishhawk's margin, which is the wrong constraint for this product.

3. **Keep reviewers in `fishhawkd`; store per-account customer credentials.** Rejected by the operator preference to manage no customer secrets, and it concentrates every tenant's key in a control plane that makes outbound calls with them.

4. **Reviewer agents run on the runner**, dispatched like any other stage, returning a signed verdict. `fishhawkd` orchestrates and records; it never executes an agent.

## Recommendation

**Option 4.** The principle it establishes is worth stating independently of the immediate driver:

> **`fishhawkd` orchestrates and records. It never executes an agent.**

That is *almost* already true — stage agents and acceptance both run on the runner. Reviews are the last exception, and closing it makes the control plane a pure orchestrator rather than "an orchestrator that also runs some agents."

**What follows for free:**

- All inference runs on the customer's credential in the customer's environment. BYOK needs no exception clause.
- **E61's per-account credential storage is deleted** — the largest and most security-sensitive piece of that epic. Its threat model is resolved by removal rather than mitigation.
- The control-plane image carries no agent CLIs and spawns no agent subprocesses, simplifying **E62.2** and keeping **E51**'s containment story scoped to runner gate commands.
- Mode 1 and Mode 2 become behaviourally identical instead of differing in where reviews execute.
- The customer-configured-COGS problem disappears: the customer configures reviewers *and* pays for them, on their own machine.

**Reuse the acceptance pattern**, which already solves the hard part: runner-hosted evaluation returning a signed verdict to an authenticated, run-scoped endpoint.

**Prompt assembly stays backend-side.** Review prompts are built by `backend/internal/prompt` and shipped to the runner exactly as stage prompts already are — so **E60.1's intake sanitization applies unchanged**, and the ADR-050 envelope work is unaffected.

### Open sub-decisions

- **Reviewer independence.** Today reviews are independent by construction — different process, machine and credential from the implement agent. Runner-hosted means the same CI the implement agent just ran in. A fresh dispatch with a clean checkout taking only the diff and plan as input is the likely answer (roughly what acceptance does when it rebuilds a preview), but it should be decided rather than assumed.
- **Latency.** Reviews currently land ~60–90s after the implement trace uploads. A CI dispatch is minutes, and that compounds with the gate-latency problem E60.3 exists to address.
- **Concurrency.** The backend fans out to two heterogeneous reviewers in parallel today. On the runner that is two dispatched jobs or one job running both — different cost and latency profiles.
- **Customer CI minutes.** A new cost customers will see that is not tokens. Two reviewers × two stages × N runs is real Actions consumption, and it belongs in the onboarding conversation rather than on a first bill.

## Decision

**Accepted (2026-07-27).** Adopt **Option 4**. The principle, stated independently of the driver that surfaced it:

> **`fishhawkd` orchestrates and records. It never executes an agent.**

Stage agents and acceptance already run on the runner; reviewers were the last exception. Four sub-decisions were settled; where they refine the Recommendation, **these govern**.

### 1. Fresh dispatch, clean checkout at base ref

A review runs in its own runner invocation with a **clean checkout at the base ref**, taking the diff and the approved plan as input. The reviewer sees the artifact, not the environment that produced it — the same shape acceptance uses when it rebuilds a preview.

**This preserves the two properties that matter.** Independence: the implement agent's working tree cannot leak into its own review. And **grounded citation**: plan-review criterion 6 requires a cited rule be quotable *"from the context in this prompt or a repository file you actually read"* — so the reviewer keeps repo access and can verify a convention by reading the file that states it. A prompt-only reviewer was rejected for exactly this reason; it would undercut ADR-068's conventions work, which depends on rules being verifiable in-repo.

Appending a review step to the implement job was rejected: cheapest in CI, weakest possible independence.

### 2. Latency is accepted — it hides inside gate wait

Review settlement moves from ~60–90s to roughly 3–5 minutes. Measured against the loop as it actually runs, that is invisible:

| | |
|---|---|
| plan→implement gate wait | **median 10.4 min**, p90 30.1, p99 97.1 |
| plan agent work | 2.0 min |
| review today | ~60–90s |
| review on the runner | ~3–5 min |

The operator is the slow part, not the review. Added latency is absorbed entirely by the window a human already takes to respond.

**No backend fast path.** An opt-out that kept the SDK reviewer for some tenants would reintroduce the two-behaviours problem this ADR exists to remove, and would re-diverge Mode 1 from Mode 2.

If the gate-latency work in E60.3 succeeds and operator response time drops sharply, revisit this — the justification is explicitly relative to current operator cadence, not absolute.

### 3. One dispatched job per reviewer

Each declared reviewer in `reviewers.agents[]` gets its own dispatched job, running in parallel.

This preserves **independent failure**, which `reviewers.agents[i].optional` (#1495) already depends on — one reviewer failing must not take the other's verdict with it. It also avoids installing `claude` and `codex` in a single job with both credentials present; heterogeneous reviewers exist to be genuinely different, and co-locating them in one environment works against that.

Accepted cost: 2 reviewers × 2 stages = **4 job spin-ups per run**.

### 4. CI minutes are documented, not engineered around

Reviews consume customer CI minutes — a new cost that is not tokens. Name it in onboarding (#2262 getting-started, #2257 partner playbook), and note that **the lever already exists**: `reviewers.agents[]` is customer-declared, so a tenant wanting cheaper reviews declares one reviewer instead of two.

Folding CI-minute estimation into E59's plan-gate forecast was considered and deferred — it is a second forecast to build, calibrate and keep from becoming a false-precision surface, for a cost the customer can already control directly.

Named approver: repository maintainer (human).

## Consequences

The control plane stops processing customer code entirely, which is a simpler and more defensible trust boundary than the current split — and it makes *"your code never leaves your infrastructure"* true rather than approximately true. Mode 1 and Mode 2 converge. BYOK completes without an exception. Three epics get smaller (E61, E62.2, E51).

Costs and risks:

- **The review path is a real refactor.** Reviews settle asynchronously control-plane-side today (post-#584); dispatch-and-await is a different lifecycle, and the reviewer-provider configuration (`FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` / `_CODEX_REVIEWER` and the deployment-configured model defaults) moves from a backend concern to a runner concern.
- **Reviewer independence weakens unless deliberately preserved**, per the sub-decision above.
- **Review latency increases**, on a loop where gate latency is already ~5x agent time.
- **Customers bear CI cost** they did not previously see.
- **Cost attribution changes shape**: review spend would arrive via the runner's signed bundle manifest rather than the backend's own token accounting. That actually *unifies* the cost path — one mechanism instead of two — but it is a change to `cost_recorded` provenance and touches E59's statistics.
- Spec grammar for `reviewers.agents[]` needs revisiting: provider availability becomes a property of the runner environment, not the deployment.
