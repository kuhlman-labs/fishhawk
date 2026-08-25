---
title: What Fishhawk is
description: A workflow engine that gates agent-driven software changes behind human approvals and records what happened.
---

Fishhawk sits between a coding agent and your repository. It decides what the
agent is allowed to attempt, when a human has to say yes, and what gets written
down.

A change moves through an ordered sequence of stages — typically plan, then
implement, then review. Each stage names who executes it (an agent, a human, or
neither), what it consumes, what it produces, and what it may not touch. Between
stages sit gates: a stage does not advance until its gate is satisfied, and an
approval gate is satisfied by a person, not by the agent that did the work.

## What that buys you

**The plan is reviewable before the code exists.** The agent proposes a scope —
which files it will touch, what tests it will add, how it will verify the
result — and a human approves or rejects that proposal. Rejecting a plan costs
one stage. Rejecting a finished pull request costs the whole change.

**Policy is enforced, not requested.** A constraint like `forbidden_paths` is
checked against the real diff. An agent that writes to a forbidden path fails
the stage; it does not get a warning it can talk its way past.

**The record survives the conversation.** Every plan, approval, rejection,
verdict, and outcome lands in an append-only audit log with a verifiable hash
chain. You can export it and re-verify it offline, without Fishhawk running.

## Where it runs

Fishhawk is tool-agnostic and agent-agnostic. The control plane (`fishhawkd`) is
a Go service backed by Postgres. The runner executes agent stages on your CI or
on a local machine. The workflow itself is declared in a file in the repository
it governs, `.fishhawk/workflows.yaml`, so the policy is versioned alongside the
code it constrains.

## The honest trade-off

This adds friction. A gated change is slower than an ungated one, and the gates
are the product — if you remove them you have a coding agent with extra steps.
Fishhawk is worth adopting when you need to answer *who approved this, and on
what basis* months after the fact. If nobody is going to ask that question, the
overhead is not paying for anything.

Next: [what Fishhawk is not](/fishhawk/start/what-fishhawk-is-not/).
