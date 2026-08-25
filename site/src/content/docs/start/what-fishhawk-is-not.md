---
title: What Fishhawk is not
description: The three things Fishhawk is regularly mistaken for, and what to use instead.
---

The negative surface matters as much as the positive one. Fishhawk is narrow on
purpose, and knowing what it declines to do is the fastest way to tell whether
it fits.

## Not a coding agent

Fishhawk does not write code. It invokes an agent you already use — Claude Code
today, others as adapters land — and constrains what that agent is permitted to
do. If you are looking for something to generate a patch, you want the agent;
Fishhawk is what you put around it.

## Not a CI/CD platform

Fishhawk holds no build, test, or deploy logic of its own. It calls your test
entrypoint and reads the exit code. It does not replace GitHub Actions, GitLab
CI, or whatever else runs your pipeline — it runs alongside them and records
what they concluded.

## Not a general-purpose workflow engine

Fishhawk models one shape: a software change moving through review. The stage
types, the artifacts, and the gates are specific to that shape. If you need to
orchestrate arbitrary business processes, a general workflow engine is the right
tool and Fishhawk will fight you.

## Not a compliance product you can point at an existing process

The audit log is only worth something if the work actually flowed through the
gates. Fishhawk cannot retroactively certify changes that bypassed it, and it
does not try to. The record covers what it governed.

Next: [your first run](/fishhawk/start/first-run/).
