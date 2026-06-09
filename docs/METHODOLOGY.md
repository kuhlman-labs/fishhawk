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

## What "agents do the work, humans approve the work" means here

Fishhawk's product thesis is that humans setting direction and approving outcomes is the durable model — not transitional scaffolding. The autonomy tiers above reflect that. Even at high autonomy, humans authored the workflow that decided what the agent could do; the workflow itself is reviewed and approved by humans. Accountability never disappears, even when the keystrokes do.

---

## What is deliberately not committed to

- A claim that agents wrote *all* of Fishhawk. They did not, and will not. The low-autonomy tier exists because some surfaces of the system require human authorship.
- A timeline for any specific percentage of agent-authored code. The point is not to hit a number; the point is to do the work well, with the right level of agent involvement for each kind of change.
- A claim that the methodology is finished. The autonomy tiers above will be revised based on what the audit log shows. When they change, this document changes with them.

---

*See also:* [`MVP_SPEC.md`](MVP_SPEC.md) §12 for the methodology commitment in the v0 spec, and [`BRAND_FOUNDATIONS.md`](BRAND_FOUNDATIONS.md) §9 for how this shows up in the brand.
