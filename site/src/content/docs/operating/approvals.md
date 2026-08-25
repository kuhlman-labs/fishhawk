---
title: Approvals
description: Orientation for granting, conditioning, and refusing approvals — stub, filled by E12.3 (#2263).
---

:::note[This page is a stub]
The full account of approvals — who may grant one, how conditions bind, how
delegation and escalation change the bar — is written by [E12.3
(#2263)](https://github.com/kuhlman-labs/fishhawk/issues/2263). What follows is
orientation.
:::

## What approving does

Approving a [gate](/fishhawk/concepts/gate/) is a write. It records who
approved, when, on what artifact version, and with what reason — and that entry
is what a later reader relies on. It is not an acknowledgement that advances a
UI.

## Conditions bind

A plan approval can carry conditions. They are delivered to the implementing
agent as mandatory text that wins over the plan where the two conflict, and the
agent is expected to confirm each one in the pull request. Use them for the
narrow corrections that do not justify a full replan.

## Who may approve

By default the change's author cannot approve their own change, and no agent
can approve anything — `not: [author, agent]`. A workflow may raise the bar
further for sensitive paths: more approvals, a named team, a higher permission
level.

## Rejecting

Rejecting is the cheaper action at the plan gate than at the review gate. A
rejection reason propagates into the replan, so the useful rejection says what
was wrong with the approach, not just that it was wrong.
