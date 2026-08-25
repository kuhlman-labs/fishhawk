---
title: Deciding at a gate
description: What each gate asks of you, the evidence to read first, and what a good decision looks like — organised by gate, from plan to merge.
---

A [gate](/fishhawk/concepts/gate/) is a decision only a person can make. This
page is organised by gate: for each, the judgment it asks, the evidence to read
first, and what a good decision looks like. The mechanics of *who* may approve
and *how conditions bind* are at the end, because they apply to every gate.

## The plan gate

You are deciding whether the approach is right **before** an agent spends an hour
implementing it. The question is not "is this plan well-formed" — it is "is this
the change I want, at this scope, verified this way."

Read, in this order:

1. **`scope.files`** against the issue. This is the load-bearing read. A scope
   that is missing a file the change obviously needs, or that reaches into files
   the issue never mentioned, is the cheapest thing to catch here and the most
   expensive to catch later.
2. **The approach** — is it the design you want, or a plausible wrong fork.
3. **The verification** — will the named test actually prove the change, or is it
   a green that leaves the done-means unmet.
4. **The advisory reviews** — two agent readings, surfaced before your decision.
   Treat them as [advisory reviews](/fishhawk/operating/reviews/): read the
   concern notes, not the verdict labels.

The corrections, cheapest first:

- **Reject with a specific reason.** A rejection does not restart the same run in
  place — the run stops, and you start the replacement, with your rejection
  rationale carried into it as prior-rejection feedback. So the useful rejection
  says *what was wrong with the approach*, not just that it was wrong: that
  rationale is the steering input for the next plan.
- **`plan revise` with a constraint** when the approach is close but a specific
  requirement is missing.
- **Approve with conditions** for narrow corrections that do not justify a
  replan.

### Two traps that cost real runs

Both are consequences of the same fact — **an approval reason is binding** — and
both have bitten this repository:

- **The reason is injected verbatim into the implement agent's prompt** as
  conditions it must follow. Write it as an instruction to the implementer, not a
  note to yourself.
- **A repository-relative path written into that prose folds into required
  scope.** Name a file like `dir/thing.go` in an approval reason and it becomes a
  file the stage is required to touch — so a file you named only to *explain*
  something, and the agent then correctly does not modify, fails the stage on
  scope completeness. To add files deliberately, name them explicitly for that
  purpose or use the add-scope-files path; do not sprinkle paths into rationale
  prose.

## The scope-amendment gate

Mid-implementation, the agent found a file it must change that the approved plan
did not declare — a coupled test, a registration table, a doc companion — and
stopped to ask rather than editing it silently (an undeclared edit is dropped
from the commit; an undeclared new file fails the stage).

You are deciding one thing: **is this file a legitimate consequence of the
approved approach, or is it scope creep.** Read the amendment's paths and reason,
approve if the file genuinely follows from the design, deny with a reason if it
does not. Decide promptly — the agent is holding a bounded wait while you decide,
and a slow answer can expire the request and force a retry.

## The scope-completeness gate

A declared scope file was **not** touched, or a binding assertion went
unsatisfied. You are deciding whether the shortfall is benign or real:

- **Benign** — the file genuinely needed no change after the work was done. Mark
  it exempt with a specific reason and the held commit ships.
- **Real** — the change is incomplete. Fail it and replan.

The judgment is whether the done-means is actually met, not whether every
declared path has a diff. A file correctly left untouched is exempt; a file left
untouched because the work was skipped is a fail.

## The review gate

The approval act is here; the *arbitration* of what two reviewers said is its own
subject, on [advisory reviews and disagreement](/fishhawk/operating/reviews/).
At this gate you are recording that the change, as reviewed, is one you approve.

## The merge gate

The last point where the record is still cheap to correct. Approve the pull
request under **your own identity**, and treat the merge verdict as the place you
state what you knowingly merged past — an acceptance that validated nothing, a
concern you waived, a follow-up you filed. A run driven to merge without that
sentence loses the one cheap chance to record the judgment.

## Who may approve

Approving is a **write**, not an acknowledgement that advances a UI. It records
who approved, when, on what artifact version, and with what reason — and that
entry is what a later reader relies on.

By default the change's author cannot approve their own change, and no agent can
approve anything: `not: [author, agent]`. A gate an agent can satisfy is not a
gate. A workflow may raise the bar further for
[sensitive paths](/fishhawk/operating/autonomy/) — more approvals, a named team,
a higher permission level — and that escalation fires on the real change, so a
change reaching into an escalated path cannot be approved under the ordinary bar.

## How conditions bind

A plan approval can carry conditions. They are delivered to the implementing
agent as **mandatory text that wins over the plan** where the two conflict, and
the agent is expected to confirm each one in the pull request. That is exactly
why the plan-gate traps above matter: a condition is not a comment. Use
conditions for the narrow corrections that do not justify a full replan, and
write them as instructions the implementer must follow.
