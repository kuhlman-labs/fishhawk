---
title: audit log
description: The append-only, hash-chained record of every decision and outcome in a run.
---

The audit log is the record Fishhawk exists to produce. Every run, stage
transition, plan, approval, rejection, review verdict, constraint result, and
terminal outcome is written to it as an entry, in order, and never edited.

## Append-only and chained

Each entry carries the hash of the previous entry, so the log forms a chain.
Altering an entry after the fact invalidates every entry after it, which is
detectable without trusting the service that wrote it. A chain can be exported
and re-verified offline with the standalone verifier — the check does not
require Fishhawk to be running, or to be honest.

## What an entry answers

The questions the log is designed to answer months later:

- What did the agent propose, in its own words, before the code existed?
- Who approved it, when, and under what conditions?
- Did the constraints pass, and on what diff?
- What did the reviewers conclude, and were their concerns resolved or waived?

## The limit worth stating

The log covers what Fishhawk governed. A change that bypassed the workflow leaves
no entry, and the chain does not claim otherwise — its integrity guarantee is
that recorded entries were not altered, not that every change was recorded. That
is a property of how you adopt it, not of the hash chain.

## Related

- [When a run fails](/fishhawk/operating/when-a-run-fails/) — reading the log to
  find out what happened.
