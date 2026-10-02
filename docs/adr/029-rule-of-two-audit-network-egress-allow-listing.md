---
id: ADR-029
title: "Rule-of-Two / lethal-trifecta audit + network egress allow-listing"
status: accepted
date: 2026-06-09
issue: https://github.com/kuhlman-labs/fishhawk/issues/650
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-029: Rule-of-Two / lethal-trifecta audit + network egress allow-listing

## Context

ADR-028 (#642) settled **filesystem** confinement (hosted = container mount-confinement; local = detection-only). Two adjacent exposures remain unaddressed:

1. **Network egress** is unbounded. The implement agent runs with `--dangerously-skip-permissions` and arbitrary Bash; nothing allow-lists outbound network.
2. **Lethal trifecta.** Since #618 the plan agent ingests **untrusted issue-comment text** *and* can change state (writes code) *and* can reach the network (Bash). That is exactly the combination major labs call unsafe — and prompt injection is publicly acknowledged as unsolved.

The Agentic-SDLC report (#6) recommends designing *around* this structurally: least privilege, allow-listed egress, and **privilege separation** (a privileged planner that sees only trusted input; a quarantined LLM for untrusted content).

## Options

- **A — Status quo + document the risk.** Cheapest; leaves the trifecta open.
- **B — Egress allow-list on the hosted runner** (container network policy) + keep local detection-only. Closes the network leg for hosted.
- **C — B plus privilege separation**: the planner consumes a *quarantined summary* of untrusted issue comments rather than raw text, breaking the trifecta on the plan stage.

## Recommendation

Decide per-agent via Meta's **Rule of Two** (an agent should satisfy at most two of: processes untrusted input / accesses sensitive data / changes state or communicates externally). Apply to plan, implement, and review agents; record the egress posture for the hosted runner and the planner-input decision.

## Decision

**Adopt the Rule-of-Two posture — option C (plan) + option B (hosted egress) + detection-only (local).** Decided 2026-06-09 (operator-confirmed). Rule of Two = an agent should hold at most two of {processes untrusted input, accesses sensitive data / changes state, communicates externally}.

**Per-agent verdict (recorded):**
- **Plan agent — currently VIOLATES (all three legs):** untrusted issue-comment text (`backend/internal/prompt/prompt.go:148` `IssueComments`, the #618 snapshot) + produces the plan (state) + network (Bash). **Priority to fix.**
- **Implement agent — 2 legs (OK), conditionally:** works from the human-APPROVED plan (the approval gate is the trust boundary) + writes code + network. **Invariant: implement never re-ingests raw untrusted comment text.**
- **Review agent — 1–2 legs:** reads the diff (the agent's own output) + emits a verdict; lowest risk.

**Decision detail:**
1. **Plan agent → option C.** The planner consumes a **quarantined/sanitized summary** of issue comments, not raw text (a quarantine pass over `IssueComments` before they reach the plan prompt). Preferred over removing network from the plan stage, since comment content is the load-bearing signal (#618); the quarantine is the structural fix the Agentic-SDLC report (#6) calls for. Drops the plan agent to 2 legs. **Implementable now** (`backend/internal/prompt/`).
2. **Hosted egress → option B.** Allow-list outbound network on the hosted runner (container network policy), owned by the ADR-022 / #388 hosted-container requirements. Closes the network leg for all agents in the hosted path. **Gated on the hosted-container work (#388).**
3. **Local → detection-only**, consistent with ADR-028 (#642)'s local filesystem posture. Document the residual risk; do not attempt to network-confine the operator's own machine.
4. **Record the per-agent verdict** (plan = quarantined-input; implement = approved-plan-only + hosted-egress-allow-list + never-re-ingest invariant; review = low-risk) so the posture is explicit and auditable.

**Gate-subprocess credential isolation (the #800 input) — IN SCOPE, tracked separately.** The runner runs agent-authored worktree code during the compile gate (#728/#766), the test gate (#800), and the verify gate (#441/#651), each inheriting the runner process env (which may carry the GH installation token). That is the lethal-trifecta shape (agent code + network + runner creds). **Decision: run these gate subprocesses with a stripped/minimal env (no GH token / exfil-relevant secrets), and/or egress allow-listing during agent-code execution.** Recorded as in-scope for the ADR-029 posture but filed as its own dedicated impl issue.

**Impl issues (Project #7, Backlog):** #928 (plan-comment quarantine, now), #929 (implement never-re-ingest invariant + posture doc, now), #930 (hosted egress allow-list, blocked on #388), #931 (gate-subprocess credential isolation, now).

## Consequences

- The plan stage stops feeding raw untrusted text to a network-and-state-capable agent — the priority lethal-trifecta leg is broken structurally.
- Hosted runs get bounded egress once #388 lands; local runs remain detection-only with documented residual risk.
- The runner's gate phases stop exposing the GH token to agent-controlled code.
- The per-agent Rule-of-Two posture becomes an auditable invariant rather than an implicit property.

## Notes

Derived from the 2026-06-01 review of `agentic-sdlc-report.md` (gap G4). Cross-references ADR-028 (#642, filesystem confinement) and ADR-022 / #388 (pluggable runner backends — owns the hosted container requirements). Touches `runner/internal/agent/claudecode/claudecode.go` (egress) and `backend/internal/prompt/` (untrusted-comment ingestion, #618).

Parent epic: #389
