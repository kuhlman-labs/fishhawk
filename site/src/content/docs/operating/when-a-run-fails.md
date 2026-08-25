---
title: When a run fails
description: Orientation for diagnosing and recovering a failed run — stub, filled by E12.3 (#2263).
---

:::note[This page is a stub]
The recovery playbook — reading the trace, classifying the failure, and choosing
between retry, fix-up, and abandon — is written by [E12.3
(#2263)](https://github.com/kuhlman-labs/fishhawk/issues/2263). What follows is
orientation.
:::

## Failed is not the same as waiting

A run parked at a gate is waiting for a person. A run that has failed has a
terminal stage with a recorded cause. Check which one you have before reaching
for a recovery verb.

## Three broad causes

- **The agent failed.** A timeout, a token budget exceeded, a transport error
  from the agent CLI. Often transient, and retrying the stage is the first
  thing to try.
- **A constraint failed.** The change exceeded its declared scope, wrote a
  forbidden path, or missed a required outcome. This is the system working; the
  fix is to amend the scope or to replan, not to retry unchanged.
- **The verification failed.** Tests or lint went red. The agent can be asked to
  fix its own work in a follow-up pass against the same run, which is usually
  better than starting over.

## Where the evidence is

The [audit log](/fishhawk/concepts/audit-log/) carries the decision record and
the stage trace carries what the agent actually did. Between them, a failed run
is diagnosable after the fact by someone who was not watching it — which is the
point of recording it.
