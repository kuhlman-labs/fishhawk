# Methodology — Fishhawk is built using Fishhawk

> **Status:** Draft v0.1
> **Last revised:** 2026-04-30

Fishhawk is the governed, auditable workflow for agent-driven software development. Fishhawk is also built that way. This document explains what that means in practice and what readers can expect to see in this repository over time.

This is a methodology commitment, not a marketing line. The honest version of "built by AI" is *specific* and *verifiable*. The commitments below are the form that takes here.

---

## The commitment

1. **The workflow spec is public.** [`.fishhawk/workflows.yaml`](../.fishhawk/workflows.yaml) is the workflow Fishhawk's own development is governed by. It exists in this repository from day one as a public commitment, even before the product can execute it.

2. **Self-hosting begins at day 21.** Per [`MVP_SPEC.md`](MVP_SPEC.md) §8, day 21 is the milestone where Fishhawk begins shipping its own changes through Fishhawk. From that day forward, every PR carries a workflow run ID and a link to its audit log entry.

3. **The audit log is published.** Once Fishhawk is self-hosting, the audit log of its own development is published as a public artifact. Outside readers can verify what agents did, who approved it, and when.

4. **Autonomy tiers are declared, not implied.** Different categories of work run at different levels of agent autonomy. The categories are listed below. They are honest about where human judgment is load-bearing and where it is not.

5. **No founder bypass under pressure.** The temptation to ship faster by skipping the workflow is treated as a signal — either the workflow has a friction point that needs design attention, or the pressure is illegitimate and the discipline is the point. Either way, the response is to investigate, not to bypass. Emergency paths exist, are themselves audited, and require post-hoc justification.

6. **Claims are specific.** Where we describe the role of agents in Fishhawk's development — in a blog post, a sales conversation, a marketing page — we cite the audit log. "73% of merged PRs in Q3 were implemented end-to-end by Claude Code under the medium-autonomy workflow, with human plan approval and human PR review" is the form. Vague claims like "built with AI" are not.

---

## Autonomy tiers

The tiers describe how Fishhawk's own changes are produced. They are not a feature of the product; they are a commitment about how this codebase is developed.

### Low autonomy (human-led)

Human writes the code. Agents may assist (autocomplete, code review feedback, test generation), but the human is the author and reviewer of record.

Applies to:

- Workflow spec parser and validator
- Audit log integrity layer
- Policy engine
- Anything cryptographic (signing, key issuance, signature verification)
- GitHub App authentication flow
- Anything else where a subtle bug has catastrophic consequences

### Medium autonomy (agent-implements, human-approves)

Agents implement the change end to end under a Fishhawk workflow run. A human approves the plan before implementation begins, and a human approves the resulting PR before merge. The agent does not merge.

Applies to:

- UI components
- Provider adapters (GitHub Issues, Linear, Jira, etc.)
- REST API endpoints
- The runner action (most of it; the cryptographic surface stays low-autonomy)
- Most product feature work

### High autonomy (agent-implements, agent-merges)

Agents implement the change end to end under a Fishhawk workflow run, and the workflow permits the agent to merge if all gates pass. A human is still on the hook as the named approver of the workflow that allowed the merge — accountability does not disappear, it moves up a level.

Applies to:

- Documentation
- Tests for existing behavior (not new behavior)
- Dependency bumps that pass CI
- Internal tooling
- Lint and format fixes

---

## Autonomy tiers in the workflow spec

The tiers above are **declared in the workflow spec**, and the tier name
is the declaration: `autonomy: low | medium | high` (ADR-066 / #2222;
`docs/spec/workflow-v2.md`). The tier expands to the **action matrix** —
one entry per action class, each naming a `mode`:

| Mode | Who acts |
|---|---|
| `gated` | The human acts. The fail-closed default: an unlisted class, an absent matrix and an absent `autonomy` all mean this. |
| `auto` | The operator agent may act, and only then is a `when` condition required — one named, backend-evaluable predicate the backend can answer from run state. |
| `report` | The operator agent surfaces a **proposal** and does not act: it records a `run_auto_driven` `act:report` row at a live gate (once per gate occurrence) for a human to act on. |

An explicit `actions` entry overrides the tier **for that class only**;
an approval gate declaring `autonomy` or `actions` supplies the whole
block for that gate and inherits nothing. The class-name set is open
(ADR-065), so a workflow type may declare its own class — an unknown
class is safe by construction: accepted at `gated` and `report`, where
it delegates nothing, and rejected at `auto`, where it would need a
backend-evaluable condition that does not exist.

