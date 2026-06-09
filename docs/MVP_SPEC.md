# Fishhawk — MVP Specification

> **Status:** Draft v0.1
> **Owner:** Brett
> **Last revised:** 2026-04-30
> **Purpose:** Canonical reference for the v0 build. This document is the single source of truth for what we are building, why, and in what order. When this document and reality disagree, update this document.

---

## 1. Product thesis

Coding agents are now capable enough that the bottleneck has shifted from "can the agent write the code" to "can the business trust what the agent did, prove it, and reproduce the process across a team." No one is selling that as a first-class product.

Fishhawk is the **governed, auditable workflow that sits above coding agents**. It is agent-agnostic, tool-agnostic, and opinionated about *process*. It is not a coding agent. It is the layer that turns a collection of agent invocations into a governed, auditable, organizational workflow.

### What Fishhawk is

- An opinionated workflow engine for agent-driven software changes
- A policy enforcement layer for what agents can and cannot do
- An immutable audit trail of agent activity, plans, approvals, and outcomes
- A coordination layer between developers, agents, project trackers, and code hosts

### What Fishhawk is not

- A coding agent (we orchestrate them; we don't build one)
- A project management tool (the customer's tracker is source of truth)
- A CI/CD platform (we run on the customer's CI)
- A monitoring or incident response system (out of scope for v0; possible v2+)

### The vision (for context, not v0 scope)

The future of software development is **humans setting direction and approving outcomes; agents handle implementation.** Fishhawk is the coordination layer that makes this work at organizational scale. Human accountability is not transitional scaffolding — it is the durable model. Agents radically extend human leverage; humans retain accountability for anything consequential.

---

## 2. Target customer

**Primary ICP for v0 design partners:**

- **Size:** 50–300 engineers
- **Stack:** GitHub (Issues, PRs, Actions), already using at least one coding agent (Claude Code, Cursor, Copilot)
- **Profile:** Compliance-conscious — regulated industry (fintech, healthtech, regulated B2B SaaS) or selling into one. Has a real answer to "how do you control what AI does to our code" as a procurement requirement.
- **Pain:** Coding agents are in use but ungoverned. No standardized workflow. No auditable record of what agents did or who approved it. Different developers use agents differently. Compliance, security, and engineering leadership are uncomfortable but can't articulate the missing primitives.

**Buyer:** Engineering leadership (VP Eng, Director of Platform, Head of DevX). Compliance and security are influencers; the budget sits in engineering.

**User:** Individual developers and tech leads.

---

## 3. Positioning

> Fishhawk is the governed, auditable workflow for agent-driven software development. We give engineering teams an opinionated, auditable process for how AI agents plan, implement, and ship changes — without locking you into any specific agent, tracker, or stack.

**Differentiation against incumbents:**

| Incumbent | Their angle | Fishhawk's wedge |
|---|---|---|
| IBM Bob | Full-stack SDLC orchestration, IBM-aligned | Tool-agnostic, opinionated workflow, OSS-friendly |
| AWS DevOps Agent | AWS-centric ops + agent | Multi-stack; planning + implementation, not just ops |
| GitHub Copilot agent | Tightly integrated, single-vendor | Multi-agent orchestration; governance layer above |
| LangGraph / CrewAI | Generic agent orchestration | Opinionated for SDLC specifically; built-in audit/policy |
| loopctl | Closest direct competitor | OSS, marketplace distribution, audit-first |

**The moat we are building toward:** the workflow spec becomes a standard the team encodes in its repo, plus the audit history they accumulate. Once a team has six months of audit data they need to retain, switching cost is high.

---

## 4. Core abstractions

### 4.1 Workflow spec

Lives in the customer's repo at `.fishhawk/workflows.yaml`. Declarative YAML. Version-controlled. Diffable. Validated by `fishhawk validate` CLI before commit.

**Six primitives, no more:**

1. **Workflow** — named, versioned definition of a process
2. **Stage** — unit of work in a workflow (type: `plan` | `implement` | `review`)
3. **Gate** — exit condition on a stage (type: `approval` | `check`)
4. **Constraint** — rule enforced *during* a stage (e.g., forbidden paths, max files)
5. **Approver** — human or role reference, resolved against GitHub teams in v0
6. **Artifact** — typed output of a stage (`plan` | `pull_request`), persisted with explicit destinations

**Three stage types only:** `plan`, `implement`, `review`. No custom types in v0.

**No conditionals, no parallelism, no sub-workflows in v0.** Different work types use different workflows (e.g., `feature_change` vs. `docs_change`). Conditional logic is where governance dies.

### 4.2 Workflow spec example (canonical reference)

```yaml
version: "0.3"

roles:
  tech_lead:
    members: ["@org/tech-leads"]
  senior_engineer:
    members: ["@org/senior-engineers", "@org/tech-leads"]
  any_engineer:
    members: ["@org/engineering"]

workflows:
  feature_change:
    description: "Default workflow for new features and non-trivial changes."

    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        inputs:
          - source: github_issue
            required: true
        produces:
          - artifact: plan
            schema: standard_v1
            persistence:
              - target: originating_issue
                mode: rendered_comment
                update_on_change: true
              - target: fishhawk_audit_log
                mode: canonical
        budget:
          max_tokens: 200000
          max_runtime_minutes: 15
          enforcement: advisory  # v0; "blocking" in v0.x
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead, senior_engineer]
            sla: 4_business_hours

      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
        constraints:
          - max_files_changed: 30
          - forbidden_paths:
              - "infra/**"
              - ".github/workflows/**"
              - "security/**"
              - ".fishhawk/**"
          - required_outcomes:
              - tests_added_or_updated
              - ci_green
        budget:
          max_tokens: 500000
          max_runtime_minutes: 30
          enforcement: advisory

      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvers:
              any_of: [senior_engineer]
```

> Note: spec v0.2 (ADR-017 / #249) dropped the gate-level
> `blocking_checks` field. Required CI checks are now derived from
> GitHub branch protection at run-create time and snapshotted onto
> the run row. The `fishhawk_audit_complete` signal is published as
> a Check Run (#231) and enforced via branch protection, not the
> spec.

### 4.3 Plan artifact schema (`standard_v1`)

Plans are first-class artifacts with required structure. The schema is versioned (`standard_v1`, `standard_v2`...). Old plans in the audit log remain readable forever; we never break old schemas.

Required fields:
- `plan_version` (schema version)
- `ticket_reference` (origin)
- `generated_by` (agent, model, timestamp)
- `summary` (human-readable description)
- `scope` (files to modify/create, estimated lines changed)
- `approach` (ordered steps)
- `verification` (test strategy, rollback plan)

Optional fields:
- `risks_and_assumptions`

Plans are persisted in two places per workflow run:
- **Fishhawk audit log:** canonical version, full structure, immutable.
- **Originating GitHub issue:** rendered as a formatted comment, kept in sync if the plan changes. The issue is the durable in-context view; Fishhawk is canonical for content.

### 4.4 Audit log

The audit log is the central artifact of the product. Customers' compliance posture depends on it.

**Per workflow run, the audit log captures:**

- The originating ticket / request (URL, content snapshot)
- The workflow spec version (git SHA of `.fishhawk/workflows.yaml` at run time)
- The plan artifact (full content, all versions if regenerated)
- The agent identifier and model version used
- Full prompt and tool-call trace (or content-addressed pointer to it)
- All gate evaluations: who approved, when, from what surface, what was the policy decision
- The diff implemented
- Test results and CI outcomes
- All policy events (constraint hits, budget overruns)
- Any failure events with category and detail
- The final PR identifier and merge commit (if merged)

**Properties:**
- Append-only at the application layer
- Signed entries (per-run ephemeral key issued by Fishhawk backend)
- Content-addressed for trace storage (deduplicates re-runs)
- Exportable in standard formats (JSON, CSV; SOC 2-shaped reports as v0.x feature)

**Storage:**
- Postgres for queryable metadata
- S3-compatible object storage for full prompt/response payloads
- Redaction model documented from day one (prompts will contain secrets; we redact known patterns and store an unredacted version with stricter access control)

---

## 5. Architecture

### 5.1 Components

1. **Backend control plane** (Go or TypeScript service)
   - Workflow execution state machine
   - Policy evaluator
   - Approval state management
   - Audit log writer
   - GitHub App webhook receiver
   - REST API for CLI and UI clients

2. **GitHub Actions runner** (published as `fishhawk/runner@v1`)
   - Invokes the configured agent (Claude Code in v0)
   - Captures full execution trace
   - Validates artifacts against schema
   - Enforces constraints post-hoc on stage output
   - Signs and ships trace bundle to backend
   - Versioned, customers reference but don't fork

3. **Web UI**
   - Workflow run visualization
   - Plan review and approval
   - Audit log search
   - Workspace and config management

4. **CLI** (`fishhawk`)
   - `fishhawk validate` — workflow spec validation
   - `fishhawk run start` — trigger a workflow run
   - `fishhawk run status <id>` — check status
   - `fishhawk run open <id>` — open canonical view in browser
   - **Not** in scope: plan review, approval, audit interaction

5. **GitHub App**
   - Installed by customers from the GitHub Marketplace
   - Scoped permissions: contents (rw), issues (rw), pull requests (rw), checks (rw), workflow (w), metadata
   - Per-installation tokens for repo access
   - Webhook events drive workflow triggers and state updates

### 5.2 Execution model

**Plan generation runs on customer CI infrastructure (GitHub Actions for v0).** The customer's code never leaves their environment. Fishhawk's backend orchestrates and observes; the runner executes.

Flow:
1. Trigger arrives (GitHub issue assigned/labeled, CLI `run start`, or UI button)
2. Backend validates workflow spec, creates run record, dispatches a `workflow_dispatch` event to the customer's repo, invoking the Fishhawk runner action
3. Runner action checks out the repo, invokes Claude Code with the constructed prompt, captures the full trace
4. Runner validates the produced plan against `standard_v1` schema
5. Runner signs the trace bundle with the per-run ephemeral key, ships to backend
6. Backend verifies signature, stores the artifact, posts the rendered plan as a comment on the originating issue, transitions stage state, notifies approvers
7. Approver reviews the plan in Fishhawk's UI (canonical surface), approves
8. Backend dispatches the next stage to the runner with the approved plan
9. Runner invokes Claude Code with the implement prompt, monitors output for constraint violations, validates `required_outcomes` post-hoc
10. Runner pushes branch, opens PR via the GitHub App credentials, ships final trace
11. Backend transitions to review stage, awaits human approval on the PR
12. On merge, backend writes the final audit log entry

### 5.3 Trust model

- **Code:** lives only in the customer's environment (their repo, their CI runners). Fishhawk never sees the source.
- **Traces:** captured by the runner, signed with a per-run ephemeral key issued by Fishhawk at job-start, verified by Fishhawk before storage. A tampered or modified runner cannot inject false traces because the signing key is short-lived and tied to a backend-issued run ID.
- **Constraints:** enforced post-hoc by the runner on stage output. The runner is published, versioned, and pinned via the workflow spec. Customers reference (`uses: fishhawk/runner@v1`), they don't fork. A compromised runner is a supply-chain failure mode; we treat it accordingly (signed releases, SBOM, security disclosure process).
- **Credentials:**
  - Repo access: per-installation token from the GitHub App, scoped to the workflow run.
  - Agent API key (Anthropic for Claude Code): customer-supplied via their GitHub Secrets in v0. Fishhawk-issued ephemeral key path comes in v0.x to enable centralized budget enforcement.

### 5.4 Identity and authentication

- **Sign-in:** GitHub OAuth for v0. Users authenticate as their GitHub identity.
- **Approvers:** resolved against GitHub teams referenced in the workflow spec's `roles` block.
- **API access:** scoped tokens issued by Fishhawk's backend, attached to a user identity, revocable, audited.
- **SSO/SAML:** v1+ enterprise tier feature. Identity surface is designed now to make this addition non-breaking.

---

## 6. Failure handling

Failures are first-class. All failure modes are recorded in the audit log with category metadata, surfaced in the UI, and reflected in a comment on the originating issue. Re-execution is allowed in all cases.

| Category | Description | Re-execution |
|---|---|---|
| A: Agent failure | Agent errored, produced invalid output, or ran out of tokens | Retry stage |
| B: Constraint/policy violation | Output violated forbidden paths, exceeded limits, etc. | Modify-and-retry (may need different inputs) |
| C: Infrastructure failure | Runner crashed, network failure, backend outage | Retry; audit log honestly reflects gaps |
| D: Approval timeout | SLA elapsed without approver action | Retry; no auto-escalation in v0 |

**UI principle:** failed runs are equally visible to successful ones. No hiding, no auto-clearing.

**Audit principle:** the log honestly reflects what happened, including "we lost the trace for stage X due to runner crash." Honesty about gaps beats fictional completeness.

---

## 7. Trigger surfaces and surfaces of presence

### 7.1 Triggers (where workflows start)

**v0:**
- GitHub issue assignment to the Fishhawk bot, or labeling with `fishhawk`
- Web UI "Start run" button (onboarding + ad-hoc cases)
- CLI: `fishhawk run start --workflow feature_change --issue gh:org/repo#1247`

**v0.1+:**
- Linear ticket assignment
- Jira ticket label trigger
- Slack slash command (deferred per Brett — on roadmap)

### 7.2 Presence (where users live during a run)

- **Fishhawk Web UI:** canonical surface for plan review, approval, audit, workflow visualization. Where the differentiated experience lives.
- **GitHub Issue:** rendered plan as comment, status updates as comments, link back to canonical view in Fishhawk. The customer's source of truth.
- **GitHub PR:** created by Fishhawk bot account during implement stage, with `Co-authored-by:` trailers attributing approver and agent. Description references workflow run ID.
- **Email:** notifications for approval requests with deep links to the UI.
- **Slack:** v0.x — notifications first, low-risk approvals later. Never bypasses policy engine.

---

## 8. Build plan (90 days)

### Days 1–10: Foundations

- Workflow spec parser and validator (covers all v0 primitives)
- `fishhawk validate` CLI
- Postgres schema for runs, stages, artifacts, audit entries (carefully designed — this schema is hard to migrate)
- GitHub App registered, OAuth flow working
- Backend control plane scaffold with REST API
- GitHub Actions runner scaffold that can invoke Claude Code, capture trace, sign, and ship
- Per-run ephemeral key issuance and verification

**Methodology:** founder-built, by hand. No Fishhawk-on-Fishhawk yet. Abstractions aren't stable.

### Days 11–21: First self-execution

- End-to-end plan stage: GitHub issue → backend → runner → Claude Code → plan artifact → audit log → rendered comment on issue → approval flow in basic UI
- First Fishhawk PR ships through Fishhawk itself
- Workflow spec syntax v0 frozen for the rest of the 90 days (changes only via spec versioning)
- Plan artifact `standard_v1` schema frozen

**Milestone:** day 21, Fishhawk builds Fishhawk. Every PR from this point flows through the system.

### Days 22–60: Build through Fishhawk

- Implement stage with constraint enforcement (forbidden paths, max files, required outcomes)
- Review stage with PR-linked approval
- Plan diff viewer in UI (regenerated plans diff against prior versions)
- Audit log search (by repo, approver, date, outcome, run ID)
- Failure handling for all four categories
- Budget visibility (token/runtime tracked and surfaced; not yet enforced)
- Bidirectional sync hardening: edits to plan in UI → tracker comment update; tracker comment edits → comments on canonical
- Marketplace listing application submitted (process runs in parallel)

**Autonomy tiers (Brett's own dev):**
- **Low autonomy / human-led:** workflow spec parser, audit integrity layer, policy engine, anything cryptographic, GitHub App auth flow
- **Medium autonomy:** UI components, provider adapters, REST API endpoints, the runner action
- **High autonomy:** docs, tests for existing code, dependency bumps, internal tooling, lint/format

### Days 61–90: Design partners

- 3–5 design partners actively running real work through Fishhawk
- At least 2 of them in compliance-conscious environments where audit is the *reason*, not a nice-to-have
- First compliance report export (JSON + human-readable summary of "all changes by agents in date range")
- Pricing decided based on partner conversations
- OSS repository public; Apache 2.0 on core, BSL on enterprise modules
- First post-mortem on Fishhawk's own development methodology, using Fishhawk's own audit log as evidence

**Milestone:** day 90, paying conversations with at least 2 partners. v0 feature-complete.

---

## 9. v0 / v0.x / v1 boundaries

### v0 (90-day MVP)

- Workflow spec primitives: workflow, stage (3 types), gate, constraint (closed set), approver, artifact
- GitHub Issues + GitHub Actions + Claude Code integration
- Server-side plan generation (on customer CI)
- Audit log with signed traces, redaction, full-fidelity capture
- Web UI: plan review, approval, run visualization, audit search
- CLI: validate, start, status, open
- Failure handling for four categories
- Budget visibility (advisory enforcement only)
- GitHub App + OAuth sign-in
- Hosted-only deployment

### v0.x (next 90 days)

- Linear and Jira providers
- Cursor and Copilot agent providers (proving agent-agnosticism)
- Real budget enforcement (Fishhawk-issued ephemeral agent keys + proxy)
- Slack notifications and low-risk approvals
- Compliance report exports (SOC 2-shaped)
- Plan editing in UI (not just regeneration)
- Marketplace launch

### v1

- SSO/SAML
- Multi-tenant audit retention tiers
- Workflow spec custom predicates (Rego or Cedar)
- BYO compute / self-hosted runner alternative
- Multi-CI providers beyond GitHub Actions

### Explicitly out of scope

- Multi-repo workflows (separate product question)
- Deploy monitoring, incident response, rollback orchestration (entirely separate product, possibly v2 or never)
- Custom stage types beyond plan/implement/review
- Workflow conditionals
- Workflow marketplace / templates (until there's a community)

---

## 10. Open decisions (with deadlines)

| # | Decision | Deadline | Notes |
|---|---|---|---|
| 1 | Backend implementation language | ~~Day 1~~ **Decided 2026-04-30: Go.** | Go for agent-runtime semantics, single-binary deploys, and ecosystem fit with the GitHub Actions runner. |
| 2 | Hosted infrastructure (cloud, primary services) | Day 3 | Likely AWS or GCP. Postgres + S3-compatible object storage + container runtime for backend. |
| 3 | Pricing for design partners | Day 60 | Free during DP phase, locked-in pricing later? Specific dollars TBD. |
| 4 | Pricing model: per-engineer vs. per-run | Day 60 | Leaning per-engineer. |
| 5 | Design partner sourcing strategy | Day 30 | Network, cold outreach, both? Target: 5 partners by day 75. |
| 6 | OSS repo: public from day 1 or at v0.1? | ~~Day 14~~ **Decided 2026-04-30: public from day 1.** | Repository is live at github.com/kuhlman-labs/fishhawk under Apache 2.0. |
| 7 | Contribution model (CLA vs. DCO) | ~~Day 30~~ **Decided 2026-04-30: DCO.** | See CONTRIBUTING.md. Sign-off required on all commits. |
| 8 | Marketplace billing: through GitHub or direct? | Day 45 | Through GitHub for early-stage convenience; revisit for enterprise. |

---

## 11. Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Incumbent (GitHub, IBM, AWS) ships overlapping features | High | High | Workflow spec adoption + audit history as moat; speed and OSS distribution |
| Workflow spec design wrong, requires breaking changes | Medium | High | Spec versioning from day one; freeze v0 syntax by day 21; iterate on v2 if needed without breaking v1 |
| Audit integrity bug (lost entries, leaked secrets, broken signatures) | Medium | Catastrophic | Human-led implementation of audit/policy/crypto layers; security review before any paying customer; redaction model documented and tested from day one |
| Runner supply-chain compromise | Low | High | Signed releases, SBOM, pinned versions in workflow spec, security disclosure process |
| Solo founder velocity insufficient | Medium | High | Aggressive scope discipline (this document), agent-assisted dev with autonomy tiers, hire customer success early |
| Design partners don't materialize | Medium | High | Start outreach by day 30; have a "no design partners by day 60" pivot plan (likely: tighten ICP and try again) |
| Methodology theater accusations | Medium | Medium | Honest, specific claims; published audit log of own development; verifiable autonomy tier metrics |
| Bypassing own workflow under pressure | High | High | Emergency-path workflow designed and itself audited; commitment that founders never bypass |

---

## 12. Methodology commitment

Fishhawk is built using Fishhawk, starting day 21. This is a constraint of the build, not a marketing line.

- Every change to Fishhawk flows through a Fishhawk workflow run after day 21
- Autonomy tiers (section 8) are enforced for the founder's own development
- The audit log of Fishhawk's own development is published as a public artifact
- The workflow spec for Fishhawk's own dev is open in `.fishhawk/workflows.yaml` in the public repo
- Emergency paths exist, are themselves audited, and require post-hoc justification
- No founder bypass under pressure

If at any point the founder finds themselves wanting to skip the workflow to ship faster, that's a signal that either (a) the workflow has a real friction point that needs design attention, or (b) the pressure is illegitimate and the discipline is the point. Either way, the response is to investigate, not to bypass.

---

## 13. Definitions of "done" for v0

The MVP is complete when all of the following are true:

- [ ] A new customer can install Fishhawk from GitHub Marketplace and run their first workflow within 10 minutes
- [ ] Fishhawk itself ships every PR through a Fishhawk workflow run, with public audit log
- [ ] At least 3 design partners are running real (non-toy) work through Fishhawk
- [ ] At least 2 of those partners are in compliance-conscious environments
- [ ] First compliance report export works end-to-end
- [ ] Audit log integrity is verifiable: any external party can take an exported log + signing key chain and verify entries
- [ ] Failure handling for all four categories is tested and documented
- [ ] OSS repo is public, Apache 2.0 on core
- [ ] At least one design partner has agreed to be a named reference
- [ ] Pricing is decided and at least one design partner is ready to convert to paid

---

## 14. What this document is not

This document is the spec for v0. It is not:

- A technical design document (architecture sketches here, not detailed designs)
- A go-to-market plan (covered separately as decisions firm up)
- A long-term product vision (the vision is Section 1; the roadmap beyond v1 is deliberately undefined)
- A contract or commitment (decisions in section 10 may shift; this document is updated when they do)

When this document and reality disagree: update this document. When this document and the workflow spec syntax in `.fishhawk/workflows.yaml` disagree: the syntax wins, this document is updated.

---

*End of v0 spec.*
