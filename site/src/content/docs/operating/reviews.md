---
title: Advisory reviews and disagreement
description: Why two reviewers disagreeing is the designed behaviour, how to arbitrate their concerns, and where each disposition is recorded.
---

Every plan and implement stage in the shipped workflow is read by two agent
reviewers before your gate. This page is about what to do with what they say —
especially when they disagree, which is the designed behaviour, not a
malfunction.

## Advisory means the human gate decides

A reviewer has an `authority`: `advisory` or `gating`. The shipped `medium`
preset declares `authority: advisory` **explicitly on both** the plan and the
implement stage. Under advisory authority the agent verdicts *surface* and cannot
block — they are input to your decision at the [gate](/fishhawk/concepts/gate/),
not the decision itself. (A `gating` reviewer, which the preset does not use,
would block the stage on a reject.)

The explicit `authority: advisory` makes visible what the reviewer counts already
resolve to, so a reader of the workflow sees it rather than inferring it. The
canonical shape is
[`workflow-preset-medium.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/backend/internal/spec/presets/workflow-preset-medium.yaml).

## Two reviewers, on purpose, disagreeing on purpose

The preset runs a **heterogeneous pair** — a Claude reviewer and a Codex reviewer
— concurrently. That is deliberate: two independent readings from two different
models carry more information than two identical readers agreeing would. So
**disagreement is a feature of the topology**, not a fault to reconcile away. One
reviewer rejecting while the other approves does not block the run; it hands you
two readings to weigh.

Disagreement is also, sometimes, a plain factual error — a reviewer asserting the
plan omitted something it demonstrably carried. Which leads to the one rule that
matters most here:

> When two reviewers conflict on something **checkable**, check it against the
> artifact. Do not let a confident review override what the plan or the diff
> actually says.

## Arbitrating

1. **Wait for both verdicts.** The review status stays `pending` until *every*
   configured reviewer has landed a terminal verdict, so a single reject you read
   early is not the outcome. Act on the pair.
2. **Read the concern notes verbatim, not the verdict labels.** "Changes
   requested" tells you nothing; the concern text tells you what and why.
3. **Classify each concern.** Is it a *correctness* claim you can check against
   the diff, a *taste* claim, or a claim about something *outside* this change?
4. **Route it:**
   - **Fix-up** if it is convergent and worth an agent pass — re-opens the
     implement stage for another pass against the same run.
   - **Waive** if you judge it wrong, or right but not worth fixing.
   - **Defer** if it is real but belongs in a different change — this files the
     follow-up.

Waive and defer both **write a reason** that a later reader relies on. Record why
you waived, not just that you did.

## Two operational facts that bite

- **The fix-up budget is hard-capped at three passes** per implement stage. Past
  the ceiling, the fix-up verb refuses and the override hint stops offering.
  [When a run fails](/fishhawk/operating/when-a-run-fails/) covers the sanctioned
  remedy past the ceiling — it is not a fourth pass.
- **A reviewer reported as external- or OOM-failed is usually a misclassified
  adapter error**, not a real rejection. Check the underlying error before
  treating it as one; a non-blocking adapter hiccup is not a verdict.

## Reviewer reject and the human page

Two distinct events reach you when a reviewer rejects, and the difference is the
reviewer's authority:

- `advisory_reviewer_reject` — surfaced for your arbitration; the run is not
  blocked.
- `gating_reviewer_reject` — a `page_human_on` event that always reaches a person
  regardless of autonomy tier.

`gating_reviewer_reject` is one of the seven events on the non-delegable page
list, which is why raising your [autonomy tier](/fishhawk/operating/autonomy/)
delegates the clean-path verbs but never disagreement arbitration — that always
stays with you.
