---
id: ADR-052
title: "Intake and refinement: natural-language brief to gated epic/children drafts"
status: accepted
date: 2026-07-03
issue: https://github.com/kuhlman-labs/fishhawk/issues/1580
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-052: Intake and refinement: natural-language brief to gated epic/children drafts

## Context

Issue quality bounds everything downstream in the loop — observed live during the E31 campaign: the plan_acceptance_precheck rejected criteria-less plans (#1543 traced the gap to authorship), and campaign quality tracked issue quality throughout. Today issues arrive fully formed because the founder writes them; the SDLC cycle has no upstream stage. The filing primitives all exist — POST /v0/work-items renders conventions-complete items (workitems.go), the GitHub provider links sub-issues and writes depends_on markers, campaign assembly parses them into a wave DAG — but nothing DRAFTS: there is no natural-language-to-structured-epic surface, and no preview gate between an agent&#39;s draft and the tracker.

## Options

**Option A — operator-verb refinement agent with a draft-preview gate (RECOMMENDED).** A new backend surface takes an NL brief and runs a drafting agent that produces a structured draft — epic + children, each child carrying Summary/Scope/acceptance criteria/depends_on edges — rendered through the SAME workmgmt conventions the filing path uses, persisted as a reviewable draft artifact. NOTHING files until the operator approves; edits re-render; approval is audited; the filing executor then creates the epic + sub-issues + markers through the existing provider pipeline, idempotently. Drafts are pre-screened by the plan_acceptance_precheck rule set per child (a criteria-less behavioral child is flagged before the operator sees it) — closing the #1543 loop at intake, where it is cheapest.

**Option B — reuse the plan stage in a `refinement` workflow.** A run whose plan stage&#39;s decomposition becomes the epic&#39;s children. Pros: gates/audit/review machinery free. Cons: the plan artifact&#39;s sub_plan shape (scope hints, runtime predictions) mismatches issue-body needs (Summary/Scope/criteria per child); contorting the plan schema for tracker filing couples two contracts that evolve independently.

**Option C — freeform chat drafting (operator pastes agent output into gh).** No product work, no gate, no audit, no conventions enforcement. Rejected: this is the status quo minus the discipline.

## Recommendation

Option A. The draft-never-files-without-approval gate is the load-bearing property, mirroring the plan-approval gate&#39;s trust boundary; conventions reuse guarantees drafted items are byte-compatible with hand-filed ones; the intake-time criteria precheck is the highest-leverage quality intervention the session&#39;s evidence supports. Implementation epic: E34 (filed alongside) — draft model+agent, preview/approval gate, filing executor, MCP verbs, criteria quality gate, dogfood.

## Decision

**ACCEPTED 2026-07-03** (founder-delegated ratification, following ADR-054/E9's completed campaign). Option A is adopted:

1. **Draft-never-files-without-approval is the load-bearing invariant.** The drafting agent proposes; the operator decides. No provider write occurs before an audited operator approval; edits re-render the draft; the approval and the filing are separate audited acts.
2. **Conventions reuse, not a parallel renderer.** Drafts render through the SAME workmgmt conventions pipeline the filing path uses, so a drafted item is byte-compatible with a hand-filed one — E34's wave-0 conventions children (#1614–#1617) land FIRST and apply to both paths.
3. **Intake-time criteria gate.** Every behavioral child in a draft is pre-screened by the plan_acceptance_precheck rule set; a criteria-less child is flagged before the operator sees the preview (closes the #1543 loop at its cheapest point).
4. **Filing executor is idempotent** over partial failures (epic created, child N fails → resume completes the remainder, never duplicates).

Unblocks the E34 (#1584) campaign: wave-0 #1614/#1615/#1616/#1617 → #1592 → #1593 → {#1594, #1595, #1596} → #1597.

## Consequences

Positive: completes the upstream arc (idea → campaign-ready epic) and raises floor quality of everything downstream; drafted epics are campaign-assembly-ready by construction. Negative/risks: a drafting agent inherits the issue author&#39;s ambiguity — the gate exists precisely because drafts will sometimes be confidently wrong; partial filing failures (epic created, child N fails) need idempotent recovery; the operator remains the scope authority — the draft agent proposes, never decides.