| Tier | Preset | Expands to |
|---|---|---|
| Low | `low` | Every class `gated`. Nothing is delegated; every judgment — approval, fix-up routing, waiver, retry, merge — pages the human. This is also what a spec declaring no autonomy block resolves to. |
| Medium | `medium` | `approve: auto (clean_dual_approval)`, `fixup: auto (convergent_concerns)`, `retry: auto (infra_flake)`; `waive` and `merge` stay `gated`. Carries the full `page_human_on` event list (`gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`). The operator agent advances mechanical judgments whose evidence is unambiguous; waivers and merges stay human. |
| High | `high` | Medium plus `waive: auto (solo_low)` and `merge: auto (gates_resolved_ci_green)`. `page_human_on` still carries the full event list — high autonomy delegates clean-path verbs, never disagreement arbitration. |

Every class resolution carries its **provenance** — whether an explicit
entry, the tier expansion, or the fail-closed default decided it —
surfaced on `GET /v0/runs/{id}`'s `delegation.matrix` so an operator
reading `approve: gated (tier)` sees why an action was not taken.

Below workflow-v2 the same three tiers are declared as the `operator_agent`
block's `may_*` knobs (ADR-040 / #1026; `docs/spec/workflow-v0.md`), which
v2 replaced: knob-absence is `mode: gated`, `may_approve` is
`actions.approve`, `may_route_fixup` + `route_fixup_min_severity` is
`actions.fixup`, and `must_page_human` is `actions.page_human_on`. The
preset names align with the operator-role overlay's reserved
`knob_presets` key (#1025 / #1042 — soft dependency; the overlay may
reference these presets once it ships).

Two invariants hold at every tier (ADR-027 authority unchanged):

- A delegated action is **condition-gated, not trust-gated**: the
  backend re-evaluates the named condition against current run state at
  action time and refuses with the exact failed predicate otherwise.
  The delegation never widens what the action itself may do.
- `page_human_on` events (v0/v1: `must_page_human`) are non-delegable. A
  reviewer reject, a plan rejection, a scope amendment, any
  budget/policy override, an exception request, or a requirement
  arbitration always reaches the human, regardless of tier — and wins
  over a `report` proposal too: an event that must page is not a
  suggestion.

---

## Which changes may use which workflow (`applies_to`, machine-enforced)

The tier lists above say which *kinds of change* belong at which autonomy
tier — documentation and dependency bumps at high autonomy, the spec
parser and anything cryptographic at low. Until E53.3 that mapping was a
**convention**: the tier was a property of the workflow, and nothing
checked that the change routed through a workflow was the kind of change
that workflow was written for. An operator could start a run against the
parser under a high-autonomy docs workflow, and the run would proceed at
that workflow's tier.

A workflow's optional `applies_to` predicate (ADR-066 / #2226;
`docs/spec/workflow-v2.md`) closes that gap by making the mapping
**declared and enforced**. It states which changes a workflow may be used
for — by issue `labels`, by `trigger` form, and by `paths` — and the
backend refuses a run that does not satisfy the declaration of the
workflow it named. So "documentation is high autonomy" can stop being a
sentence in this file that a human is trusted to honour, and become
`applies_to: {paths: ["docs/**"]}` on the high-autonomy workflow, which a
run touching the parser cannot satisfy.

Enforcement is **fail-closed at two points**, each the earliest at which
the criterion has a producer: `labels` and `trigger` at run admission,
`paths` at the plan gate against the plan's `scope.files`. Both fire
before any implement work, so a refusal costs a re-run and never
half-applied work — and because `scope.files` is binding rather than
descriptive, a run cleared under a `docs/**` declaration is *confined* to
docs for the rest of the run, not merely claimed to be.

**The override is post-hoc-justified, not pre-authorized, and that is the
point.** The sanctioned exception is `applies_to_override` plus a
**required** reason on the create request, recorded as a run-scoped audit
entry that names what was bypassed and why. It is deliberately not a
permission an operator holds in advance and not a knob in the workflow
spec: nothing is checked *before* the override is used, and everything
about it is legible *after*. That places it with the other overrides in
this methodology rather than with the delegated actions — it does not
widen what any tier may do, it records a human stepping outside the
declaration for one run.

The first adoption of an `applies_to` declaration **will refuse a run
someone wanted**. That is the control working, and the intended responses
are, in order: amend the declaration (a reviewable change to the
governance file), or use the audited override for the single run.
Downgrading enforcement to warn-only was considered and rejected in
ADR-066 — a routing control nobody is refused by is a comment.

The trust boundary is worth stating plainly, because a governance control
that oversells itself is worse than one that doesn't exist: labels are
fetched by the caller and shipped inline on the create request, so
`applies_to` prevents **misrouting**, not a determined authorized caller.
Server-side label fetching is the named hardening path.

---

## The acceptance agent (medium autonomy, advisory validator)

