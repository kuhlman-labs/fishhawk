# Fishhawk Charter

The anchor for prioritization. A backlog grooming run (ADR-065 / E54) reads this
document from the base ref, and every ranking it proposes must cite a rubric line
below by id. If a proposed priority cannot cite a line here, the proposal is wrong
or this document is incomplete — both are useful signals, and both are fixed by a
human editing this file, never by an agent.

**This document is human-authored.** Agents read it; agents do not write it. It is
resolved from the run's base ref precisely so that a change cannot alter the
charter constraining it.

---

## 1. North star

Coding agents can write the code. The bottleneck is whether an organization can
say what its agents did, prove it, and reproduce the process across a team.

Fishhawk is the governed, auditable workflow layer above coding agents. It is
agent-agnostic, forge-agnostic, and opinionated about process. The workflow spec,
the audit chain, and the approval gates are not compliance bolt-ons — they are the
product.

The durable model is asymmetric and is not transitional scaffolding: **humans set
direction and approve outcomes; agents implement.** Every design decision should
make that asymmetry cheaper to operate, not erode it.

> Your agents do the work. Your team approves the work. Fishhawk holds the record.

---

## 2. Current phase: alpha

**Alpha means ADR-057 Mode 1 works end-to-end for someone who is not us.** An
external team self-hosts Fishhawk on their own Kubernetes cluster and runs governed
changes against their own repository, on GitHub or GitLab. No hosted service, no
multi-tenancy, no design-partner program — those are beta (E57 #2255).

Alpha is tracked by E64 #2308.

### Phase themes

- **T1 — An external repo runs the loop.** Onboarding, `init`/`doctor`, external
  repo support, intake sanitization. Someone who did not build this can start a
  governed run without reading the source.
- **T2 — Two forges, not one.** GitHub and GitLab both reach a governed merge. Forge
  agnosticism claimed in positioning has to be demonstrable, not architectural.
- **T3 — The spec is the governance surface, and its break-window closes here.**
  Workflow spec v2 consolidation (E52) and the declared control surface (E53). A
  major cannot be broken in place after the first external consumer, and an alpha
  user *is* an external consumer.
- **T4 — The install artifact is production-posture.** A Helm chart an external
  operator can actually deploy, with real secrets, ingress, and a migration hook
  proven under failure.
- **T5 — The loop survives contact with people who are not the founder.** Recovery
  and campaign reliability are the weakest subsystems; the happy path is strong. A
  failure mode that requires operator folklore to escape is an alpha defect.
- **T6 — The backlog stays decision-ready without a human sweeping it.** This
  charter's own reason for existing (E54). Fishhawk generates inflow faster than a
  human grooms it.

### What alpha does *not* require

Web UI (E40 — alpha operators drive via MCP and CLI, as this repo has since day 22;
the accepted risk is that a non-engineer approver cannot act on a gate), hosted
multi-tenancy, BYOK (E61) and runner-hosted reviewers (E63) — both satisfied by
construction under Mode 1, since the customer's own daemon holds their key inside
their own perimeter.

---

## 3. Non-goals

These are stable across phases. An item that advances one of these is drift, and a
grooming run should flag it as such rather than ranking it.

- **N1 — Fishhawk does not build a coding agent.** It orchestrates them. Work that
  improves an agent's coding ability rather than the governance around it is out.
- **N2 — Fishhawk is not a project management tool.** The customer's tracker is
  source of truth. The board is a projection and a selection surface, never a
  coordination bus (ADR-064).
- **N3 — Fishhawk is not a CI/CD platform.** It runs on the customer's CI.
- **N4 — Fishhawk does not own deploy mechanics.** The governed deploy *gate* and
  the signed record are in scope (ADR-038); pipeline logic, production credential
  custody, and rollout execution are not.
- **N5 — No monitoring or incident-response product.** Post-deploy verification
  feeding the dev loop is in scope (ADR-053); competing with an observability vendor
  is not.
- **N6 — No user-extensible stage types and no workflow conditionals.** The stage
  set is a small closed set. Conditional logic is where governance dies.
- **N7 — No multi-repo workflows.** A separate product question.
- **N8 — Flexibility is not a goal.** Fishhawk is opinionated on purpose. "Make it
  configurable" is usually the wrong answer to a disagreement about process.

---

## 4. Prioritization rubric

Each line has a stable id. **A proposed ranking must cite at least one id.** Lines
are ordered by weight within their group; groups are not strictly ordered against
each other — an item scoring V1 and an item scoring R1 both belong near the top,
and the report should say which it is rather than blending them into one number.

### Value — does it move the current phase?

| id | line |
|---|---|
| **V1** | Directly unblocks the current phase definition (§2). For alpha: an external team cannot self-host, or cannot run on their forge, without it. |
| **V2** | Advances a named phase theme T1–T6 without being strictly blocking. |
| **V3** | Removes recurring operator toil that Fishhawk itself generates. Toil the product creates and does not absorb is a defect in the product, not a cost of doing business. |
| **V4** | Makes the governance story demonstrable to an evaluator — the question "how do I constrain what the agent may do?" needs a real answer, not an architecture diagram. |
| **V5** | Improves the product for a phase that is not the current one. Real value, wrong time; rank below V1–V4 and say so. |

### Risk — what does deferring it cost?

| id | line |
|---|---|
| **R1** | A window that closes. A breaking change that becomes unmakeable after the next milestone (the E52 case: a spec major cannot be broken in place after the first external consumer). Deferring converts a cheap change into a permanent constraint. |
| **R2** | A safety or containment property. Anything where the failure mode is an agent acting outside what a human approved, or an audit chain that cannot substantiate a claim. Governance defects outrank feature work. |
| **R3** | A correctness or data-integrity defect on a load-bearing path. |
| **R4** | Compounding cost: the item gets more expensive the longer it waits, typically because more code is written against the shape it should have had. |
| **R5** | Reputational exposure at the current phase — something an external operator would hit early and read as unreadiness. |

### Dependency unblocking — what does it free?

| id | line |
|---|---|
| **U1** | Blocks two or more other items, or blocks an entire epic's critical path. |
| **U2** | Is on the critical path of the current phase's tracking epic. |
| **U3** | Reserves a seam whose retrofit cost across multiple implementations is high — cheap now, expensive after the second consumer exists. |
| **U4** | Blocks nothing, and nothing blocks it. Schedule on value alone. |

### Staleness and hygiene — is the item still true?

| id | line |
|---|---|
| **S1** | The body no longer describes the remaining work (scope moved, part already merged, a re-scope updated the title but not the body). Re-scope before ranking — a stale body produces a wrong plan, and this failure is recurrent. |
| **S2** | Superseded or duplicated by another item. Flag the pair and the basis; never close as a side effect of grooming. |
| **S3** | Depends on a decision that has since been made, or on an ADR that has since been ratified or rejected. Reconcile the body against the decision. |
| **S4** | Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class. |
| **S5** | Aged out. Filed against a phase that has passed and not advanced since. Propose icebox, never silent closure. |

---

## 5. How a grooming run should use this

1. **Cite, do not blend.** Every ranking entry names the rubric id justifying it. A
   score with no citation fails validation (#2235).
2. **Prefer flagging to resolving.** An ambiguous scope call surfaced is worth more
   than a confident plan whose framing nobody agreed to. `mode: report` exists for
   this.
3. **Drift is a finding, not a ranking.** An item advancing a §3 non-goal is
   reported as drift. Do not quietly rank it last.
4. **Propose nothing when nothing changed.** Sub-threshold churn trains
   rubber-stamping, which erodes the gate every other control rests on (#2240).
5. **Nothing destructive by default.** Duplicate and scoping proposals are surfaced
   for a human. Nothing auto-closes.
6. **A gap here is a finding.** If a genuinely important item cannot cite any line
   above, say so in the report. That is a charter defect, and the fix is a human
   editing this file.

---

## 6. Amending this document

Edited by the repository maintainer, in its own change, reviewed like any other
governance surface. Because grooming resolves it from the base ref, an amendment
takes effect on runs starting after it merges — a change cannot loosen the charter
that constrains it.

Rubric ids are stable identifiers. Reuse of a retired id for a different meaning
would silently rewrite the justification of every past ranking that cited it: retire
ids, never recycle them.
