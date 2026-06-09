# Fishhawk — System Overview

> Executive-level architecture. For the technical realization, see [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md).

**Fishhawk is the governed, auditable workflow for agent-driven software changes.** Teams commit a workflow spec to their repo; Fishhawk runs the coding agent under typed constraints, captures a signed audit trail, and gates every stage on human approval.

---

## What it is

```mermaid
flowchart LR
    subgraph CUST["Customer's world"]
        DEV([Engineer]):::person
        REPO[("Repo<br/>+ .fishhawk/workflows.yaml")]:::repo
        CI["CI runner<br/>(their compute)"]:::ext
    end

    subgraph FH["Fishhawk"]
        CP{{"Control plane<br/>orchestrate · gate · audit"}}:::core
    end

    AGENT(["Coding agent<br/>(pluggable)"]):::agent

    DEV -->|"commit spec · approve"| REPO
    REPO -->|"trigger run"| CP
    CP -->|"dispatch"| CI
    CI -->|"invoke"| AGENT
    AGENT -->|"signed trace + plan"| CP
    CP -->|"open PR"| REPO

    classDef person fill:#1f2937,stroke:#0f172a,color:#fff
    classDef repo fill:#e0f2fe,stroke:#0369a1,color:#0c4a6e
    classDef core fill:#0ea5e9,stroke:#075985,color:#fff
    classDef agent fill:#fef3c7,stroke:#b45309,color:#7c2d12
    classDef ext fill:#f1f5f9,stroke:#94a3b8,color:#334155
```

The agent runs on the **customer's own CI** — Fishhawk never holds their code. Fishhawk owns orchestration, policy, approvals, and the immutable audit record.

---

## The five surfaces

```mermaid
flowchart TB
    subgraph PLANE["Fishhawk control plane — fishhawkd (Go)"]
        API["REST API"]:::core
        SM["Workflow state machine"]:::core
        POL["Policy / constraint evaluator"]:::core
        AUD[("Signed audit log")]:::store
    end

    UI["Web UI<br/>plan review · approval · audit"]:::surface
    CLI["CLI<br/>validate · trigger · inspect"]:::surface
    APP["GitHub App<br/>tokens · sign-in · webhooks"]:::surface
    RUN["Runner action<br/>runs agent on customer CI"]:::surface

    STORE[("Postgres + object storage<br/>state · trace bundles")]:::store

    UI --> API
    CLI --> API
    APP --> API
    RUN --> API
    PLANE --- STORE

    classDef core fill:#0ea5e9,stroke:#075985,color:#fff
    classDef surface fill:#e0f2fe,stroke:#0369a1,color:#0c4a6e
    classDef store fill:#ede9fe,stroke:#6d28d9,color:#4c1d95
```

| Surface | Role |
|---|---|
| **Control plane** (`fishhawkd`) | The brain: workflow state, policy, approvals, audit, API. |
| **Runner** | Executes the agent on the customer's CI; signs and ships the trace. |
| **Web UI** | Where humans review plans, approve, and search the audit history. |
| **CLI** | Validate specs and drive runs from the terminal. |
| **GitHub App** | Repo access, user sign-in, and the webhook triggers. |

---

## How a change flows — and where humans decide

```mermaid
flowchart LR
    T(["Trigger<br/>issue · CLI · UI"]):::start
    P["Plan<br/>agent proposes"]:::agent
    G1{{"Human<br/>approves plan"}}:::gate
    I["Implement<br/>agent codes under constraints"]:::agent
    G2{{"Human<br/>reviews PR"}}:::gate
    M(["Merge<br/>audit sealed"]):::done

    T --> P --> G1 -->|approve| I --> G2 -->|approve| M
    G1 -.->|reject| P

    classDef start fill:#1f2937,stroke:#0f172a,color:#fff
    classDef agent fill:#fef3c7,stroke:#b45309,color:#7c2d12
    classDef gate fill:#fee2e2,stroke:#b91c1c,color:#7f1d1d
    classDef done fill:#dcfce7,stroke:#15803d,color:#14532d
```

**Every agent action is bounded by typed constraints and bracketed by a human gate.** Plans can be rejected back to the agent; implementation is checked against the approved scope before a PR ever opens; nothing merges without review. The full run is captured as a cryptographically signed, append-only audit trail.

---

## Why it matters

- **Control** — typed constraints (allowed paths, file limits, required outcomes) enforced automatically, not by convention.
- **Accountability** — every plan, approval, and diff is signed and immutable; "who approved what, when" is always answerable.
- **Trust boundary** — agents run on the customer's compute; Fishhawk governs without holding the code.
- **Pluggable** — the coding agent is swappable; the governance model is the product.