The acceptance stage (ADR-049 / ADR-050; `docs/spec/workflow-v1.md`) runs under
the **medium-autonomy** `feature_change` workflow, after the human PR review
settles and before merge. It is deliberately the *narrowest*-authority agent in
the system: it validates the running preview instance against the approved
plan's `verification.acceptance_criteria` and emits a **signed verdict** — and
nothing else. It is **advisory and zero-write**: it holds no repo, MCP, or
Fishhawk backend credentials (evidence ships signature-authed, not
token-authed), so a fully prompt-injected acceptance agent can at worst emit a
*wrong verdict*, never mutate a repo, a run, or the audit log (ADR-050; the
containment posture — default-deny egress, credential minimization, zero-write
authority — is the §6 Rule-of-Two "acceptance" row of `ARCHITECTURE.md`).

Where judgment is load-bearing here, it is **not** the agent's:

- A **failed verdict** is routed by **deterministic server-side triage**, not by
  agent discretion. Class 1 (an error, or all failed criteria trace to explicit
  issue-stated sources) auto-routes an implement fix-up; class 2 (no failed
  criterion, a skip/flake) auto-reruns the acceptance stage. Both are bounded —
  a spent rerun/fix-up budget pages the human.
- The **non-convergent and ambiguous classes always page the human**: class 3 (a
  failed *inferred*-source criterion — the acceptance signal disagrees with what
  the plan inferred, so the miss belongs to plan review, recorded as a
  `plan_review_miss`) and class 4 (an unitemized failure) route to a human with
  no auto-transition.
- The **merge gate stays with the human/operator**. `acceptance_passed` is an
  operator-loop `next_actions` condition, not a delegated `may_merge`: the
  acceptance agent's verdict informs the merge decision but never makes it.

So the acceptance agent adds an automated *check*, not automated *authority* —
consistent with the medium tier, where agents do the work and humans approve it.

---

## The intake refinement stage (medium autonomy, human-gated filing)

The intake refinement flow (ADR-052; `fishhawk_draft_epic`) turns a
natural-language **brief** into a structured epic-plus-children draft, gated
behind a preview and an operator approval before anything files. It runs at the
**medium** tier, and — like the acceptance agent — it is deliberately arranged
so the agent adds *work*, never *authority*.

Where the agent does the work:

- **Drafting is agent work.** The drafter decomposes the brief into an epic and
  its children (summary, proposal, done-means, acceptance criteria, labels,
  1-based `depends_on` sibling ordinals). This is the medium-tier pattern — the
  agent produces the structured artifact from the human's intent.

Where judgment is load-bearing, it is **not** the agent's:

- **The criteria gate is a deterministic advisory check, never a block.** The
  E34.5 criteria quality gate grades every drafted child's acceptance criteria
  through the same deterministic rule set the plan-stage acceptance pre-check
  runs, surfaced as the `criteria_precheck` block on the session view. A child
  with an unjustified missing blocking criterion is flagged `needs_attention` —
  but approval and filing remain legal. The gate informs the operator's verdict;
  it does not make it.
- **Approval is an operator gate.** The approve/reject decision requires a
  `write:approvals` operator token (the intake analogue of the plan-approval
  gate — no new scope), a required reason, and is decide-once per revision. An
  edit after approval structurally re-gates via the pinned content hash (an
  approval whose hash no longer matches fails closed to `awaiting_approval`), so
  a stale approval can never carry a changed draft to filing.
- **Filing is deterministic and idempotent, and runs only after that human
  approval.** The drafter itself never files (ADR-052 decision 1). The filing
  executor writes to the provider only over the resolved-and-approved,
  hash-pinned draft, resuming at the first unfiled item on a retry and never
  re-filing a recorded one.

**The parity invariant.** Drafted children ride the *exact* hand-filed
conventions pipeline (`workmgmt.Apply`, byte-compatible by construction), so an
agent-drafted item is indistinguishable from a hand-filed one — the capstone's
conventions-parity spot-check. Like the acceptance agent, intake adds automated
*work*, not automated *authority*: the human still approves what files, and the
filing itself is a deterministic executor, not agent discretion.

---

## What "agents do the work, humans approve the work" means here

Fishhawk's product thesis is that humans setting direction and approving outcomes is the durable model — not transitional scaffolding. The autonomy tiers above reflect that. Even at high autonomy, humans authored the workflow that decided what the agent could do; the workflow itself is reviewed and approved by humans. Accountability never disappears, even when the keystrokes do.

---

## What is deliberately not committed to

- A claim that agents wrote *all* of Fishhawk. They did not, and will not. The low-autonomy tier exists because some surfaces of the system require human authorship.
- A timeline for any specific percentage of agent-authored code. The point is not to hit a number; the point is to do the work well, with the right level of agent involvement for each kind of change.
- A claim that the methodology is finished. The autonomy tiers above will be revised based on what the audit log shows. When they change, this document changes with them.

---

*See also:* [`MVP_SPEC.md`](MVP_SPEC.md) §12 for the methodology commitment in the v0 spec, and [`BRAND_FOUNDATIONS.md`](BRAND_FOUNDATIONS.md) §9 for how this shows up in the brand.
