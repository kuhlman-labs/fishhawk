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

Coding agents can write the code. The bottleneck is whether **one person can run a
repository the way a team would**: set its direction, trust what the agents did,
prove it, and hand it to the next person without losing how it was run.

Fishhawk is the governed, auditable workflow layer above coding agents. It is
agent-agnostic, forge-agnostic, and opinionated about process. The workflow spec,
the audit chain, and the approval gates are not compliance bolt-ons — they are the
product.

The unit is the **repository**. The human who commands a repository is its
**captain**. The agents that do the work are its **crew**: planner, implementer,
reviewers, acceptance, the operator agent as first officer, and the roles that
keep a product running after the merge. The captain decides how much the crew may
decide and where; the crew advises, disagrees on the record, and escalates what it
may not settle. An organization is a fleet of such repositories, each with its own
captain — the enterprise case is the same model repeated, not a different product.

The durable model is asymmetric and is not transitional scaffolding: **humans set
direction and approve outcomes; agents implement.** Every design decision should
make that asymmetry cheaper to operate, not erode it.

Doctrine belongs to the ship, not the captain. Standing orders (this charter, the
workflow spec, the operator overlay), decisions and their reasons, and the record
of how past gates were judged live in the repository and on the audit chain — so a
new captain inherits how the repository is run, and changes it only in the open.

> Your agents do the work. You command the crew. Fishhawk holds the record.

---

## 2. Current phase: alpha

**Alpha means one developer, on their own machine, commands the full crew against
their own repository from the bridge — and could hand that repository to another
developer who picks it up without the first one in the room.** No Kubernetes, no
hosted service, no second approver, no founder assistance. The local stack
(`fishhawkd`, Postgres, object storage, the runner, the MCP server, and the Web UI)
comes up with the documented local path and nothing else.

The full crew, each ready in the sense of the done-means below:

| Role | Ready means | Carried by |
|---|---|---|
| Planner, implementer, reviewers | The plan → implement → review loop reaches a governed merge on a repo that is not this one | shipped |
| Acceptance | A running instance is validated against the plan's criteria | shipped; E72 #3324 |
| First officer (operator agent) | Drives runs and campaigns within delegation and pages the captain with a distilled hand-off | shipped |
| Groomer | Keeps the backlog decision-ready against this charter | E54 #2232 |
| Product manager | Proposes direction and release briefs for the captain to ratify | E71 #3240 |
| Release manager | Cuts an evidence-backed release the captain approves | E33 #1583 |
| Ops | Verifies a deploy and turns an alert into a governed incident issue | E35 #1585 |
| Architect | Checks plans against the in-repo decision record | E78 #3699 |
| Historian | Reads the chain back: precedent at the gate, digests, handover briefs | E75 #3696 |
| Chief engineer | Files and runs routine upkeep (dependencies, flakes, deprecations) at high autonomy | E79 #3700 |
| Security officer | Reviews declared sensitive paths with a threat-model lens and watches advisories | E80 #3701 |
| Comms | Turns user feedback into charter-anchored draft issues | E81 #3702 |

Alpha is tracked by E73 #3694 (ADR-080 #3693). The previous
alpha definition — an external team self-hosting on Kubernetes (E64 #2308) — moves
to beta.

### Phase themes

Themes T1–T6 are retired with the previous phase definition; their ids are not
reused (see §6).

- **T7 — The ship installs itself.** One documented local path brings up the whole
  stack on a developer's machine, from published binaries, with `init`/`doctor`
  getting a new repository to its first governed change without reading the source.
- **T8 — The bridge.** The Web UI is where the captain decides: an attention queue
  of what needs a human, gate decisions made there rather than in chat, and a
  per-repository view of crew activity and the record (E40). An approver who is not
  an engineer can act on a gate.
- **T9 — The full crew reports for duty.** Every role in the table above reaches its
  done-means on this repository and on one repository that is not this one.
- **T10 — The crew talks on the record.** Agents consult and flag one another through
  typed, addressed messages on the audit chain. A message is advice, never an
  order; a disagreement escalates to the captain rather than looping.
- **T11 — Change of command.** Precedent, decisions, and open work are readable by
  a captain who did not create them; a handover is a recorded event with a brief;
  a divergence from precedent is surfaced at the gate and resolved either as a
  one-off or as a doctrine change.
- **T12 — The loop survives contact with someone who is not the founder.** Recovery
  and campaign reliability hold without operator folklore. A failure mode that needs
  founder knowledge to escape is an alpha defect.

### What alpha does *not* require

Kubernetes or a production-posture Helm deployment (E62, E69 — beta), hosted
multi-tenancy (E44), BYOK (E61 — satisfied by construction when the captain's own
daemon holds their key), runner-hosted reviewers (E63), MCP over HTTP (E66 — local
MCP is stdio), a second eligible approver or quorum, and a second forge as a
blocker (GitLab support stays shipped; its live walk is beta — see ADR-080 #3693).
Earned autonomy — the crew's record recommending changes to its own delegation —
is designed in alpha and may land after it; the captain always ratifies.

---

## 3. Non-goals

These are stable across phases. An item that advances one of these is drift, and a
grooming run should flag it as such rather than ranking it.

- **N1 — Fishhawk does not build a coding agent.** It orchestrates them. Work that
  improves an agent's coding ability rather than the governance around it is out.
- **N2 — Fishhawk is not a project management tool.** The customer's tracker is
  source of truth. The board is a projection and a selection surface, never a
  coordination bus (ADR-064). This is about the customer's work: Fishhawk does not
  replace their tracker or own their planning. Fishhawk planning its *own*
  program — a ratified charter, a release brief the groomer reads from, readiness
  against approved criteria (E71) — reads from the tracker and is in scope; it is
  not N2 drift.
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
| **V1** | Directly unblocks the current phase definition (§2). For alpha: a solo developer cannot bring the stack up locally, cannot command a crew role from the bridge, or cannot hand the repository to another captain without it. |
| **V2** | Advances a named phase theme T7–T12 without being strictly blocking. |
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
| **R6** | Verification integrity: a control that cannot fail for the reason it names. An assertion, gate or harness that stays green when the property it claims to protect is removed; a control whose deletion leaves the suite passing; an invariant asserted in prose or a doc comment with no executable counterpart. Rank at or above a comparable missing-coverage item — an unfalsifiable test is worse than an absent one, because it also suppresses the signal that the test is missing. A proposal citing this line names the counterfactual that would demonstrate the control is load-bearing: delete it, observe the failure, restore. |

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
